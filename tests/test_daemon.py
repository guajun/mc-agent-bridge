from __future__ import annotations

import asyncio
import contextlib
import json
import os
import tempfile
import unittest
from pathlib import Path

from mc_agent_bridge import fork
from mc_agent_bridge.daemon import BridgeDaemon, categorize, resolve_mod_port
from mc_agent_bridge.local_api import LocalApiClient

from .fake_mod import FakeMod
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

    def test_explicit_port_wins(self) -> None:
        self.assertEqual(resolve_mod_port(1234, None), (1234, "argument"))
        self.assertEqual(resolve_mod_port(None, None)[0], 25580)


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

        self.assertEqual(self.mod.lines, ["SNAPSHOT 0 before"])
        self.assertEqual(ack["type"], "snapshot_ack")
        self.assertEqual(ack["entities"], len(ENTITIES))
        self.assertEqual(ack["orderHash"], fork.order_hash([e["uuid"] for e in ENTITIES]))
        meta, entities = fork.read_snapshot(ack["dir"])
        self.assertEqual(meta["tick"], 104233)
        self.assertEqual([e["uuid"] for e in entities], [e["uuid"] for e in ENTITIES])

    async def test_fork_freezes_snapshots_copies_and_unfreezes_in_order(self) -> None:
        result = await self.api.call("fork", {"name": "before", "radius": 64}, timeout=60)

        self.assertEqual(
            self.mod.lines,
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
        self.assertEqual(self.mod.lines, ["CMD /save-all flush", "SNAPSHOT 0 entities-only"])
        self.assertTrue(result["snapshotDir"])
        self.assertIsNone(result["forkDir"])
        self.assertIsNone(result["manifest"])
        self.assertIsNone(result["worldDir"])
        self.assertEqual(result["orderHash"], fork.order_hash([e["uuid"] for e in ENTITIES]))

    async def test_fork_without_a_name_leaves_the_name_to_the_mod(self) -> None:
        """The consumer's client can omit the name, so the ack has to supply it."""
        result = await self.api.call("fork", {"freeze": False, "regions": False}, timeout=60)

        self.assertEqual(self.mod.lines, ["CMD /save-all flush", "SNAPSHOT"])
        self.assertEqual(result["name"], "snapshot")
        self.assertTrue(result["snapshotDir"].endswith("snapshot"))

    async def test_restore_dry_runs_first_and_then_summons_in_recorded_order(self) -> None:
        ack = await self.api.call("snapshot", {"name": "before"})

        dry = await self.api.call("restore", {"directory": ack["dir"]})
        self.assertTrue(dry["dryRun"])
        self.assertEqual(dry["count"], len(ENTITIES))
        self.assertEqual(dry["commands"], SUMMON_COMMANDS)
        self.assertEqual(self.mod.commands, [], "a dry run must not touch the world")

        result = await self.api.call(
            "restore", {"directory": ack["dir"], "dry_run": False, "target": "lab"}
        )
        self.assertFalse(result["dryRun"])
        self.assertEqual(result["issued"], len(ENTITIES))
        self.assertEqual(result["target"], "lab")
        self.assertEqual(result["orderHash"], ack["orderHash"])
        self.assertEqual(self.mod.commands, SUMMON_COMMANDS)

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
        self.assertEqual(self.mod.lines, [])
