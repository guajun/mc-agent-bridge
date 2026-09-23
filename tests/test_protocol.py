from __future__ import annotations

import asyncio
import json
import os
import tempfile
import unittest

from mc_agent_bridge.protocol import ModClient, is_event, read_port_file

from .fake_mod import FakeMod


class IsEventTests(unittest.TestCase):
    def test_documented_events_are_events(self) -> None:
        self.assertTrue(is_event({"type": "chat"}))
        self.assertTrue(is_event({"type": "sample_done"}))

    def test_diagnostic_lines_are_events_but_error_replies_are_not(self) -> None:
        self.assertTrue(is_event({"type": "task_error"}))
        self.assertTrue(is_event({"type": "portfile_error"}))
        self.assertFalse(is_event({"type": "error"}))
        self.assertFalse(is_event({"type": "state"}))
        self.assertFalse(is_event({}))


class PortFileTests(unittest.TestCase):
    def test_reads_trimmed_integer(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = os.path.join(directory, "port.txt")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write("25581\n")
            self.assertEqual(read_port_file(path), 25581)

    def test_missing_file_is_none(self) -> None:
        self.assertIsNone(read_port_file(os.path.join(tempfile.gettempdir(), "definitely-missing")))


class ModClientTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.mod = FakeMod()
        await self.mod.start()
        self.events: list[dict] = []

    async def asyncTearDown(self) -> None:
        await self.mod.stop()

    async def test_roundtrip_and_events(self) -> None:
        client = ModClient("127.0.0.1", self.mod.port, on_event=self.events.append)
        hello = await client.connect(retry=False)
        self.assertEqual(hello["mod"], "mc-agent-interface")
        self.assertEqual(self.events[0]["type"], "hello")

        state = await client.request("STATE")
        self.assertEqual(state["type"], "state")

        await self.mod.push({"type": "chat", "text": "hi", "sender": "someone"})
        for _ in range(50):
            if any(event["type"] == "chat" for event in self.events):
                break
            await asyncio.sleep(0.02)
        self.assertEqual([e["type"] for e in self.events][-1], "chat")

        # A stray diagnostic must not be mistaken for a reply.
        await self.mod.push({"type": "task_error", "message": "boom"})
        reply = await client.request("CAPS")
        self.assertEqual(reply["type"], "capabilities")
        await client.close()

    async def test_error_reply_raises_via_daemon_contract(self) -> None:
        client = ModClient("127.0.0.1", self.mod.port)
        await client.connect(retry=False)
        reply = await client.request("NONSENSE")
        self.assertEqual(reply["type"], "error")
        await client.close()

    async def test_request_without_listener_still_works(self) -> None:
        client = ModClient("127.0.0.1", self.mod.port)
        await client.connect(retry=False)
        self.assertTrue(client.connected)
        payload = json.loads(json.dumps(await client.request("STATE")))
        self.assertEqual(payload["tick"], 1)
        await client.close()
