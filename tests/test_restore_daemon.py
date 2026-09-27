from __future__ import annotations

import asyncio
import contextlib
import json
import tempfile
import unittest
from pathlib import Path

from mc_agent_bridge import fork
from mc_agent_bridge.daemon import BridgeDaemon
from mc_agent_bridge.local_api import LocalApiClient

from .fake_mod import SERVER_CAPABILITIES, FakeMod


def uuid_ints(value: str) -> list[int]:
    plain = value.replace("-", "")
    return [
        int.from_bytes(bytes.fromhex(plain[index : index + 8]), "big", signed=True)
        for index in range(0, 32, 8)
    ]


def cart(uuid: str, x: float, y: float, item: str, count: int, motion=(0.0, 0.0, 0.0)) -> dict:
    ints = ",".join(str(value) for value in uuid_ints(uuid))
    nbt = (
        '{"Items":[{"Slot":0b,"id":"%s","count":%d}],'
        '"Motion":[%sd,%sd,%sd],"UUID":[I;%s]}' % (item, count, motion[0], motion[1], motion[2], ints)
    )
    return {
        "order": 0,
        "uuid": uuid,
        "type": "minecraft:chest_minecart",
        "pos": [x, y, -3.5],
        "vel": list(motion),
        "nbt": nbt,
        "passengers": [],
        "vehicle": None,
        "restorable": True,
    }


CART_A = cart("11111111-1111-4111-8111-111111111111", 10.5, 64.0, "minecraft:diamond", 5)
CART_B = cart("22222222-2222-4222-8222-222222222222", 10.5, 65.0, "minecraft:emerald", 2)
PASSENGER = {
    "order": 0,
    "uuid": "33333333-3333-4333-8333-333333333333",
    "type": "minecraft:zombie",
    "pos": [10.5, 66.0, -3.5],
    "vel": [0.0, 0.0, 0.0],
    "nbt": '{"UUID":[I;1,2,3,4]}',
    "passengers": [],
    "vehicle": CART_B["uuid"],
    "restorable": False,
}
for index, entity in enumerate([CART_A, CART_B, PASSENGER]):
    entity["order"] = index

FIXTURE = [CART_A, CART_B]
WITH_PASSENGER = [CART_A, CART_B, PASSENGER]


