"""Optional MCP front-end for the bridge.

MCP is one *possible* way for a Harness to reach the bridge, not a requirement:
the daemon also serves a plain JSON-lines API and a CLI that any process can
use. Because the client spawns MCP servers, this front-end is a good fit for
"Harness pulls game data" workflows, while the daemon plus the event buffer
covers "game events start work" workflows.

The tool list is not static: at startup the front-end asks the daemon for the
connected mod's capability surface and registers only the operations that
instance can serve. A server-vantage session therefore never advertises
client-only tools such as ``mc_screen`` or ``mc_connect``, and the unmerged
per-player/context operations appear only once the mod advertises them.

Requires the optional dependency: ``pip install "mc-agent-bridge[mcp]"``.
"""

from __future__ import annotations

import asyncio
import contextlib
import functools
import os
from collections.abc import Awaitable, Callable
from typing import Any

from .local_api import LocalApiClient
from .toolkit import SERVER_VANTAGE, static_supported

#: Registered even when the daemon has not answered yet: they describe the
#: bridge itself and the local event buffer, not a mod capability.
ALWAYS_REGISTERED = frozenset({"status", "capabilities", "events"})


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


# --------------------------------------------------------------------------- tools


async def _mc_status() -> Any:
    """Bridge health: mod connection, vantage, resolved port, buffered events."""
    return await call("status")


async def _mc_capabilities() -> Any:
    """The connected mod's capabilities and the filtered toolkit surface.

    ``capabilities`` is the raw CAPS list from the mod; ``surface.supported``
    and ``surface.unsupported`` say which operations this connection can serve,
    with a reason (and the tracking mod issue) for anything unavailable.
    """
    return await call("capabilities")


async def _mc_player(player: str) -> Any:
    """Server-side context for one player: identity, position, rotation, view target.

    ``player`` is a UUID (preferred) or a display name. This is an adaptive
    operation: it appears only when the connected mod advertises the capability
    from mc-agent-interface-mod#1. The alternative is the global ``mc_state``
    player list, which carries name, UUID, position and dimension but no view.
    """
    return await call("player", {"player": player})


async def _mc_state() -> Any:
    """World/server state: tick, players, levels, and the world directory."""
    return await call("state")


async def _mc_entities(radius: float = 64.0, limit: int = 40, types: str = "") -> Any:
    """Entities near the player, summarised.

    Returns counts by type plus the ``limit`` closest entities, because a
    busy world answers with hundreds of kilobytes of raw JSON - far more
    than a model can usefully read. Pass ``types`` (comma separated, e.g.
    "sulfur_cube,minecart") to filter, or raise ``limit`` for more detail.

    A radius needs a player on the server vantage (the mod measures from the
    first player); use radius 0 for every entity the level ticks.
    """
    state = await call("state")
    payload = await call("entities", {"radius": radius or None}, timeout=120.0)
    entities = list(payload.get("entities") or [])

    wanted = [part.strip().lower() for part in types.split(",") if part.strip()]
    if wanted:
        entities = [e for e in entities if any(w in e.get("type", "").lower() for w in wanted)]

    counts: dict[str, int] = {}
    for entity in entities:
        key = entity.get("type", "unknown")
        counts[key] = counts.get(key, 0) + 1

    origin = (state.get("x"), state.get("y"), state.get("z"))
    players = state.get("playerList")
    if None in origin and isinstance(players, list) and players:
        first = players[0]
        origin = (first.get("x"), first.get("y"), first.get("z"))
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


async def _mc_command(command: str) -> Any:
    """Run a command as the mod's command source, without the leading slash."""
    return await call("command", {"command": command})


async def _mc_command_output(command: str, wait: float = 2.0) -> Any:
    """Run a command and return what it answered.

    Preferred over ``mc_command`` whenever the answer matters. The server
    vantage answers with the output in the command reply itself; the client
    vantage collects the game/chat events the command produced during
    ``wait`` seconds. Either way the result is ``{"command", "output",
    "source"}``.
    """
    return await call("command_output", {"command": command, "wait": wait})


async def _mc_chat(message: str) -> Any:
    """Send a chat message as the client-vantage player."""
    return await call("chat", {"message": message})


async def _mc_record_start(ticks: int = 200, radius: float = 64.0, interval: int = 1) -> Any:
    """Start sampling entity state into samples.jsonl (client vantage)."""
    return await call(
        "record_start", {"ticks": ticks, "radius": radius, "interval": interval}
    )


async def _mc_record_stop() -> Any:
    """Stop the running sampling session (client vantage)."""
    return await call("record_stop")


