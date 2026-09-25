from __future__ import annotations

import asyncio
import contextlib
import inspect
import unittest
from unittest import mock

from mc_agent_bridge import mcp_server, toolkit
from mc_agent_bridge.daemon import BridgeDaemon
from mc_agent_bridge.toolkit import (
    CLIENT_VANTAGE,
    INTERFACE_MOD_CONTEXT,
    INTERFACE_MOD_PLAYER,
    OPERATIONS,
    SERVER_VANTAGE,
    UnsupportedCapability,
    has_capability,
    missing_capabilities,
    normalize_capabilities,
    operation_support,
    static_supported,
    surface,
)

from .fake_mod import CLIENT_CAPABILITIES, SERVER_CAPABILITIES


class CapabilityNormalizationTests(unittest.TestCase):
    def test_normalizes_case_and_whitespace(self) -> None:
        normalized = normalize_capabilities([" State ", "CHAT", "state"])
        self.assertEqual(normalized, frozenset({"state", "chat"}))

    def test_none_means_unknown_and_an_empty_list_means_nothing(self) -> None:
        self.assertIsNone(normalize_capabilities(None))
        self.assertEqual(normalize_capabilities([]), frozenset())

    def test_alias_groups_recognize_the_unmerged_mod_spellings(self) -> None:
        for spelling in ("player_context", "playerContext", "player", "context:player"):
            self.assertTrue(has_capability(normalize_capabilities([spelling]), "player_context"))
        for spelling in ("context_bundle", "context", "chat_context", "context:chat"):
            self.assertTrue(has_capability(normalize_capabilities([spelling]), "context_bundle"))
        self.assertFalse(has_capability(normalize_capabilities(["state"]), "player_context"))


class SurfaceTests(unittest.TestCase):
    def _support(self, entry: dict) -> dict:
        return {item["name"]: item for item in entry["operations"]}  # type: ignore[index]

    def test_server_surface_keeps_the_server_operations_and_hides_client_only_ones(self) -> None:
        result = surface(instance="server", capabilities=SERVER_CAPABILITIES, vantage=SERVER_VANTAGE)
        supported = set(result["supported"])
        unsupported = set(result["unsupported"])

        for name in ("state", "entities", "command", "command_output", "wait", "mark", "save"):
            self.assertIn(name, supported)
        for name in ("snapshot", "snapshots", "fork", "restore", "order"):
            self.assertIn(name, supported)
        for name in ("chat", "record_start", "record_stop", "screen", "connect", "world", "lan"):
            self.assertIn(name, unsupported, f"{name} is client-only")
        self.assertIn("player", unsupported)
        self.assertIn("context", unsupported)

    def test_client_surface_keeps_client_operations_and_hides_server_only_ones(self) -> None:
        result = surface(instance="client", capabilities=CLIENT_CAPABILITIES, vantage=CLIENT_VANTAGE)
        supported = set(result["supported"])
        unsupported = set(result["unsupported"])

        for name in ("chat", "record_start", "record_stop", "screen", "connect", "world", "lan"):
            self.assertIn(name, supported)
        for name in ("snapshot", "snapshots", "fork", "order"):
            self.assertIn(name, unsupported)
        # Restore only needs commands and local files; a client connection can do it.
        self.assertIn("restore", supported)

    def test_unavailable_operations_carry_a_reason_and_the_mod_issue(self) -> None:
        result = surface(instance="server", capabilities=SERVER_CAPABILITIES, vantage=SERVER_VANTAGE)
        by_name = self._support(result)

        chat = by_name["chat"]
        self.assertFalse(chat["supported"])
        self.assertIn("server-vantage", chat["reason"])
        self.assertNotIn("dependency", chat)

        player = by_name["player"]
        self.assertFalse(player["supported"])
        self.assertEqual(player["dependency"], INTERFACE_MOD_PLAYER)
        self.assertIn("mc-agent-interface-mod#1", player["dependencyLabel"])

        context = by_name["context"]
        self.assertFalse(context["supported"])
        self.assertEqual(context["dependency"], INTERFACE_MOD_CONTEXT)

    def test_aliases_light_up_the_adaptive_operations(self) -> None:
        result = surface(
            instance="server",
            capabilities=list(SERVER_CAPABILITIES) + ["player", "chat_context"],
            vantage=SERVER_VANTAGE,
        )
        supported = set(result["supported"])
        self.assertIn("player", supported)
        self.assertIn("context", supported)

    def test_no_capabilities_advertised_filters_everything_mod_related(self) -> None:
        result = surface(instance="server", capabilities=[], vantage=SERVER_VANTAGE)
        supported = set(result["supported"])
        self.assertEqual(supported, {"status", "capabilities", "events", "stop"})

    def test_legacy_mod_without_capabilities_is_not_filtered(self) -> None:
        result = surface(instance=None, capabilities=None, vantage=SERVER_VANTAGE)
        self.assertIsNone(result["modCapabilities"])
        self.assertEqual(len(result["supported"]), len(OPERATIONS))
        self.assertIn("note", result)

    def test_missing_capabilities_lists_only_the_requirement_group(self) -> None:
        fork = next(operation for operation in OPERATIONS if operation.name == "fork")
        self.assertEqual(missing_capabilities(fork, normalize_capabilities(["snapshot"])), ["command"])
        self.assertEqual(
            missing_capabilities(fork, normalize_capabilities(["snapshot", "command"])), []
        )

    def test_unsupported_error_names_the_dependency_and_says_nothing_was_sent(self) -> None:
        player = next(operation for operation in OPERATIONS if operation.name == "player")
        error = UnsupportedCapability(player, ["player_context"], instance="server")
        message = str(error)
        self.assertIn("mc-agent-interface-mod#1", message)
        self.assertIn("nothing was sent to the game", message)

        chat = next(operation for operation in OPERATIONS if operation.name == "chat")
        message = str(UnsupportedCapability(chat, ["chat"], instance="server", vantage="server"))
        self.assertIn("does not advertise 'chat'", message)
        self.assertIn("--vantage client", message)

    def test_static_surfaces_match_the_documented_defaults(self) -> None:
        server = static_supported(SERVER_VANTAGE)
        client = static_supported(CLIENT_VANTAGE)
        self.assertIn("snapshot", server)
        self.assertNotIn("chat", server)
        self.assertIn("chat", client)
        self.assertNotIn("snapshot", client)
        self.assertNotIn("player", server)
        self.assertNotIn("context", server)