async def wait_for(predicate, timeout: float = 5.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout
    while asyncio.get_running_loop().time() < deadline:
        if predicate():
            return
        await asyncio.sleep(0.02)
    raise AssertionError("condition not met in time")


class GuardedRestoreDaemonTests(unittest.IsolatedAsyncioTestCase):
    """The guarded restore flow against the fake mod: guards, evidence, verdicts."""

    async def asyncSetUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.world = self.root / "saves" / "dst"
        self.world.mkdir(parents=True)
        self.snapshots = self.root / "snapshots"
        self.mod = FakeMod(
            world_dir=self.world,
            snapshots_dir=self.snapshots,
            entities=list(FIXTURE),
            tick=1000,
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

    async def fork_fixture(self, name: str = "fixture") -> str:
        ack = await self.api.call("snapshot", {"name": name}, timeout=30)
        return ack["dir"]

    def destination_is_empty(self) -> None:
        self.mod.entity_records = []

    async def test_dry_run_shows_the_commands_and_proves_the_endpoint(self) -> None:
        directory = await self.fork_fixture()
        self.mod.lines.clear()

        result = await self.api.call(
            "restore",
            {
                "directory": directory,
                "target": "lab-b",
                "expect_instance": "server",
                "expect_world_dir": str(self.world),
            },
        )

        self.assertTrue(result["dryRun"])
        self.assertTrue(result["targetIsLabel"])
        self.assertEqual(result["target"], "lab-b")
        self.assertEqual(result["count"], 2)
        self.assertEqual(result["summonable"], 2)
        self.assertEqual(len(result["commands"]), 2)
        self.assertTrue(result["commands"][0].startswith("/summon minecraft:chest_minecart 10.5 64.0 -3.5 "))
        self.assertTrue(result["checks"]["endpoint"]["verified"])
        # A dry run mutated nothing, so it is not a restore success either.
        self.assertIsNone(result["ok"])
        self.assertFalse(result["verified"])
        self.assertEqual(result["verdict"], "dry-run")
        self.assertEqual(self.mod.commands, [], "a dry run must not send a command")

    async def test_wrong_endpoint_is_refused_before_anything_is_sent(self) -> None:
        directory = await self.fork_fixture()
        self.mod.lines.clear()

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call(
                "restore",
                {"directory": directory, "dry_run": False, "expect_instance": "lab-b"},
            )
        self.assertIn("expect_instance", str(caught.exception))
        self.assertIn("does not route", str(caught.exception))

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call(
                "restore",
                {
                    "directory": directory,
                    "dry_run": False,
                    "expect_world_dir": str(self.root / "saves" / "src"),
                },
            )
        self.assertIn("expect_world_dir", str(caught.exception))
        self.assertEqual(self.mod.commands, [])

    async def test_corrupt_metadata_is_refused_before_any_command(self) -> None:
        directory = self.root / "corrupt"
        directory.mkdir()
        (directory / "entities.jsonl").write_text(
            "\n".join(json.dumps(entity) for entity in FIXTURE) + "\n", encoding="utf-8"
        )
        meta = {
            "protocol": 1,
            "entities": 2,
            "orderHash": "0" * 16,
            "dimension": "minecraft:overworld",
        }
        (directory / "meta.json").write_text(json.dumps(meta), encoding="utf-8")
        self.mod.lines.clear()

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("restore", {"directory": str(directory), "dry_run": False, "expect_world_dir": str(self.world)})
        self.assertIn("order_hash", str(caught.exception))
        self.assertEqual(self.mod.commands, [])

    async def test_wrong_dimension_is_refused_before_the_summons(self) -> None:
        self.mod.dimension = "minecraft:the_nether"
        directory = await self.fork_fixture()
        self.mod.dimension = "minecraft:overworld"
        self.destination_is_empty()
        self.mod.lines.clear()

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world)})
        self.assertIn("dimension", str(caught.exception))
        self.assertEqual(
            [command for command in self.mod.commands if command.startswith("/summon ")], []
        )
        self.assertIn("/tick unfreeze", self.mod.commands)
        self.assertFalse(self.mod.tick_frozen, "a refusal must not leave the world frozen")

    async def test_pre_existing_duplicates_are_refused_with_evidence(self) -> None:
        directory = await self.fork_fixture()
        self.mod.lines.clear()

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world)})
        message = str(caught.exception)
        self.assertIn("duplicate", message)
        self.assertIn("uuid collisions: 2", message)
        self.assertEqual(
            [command for command in self.mod.commands if command.startswith("/summon ")], []
        )
        self.assertEqual(len(self.mod.entity_records), 2, "the destination was not mutated")
        self.assertIn("/tick unfreeze", self.mod.commands)
        self.assertFalse(self.mod.tick_frozen, "a refusal must not leave the world frozen")

    async def test_replace_existing_clears_and_flushes_before_restoring(self) -> None:
        directory = await self.fork_fixture()
        self.mod.snapshot_tick_states.clear()
        events: list[tuple[str, bool]] = []

        def on_command(command: str) -> None:
            events.append((command, self.mod.tick_frozen))
            if command.startswith("kill "):
                self.mod.entity_records = []

        self.mod.on_command = on_command
        self.mod.lines.clear()

        result = await self.api.call(
            "restore",
            {
                "directory": directory,
                "dry_run": False,
                "replace_existing": True,
                "expect_world_dir": str(self.world),
            },
        )

        self.assertTrue(result["ok"], result["verification"])
        self.assertEqual(result["checks"]["existing"]["action"], "cleared+flushed")
        self.assertEqual(result["checks"]["existing"]["remainingCollisions"], 0)
        self.assertEqual(result["checks"]["existing"]["remainingAtRecordedPositions"], 0)
        self.assertEqual(result["issued"], 2)
        self.assertEqual(result["failed"], [])
        self.assertEqual(result["verdict"], "ok")
        self.assertTrue(result["verified"])

        def first(prefix: str) -> int:
            return next(index for index, event in enumerate(events) if event[0].startswith(prefix))

        def last(prefix: str) -> int:
            return next(
                index
                for index in range(len(events) - 1, -1, -1)
                if events[index][0].startswith(prefix)
            )

        # Freeze first so the evidence cannot age, unfreeze only for the kill
        # (frozen kills leave dying entities and drops), re-freeze before the
        # recheck, summon while frozen, and unfreeze last because we froze.
        self.assertLess(first("tick freeze"), first("forceload"))
        self.assertLess(first("tick freeze"), first("tick unfreeze"))
        self.assertLess(first("tick unfreeze"), first("kill "))
        self.assertLess(first("kill "), first("save-all flush"))
        self.assertLess(first("save-all flush"), last("tick freeze"))
        self.assertLess(last("tick freeze"), first("summon "))
        self.assertLess(last("summon "), last("tick unfreeze"))
        self.assertEqual(events[first("kill ")][1], False, "the kill must run while not frozen")
        self.assertEqual(events[first("save-all flush")][1], False)
        self.assertEqual(result["tick"]["clearedWhileRunning"], True)
        self.assertTrue(result["tick"]["refrozen"])
        self.assertTrue(result["tick"]["preserved"])
        # Baseline, recheck and verification snapshots were taken frozen.
        self.assertEqual(self.mod.snapshot_tick_states, [True, True, True])

    async def test_a_failed_summon_is_reported_and_the_verdict_is_false(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.failing_commands["minecraft:diamond"] = "Unable to summon entity"
        self.mod.lines.clear()

        result = await self.api.call("restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world)})

        self.assertFalse(result["ok"])
        self.assertTrue(result["partial"])
        # The batch runs; the silent failure is caught by the post-restore
        # snapshot and reported with the game's message as evidence.
        self.assertEqual(result["issued"], 2)
        self.assertEqual(len(result["failed"]), 1)
        failure = result["failed"][0]
        self.assertEqual(failure["uuid"], CART_A["uuid"])
        self.assertIn("not present", failure["error"])
        self.assertEqual(failure["gameOutput"], ["Unable to summon entity"])
        self.assertEqual(result["notAttempted"], 0)
        self.assertEqual(result["verification"]["missing"]["count"], 1)

    async def test_keep_going_records_every_failure(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.failing_commands["minecraft:diamond"] = "refused"
        self.mod.failing_commands["minecraft:emerald"] = "refused too"

        result = await self.api.call(
            "restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world), "keep_going": True}
        )
        self.assertEqual(len(result["failed"]), 2)
        self.assertEqual(result["issued"], 2)
        self.assertEqual(result["notAttempted"], 0)

    async def test_passengers_are_not_summoned_and_are_reported(self) -> None:
        self.mod.entity_records = list(WITH_PASSENGER)
        directory = await self.fork_fixture("with-passenger")
        self.destination_is_empty()
        self.mod.lines.clear()

        result = await self.api.call(
            "restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world), "verify": False}
        )

        self.assertEqual(result["issued"], 2)
        self.assertEqual(result["count"], 3)
        self.assertEqual(result["summonable"], 2)
        self.assertEqual(len(result["skipped"]), 1)
        self.assertEqual(result["skipped"][0]["uuid"], PASSENGER["uuid"])
        # Verification was disabled, so the result is explicitly unverified.
        self.assertIsNone(result["ok"])
        self.assertFalse(result["verified"])
        self.assertEqual(result["verdict"], "unverified")
        summons = [command for command in self.mod.commands if command.startswith("/summon ")]
        self.assertEqual(len(summons), 2)
        self.assertNotIn(PASSENGER["uuid"], " ".join(summons))

    async def test_frozen_state_before_the_restore_is_left_alone(self) -> None:
        self.mod.tick_frozen = True
        directory = await self.fork_fixture()
        self.destination_is_empty()

        result = await self.api.call("restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world)})

        self.assertTrue(result["ok"], result["verification"])
        self.assertEqual(result["tick"]["prior"], "frozen")
        self.assertEqual(result["tick"]["priorSource"], "query")
        self.assertFalse(result["tick"]["frozenByBridge"])
        self.assertTrue(result["tick"]["preserved"])
        self.assertNotIn("/tick freeze", self.mod.commands)
        self.assertNotIn("/tick unfreeze", self.mod.commands)

    async def test_running_state_is_frozen_for_the_restore_and_restored(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()

        result = await self.api.call("restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world)})

        self.assertEqual(result["tick"]["prior"], "running")
        self.assertEqual(result["tick"]["priorSource"], "query")
        self.assertTrue(result["tick"]["frozenByBridge"])
        self.assertTrue(result["tick"]["restored"])
        self.assertTrue(result["tick"]["preserved"])
        self.assertIn("/tick freeze", self.mod.commands)
        self.assertIn("/tick unfreeze", self.mod.commands)
        self.assertFalse(self.mod.tick_frozen)

    async def test_verification_disabled_is_never_a_success(self) -> None:
        """P1 regression: verify=false must never report ok=true.

        The destination is missing an entity on purpose; with verification
        skipped the bridge cannot know, so the honest verdict is unverified,
        not a successful restore.
        """
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.failing_commands["minecraft:diamond"] = "Unable to summon entity"
        self.mod.lines.clear()

        result = await self.api.call(
            "restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world), "verify": False}
        )

        self.assertIsNot(result["ok"], True, "commands issued is not restored state")
        self.assertIsNone(result["ok"])
        self.assertFalse(result["verified"])
        self.assertEqual(result["verdict"], "unverified")
        self.assertIsNone(result["verification"])
        self.assertEqual(result["issued"], 2)
        self.assertEqual(result["failed"], [])
        self.assertIn("not a claim", result["note"])

    async def test_verification_disabled_still_reports_transport_failures(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.raising_commands["minecraft:diamond"] = "the game refused the command"

        result = await self.api.call(
            "restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world), "verify": False}
        )

        self.assertIs(result["ok"], False)
        self.assertFalse(result["verified"])
        self.assertEqual(result["verdict"], "failed")
        self.assertEqual(len(result["failed"]), 1)
        self.assertIn("refused", result["failed"][0]["error"])
        self.assertEqual(result["notAttempted"], 1, "fail-fast stops the batch")

    async def test_unknown_prior_tick_state_refuses_before_mutation(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.tick_query_output = ["参数错误"]
        self.mod.lines.clear()

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world)})
        self.assertIn("cannot determine", str(caught.exception))
        self.assertEqual(self.mod.commands, ["/tick query"], "nothing after the failed query")
        self.assertFalse((self.snapshots / "restore-pre").exists())
        self.assertEqual(len(self.mod.entity_records), 0)

    async def test_prior_tick_state_can_be_stated_explicitly(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.tick_query_output = ["参数错误"]
        self.mod.lines.clear()

        result = await self.api.call(
            "restore",
            {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world), "prior_tick_state": "running"},
        )
        self.assertEqual(result["tick"]["prior"], "running")
        self.assertEqual(result["tick"]["priorSource"], "caller")
        self.assertTrue(result["ok"], result["verification"])
        self.assertNotIn("/tick query", self.mod.commands)
        self.assertIn("/tick freeze", self.mod.commands)

        # A stated frozen state is left frozen: no freeze and no unfreeze.
        self.mod.tick_frozen = True
        self.mod.entity_records = []
        self.mod.lines.clear()
        result = await self.api.call(
            "restore",
            {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world), "prior_tick_state": "frozen"},
        )
        self.assertEqual(result["tick"]["priorSource"], "caller")
        self.assertFalse(result["tick"]["frozenByBridge"])
        self.assertTrue(result["tick"]["preserved"])
        self.assertTrue(result["ok"], result["verification"])
        self.assertNotIn("/tick query", self.mod.commands)
        self.assertNotIn("/tick freeze", self.mod.commands)
        self.assertNotIn("/tick unfreeze", self.mod.commands)

    async def test_an_invalid_prior_tick_state_is_a_parameter_error(self) -> None:
        directory = await self.fork_fixture()
        with self.assertRaises(RuntimeError) as caught:
            await self.api.call(
                "restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world), "prior_tick_state": "maybe"}
            )
        self.assertIn("prior_tick_state", str(caught.exception))

    async def test_a_failed_verification_snapshot_is_not_a_success(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()

        def fail_the_next_snapshot(command: str) -> None:
            if command.startswith("summon "):
                self.mod.snapshot_error = "disk full"

        self.mod.on_command = fail_the_next_snapshot
        self.mod.lines.clear()

        result = await self.api.call("restore", {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world)})

        self.assertFalse(result["ok"])
        self.assertEqual(result["issued"], 2)
        self.assertIn("verification snapshot failed", result["verification"]["error"])

    async def test_replacement_refuses_when_collisions_remain_after_clearing(self) -> None:
        directory = await self.fork_fixture()
        first_kill = {"done": False}

        def on_command(command: str) -> None:
            if command.startswith("kill ") and not first_kill["done"]:
                first_kill["done"] = True
                # Killing a chest minecart drops its inventory; if the drop
                # cannot be removed, the restore must refuse.
                drop = dict(CART_A, uuid="99999999-9999-4999-8999-999999999999", type="minecraft:item")
                self.mod.entity_records = [drop]
                self.mod.summon_uuids = []

        self.mod.on_command = on_command
        self.mod.lines.clear()

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call(
                "restore",
                {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world), "replace_existing": True},
            )
        self.assertIn("still at the recorded positions", str(caught.exception))
        self.assertEqual(
            [command for command in self.mod.commands if command.startswith("/summon ")], []
        )
        self.assertIn("/tick unfreeze", self.mod.commands)
        self.assertFalse(self.mod.tick_frozen, "a refusal must not leave the world frozen")

    async def test_apply_without_an_endpoint_proof_refuses_before_any_command(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.lines.clear()

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call("restore", {"directory": directory, "dry_run": False})
        self.assertIn("endpoint proof", str(caught.exception))
        self.assertEqual(self.mod.commands, [], "nothing may be sent to an unproven destination")
        self.assertEqual(len(self.mod.entity_records), 0)

        result = await self.api.call(
            "restore",
            {
                "directory": directory,
                "dry_run": False,
                "allow_unproven_destination": True,
            },
        )
        self.assertEqual(result["checks"]["endpoint"]["action"], "allowed (allow_unproven_destination=true)")
        self.assertTrue(result["ok"], result["verification"])

    async def test_apply_refuses_when_state_fails_and_a_proof_was_given(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.state_error = "STATE unavailable"
        self.mod.lines.clear()

        with self.assertRaises(RuntimeError) as caught:
            await self.api.call(
                "restore",
                {"directory": directory, "dry_run": False, "expect_world_dir": str(self.world)},
            )
        self.assertIn("cannot verify the destination endpoint", str(caught.exception))
        self.assertEqual(self.mod.commands, [])

        allowed = await self.api.call(
            "restore",
            {
                "directory": directory,
                "dry_run": False,
                "allow_unproven_destination": True,
            },
        )
        self.assertTrue(allowed["checks"]["endpoint"]["overridden"])
        self.assertTrue(allowed["ok"], allowed["verification"])

    async def test_replace_existing_without_check_existing_is_a_parameter_error(self) -> None:
        directory = await self.fork_fixture()
        with self.assertRaises(RuntimeError) as caught:
            await self.api.call(
                "restore",
                {
                    "directory": directory,
                    "dry_run": False,
                    "expect_world_dir": str(self.world),
                    "check_existing": False,
                    "replace_existing": True,
                },
            )
        self.assertIn("replace_existing=true requires check_existing=true", str(caught.exception))
        self.assertEqual(self.mod.commands, [])

    async def test_narrow_clear_targets_only_the_collision_positions(self) -> None:
        directory = await self.fork_fixture()
        self.mod.on_command = lambda command: (
            setattr(self.mod, "entity_records", []) if command.startswith("kill ") else None
        )
        self.mod.lines.clear()

        result = await self.api.call(
            "restore",
            {
                "directory": directory,
                "dry_run": False,
                "expect_world_dir": str(self.world),
                "replace_existing": True,
            },
        )

        clear_commands = result["checks"]["existing"]["clearCommands"]
        self.assertTrue(clear_commands)
        for command in clear_commands:
            self.assertNotIn("dx=", command, "no padded volume kill")
            self.assertNotIn("x=-16", command)
            self.assertIn("distance=..", command)
        # Every entity kill is at one of the two recorded positions.
        self.assertTrue(
            all("x=10.5,y=64.0,z=-3.5" in command or "x=10.5,y=65.0,z=-3.5" in command for command in clear_commands),
            clear_commands,
        )
        self.assertTrue(result["ok"], result["verification"])

    async def test_a_failed_unfreeze_fails_the_verdict_and_is_reported(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.raising_commands["tick unfreeze"] = "unfreeze refused"
        self.mod.lines.clear()

        result = await self.api.call(
            "restore",
            {
                "directory": directory,
                "dry_run": False,
                "expect_world_dir": str(self.world),
            },
        )

        self.assertIs(result["ok"], False)
        self.assertEqual(result["verdict"], "failed")
        self.assertFalse(result["tick"]["restored"])
        self.assertFalse(result["tick"]["preserved"])
        self.assertEqual(result["tick"]["unfreezeError"], "unfreeze refused")
        self.assertTrue(self.mod.tick_frozen, "the fake leaves the world frozen like the real refusal")

    async def test_the_freeze_watchdog_releases_a_stuck_restore(self) -> None:
        directory = await self.fork_fixture()
        self.destination_is_empty()
        self.mod.slow_commands["summon minecraft:chest_minecart"] = 0.3
        self.mod.lines.clear()

        result = await self.api.call(
            "restore",
            {
                "directory": directory,
                "dry_run": False,
                "expect_world_dir": str(self.world),
                "freeze_timeout_seconds": 0.05,
            },
            timeout=30,
        )

        self.assertTrue(result["tick"]["watchdogUnfroze"])
        self.assertFalse(self.mod.tick_frozen, "the watchdog must release the world")
        self.assertIs(result["ok"], False)
        self.assertEqual(result["verdict"], "failed")
        self.assertIn("watchdog", result["note"])

    async def test_verify_fails_on_a_dimension_mismatch(self) -> None:
        directory = await self.fork_fixture()
        self.mod.dimension = "minecraft:the_nether"

        result = await self.api.call("verify", {"directory": directory, "target": "lab-b"})

        self.assertFalse(result["ok"])
        self.assertEqual(
            result["verification"]["dimension"],
            {
                "expected": "minecraft:overworld",
                "actual": "minecraft:the_nether",
                "match": False,
            },
        )

    async def test_verify_fails_on_a_malformed_actual_record(self) -> None:
        directory = await self.fork_fixture()
        self.mod.entity_records[0] = dict(self.mod.entity_records[0], pos="not a position")

        result = await self.api.call("verify", {"directory": directory, "target": "lab-b"})

        self.assertFalse(result["ok"])
        self.assertGreaterEqual(result["verification"]["actualMalformed"]["count"], 1)
        self.assertTrue(result["actualIssues"], "the destination snapshot is validated too")

    async def test_verify_reports_inventory_change_even_when_the_order_hash_matches(self) -> None:
        directory = await self.fork_fixture()
        self.mod.entity_records[0]["nbt"] = self.mod.entity_records[0]["nbt"].replace(
            '"count":5', '"count":64'
        )

        result = await self.api.call("verify", {"directory": directory, "target": "lab-b"})

        self.assertFalse(result["ok"])
        self.assertTrue(result["verification"]["orderHash"]["match"])
        self.assertTrue(result["verification"]["uuidOrder"]["match"])
        self.assertEqual(result["verification"]["nbtMismatches"]["count"], 1)
        mismatch = result["verification"]["nbtMismatches"]["sample"][0]
        self.assertEqual(mismatch["uuid"], CART_A["uuid"])
        self.assertIn('"count":5', mismatch["expectedInventory"])
        self.assertIn('"count":64', mismatch["actualInventory"])

    async def test_verify_passes_after_a_faithful_state(self) -> None:
        directory = await self.fork_fixture()

        result = await self.api.call("verify", {"directory": directory})

        self.assertTrue(result["ok"], result["verification"])
        self.assertTrue(result["verification"]["counts"]["match"])
        self.assertEqual(result["verification"]["nbtMismatches"]["count"], 0)


if __name__ == "__main__":
    unittest.main()