async def _mc_wait(ticks: int) -> Any:
    """Block until the game has advanced the given number of ticks."""
    return await call("wait", {"ticks": ticks}, timeout=ticks / 20.0 + 30.0)


async def _mc_screen() -> Any:
    """Current client GUI screen, plus whether the player is in a world."""
    return await call("screen")


async def _mc_mark(text: str) -> Any:
    """Annotate the event stream, e.g. start/end of an experiment."""
    return await call("mark", {"text": text})


async def _mc_connect(address: str) -> Any:
    """Join a server, e.g. '127.0.0.1:25565' (client vantage)."""
    return await call("connect", {"address": address})


async def _mc_world(level: str) -> Any:
    """Open a single-player world by its folder name (client vantage only)."""
    return await call("world", {"level": level})


async def _mc_lan(port: int = 0, mode: str = "") -> Any:
    """Publish the single-player world to the LAN (client vantage only).

    This is the "Open to LAN" button without the button: after it, another
    client (the Harness's own player) can join, so the Harness has an identity
    separate from yours. Calling it again while published just reports the
    current port.

    ``mode="offline"`` stops the host from checking Mojang sessions, which is
    what lets a spare client with no account join. Use it only on a LAN you
    trust.
    """
    return await call("lan", {"port": port, "mode": mode})


async def _mc_events(since: int = 0, limit: int = 200, category: str = "") -> Any:
    """Replay buffered events (chat, game, mark, sample, error) after a cursor.

    Pass the returned ``next`` value back as ``since`` to poll incrementally.
    Chat events may carry ``contextId``: feed it to ``mc_context`` to get the
    sender's context at the moment the server received the message.
    """
    return await call(
        "events",
        {"since": since or None, "limit": limit, "category": category or None},
    )


async def _mc_context(context_id: str) -> Any:
    """Retrieve a chat-time context bundle by the id on its chat event.

    The bundle is the sender's server-known context when the message arrived:
    dimension, position, rotation, view target, tick, and a schema version.
    This is an adaptive operation: it appears only when the connected mod
    advertises the capability from mc-agent-interface-mod#2. Unknown or expired
    ids come back as ``{"found": false, "status": ...}``, never as another
    player's context.
    """
    return await call("context", {"id": context_id})


async def _mc_save() -> Any:
    """World-save metadata: level name, world directory on the server host, players.

    Read-only and cheap (the server vantage already reports these fields in
    STATE). Use it to find the save a fork would copy, or to confirm the world
    the connected instance is running.
    """
    return await call("save")


async def _mc_snapshot(radius: float = 0.0, name: str = "") -> Any:
    """Write the entity set, **in tick order**, to a snapshot on the instance.

    The part of a world that never reaches disk: a save file has blocks and
    entity NBT but rebuilds the tick order at load time, and that order
    decides the result of anything computed entity by entity (pushes,
    cramming, explosions). The reply carries ``orderHash``, the one value
    that proves a later restore reproduced the order, and ``dir``, where
    ``entities.jsonl`` and ``meta.json`` were written.

    ``radius`` is in blocks; 0 (the default) means every entity the level
    ticks. ``name`` chooses the directory; asking for an existing name
    overwrites it. Use ``mc_fork`` instead when you also need the blocks.
    """
    return await call(
        "snapshot",
        {"radius": radius or None, "name": name or None},
        timeout=120.0,
    )


async def _mc_snapshots() -> Any:
    """List the snapshots already on the instance (name, dir, hash, tick).

    Cheap and read-only: use it to see what can be restored or compared
    before taking another snapshot.
    """
    return await call("snapshots")


async def _mc_fork(
    name: str,
    radius: float = 0.0,
    regions: bool = True,
    world_dir: str = "",
    freeze: bool = True,
) -> Any:
    """Fork the live world: freeze it, snapshot the entities, copy the world files, resume.

    Use it before an experiment you cannot repeat - an explosion, a farm
    test, anything the tick order decides - so the exact state can be
    restored into an isolated lab instance later. It runs ``/tick freeze``,
    ``/save-all flush``, then ``SNAPSHOT`` (entities plus order), then
    copies the instance's world directory into ``<snapshotDir>/world``, then
    ``/tick unfreeze``. The world stays frozen for the whole copy and is
    unfrozen even when a step fails, so a failed fork cannot leave the game
    stopped.

    ``world_dir`` overrides where the world is (otherwise the instance's
    ``STATE`` field ``worldDir`` is used). ``regions=False`` takes the
    entity-only fork: no world files, so ``forkDir``/``manifest`` come back
    null. ``freeze=False`` skips the tick commands for a world you already
    stopped yourself. Returns ``snapshotDir``, ``forkDir``, ``manifest``
    (files, bytes, skipped), ``orderHash`` and the entity count.
    """
    return await call(
        "fork",
        {
            "name": name,
            "radius": radius or None,
            "regions": regions,
            "world_dir": world_dir or None,
            "freeze": freeze,
        },
        timeout=600.0,
    )


