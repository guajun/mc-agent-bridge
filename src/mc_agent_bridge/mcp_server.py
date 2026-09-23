"""Optional MCP front-end for the bridge.

MCP is one *possible* way for an agent runtime to reach the bridge, not a
requirement: the daemon also serves a plain JSON-lines API that any process can
speak. Because the client spawns MCP servers, this front-end is a good fit for
"agent pulls state" workflows, while the daemon plus a chat listener covers
"game events wake the agent" workflows.

Requires the optional dependency: ``pip install "mc-agent-bridge[mcp]"``.
"""

from __future__ import annotations

import asyncio
import os
from typing import Any

from .local_api import LocalApiClient


def api_host() -> str:
    return os.environ.get("MC_AGENT_API_HOST", "127.0.0.1")


def api_port() -> int:
    return int(os.environ.get("MC_AGENT_API_PORT", "8765"))


def _distance_squared(entity: dict[str, Any], origin: tuple[Any, Any, Any]) -> float:
    try:
        return float(
            (entity["x"] - origin[0]) ** 2
            + (entity["y"] - origin[1]) ** 2
            + (entity["z"] - origin[2]) ** 2
        )
    except (KeyError, TypeError, ValueError):
        return float("inf")


def _compact(entity: dict[str, Any], origin: tuple[Any, Any, Any]) -> dict[str, Any]:
    """Trim a raw entity record down to what a model actually needs."""
    compact: dict[str, Any] = {
        "id": entity.get("id"),
        "type": entity.get("type"),
        "pos": [round(float(entity.get(axis, 0.0)), 2) for axis in ("x", "y", "z")],
        "vel": [round(float(entity.get(axis, 0.0)), 3) for axis in ("vx", "vy", "vz")],
    }
    distance = _distance_squared(entity, origin)
    if distance != float("inf"):
        compact["distance"] = round(distance**0.5, 2)
    if entity.get("alive") is False:
        compact["alive"] = False
    for key in ("health", "fuse", "primed", "vehicle", "passengers", "bodyItem", "bodyCount"):
        if key in entity:
            compact[key] = entity[key]
    return compact


async def call(method: str, params: dict[str, Any] | None = None, timeout: float = 60.0) -> Any:
    """One-shot call against the running bridge daemon."""
    client = LocalApiClient(api_host(), api_port())
    try:
        await client.connect(retry=False)
        return await client.call(method, params or {}, timeout=timeout)
    finally:
        await client.close()


def build_server() -> Any:
    server_class = _server_class()
    mcp = server_class("mc-agent-bridge")

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
    async def mc_entities(radius: float = 64.0, limit: int = 40, types: str = "") -> Any:
        """Entities near the player, summarised.

        Returns counts by type plus the ``limit`` closest entities, because a
        busy world answers with hundreds of kilobytes of raw JSON - far more
        than a model can usefully read. Pass ``types`` (comma separated, e.g.
        "sulfur_cube,minecart") to filter, or raise ``limit`` for more detail.
        """
        state = await call("state")
        payload = await call("entities", {"radius": radius}, timeout=120.0)
        entities = list(payload.get("entities") or [])

        wanted = [part.strip().lower() for part in types.split(",") if part.strip()]
        if wanted:
            entities = [e for e in entities if any(w in e.get("type", "").lower() for w in wanted)]

        counts: dict[str, int] = {}
        for entity in entities:
            key = entity.get("type", "unknown")
            counts[key] = counts.get(key, 0) + 1

        origin = (state.get("x"), state.get("y"), state.get("z"))
        if None not in origin:
            entities.sort(key=lambda e: _distance_squared(e, origin))

        shown = [_compact(e, origin) for e in entities[: max(0, limit)]]
        return {
            "radius": payload.get("radius"),
            "total": len(entities),
            "counts": dict(sorted(counts.items(), key=lambda kv: -kv[1])),
            "shown": shown,
            "truncated": max(0, len(entities) - len(shown)),
        }

    @mcp.tool()
    async def mc_command(command: str) -> Any:
        """Run a command as the player, without the leading slash."""
        return await call("command", {"command": command})

    @mcp.tool()
    async def mc_command_output(command: str, wait: float = 2.0) -> Any:
        """Run a command and return what it answered.

        Preferred over ``mc_command`` whenever the answer matters: the reply to
        a command arrives as game/chat messages, not as the command's own
        return value. This sends the command and collects the feedback it
        produced, so ``data get entity <name> Motion`` and friends come back as
        data instead of disappearing into the chat log.
        """
        backlog = await call("events", {"limit": 1})
        cursor = max((event["seq"] for event in backlog["events"]), default=0)
        ack = await call("command", {"command": command})
        await asyncio.sleep(max(0.1, wait))
        events = await call("events", {"since": cursor, "limit": 100})
        output = [
            str(event.get("text") or "")
            for event in events.get("events", [])
            if event.get("category") in ("game", "chat")
        ]
        return {"command": ack.get("detail", command), "output": output}

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
    async def mc_world(level: str) -> Any:
        """Open a single-player world by its folder name (the client must be idle)."""
        return await call("world", {"level": level})

    @mcp.tool()
    async def mc_lan(port: int = 0, mode: str = "") -> Any:
        """Open the single-player world to the LAN and report the port to dial.

        This is the "Open to LAN" button without the button: after it, another
        client (the agent's own player) can join, so the agent has an identity
        separate from yours. Calling it again while published just reports the
        current port.

        ``mode="offline"`` stops the host from checking Mojang sessions, which is
        what lets a spare client with no account (the agent) join. Use it only on
        a LAN you trust.
        """
        return await call("lan", {"port": port, "mode": mode})

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


def _server_class() -> Any:
    """The MCP server class, across SDK generations.

    The Python SDK renamed ``FastMCP`` to ``MCPServer`` in 2.0; the v1 module
    still exists in 2.x but raises a pointer to the migration guide, so a plain
    import attempt is enough to pick the right one.
    """
    try:
        from mcp.server.fastmcp import FastMCP  # mcp < 2

        return FastMCP
    except ModuleNotFoundError:
        pass
    try:
        from mcp.server.mcpserver import MCPServer  # mcp >= 2

        return MCPServer
    except ModuleNotFoundError as error:  # pragma: no cover - depends on the extra
        raise SystemExit(
            'the MCP front-end needs the optional dependency: pip install "mc-agent-bridge[mcp]"'
        ) from error


def main(transport: str = "stdio") -> None:
    build_server().run(transport=transport)