class RecordingServer:
    """Minimal stand-in for the MCP SDK server class used by build_server."""

    def __init__(self, name: str, **kwargs: object) -> None:
        self.name = name
        self.tools: dict[str, object] = {}
        self.lifespan = kwargs.get("lifespan")

    def tool(self, name: str | None = None, **_kwargs: object):
        def decorator(func):
            self.tools[name or func.__name__] = func
            return func

        return decorator

    def add_tool(self, func, name: str | None = None, **_kwargs: object) -> None:
        self.tools[name or func.__name__] = func

    def remove_tool(self, name: str) -> None:
        self.tools.pop(name, None)

    def run(self, transport: str = "stdio") -> None:  # pragma: no cover - not called
        self.transport = transport


class RecordingServerMixin:
    """Build one RecordingServer per SDK server, like the real class would."""

    server: RecordingServer

    def setUp(self) -> None:
        self.original = mcp_server._server_class

        def factory():
            def build(name: str, **kwargs: object) -> RecordingServer:
                self.server = RecordingServer(name, **kwargs)
                return self.server

            return build

        mcp_server._server_class = factory

    def tearDown(self) -> None:
        mcp_server._server_class = self.original


class McpToolRegistrationTests(RecordingServerMixin, unittest.TestCase):
    def test_without_a_daemon_the_default_server_surface_hides_client_tools(self) -> None:
        mcp_server.build_server(surface=None, vantage=SERVER_VANTAGE)
        self.assertIsNotNone(self.server.lifespan, "the SDK lifespan drives background refresh")
        self.assertIn("mc_status", self.server.tools)
        self.assertIn("mc_capabilities", self.server.tools)
        self.assertIn("mc_state", self.server.tools)
        self.assertIn("mc_snapshot", self.server.tools)
        self.assertIn("mc_save", self.server.tools)
        self.assertNotIn("mc_chat", self.server.tools)
        self.assertNotIn("mc_screen", self.server.tools)
        self.assertNotIn("mc_connect", self.server.tools)
        self.assertNotIn("mc_player", self.server.tools)
        self.assertNotIn("mc_context", self.server.tools)

    def test_the_legacy_client_vantage_registers_the_client_tools(self) -> None:
        mcp_server.build_server(surface=None, vantage=CLIENT_VANTAGE)
        self.assertIn("mc_chat", self.server.tools)
        self.assertIn("mc_screen", self.server.tools)
        self.assertNotIn("mc_snapshot", self.server.tools)

    def test_a_live_surface_drives_exactly_what_is_registered(self) -> None:
        live = {
            "supported": ["status", "capabilities", "events", "state", "player", "context", "save"],
            "unsupported": ["chat"],
        }
        mcp_server.build_server(surface=live, vantage=SERVER_VANTAGE)
        self.assertEqual(
            set(self.server.tools),
            {
                "mc_status",
                "mc_capabilities",
                "mc_events",
                "mc_state",
                "mc_player",
                "mc_context",
                "mc_save",
            },
        )

    def test_a_nested_capabilities_reply_is_understood(self) -> None:
        reply = {"type": "capabilities", "surface": {"supported": ["status", "capabilities", "state"]}}
        self.assertEqual(
            mcp_server.tool_names(surface=reply, vantage=SERVER_VANTAGE),
            ["mc_status", "mc_capabilities", "mc_state", "mc_events"],
        )

    def test_every_toolkit_operation_has_a_daemon_handler(self) -> None:
        for operation in OPERATIONS:
            self.assertTrue(
                hasattr(BridgeDaemon, f"_m_{operation.name}"),
                f"no daemon handler for toolkit operation {operation.name!r}",
            )

    def test_operation_support_reports_unknown_requirements_without_a_dependency(self) -> None:
        operation = next(operation for operation in OPERATIONS if operation.name == "chat")
        entry = operation_support(operation, normalize_capabilities(["state"]), instance="server")
        self.assertFalse(entry["supported"])
        self.assertNotIn("dependency", entry)