async def _mc_restore(directory: str, dry_run: bool = True, target: str = "") -> Any:
    """Recreate a fork's entities with ``/summon``, in the recorded order.

    One command per entity, in file order, because the order the entities
    are created in is the order they will tick in. Always run it dry first:
    the dry run returns every command without sending one, so you can check
    the count and the destination before touching a world. Then call it with
    ``dry_run=False`` against the lab instance (its own bridge, pointed at the
    client or server that loaded the copied ``world`` directory).

    ``target`` labels the instance you are restoring into; this bridge talks
    to one mod connection, so it is carried through for the record rather
    than used for routing. Verify with ``mc_order`` afterwards.
    """
    return await call(
        "restore",
        {"directory": directory, "dry_run": dry_run, "target": target or None},
        timeout=600.0,
    )


async def _mc_order(directory: str, target: str = "") -> Any:
    """Check whether a world now ticks its entities in the same order as a fork.

    Takes a throwaway snapshot of the connected instance and compares its
    ``orderHash`` with the one recorded in ``directory``: ``match: true``
    means the restore reproduced the tick order, which is the acceptance
    test for a fork/restore round trip. Run it in the lab after
    ``mc_restore``; use ``target`` to label which instance it re-snapshotted.
    """
    return await call(
        "order", {"directory": directory, "target": target or None}, timeout=120.0
    )


#: ``(MCP tool name, toolkit operation, function)``. The toolkit operation is
#: what the capability filter matches against.
TOOLS: tuple[tuple[str, str, Callable[..., Any]], ...] = (
    ("mc_status", "status", _mc_status),
    ("mc_capabilities", "capabilities", _mc_capabilities),
    ("mc_player", "player", _mc_player),
    ("mc_state", "state", _mc_state),
    ("mc_entities", "entities", _mc_entities),
    ("mc_command", "command", _mc_command),
    ("mc_command_output", "command_output", _mc_command_output),
    ("mc_chat", "chat", _mc_chat),
    ("mc_record_start", "record_start", _mc_record_start),
    ("mc_record_stop", "record_stop", _mc_record_stop),
    ("mc_wait", "wait", _mc_wait),
    ("mc_screen", "screen", _mc_screen),
    ("mc_mark", "mark", _mc_mark),
    ("mc_connect", "connect", _mc_connect),
    ("mc_world", "world", _mc_world),
    ("mc_lan", "lan", _mc_lan),
    ("mc_events", "events", _mc_events),
    ("mc_context", "context", _mc_context),
    ("mc_save", "save", _mc_save),
    ("mc_snapshot", "snapshot", _mc_snapshot),
    ("mc_snapshots", "snapshots", _mc_snapshots),
    ("mc_fork", "fork", _mc_fork),
    ("mc_restore", "restore", _mc_restore),
    ("mc_order", "order", _mc_order),
)


def enabled_operations(
    surface: dict[str, Any] | None = None, vantage: str = SERVER_VANTAGE
) -> frozenset[str]:
    """Toolkit operation names the MCP front-end should register.

    A live daemon capabilities reply wins; without one, fall back to the
    documented default surface for the vantage so a Harness can start the MCP
    server before the game is up.
    """
    if isinstance(surface, dict):
        candidates = [surface]
        nested = surface.get("surface")
        if isinstance(nested, dict):
            candidates.append(nested)
        for candidate in candidates:
            supported = candidate.get("supported")
            if isinstance(supported, list):
                return frozenset(str(name) for name in supported)
    return static_supported(vantage)


def tool_names(surface: dict[str, Any] | None = None, vantage: str = SERVER_VANTAGE) -> list[str]:
    """The MCP tool names that will be registered for one surface."""
    supported = enabled_operations(surface, vantage) | ALWAYS_REGISTERED
    return [name for name, operation, _func in TOOLS if operation in supported]


