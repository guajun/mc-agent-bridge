from __future__ import annotations

import asyncio
import contextlib
import unittest

from mc_agent_bridge.daemon import BridgeDaemon, categorize, resolve_mod_port
from mc_agent_bridge.local_api import LocalApiClient

from .fake_mod import FakeMod


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
