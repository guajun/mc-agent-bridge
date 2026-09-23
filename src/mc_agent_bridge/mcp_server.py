"""Optional MCP front-end for the bridge.

MCP is one *possible* way for an agent runtime to reach the bridge, not a
requirement: the daemon also serves a plain JSON-lines API that any process can
speak. Because the client spawns MCP servers, this front-end is a good fit for
"agent pulls state" workflows, while the daemon plus a chat listener covers
"game events wake the agent" workflows.

Requires the optional dependency: ``pip install "mc-agent-bridge[mcp]"``.
"""

from __future__ import annotations

import os
from typing import Any

from .local_api import LocalApiClient


def api_host() -> str:
    return os.environ.get("MC_AGENT_API_HOST", "127.0.0.1")


def api_port() -> int:
    return int(os.environ.get("MC_AGENT_API_PORT", "8765"))


async def call(method: str, params: dict[str, Any] | None = None, timeout: float = 60.0) -> Any:
    """One-shot call against the running bridge daemon."""
    client = LocalApiClient(api_host(), api_port())
    try:
        await client.connect(retry=False)
        return await client.call(method, params or {}, timeout=timeout)
    finally:
        await client.close()


def build_server() -> Any:
    try:
        from mcp.server.fastmcp import FastMCP
    except ImportError as error:  # pragma: no cover - depends on optional extra
        raise SystemExit(
            'the MCP front-end needs the optional dependency: pip install "mc-agent-bridge[mcp]"'
        ) from error

    mcp = FastMCP("mc-agent-bridge")

    @mcp.tool()
    async def mc_status() -> Any:
        """Bridge health: mod connection, resolved port, buffered events."""
        return await call("status")

    @mcp.tool()
    async def mc_capabilities() -> Any:
        """Capabilities advertised by the interface mod."""
        return await call("capabilities")

    @mcp.tool()
    async def mc_state() -> Any:
        """Player/world state: position, velocity, health, dimension, tick."""
        return await call("state")

    @mcp.tool()
    async def mc_entities(radius: float = 64.0) -> Any:
        """Entities near the player (0 or less means every tracked entity)."""
        return await call("entities", {"radius": radius})

    @mcp.tool()
    async def mc_command(command: str) -> Any:
        """Run a command as the player, without the leading slash."""
        return await call("command", {"command": command})

    @mcp.tool()
    async def mc_chat(message: str) -> Any:
        """Send a chat message as the player."""
        return await call("chat", {"message": message})

    @mcp.tool()
    async def mc_record_start(ticks: int = 200, radius: float = 64.0, interval: int = 1) -> Any:
        """Start sampling entity state into samples.jsonl."""
        return await call(
            "record_start", {"ticks": ticks, "radius": radius, "interval": interval}
        )

    @mcp.tool()
    async def mc_record_stop() -> Any:
        """Stop the running sampling session."""
        return await call("record_stop")

    @mcp.tool()
    async def mc_wait(ticks: int) -> Any:
        """Block until the game has advanced the given number of ticks."""
        return await call("wait", {"ticks": ticks}, timeout=ticks / 20.0 + 30.0)

    @mcp.tool()
    async def mc_screen() -> Any:
        """Current GUI screen, plus whether the player is in a world."""
        return await call("screen")

    @mcp.tool()
    async def mc_mark(text: str) -> Any:
        """Annotate the event stream, e.g. start/end of an experiment."""
        return await call("mark", {"text": text})

    @mcp.tool()
    async def mc_connect(address: str) -> Any:
        """Join a server, e.g. '127.0.0.1:25565'."""
        return await call("connect", {"address": address})

    @mcp.tool()
    async def mc_events(since: int = 0, limit: int = 200, category: str = "") -> Any:
        """Replay buffered events (chat, game, mark, sample, error) after a cursor.

        Pass the returned ``next`` value back as ``since`` to poll incrementally.
        """
        return await call(
            "events",
            {"since": since or None, "limit": limit, "category": category or None},
        )

    return mcp


def main(transport: str = "stdio") -> None:
    build_server().run(transport=transport)