class ToolkitRegistry:
    """Keeps one MCP server's tool list aligned with the live capability surface.

    The daemon is a separate long-lived process, so the MCP front-end can start
    before the game and can outlive a game restart. The registry starts from the
    surface the daemon reported at startup (or the documented default if the
    daemon was down), then refreshes in two ways:

    * ``mc_capabilities`` observes its own reply - the call that just learned
      the live surface immediately aligns the registry with it; and
    * a background poll runs for as long as the MCP session lives, so a late
      connection or a capability change is picked up even when the Harness only
      ever calls ``tools/list``.

    A failed probe keeps the last known surface instead of emptying the tool
    list while the daemon restarts.
    """

    def __init__(
        self,
        vantage: str = SERVER_VANTAGE,
        probe: Callable[[], Awaitable[dict[str, Any] | None]] | None = None,
        refresh_interval: float = 5.0,
    ) -> None:
        self.vantage = vantage
        self.probe = probe or probe_surface_async
        self.refresh_interval = refresh_interval
        self.mcp: Any = None
        self.registered: set[str] = set()

    def bind(self, mcp: Any) -> None:
        self.mcp = mcp

    @contextlib.asynccontextmanager
    async def lifespan(self, _server: Any):
        """Poll the daemon while this MCP session is alive."""
        task = asyncio.create_task(self.watch())
        try:
            yield {}
        finally:
            task.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await task

    async def watch(self) -> None:
        while True:
            await self.refresh()
            await asyncio.sleep(self.refresh_interval)

    async def refresh(self) -> set[str]:
        try:
            surface = await self.probe()
        except Exception:  # noqa: BLE001 - a polling loop must survive a daemon restart
            surface = None
        if surface is None:
            return set(self.registered)
        return self.sync(surface)

    def supported_tools(self, surface: dict[str, Any] | None = None) -> set[str]:
        operations = enabled_operations(surface, self.vantage) | ALWAYS_REGISTERED
        return {name for name, operation, _func in TOOLS if operation in operations}

    def sync(self, surface: dict[str, Any] | None = None) -> set[str]:
        """Add tools the surface supports and remove tools it no longer does."""
        desired = self.supported_tools(surface)
        for name in sorted(self.registered - desired):
            self._remove(name)
        for name, _operation, func in TOOLS:
            if name in desired and name not in self.registered:
                self._add(name, func)
        self.registered = desired
        return set(desired)

    def _add(self, name: str, func: Callable[..., Any]) -> None:
        tool = self._observed(name, func)
        add_tool = getattr(self.mcp, "add_tool", None)
        if add_tool is not None:
            add_tool(tool, name=name)
        else:  # very old FastMCP builds only had the decorator
            self.mcp.tool(name=name)(tool)

    def _remove(self, name: str) -> None:
        remove_tool = getattr(self.mcp, "remove_tool", None)
        if remove_tool is not None:
            remove_tool(name)

    def _observed(self, name: str, func: Callable[..., Any]) -> Callable[..., Any]:
        """Wrap ``mc_capabilities`` so a fresh CAPS answer updates the registry."""
        if name != "mc_capabilities":
            return func

        # functools.wraps preserves the signature (via __wrapped__), so the SDK
        # still derives an empty input schema instead of advertising *args and
        # **kwargs as arguments the model must supply.
        @functools.wraps(func)
        async def observed(*args: Any, **kwargs: Any) -> Any:
            result = await func(*args, **kwargs)
            if isinstance(result, dict):
                self.sync(result)
            return result

        return observed


def build_server(
    surface: dict[str, Any] | None = None,
    vantage: str = SERVER_VANTAGE,
    registry: ToolkitRegistry | None = None,
) -> Any:
    server_class = _server_class()
    registry = registry if registry is not None else ToolkitRegistry(vantage)
    registry.vantage = vantage
    try:
        mcp = server_class("mc-agent-bridge", lifespan=registry.lifespan)
    except TypeError as error:
        if "lifespan" not in str(error):
            raise
        # Older MCP SDKs without a lifespan parameter still get the
        # mc_capabilities-triggered refresh, just not the background poll.
        mcp = server_class("mc-agent-bridge")
    registry.bind(mcp)
    registry.sync(surface)
    return mcp


async def probe_surface_async(timeout: float = 5.0) -> dict[str, Any] | None:
    """Ask a running daemon for its filtered surface, or ``None`` if it is down."""
    client = LocalApiClient(api_host(), api_port())
    try:
        await client.connect(retry=False)
        return await client.call("capabilities", timeout=timeout)
    except (OSError, RuntimeError, TimeoutError, ConnectionError):
        return None
    finally:
        await client.close()


def probe_surface(timeout: float = 5.0) -> dict[str, Any] | None:
    """Synchronous probe for startup, before the MCP SDK's loop exists."""
    try:
        return asyncio.run(probe_surface_async(timeout))
    except (OSError, RuntimeError, TimeoutError, ConnectionError):
        return None


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


def main(transport: str = "stdio", vantage: str = SERVER_VANTAGE) -> None:
    build_server(probe_surface(), vantage=vantage).run(transport=transport)