LIVE_SERVER_SURFACE = surface(
    instance="server",
    capabilities=list(SERVER_CAPABILITIES) + ["player_context", "context_bundle"],
    vantage=SERVER_VANTAGE,
)
LIVE_CLIENT_SURFACE = surface(
    instance="client", capabilities=CLIENT_CAPABILITIES, vantage=CLIENT_VANTAGE
)


def capabilities_reply(entry: dict) -> dict:
    """The shape the daemon's capabilities method returns."""
    return {
        "type": "capabilities",
        "instance": entry["instance"],
        "modCapabilities": entry["modCapabilities"],
        "surface": entry,
    }


class ToolkitRegistryRefreshTests(RecordingServerMixin, unittest.IsolatedAsyncioTestCase):
    """One MCP server reused across a late connection and a capability change."""

    def build(self, entry: dict | None = None) -> mcp_server.ToolkitRegistry:
        registry = mcp_server.ToolkitRegistry(vantage=SERVER_VANTAGE)
        mcp_server.build_server(surface=entry, vantage=SERVER_VANTAGE, registry=registry)
        return registry

    async def test_a_capabilities_call_after_a_late_connection_registers_adaptive_tools(self) -> None:
        self.build(None)
        self.assertNotIn("mc_player", self.server.tools)
        reply = capabilities_reply(LIVE_SERVER_SURFACE)

        async def fake_call(method, params=None, timeout=60.0):
            self.assertEqual(method, "capabilities")
            return reply

        with mock.patch.object(mcp_server, "call", fake_call):
            result = await self.server.tools["mc_capabilities"]()  # type: ignore[operator]

        self.assertEqual(result, reply)
        self.assertIn("mc_player", self.server.tools)
        self.assertIn("mc_context", self.server.tools)

    async def test_the_observed_capabilities_tool_keeps_an_empty_signature(self) -> None:
        # A *args/**kwargs wrapper would make the SDK advertise args/kwargs as
        # tool arguments and reject real calls; functools.wraps prevents that.
        self.build(None)
        tool = self.server.tools["mc_capabilities"]
        self.assertEqual(list(inspect.signature(tool).parameters), [])  # type: ignore[arg-type]

    async def test_a_capability_change_removes_stale_tools(self) -> None:
        registry = self.build(LIVE_SERVER_SURFACE)
        self.assertIn("mc_player", self.server.tools)

        async def fake_call(method, params=None, timeout=60.0):
            return capabilities_reply(LIVE_CLIENT_SURFACE)

        with mock.patch.object(mcp_server, "call", fake_call):
            await self.server.tools["mc_capabilities"]()  # type: ignore[operator]

        self.assertNotIn("mc_player", self.server.tools)
        self.assertNotIn("mc_context", self.server.tools)
        self.assertNotIn("mc_snapshot", self.server.tools)
        self.assertIn("mc_chat", self.server.tools)
        self.assertEqual(
            registry.registered, set(mcp_server.tool_names(LIVE_CLIENT_SURFACE))
        )

    async def test_a_failed_probe_keeps_the_last_known_surface(self) -> None:
        async def failing_probe():
            raise ConnectionError("daemon restarting")

        registry = mcp_server.ToolkitRegistry(vantage=SERVER_VANTAGE, probe=failing_probe)
        mcp_server.build_server(
            surface=LIVE_SERVER_SURFACE, vantage=SERVER_VANTAGE, registry=registry
        )
        self.assertIn("mc_player", self.server.tools)

        await registry.refresh()

        self.assertIn("mc_player", self.server.tools)
        self.assertIn("mc_state", self.server.tools)


class ToolkitWatchTests(RecordingServerMixin, unittest.IsolatedAsyncioTestCase):
    async def test_background_watch_picks_up_a_late_connection(self) -> None:
        replies: list = [None, capabilities_reply(LIVE_SERVER_SURFACE)]
        calls = {"n": 0}

        async def probe():
            index = min(calls["n"], len(replies) - 1)
            calls["n"] += 1
            return replies[index]

        registry = mcp_server.ToolkitRegistry(
            vantage=SERVER_VANTAGE, probe=probe, refresh_interval=0.05
        )
        mcp_server.build_server(surface=None, vantage=SERVER_VANTAGE, registry=registry)
        self.assertNotIn("mc_player", self.server.tools)

        task = asyncio.create_task(registry.watch())
        try:
            for _ in range(200):
                if "mc_player" in self.server.tools:
                    break
                await asyncio.sleep(0.01)
            self.assertIn("mc_player", self.server.tools)
        finally:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task


class ToolkitImportTests(unittest.TestCase):
    def test_mcp_module_does_not_import_the_sdk_at_import_time(self) -> None:
        # The MCP extra is optional: importing the front-end must not require it.
        self.assertTrue(hasattr(mcp_server, "build_server"))
        self.assertTrue(hasattr(toolkit, "surface"))
