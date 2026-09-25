"""Tests for the adaptive boundaries: per-player context and context bundles.

The mod APIs behind ``player``/``context`` shipped in mc-agent-interface-mod
0.6.0, so the contract these tests pin down is: (1) when the connected mod
advertises the capability, the canonical line is delegated and the reply
normalized; (2) both reply shapes seen in the wild land on the documented
``player.view``; (3) when the capability is missing, nothing is sent and the
caller gets an actionable error naming the tracking issue.
"""

from __future__ import annotations

import asyncio
import contextlib
import tempfile
import unittest
from pathlib import Path

from mc_agent_bridge.daemon import BridgeDaemon
from mc_agent_bridge.local_api import LocalApiClient

from .fake_mod import SERVER_CAPABILITIES, FakeMod


async def wait_for(predicate, timeout: float = 5.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout
    while asyncio.get_running_loop().time() < deadline:
        if predicate():
            return
        await asyncio.sleep(0.02)
    raise AssertionError("condition not met in time")


PLAYER_REPLY = {
    "type": "player",
    "player": {
        "uuid": "466e11e1-1111-2222-3333-444455556666",
        "name": "Steve",
        "dimension": "minecraft:overworld",
        "position": [10.5, 64.0, -3.25],
        "rotation": {"yaw": 90.0, "pitch": 12.5},
        "view": {"type": "block", "x": 12, "y": 63, "z": -3, "block": "minecraft:stone"},
    },
}

#: The released mod (0.6.0) splits the entity record and the ray: the view is a
#: top-level field, not a child of ``player``.
PLAYER_REPLY_SPLIT_VIEW = {
    "type": "player",
    "tick": 4210,
    "found": True,
    "matchedBy": "uuid",
    "player": {
        "uuid": "f0a8f4ba-99f5-412a-9189-db832c934913",
        "name": "Alice",
        "dimension": "minecraft:overworld",
        "x": 0.5,
        "y": 100.0,
        "z": 0.5,
        "yaw": 0.0,
        "pitch": 0.0,
        "eye": [0.5, 101.62, 0.5],
    },
    "view": {
        "eye": [0.5, 101.62, 0.5],
        "direction": [0.0, 0.0, 1.0],
        "blockRange": 4.5,
        "entityRange": 3.0,
        "target": {
            "type": "block",
            "distance": 3.5,
            "block": {"id": "minecraft:dispenser", "x": 0, "y": 101, "z": 4, "face": "north"},
        },
    },
}

CONTEXT_REPLY = {
    "type": "context",
    "context": {
        "id": "ctx-42",
        "eventSeq": 17,
        "timestamp": 1790145600000,
        "tick": 104233,
        "sender": "Steve",
        "uuid": "466e11e1-1111-2222-3333-444455556666",
        "dimension": "minecraft:overworld",
        "position": [10.5, 64.0, -3.25],
        "rotation": {"yaw": 90.0, "pitch": 12.5},
        "view": {"type": "miss"},
        "schema": 1,
    },
}


class AdapterDaemonTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.mod: FakeMod | None = None
        self.daemon: BridgeDaemon | None = None
        self.task: asyncio.Task | None = None
        self.api: LocalApiClient | None = None
        self.tmp = tempfile.TemporaryDirectory()
        self.world = Path(self.tmp.name) / "world"
        self.world.mkdir()

    async def start(self, capabilities: list[str], **mod_kwargs) -> LocalApiClient:
        self.mod = FakeMod(instance="server", capabilities=capabilities, **mod_kwargs)
        await self.mod.start()
        self.daemon = BridgeDaemon(mod_port=self.mod.port, api_port=0, reconnect_delay=0.05)
        self.task = asyncio.create_task(self.daemon.run())
        await wait_for(lambda: self.daemon.server.port != 0)
        await wait_for(lambda: self.daemon.connected and self.daemon.mod_capabilities is not None)
        self.api = LocalApiClient(port=self.daemon.server.port)
        await self.api.connect(retry=False)
        return self.api

    async def asyncTearDown(self) -> None:
        if self.api is not None:
            await self.api.close()
        if self.daemon is not None:
            await self.daemon.stop()
        if self.task is not None:
            with contextlib.suppress(asyncio.CancelledError, TimeoutError):
                await asyncio.wait_for(self.task, timeout=3)
        if self.mod is not None:
            await self.mod.stop()
        self.tmp.cleanup()

    async def test_player_delegates_the_canonical_line_and_normalizes_the_reply(self) -> None:
        api = await self.start(
            SERVER_CAPABILITIES + ["player_context"], player_reply=PLAYER_REPLY
        )
        result = await api.call("player", {"player": PLAYER_REPLY["player"]["uuid"]})

        self.assertIn(f"PLAYER {PLAYER_REPLY['player']['uuid']}", self.mod.lines)
        self.assertEqual(result["type"], "player_context")
        self.assertTrue(result["found"])
        self.assertEqual(result["uuid"], PLAYER_REPLY["player"]["uuid"])
        self.assertEqual(result["name"], "Steve")
        self.assertEqual(result["player"]["position"], [10.5, 64.0, -3.25])
        self.assertEqual(result["player"]["view"]["block"], "minecraft:stone")

    async def test_split_view_reply_keeps_the_ray_with_the_player(self) -> None:
        """The released mod's top-level ``view`` must reach the caller."""
        api = await self.start(
            SERVER_CAPABILITIES + ["player_context"], player_reply=PLAYER_REPLY_SPLIT_VIEW
        )
        result = await api.call(
            "player", {"player": PLAYER_REPLY_SPLIT_VIEW["player"]["uuid"]}
        )

        self.assertTrue(result["found"])
        self.assertEqual(result["name"], "Alice")
        player = result["player"]
        self.assertEqual(player["uuid"], PLAYER_REPLY_SPLIT_VIEW["player"]["uuid"])
        self.assertEqual(player["eye"], [0.5, 101.62, 0.5])
        self.assertEqual(player["view"]["blockRange"], 4.5)
        self.assertEqual(player["view"]["target"]["type"], "block")
        self.assertEqual(
            player["view"]["target"]["block"]["id"], "minecraft:dispenser"
        )

    async def test_nested_view_is_not_overwritten_by_a_missing_top_level_view(self) -> None:
        api = await self.start(
            SERVER_CAPABILITIES + ["player_context"], player_reply=PLAYER_REPLY
        )
        result = await api.call("player", {"player": "Steve"})

        self.assertEqual(result["player"]["view"]["block"], "minecraft:stone")

    async def test_missing_or_non_dict_view_is_not_invented(self) -> None:
        """A reply without a usable ray keeps the player object as the mod sent it."""
        api = await self.start(
            SERVER_CAPABILITIES + ["player_context"], player_reply=PLAYER_REPLY_SPLIT_VIEW
        )

        for bad_view in ("not a dict", None, [1, 2], 7):
            self.mod.player_reply = {**PLAYER_REPLY_SPLIT_VIEW, "view": bad_view}
            result = await api.call("player", {"player": "Alice"})
            self.assertTrue(result["found"])
            self.assertEqual(result["name"], "Alice")
            self.assertNotIn("view", result["player"])

        without_view = {
            key: value for key, value in PLAYER_REPLY_SPLIT_VIEW.items() if key != "view"
        }
        self.mod.player_reply = without_view
        result = await api.call("player", {"player": "Alice"})
        self.assertTrue(result["found"])
        self.assertNotIn("view", result["player"])

    async def test_unknown_player_stays_a_structured_answer(self) -> None:
        api = await self.start(
            SERVER_CAPABILITIES + ["player_context"],
            player_reply={"type": "player", "found": False, "status": "unknown", "reason": "no such player"},
        )
        result = await api.call("player", {"player": "Nobody"})

        self.assertFalse(result["found"])
        self.assertEqual(result["status"], "unknown")
        self.assertEqual(result["reason"], "no such player")
        self.assertIsNone(result["player"])

    async def test_player_without_the_capability_sends_nothing_and_explains(self) -> None:
        api = await self.start(SERVER_CAPABILITIES)

        with self.assertRaises(RuntimeError) as caught:
            await api.call("player", {"player": "Steve"})

        message = str(caught.exception)
        self.assertIn("mc-agent-interface-mod#1", message)
        self.assertIn("player_context", message)
        self.assertFalse(any(line.startswith("PLAYER") for line in self.mod.lines))

    async def test_context_delegates_and_normalizes_the_bundle(self) -> None:
        api = await self.start(
            SERVER_CAPABILITIES + ["context_bundle"], context_reply=CONTEXT_REPLY
        )
        result = await api.call("context", {"id": "ctx-42"})

        self.assertIn("CONTEXT ctx-42", self.mod.lines)
        self.assertEqual(result["type"], "context_bundle")
        self.assertEqual(result["id"], "ctx-42")
        self.assertTrue(result["found"])
        self.assertEqual(result["context"]["tick"], 104233)
        self.assertEqual(result["context"]["sender"], "Steve")

    async def test_expired_context_is_structured_not_a_different_player(self) -> None:
        api = await self.start(
            SERVER_CAPABILITIES + ["context_bundle"],
            context_reply={"type": "context", "found": False, "status": "expired"},
        )
        result = await api.call("context", {"id": "ctx-expired"})

        self.assertFalse(result["found"])
        self.assertEqual(result["status"], "expired")
        self.assertIsNone(result["context"])

    async def test_context_without_the_capability_sends_nothing_and_explains(self) -> None:
        api = await self.start(SERVER_CAPABILITIES)

        with self.assertRaises(RuntimeError) as caught:
            await api.call("context", {"id": "ctx-1"})

        message = str(caught.exception)
        self.assertIn("mc-agent-interface-mod#2", message)
        self.assertIn("context_bundle", message)
        self.assertFalse(any(line.startswith("CONTEXT") for line in self.mod.lines))

    async def test_chat_event_context_reference_is_normalized(self) -> None:
        api = await self.start(SERVER_CAPABILITIES)
        await self.mod.push({"type": "chat", "text": "look!", "sender": "Steve", "context_id": "ctx-7"})
        await wait_for(lambda: bool(self.daemon.recent_events(category="chat")["events"]))

        event = (await api.call("events", {"category": "chat"}))["events"][-1]
        self.assertEqual(event["contextId"], "ctx-7")

    async def test_command_output_prefers_the_ack_output_when_the_mod_provides_it(self) -> None:
        api = await self.start(SERVER_CAPABILITIES, cmd_output=["Motion: [0.0d, -0.0784d, 0.0d]"])
        result = await api.call("command_output", {"command": "data get entity Steve Motion"})

        self.assertEqual(result["source"], "ack")
        self.assertEqual(result["output"], ["Motion: [0.0d, -0.0784d, 0.0d]"])
        self.assertIn("CMD data get entity Steve Motion", self.mod.lines)

    async def test_command_output_falls_back_to_events_for_the_client_shape(self) -> None:
        api = await self.start(SERVER_CAPABILITIES)
        task = asyncio.create_task(
            api.call("command_output", {"command": "say hello", "wait": 0.5})
        )
        await wait_for(lambda: any(line.startswith("CMD ") for line in self.mod.lines))
        await self.mod.push({"type": "game", "text": "hello from the game"})
        result = await task

        self.assertEqual(result["source"], "events")
        self.assertIn("hello from the game", result["output"])

    async def test_save_composes_world_metadata_from_state(self) -> None:
        await self.start(SERVER_CAPABILITIES, world_dir=self.world)
        api = self.api
        result = await api.call("save")

        self.assertEqual(result["type"], "world_save")
        self.assertEqual(result["worldDir"], str(self.world))
        self.assertTrue(result["worldDirExistsOnBridgeHost"])
        self.assertIn("STATE", self.mod.lines)
