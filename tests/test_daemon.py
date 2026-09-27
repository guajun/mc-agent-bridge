from __future__ import annotations

import asyncio
import contextlib
import json
import os
import tempfile
import unittest
from pathlib import Path

from mc_agent_bridge import fork
from mc_agent_bridge.daemon import BridgeDaemon, categorize
from mc_agent_bridge.local_api import LocalApiClient

from .fake_mod import SERVER_CAPABILITIES, FakeMod
from .test_fork import ENTITIES, SKIP_FILES, WORLD_FILES, tree, write_world


async def wait_for(predicate, timeout: float = 5.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout
    while asyncio.get_running_loop().time() < deadline:
        if predicate():
            return
        await asyncio.sleep(0.02)
    raise AssertionError("condition not met in time")


class CategorizeTests(unittest.TestCase):
    def test_categories(self) -> None:
        self.assertEqual(categorize({"type": "chat"}), "chat")
        self.assertEqual(categorize({"type": "sample_progress"}), "sample")
        self.assertEqual(categorize({"type": "task_error"}), "error")
        self.assertEqual(categorize({"type": "mark"}), "mark")
        self.assertEqual(categorize({"type": "whatever"}), "other")



class DaemonTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.mod = FakeMod()
        await self.mod.start()
        self.daemon = BridgeDaemon(mod_port=self.mod.port, api_port=0, reconnect_delay=0.05)
        self.task = asyncio.create_task(self.daemon.run())
        await wait_for(lambda: self.daemon.server.port != 0)

    async def asyncTearDown(self) -> None:
        await self.daemon.stop()
        with contextlib.suppress(asyncio.CancelledError, TimeoutError):
            await asyncio.wait_for(self.task, timeout=3)
        await self.mod.stop()

    async def client(self) -> LocalApiClient:
        api = LocalApiClient(port=self.daemon.server.port)
        await api.connect(retry=False)
        return api

    async def test_status_and_passthrough(self) -> None:
        api = await self.client()
        try:
            await wait_for(lambda: self.daemon.connected)
            status = await api.call("status")
            self.assertTrue(status["connected"])
            self.assertEqual(status["mod"]["port"], self.mod.port)

            state = await api.call("state")
            self.assertEqual(state["type"], "state")
            await api.call("chat", {"message": "hello there"})
            await api.call("entities", {"radius": 32})
            await api.call("record_start", {"ticks": 10, "radius": 8, "interval": 2})
            await api.call("wait", {"ticks": 1})
        finally:
            await api.close()

        self.assertIn("CHAT hello there", self.mod.lines)
        self.assertIn("ENTITIES 32.0", self.mod.lines)
        self.assertIn("SAMPLE_START 10 8.0 2", self.mod.lines)
        self.assertIn("WAIT 1", self.mod.lines)

    async def test_events_are_buffered_and_pushed(self) -> None:
        api = await self.client()
        try:
            await api.call("subscribe", {"events": ["chat"]})
            await wait_for(lambda: self.daemon.connected)
            await self.mod.push({"type": "chat", "text": "ping", "sender": "tester"})
            queue = await api.events()
            message = await asyncio.wait_for(queue.get(), timeout=5)
            self.assertEqual(message["event"], "chat")
            self.assertEqual(message["data"]["text"], "ping")

            replay = await api.call("events", {"since": 0, "category": "chat"})
            self.assertEqual(replay["events"][-1]["text"], "ping")
            self.assertFalse(replay["dropped"])
        finally:
            await api.close()

    async def test_events_carry_a_stable_stream_and_event_id(self) -> None:
        api = await self.client()
        try:
            await wait_for(lambda: self.daemon.connected)
            await self.mod.push({"type": "chat", "text": "ping", "sender": "tester"})
            await wait_for(lambda: bool(self.daemon.recent_events(category="chat")["events"]))

            event = (await api.call("events", {"category": "chat"}))["events"][-1]
            self.assertEqual(event["streamId"], self.daemon.stream_id)
            self.assertEqual(event["eventId"], f"{self.daemon.stream_id}:{event['seq']}")
            status = await api.call("status")
            self.assertEqual(status["events"]["streamId"], self.daemon.stream_id)
        finally:
            await api.close()

    async def test_unknown_method_and_missing_mod(self) -> None:
        api = await self.client()
        try:
            with self.assertRaises(RuntimeError):
                await api.call("nope")
            await self.mod.stop()
            await wait_for(lambda: not self.daemon.connected)
            with self.assertRaises(RuntimeError):
                await api.call("state")
        finally:
            await api.close()


SUMMON_COMMANDS = [
    "/summon minecraft:sulfur_cube 103.5 56.0 52.5 {Motion:[0.0d,-0.0784d,0.0d]}",
    "/summon minecraft:tnt 12.0 70.0 -3.25 {fuse:80s}",
]


class ForkDaemonTests(unittest.IsolatedAsyncioTestCase):
    """The fork path end to end: freeze, snapshot, copy, unfreeze - no game."""

    async def asyncSetUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.world = self.root / "saves" / "live"
        self.snapshots = self.root / "snapshots"
        write_world(str(self.world))
        self.mod = FakeMod(
            world_dir=self.world,
            snapshots_dir=self.snapshots,
            entities=ENTITIES,
            tick=104233,
            instance="server",
            capabilities=SERVER_CAPABILITIES,
        )
        await self.mod.start()
        self.daemon = BridgeDaemon(mod_port=self.mod.port, api_port=0, reconnect_delay=0.05)
        self.task = asyncio.create_task(self.daemon.run())
        await wait_for(lambda: self.daemon.server.port != 0)
        await wait_for(lambda: self.daemon.connected)
        self.api = LocalApiClient(port=self.daemon.server.port)
        await self.api.connect(retry=False)

    async def asyncTearDown(self) -> None:
        await self.api.close()
        await self.daemon.stop()
        with contextlib.suppress(asyncio.CancelledError, TimeoutError):
            await asyncio.wait_for(self.task, timeout=3)
        await self.mod.stop()
        self.tmp.cleanup()

    async def test_snapshot_writes_the_entity_set_and_reports_its_order_hash(self) -> None:
        ack = await self.api.call("snapshot", {"name": "before"}, timeout=30)

        self.assertEqual(self.mod.requests, ["SNAPSHOT 0 before"])
        self.assertEqual(ack["type"], "snapshot_ack")
        self.assertEqual(ack["entities"], len(ENTITIES))
        self.assertEqual(ack["orderHash"], fork.order_hash([e["uuid"] for e in ENTITIES]))
        meta, entities = fork.read_snapshot(ack["dir"])
        self.assertEqual(meta["tick"], 104233)
        self.assertEqual([e["uuid"] for e in entities], [e["uuid"] for e in ENTITIES])

    async def test_fork_freezes_snapshots_copies_and_unfreezes_in_order(self) -> None:
        result = await self.api.call("fork", {"name": "before", "radius": 64}, timeout=60)

        self.assertEqual(
            self.mod.requests,
            [
                "CMD /tick freeze",
                "CMD /save-all flush",
                "SNAPSHOT 64.0 before",
                "STATE",
                "CMD /tick unfreeze",
            ],
        )

        snapshot_dir = Path(result["snapshotDir"])
        self.assertEqual(snapshot_dir, self.snapshots / "before")
        self.assertEqual(result["forkDir"], str(snapshot_dir / "world"))
        self.assertEqual(result["worldDir"], str(self.world))
        self.assertEqual(result["orderHash"], fork.order_hash([e["uuid"] for e in ENTITIES]))
        self.assertEqual(result["entities"], len(ENTITIES))

        manifest = result["manifest"]
        self.assertEqual(manifest["files"], len(WORLD_FILES))
        self.assertEqual(manifest["bytes"], sum(len(c) for c in WORLD_FILES.values()))
        self.assertEqual(
            manifest["skipped"],
            [
                "advancements",
                "dimensions/minecraft/overworld/logs",
                "logs",
                "playerdata",
                "players",
                "session.lock",
                "stats",
            ],
        )
        copied = tree(result["forkDir"])
        self.assertEqual(copied, set(WORLD_FILES))
        for left_behind in SKIP_FILES:
            self.assertNotIn(left_behind, copied)

    async def test_fork_uses_an_explicit_world_dir_without_asking_the_instance(self) -> None:
        other = self.root / "other-world"
        write_world(str(other))

        result = await self.api.call(
            "fork", {"name": "explicit", "world_dir": str(other)}, timeout=60
        )

        self.assertNotIn("STATE", self.mod.lines)
        self.assertEqual(result["worldDir"], str(other))
        self.assertEqual(tree(result["forkDir"]), set(WORLD_FILES))

    async def test_fork_unfreezes_when_the_snapshot_fails(self) -> None:
        self.mod.snapshot_error = "cannot write the snapshot: disk full"

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("fork", {"name": "before"})

        self.assertIn("disk full", str(caught.exception))
        self.assertEqual(
            self.mod.commands, ["/tick freeze", "/save-all flush", "/tick unfreeze"]
        )
        self.assertFalse((self.snapshots / "before" / "world").exists())

    async def test_fork_names_the_state_field_and_the_parameter_when_the_dir_is_unknown(
        self,
    ) -> None:
        self.mod.world_dir = None

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("fork", {"name": "before"})

        message = str(caught.exception)
        self.assertIn("worldDir", message)
        self.assertIn("world_dir", message)
        self.assertEqual(self.mod.commands[-1], "/tick unfreeze")

    async def test_fork_without_freezing_or_regions_is_an_entity_only_fork(self) -> None:
        result = await self.api.call(
            "fork", {"name": "entities-only", "freeze": False, "regions": False}, timeout=60
        )

        # Only the tick commands are conditional: the flush is what puts the
        # chunks on disk whether or not this call did the freezing.
        self.assertEqual(self.mod.requests, ["CMD /save-all flush", "SNAPSHOT 0 entities-only"])
        self.assertTrue(result["snapshotDir"])
        self.assertIsNone(result["forkDir"])
        self.assertIsNone(result["manifest"])
        self.assertIsNone(result["worldDir"])
        self.assertEqual(result["orderHash"], fork.order_hash([e["uuid"] for e in ENTITIES]))

    async def test_fork_without_a_name_leaves_the_name_to_the_mod(self) -> None:
        """The consumer's client can omit the name, so the ack has to supply it."""
        result = await self.api.call("fork", {"freeze": False, "regions": False}, timeout=60)

        self.assertEqual(self.mod.requests, ["CMD /save-all flush", "SNAPSHOT"])
        self.assertEqual(result["name"], "snapshot")
        self.assertTrue(result["snapshotDir"].endswith("snapshot"))

    async def test_restore_dry_runs_first_and_then_summons_in_recorded_order(self) -> None:
        ack = await self.api.call("snapshot", {"name": "before"})

        dry = await self.api.call("restore", {"directory": ack["dir"]})
        self.assertTrue(dry["dryRun"])
        self.assertEqual(dry["count"], len(ENTITIES))
        self.assertEqual(dry["commands"], SUMMON_COMMANDS)
        self.assertTrue(dry["targetIsLabel"])
        self.assertEqual(self.mod.commands, [], "a dry run must not touch the world")

        # The destination is an empty lab that loaded the copied world; the
        # guarded apply must prove the endpoint, freeze, restore and compare.
        self.mod.entity_records = []
        self.mod.summon_uuids = [entity["uuid"] for entity in ENTITIES]
        result = await self.api.call(
            "restore",
            {
                "directory": ack["dir"],
                "dry_run": False,
                "target": "lab",
                "expect_instance": "server",
                "expect_world_dir": str(self.world),
            },
        )
        self.assertFalse(result["dryRun"])
        self.assertEqual(result["issued"], len(ENTITIES))
        self.assertEqual(result["target"], "lab")
        self.assertEqual(result["orderHash"], ack["orderHash"])
        self.assertEqual(result["failed"], [])
        self.assertTrue(result["ok"], result["verification"])
        self.assertEqual(
            [command for command in self.mod.commands if command.startswith("/summon ")],
            SUMMON_COMMANDS,
        )
        self.assertIn("/tick freeze", self.mod.commands)
        self.assertIn("/tick unfreeze", self.mod.commands)
        self.assertEqual(self.mod.entity_records[0]["uuid"], ENTITIES[0]["uuid"])

    async def test_order_matches_the_fork_and_reports_a_different_order(self) -> None:
        ack = await self.api.call("snapshot", {"name": "before"})
        expected = fork.order_hash([e["uuid"] for e in ENTITIES])

        result = await self.api.call("order", {"directory": ack["dir"], "target": "lab"})
        self.assertTrue(result["match"])
        self.assertEqual(result["expected"], expected)
        self.assertEqual(result["actual"], expected)
        self.assertEqual(result["entities"], len(ENTITIES))
        self.assertTrue(result["snapshotDir"].endswith("order-lab"))

        self.mod.entity_records = list(reversed(self.mod.entity_records))
        moved = await self.api.call("order", {"directory": ack["dir"], "target": "lab"})
        self.assertFalse(moved["match"])
        self.assertEqual(moved["expected"], expected)
        self.assertEqual(
            moved["actual"], fork.order_hash([e["uuid"] for e in reversed(ENTITIES)])
        )

    async def test_order_needs_a_hash_to_compare_with(self) -> None:
        directory = self.root / "handwritten"
        os.makedirs(directory, exist_ok=True)
        with open(directory / "meta.json", "w", encoding="utf-8") as handle:
            json.dump({"protocol": 1, "entities": 0}, handle)

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("order", {"directory": str(directory)})
        self.assertIn("orderHash", str(caught.exception))

    async def test_snapshots_lists_what_the_instance_has(self) -> None:
        await self.api.call("snapshot", {"name": "one"})
        await self.api.call("snapshot", {"name": "two", "radius": 32})

        listing = await self.api.call("snapshots")
        self.assertEqual([entry["name"] for entry in listing["snapshots"]], ["one", "two"])
        self.assertEqual(listing["snapshots"][0]["entities"], len(ENTITIES))
        self.assertIn("SNAPSHOT 32.0 two", self.mod.lines)

        # The mod may name the listing reply either way; both must reach the caller.
        self.mod.snapshots_reply_type = "snapshots_ack"
        listing = await self.api.call("snapshots")
        self.assertEqual([entry["name"] for entry in listing["snapshots"]], ["one", "two"])

    async def test_a_snapshot_name_must_stay_one_token(self) -> None:
        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("snapshot", {"name": "before the fight"})
        self.assertIn("spaces", str(caught.exception))
        self.assertEqual(self.mod.requests, [])



class ServerVantageDaemonTests(unittest.IsolatedAsyncioTestCase):
    """The daemon's server-first behavior: discovery, status and capability gate."""

    async def asyncSetUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.mod = FakeMod(
            instance="server",
            capabilities=SERVER_CAPABILITIES,
            snapshots_dir=self.root / "snapshots",
        )
        await self.mod.start()
        self.daemon: BridgeDaemon | None = None
        self.task: asyncio.Task | None = None

    async def asyncTearDown(self) -> None:
        if self.daemon is not None:
            await self.daemon.stop()
        if self.task is not None:
            with contextlib.suppress(asyncio.CancelledError, TimeoutError):
                await asyncio.wait_for(self.task, timeout=3)
        await self.mod.stop()
        self.tmp.cleanup()

    async def start_daemon(self, with_port_file: bool = True) -> BridgeDaemon:
        if with_port_file:
            port_dir = self.root / "mc-agent-server"
            port_dir.mkdir(exist_ok=True)
            (port_dir / "port.txt").write_text(str(self.mod.port), encoding="utf-8")
        self.daemon = BridgeDaemon(
            server_dir=str(self.root), api_port=0, reconnect_delay=0.05
        )
        self.task = asyncio.create_task(self.daemon.run())
        await wait_for(lambda: self.daemon.server.port != 0)
        return self.daemon

    async def client(self) -> LocalApiClient:
        assert self.daemon is not None
        api = LocalApiClient(port=self.daemon.server.port)
        await api.connect(retry=False)
        return api

    async def test_missing_port_file_is_an_actionable_error_and_not_a_fallback(self) -> None:
        daemon = await self.start_daemon(with_port_file=False)
        await wait_for(lambda: daemon.last_error is not None)
        self.assertFalse(daemon.connected)
        self.assertIsNone(daemon.mod_port)
        error = daemon.last_error or ""
        self.assertIn("--port-file", error)
        self.assertIn("--vantage client", error)

        api = await self.client()
        try:
            status = await api.call("status")
            self.assertEqual(status["vantage"], "server")
            self.assertFalse(status["connected"])
            self.assertIsNone(status["discovery"]["port"])
            self.assertIn("--port-file", status["discovery"]["error"])
        finally:
            await api.close()

    async def test_the_port_file_is_picked_up_when_it_appears(self) -> None:
        daemon = await self.start_daemon(with_port_file=False)
        await wait_for(lambda: daemon.last_error is not None)
        port_dir = self.root / "mc-agent-server"
        port_dir.mkdir()
        (port_dir / "port.txt").write_text(str(self.mod.port), encoding="utf-8")

        await wait_for(lambda: daemon.connected and daemon.mod_capabilities is not None)
        self.assertEqual(daemon.mod_port, self.mod.port)
        self.assertEqual(daemon.instance, "server")
        self.assertTrue(daemon.mod_capabilities)
        self.assertEqual(daemon.discovery.source, f"port-file:{port_dir / 'port.txt'}")

    async def test_status_and_capabilities_report_the_filtered_surface(self) -> None:
        daemon = await self.start_daemon()
        await wait_for(lambda: daemon.mod_capabilities is not None)
        api = await self.client()
        try:
            status = await api.call("status")
            self.assertEqual(status["vantage"], "server")
            self.assertEqual(status["instance"], "server")
            capabilities = await api.call("capabilities")
            self.assertEqual(capabilities["instance"], "server")
            self.assertEqual(capabilities["surface"]["instance"], "server")
            self.assertIn("snapshot", capabilities["surface"]["supported"])
            self.assertIn("chat", capabilities["surface"]["unsupported"])
            operations = {entry["name"]: entry for entry in capabilities["surface"]["operations"]}
            self.assertEqual(operations["player"]["dependency"].split("/")[-1], "1")
            self.assertEqual(operations["context"]["dependency"].split("/")[-1], "2")
        finally:
            await api.close()

    async def test_a_client_only_operation_is_refused_before_reaching_the_mod(self) -> None:
        daemon = await self.start_daemon()
        await wait_for(lambda: daemon.mod_capabilities is not None)
        api = await self.client()
        try:
            with self.assertRaises(RuntimeError) as caught:
                await api.call("chat", {"message": "hello"})
            self.assertIn("chat", str(caught.exception))
            self.assertFalse(any(line.startswith("CHAT ") for line in self.mod.lines))
        finally:
            await api.close()

    async def test_server_operations_still_pass_through(self) -> None:
        daemon = await self.start_daemon()
        await wait_for(lambda: daemon.mod_capabilities is not None)
        api = await self.client()
        try:
            await api.call("snapshot", {"name": "srv"})
            self.assertTrue(any(line.startswith("SNAPSHOT") for line in self.mod.lines))
        finally:
            await api.close()

    async def test_operations_are_refused_while_capability_negotiation_is_pending(self) -> None:
        self.mod.caps_delay = 0.6
        daemon = await self.start_daemon()
        # The hello frame has arrived and the CAPS request is in flight, but
        # readiness is not published yet: no game operation may slip through
        # ungated and be answered after CAPS completes.
        await wait_for(lambda: daemon.client is not None and not daemon.connected)
        api = await self.client()
        try:
            with self.assertRaises(RuntimeError) as pending:
                await api.call("chat", {"message": "should-be-blocked"})
            self.assertIn("negotiating", str(pending.exception))
            self.assertFalse(any(line.startswith("CHAT ") for line in self.mod.lines))

            await wait_for(lambda: daemon.connected and daemon.mod_capabilities is not None)
            with self.assertRaises(RuntimeError) as gated:
                await api.call("chat", {"message": "still-blocked"})
            self.assertIn("not available", str(gated.exception))
            self.assertFalse(any(line.startswith("CHAT ") for line in self.mod.lines))
        finally:
            await api.close()
