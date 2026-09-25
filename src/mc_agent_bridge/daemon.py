"""Bridge daemon: the single process that owns the connection to the interface mod.

The daemon is deliberately the *only* component that talks to the game. The CLI,
the MCP server and any agent loop are all thin clients of the small JSON-lines
API it serves on loopback, so agent runtimes can be added, restarted or swapped
without ever touching the Minecraft process.

    agent runtime  <->  bridge daemon  <->  interface mod  <->  game
"""

from __future__ import annotations

import asyncio
import math
import os
import time
import uuid
from collections import deque
from typing import Any

from . import adapters, fork, restore
from .discovery import VANTAGE_SERVER, PortResolution, resolve_port
from .local_api import LocalApiServer
from .protocol import ModClient
from .toolkit import (
    OPERATION_BY_NAME,
    UnsupportedCapability,
    missing_capabilities,
    normalize_capabilities,
    surface,
)

DEFAULT_API_PORT = 8765

#: A snapshot writes one JSON record per entity to the instance's disk, which can
#: take far longer than a plain request.
SNAPSHOT_TIMEOUT = 120.0

_EVENT_CATEGORIES = {
    "hello": "hello",
    "chat": "chat",
    "game": "game",
    "mark": "mark",
    "error": "error",
    "disconnected": "error",
    "bridge_disconnected": "error",
}


def categorize(message: dict[str, Any]) -> str:
    """Map a raw mod message onto a coarse event category."""
    kind = str(message.get("type") or "")
    if kind in _EVENT_CATEGORIES:
        return _EVENT_CATEGORIES[kind]
    if kind.startswith("sample"):
        return "sample"
    if kind.endswith("_error"):
        return "error"
    return "other"


class BridgeDaemon:
    """Owns one mod connection, serves the loopback API, buffers recent events."""

    def __init__(
        self,
        host: str = "127.0.0.1",
        mod_port: int | None = None,
        port_file: str | None = None,
        api_host: str = "127.0.0.1",
        api_port: int = DEFAULT_API_PORT,
        buffer_size: int = 1000,
        reconnect_delay: float = 2.0,
        vantage: str = VANTAGE_SERVER,
        server_dir: str | None = None,
    ) -> None:
        self.host = host
        self.explicit_port = mod_port
        self.port_file = port_file
        self.vantage = vantage
        self.server_dir = server_dir
        self.api_host = api_host
        self.api_port = api_port
        self.reconnect_delay = reconnect_delay

        self.discovery: PortResolution = resolve_port(
            mod_port, port_file, vantage=vantage, server_dir=server_dir
        )
        self.mod_port: int | None = self.discovery.port
        self.port_source = self.discovery.source
        self.hello: dict[str, Any] | None = None
        self.instance: str | None = None
        self.mod_capabilities: frozenset[str] | None = None
        self.client: ModClient | None = None
        self.connected = False
        self.last_error: str | None = None
        self.started_at = time.time()
        #: Identifies this daemon run. Events get ``eventId = streamId:seq``, so
        #: consumers (webhook forwarders, agent loops) can deduplicate retries
        #: without coordinating with the daemon.
        self.stream_id = uuid.uuid4().hex

        self._buffer: deque[dict[str, Any]] = deque(maxlen=buffer_size)
        self._seq = 0
        self._running = False
        self._stop = asyncio.Event()
        self.server = LocalApiServer(api_host, api_port, self.handle)

    # ----------------------------------------------------------------- lifecycle

    async def run(self) -> None:
        await self.server.start()
        self._running = True
        print(
            f"[mc-agent-bridge] local API on {self.api_host}:{self.server.port} "
            f"(vantage: {self.vantage})",
            flush=True,
        )
        reported_discovery: str | None = None
        while self._running:
            self.discovery = resolve_port(
                self.explicit_port, self.port_file, vantage=self.vantage, server_dir=self.server_dir
            )
            if not self.discovery.resolved:
                # No fallback to the other vantage and no guessed default: say
                # what is missing and keep watching for the port file to appear.
                self.connected = False
                self.mod_port = None
                self.port_source = self.discovery.source
                self.last_error = self.discovery.error
                if self.discovery.error != reported_discovery:
                    print(f"[mc-agent-bridge] {self.discovery.error}", flush=True)
                    reported_discovery = self.discovery.error
                await self._sleep_or_stop(self.reconnect_delay)
                continue

            reported_discovery = None
            self.mod_port = self.discovery.port
            self.port_source = self.discovery.source
            client = ModClient(self.host, self.mod_port, on_event=self._record_event)
            try:
                hello = await client.connect(retry=False)
            except (OSError, TimeoutError, asyncio.TimeoutError) as error:
                self.connected = False
                self.last_error = (
                    f"cannot reach interface mod on {self.host}:{self.mod_port}: {error}"
                )
                print(f"[mc-agent-bridge] {self.last_error}; retrying", flush=True)
                await self._sleep_or_stop(self.reconnect_delay)
                continue

            self.client = client
            self.hello = hello
            self.last_error = None
            # Negotiate before publishing readiness: while ``client`` is set but
            # ``connected`` is false, game operations are refused instead of
            # being let through as an unknown legacy mod and sent once the CAPS
            # request releases ModClient's lock.
            instance = hello.get("instance") or None
            # CAPS is the authority for the surface; fall back to the hello
            # frame, then to "unknown" (a legacy mod the bridge cannot filter).
            advertised: object = hello.get("capabilities")
            try:
                caps_reply = await client.request("CAPS", timeout=10.0)
            except (OSError, TimeoutError, asyncio.TimeoutError, ConnectionError):
                caps_reply = None
            if isinstance(caps_reply, dict) and caps_reply.get("type") == "capabilities":
                if isinstance(caps_reply.get("capabilities"), (list, tuple)):
                    advertised = caps_reply["capabilities"]
                if caps_reply.get("instance"):
                    instance = str(caps_reply["instance"])
            self.instance = instance
            self.mod_capabilities = normalize_capabilities(advertised)
            self.connected = True
            described = (
                f"{len(self.mod_capabilities)} capabilities"
                if self.mod_capabilities is not None
                else "capabilities unknown (legacy mod)"
            )
            print(
                f"[mc-agent-bridge] connected to {self.instance or 'interface'} mod on port "
                f"{self.mod_port} ({described})",
                flush=True,
            )
            # ModClient already forwards the hello frame to on_event, so it is
            # in the buffer and in front of every later event by now.
            await client.wait_closed()
            await client.close()  # release the socket instead of waiting for the GC

            self.connected = False
            self.client = None
            self.instance = None
            self.mod_capabilities = None
            await self._record_event(
                {
                    "type": "bridge_disconnected",
                    "text": f"mod connection on port {self.mod_port} closed",
                }
            )
            if not self._stop.is_set():
                await self._sleep_or_stop(self.reconnect_delay)

    async def stop(self) -> None:
        self._running = False
        self._stop.set()
        if self.client is not None:
            await self.client.close()
        await self.server.stop()

    async def _sleep_or_stop(self, seconds: float) -> None:
        try:
            await asyncio.wait_for(self._stop.wait(), timeout=seconds)
        except (asyncio.TimeoutError, TimeoutError):
            pass

    # -------------------------------------------------------------------- events

    async def _record_event(self, message: dict[str, Any]) -> None:
        category = categorize(message)
        self._seq += 1
        payload = dict(message)
        payload["seq"] = self._seq
        payload["streamId"] = self.stream_id
        payload["eventId"] = f"{self.stream_id}:{self._seq}"
        payload["category"] = category
        payload["receivedAt"] = int(time.time() * 1000)
        # The unmerged chat-context work may spell its event reference either
        # way; normalize it so consumers have one stable field to read.
        reference = adapters.context_id(payload)
        if reference is not None:
            payload["contextId"] = reference
        self._buffer.append(payload)
        self.server.broadcast(category, payload)

    def recent_events(
        self, since: int | None = None, limit: int = 200, category: str | None = None
    ) -> dict[str, Any]:
        # `dropped` is judged against the whole buffer: filtering by category
        # must not look like a gap in the sequence numbers.
        oldest = self._buffer[0]["seq"] if self._buffer else None
        dropped = since is not None and oldest is not None and oldest > since + 1
        events = [
            event for event in self._buffer if category is None or event["category"] == category
        ]
        if since is not None:
            events = [event for event in events if event["seq"] > since]
        if limit > 0:
            events = events[-limit:]
        return {"events": events, "next": self._seq + 1, "dropped": dropped}

    # --------------------------------------------------------- request handling

    async def handle(self, method: str, params: dict[str, Any]) -> Any:
        operation = OPERATION_BY_NAME.get(method)
        if operation is not None:
            missing = missing_capabilities(operation, self.mod_capabilities)
            if missing:
                raise UnsupportedCapability(
                    operation,
                    missing,
                    instance=self.instance,
                    vantage=self.vantage,
                )
        handler = getattr(self, f"_m_{method}", None)
        if handler is None:
            raise ValueError(f"unknown bridge method: {method}")
        return await handler(params)

    def surface(self) -> dict[str, Any]:
        """The filtered operation surface for the current mod connection."""
        return surface(
            instance=self.instance,
            capabilities=self.mod_capabilities,
            vantage=self.vantage,
        )

    async def _call_mod(self, line: str, timeout: float = 15.0) -> dict[str, Any]:
        client = self.client
        if client is not None and not self.connected:
            raise RuntimeError(
                "the bridge is still negotiating capabilities with the interface mod; retry shortly"
            )
        if client is None or not self.connected:
            raise RuntimeError(
                f"interface mod is not connected (last error: {self.last_error or 'none'})"
            )
        reply = await client.request(line, timeout=timeout)
        if reply.get("type") == "error":
            raise RuntimeError(str(reply.get("message") or "mod reported an error"))
        return reply

    async def _m_ping(self, _params: dict[str, Any]) -> dict[str, Any]:
        return {"pong": True, "connected": self.connected}

    async def _m_status(self, _params: dict[str, Any]) -> dict[str, Any]:
        return {
            "connected": self.connected,
            "vantage": self.vantage,
            "instance": self.instance,
            "mod": {"host": self.host, "port": self.mod_port, "portSource": self.port_source},
            "discovery": self.discovery.as_dict(),
            "capabilities": (
                sorted(self.mod_capabilities) if self.mod_capabilities is not None else None
            ),
            "hello": self.hello,
            "lastError": self.last_error,
            "api": {
                "host": self.api_host,
                "port": self.server.port,
                "clients": len(self.server.clients),
            },
            "events": {"buffered": len(self._buffer), "lastSeq": self._seq, "streamId": self.stream_id},
            "uptimeSeconds": round(time.time() - self.started_at, 3),
        }

    async def _m_capabilities(self, _params: dict[str, Any]) -> dict[str, Any]:
        reply = await self._call_mod("CAPS")
        instance = str(reply.get("instance") or self.instance or "") or None
        advertised = reply.get("capabilities")
        if isinstance(advertised, (list, tuple)):
            self.mod_capabilities = normalize_capabilities(advertised)
            self.instance = instance
        enriched = dict(reply)
        enriched["instance"] = instance
        enriched["modCapabilities"] = (
            sorted(self.mod_capabilities) if self.mod_capabilities is not None else None
        )
        enriched["surface"] = self.surface()
        return enriched

    async def _m_state(self, _params: dict[str, Any]) -> dict[str, Any]:
        return await self._call_mod("STATE")

    async def _m_player(self, params: dict[str, Any]) -> dict[str, Any]:
        """Per-player server context, via the adaptive adapter boundary.

        The mod API is still open work (mc-agent-interface-mod#1); the daemon
        only reaches this handler when CAPS advertises the capability, so an
        unavailable operation fails in the capability gate instead of sending
        a request the mod cannot answer.
        """
        identifier = (
            str(
                params.get("player")
                or params.get("uuid")
                or params.get("name")
                or params.get("id")
                or ""
            ).strip()
        )
        if not identifier:
            raise ValueError('player needs a name or UUID: {"player": "<name-or-uuid>"}')
        reply = await self._call_mod(adapters.player_line(_one_line(identifier)))
        return adapters.player_context(reply, identifier)

    async def _m_screen(self, _params: dict[str, Any]) -> dict[str, Any]:
        return await self._call_mod("SCREEN")

    async def _m_entities(self, params: dict[str, Any]) -> dict[str, Any]:
        radius = params.get("radius")
        line = "ENTITIES" if radius in (None, "") else f"ENTITIES {float(radius)}"
        # A world with thousands of entities produces a big reply; give it room.
        return await self._call_mod(line, timeout=60.0)

    async def _m_command(self, params: dict[str, Any]) -> dict[str, Any]:
        command = str(params.get("command") or "").strip()
        return await self._call_mod(f"CMD {_one_line(command)}")

    async def _m_command_output(self, params: dict[str, Any]) -> dict[str, Any]:
        """Run a command and return the answer it produced.

        The server vantage collects command output in the ``cmd_ack`` itself
        (and mirrors it as game events); the client vantage answers only with
        an ack, so the output is collected from the event buffer instead. One
        method gives MCP and the CLI the same behavior.
        """
        command = str(params.get("command") or "").strip()
        if not command:
            raise ValueError('command_output needs a command: {"command": "..."}')
        wait = float(params.get("wait") or 2.0)
        cursor = self._seq
        ack = await self._call_mod(f"CMD {_one_line(command)}")
        ack_output = ack.get("output")
        if isinstance(ack_output, list):
            return {
                "type": "command_output",
                "command": ack.get("detail") or command,
                "output": [str(line) for line in ack_output],
                "source": "ack",
            }
        await asyncio.sleep(max(0.1, min(wait, 30.0)))
        events = self.recent_events(since=cursor, limit=200)
        output = [
            str(event.get("text") or "")
            for event in events["events"]
            if event.get("category") in ("game", "chat")
        ]
        return {
            "type": "command_output",
            "command": ack.get("detail") or command,
            "output": output,
            "source": "events",
        }

    async def _m_chat(self, params: dict[str, Any]) -> dict[str, Any]:
        message = str(params.get("message") or "")
        return await self._call_mod(f"CHAT {_one_line(message)}")

    async def _m_mark(self, params: dict[str, Any]) -> dict[str, Any]:
        text = _one_line(str(params.get("text") or ""))
        return await self._call_mod(f"MARK {text}")

    async def _m_connect(self, params: dict[str, Any]) -> dict[str, Any]:
        address = _one_line(str(params.get("address") or "")).strip()
        return await self._call_mod(f"CONNECT {address}")

    async def _m_world(self, params: dict[str, Any]) -> dict[str, Any]:
        level = _one_line(str(params.get("level") or "")).strip()
        if not level:
            raise ValueError("world needs a level name")
        return await self._call_mod(f"WORLD {level}")

    async def _m_save(self, _params: dict[str, Any]) -> dict[str, Any]:
        """World-save metadata the server vantage already reports in STATE.

        No new mod API: this is the read-only view a Harness needs to locate
        the save a fork would copy, without pulling a full entity snapshot.
        """
        state = await self._call_mod("STATE")
        world_dir = state.get("worldDir")
        exists: bool | None = None
        if isinstance(world_dir, str) and world_dir:
            exists = os.path.isdir(world_dir)
        return {
            "type": "world_save",
            "instance": state.get("instance") or self.instance,
            "levelName": state.get("levelName"),
            "worldDir": world_dir,
            "worldDirExistsOnBridgeHost": exists,
            "serverVersion": state.get("serverVersion"),
            "tick": state.get("tick"),
            "players": state.get("players"),
            "playerList": state.get("playerList"),
            "levels": state.get("levels"),
        }

    async def _m_lan(self, params: dict[str, Any]) -> dict[str, Any]:
        port = int(params.get("port") or 0)
        mode = str(params.get("mode") or "").strip().lower()
        if mode not in ("", "online", "offline"):
            raise ValueError("lan mode must be online or offline")
        if port <= 0 and not mode:
            return await self._call_mod("LAN")
        line = f"LAN {max(0, port)}" + (f" {mode}" if mode else "")
        return await self._call_mod(line.strip())

    async def _m_record_start(self, params: dict[str, Any]) -> dict[str, Any]:
        ticks = int(params.get("ticks") or 200)
        radius = float(params.get("radius") or 64.0)
        interval = int(params.get("interval") or 1)
        return await self._call_mod(f"SAMPLE_START {ticks} {radius} {interval}")

    async def _m_record_stop(self, _params: dict[str, Any]) -> dict[str, Any]:
        return await self._call_mod("SAMPLE_STOP")

    async def _m_wait(self, params: dict[str, Any]) -> dict[str, Any]:
        ticks = int(params.get("ticks") or 0)
        # The mod answers once the wait elapses, so the timeout must outlive it.
        return await self._call_mod(f"WAIT {ticks}", timeout=ticks / 20.0 + 20.0)

    # ------------------------------------------------------- snapshot and forks

    async def _m_snapshot(self, params: dict[str, Any]) -> dict[str, Any]:
        """`SNAPSHOT [radius] [name]`: the entity set, in tick order, on the mod's disk."""
        name = _snapshot_name(params.get("name")) or None
        return await self._call_mod(
            fork.snapshot_line(params.get("radius"), name), timeout=SNAPSHOT_TIMEOUT
        )

    async def _m_snapshots(self, _params: dict[str, Any]) -> dict[str, Any]:
        """`SNAPSHOTS`: pass through what is already on the instance's disk."""
        return await self._call_mod("SNAPSHOTS")

    async def _m_fork(self, params: dict[str, Any]) -> dict[str, Any]:
        """Freeze, save, snapshot the entities, copy the world, unfreeze.

        The order matters: `/save-all flush` is what turns a frozen world into
        chunks on disk, and the copy happens while the game is still frozen so
        the region files cannot move underneath it. The unfreeze is not optional
        - a fork that fails halfway must never leave the live world frozen.
        """
        # A name is optional: without one the mod picks its own directory name,
        # which the ack reports back as "id"/"dir".
        name = _snapshot_name(params.get("name")) or None
        radius = params.get("radius")
        regions = _as_bool(params.get("regions"), default=True)
        freeze = _as_bool(params.get("freeze"), default=True)
        world_dir = str(params.get("world_dir") or "").strip() or None

        try:
            if freeze:
                await self._call_mod("CMD /tick freeze")
            await self._call_mod("CMD /save-all flush")
            ack = await self._call_mod(fork.snapshot_line(radius, name), timeout=SNAPSHOT_TIMEOUT)
            snapshot_dir = str(ack.get("dir") or "").strip()
            if not snapshot_dir:
                raise RuntimeError(f"SNAPSHOT {name!r} answered without a dir: {ack!r}")

            fork_dir: str | None = None
            manifest: dict[str, Any] | None = None
            source: str | None = None
            if regions:
                source = await self._world_dir(world_dir)
                fork_dir = os.path.join(snapshot_dir, "world")
                manifest = await asyncio.to_thread(fork.copy_world, source, fork_dir)

            outcome = {
                "snapshotDir": snapshot_dir,
                "forkDir": fork_dir,
                "worldDir": source,
                "manifest": manifest,
                "orderHash": ack.get("orderHash"),
                "entities": ack.get("entities"),
                "tick": ack.get("tick"),
                "dimension": ack.get("dimension"),
                "name": name or ack.get("id"),
                "replaced": bool(ack.get("replaced")),
            }
        except BaseException:
            # Unfreezing is best effort here: it must happen, but it must not
            # hide the failure that got us here.
            if freeze:
                await self._unfreeze_best_effort()
            raise
        else:
            # Nothing failed, so an unfreeze failure is real news: the caller is
            # told the world may still be frozen instead of getting a "success".
            if freeze:
                await self._call_mod("CMD /tick unfreeze")
            return outcome

    async def _m_restore(self, params: dict[str, Any]) -> dict[str, Any]:
        """Restore a fork under guard, then prove the result (dry run by default).

        The old behaviour was "send one ``/summon`` per entity and count the
        acks". That cannot tell commands issued from state restored, so the
        apply path now validates the recording first, proves which instance and
        world it is about to write to, loads the recorded chunks, refuses to
        duplicate leftovers (or clears them explicitly), freezes the tick, then
        re-snapshots and compares full state before unfreezing. ``target`` stays
        a label: the destination is proven with ``expect_instance`` /
        ``expect_world_dir`` / ``expect_level`` and the recorded dimension,
        never by routing on a name this daemon cannot resolve.
        """
        directory = str(params.get("directory") or params.get("dir") or "").strip()
        if not directory:
            raise ValueError("restore needs a directory: the snapshot mc_fork returned")
        dry_run = _as_bool(params.get("dry_run"), default=True)
        target = str(params.get("target") or "").strip() or None
        expect_instance = _text(params.get("expect_instance"))
        expect_world_dir = _text(params.get("expect_world_dir"))
        expect_level = _text(params.get("expect_level"))
        expect_dimension = _text(params.get("expect_dimension"))
        freeze = _as_bool(params.get("freeze"), default=True)
        forceload = _as_bool(params.get("forceload"), default=True)
        release_forceload = _as_bool(params.get("release_forceload"), default=False)
        check_existing = _as_bool(params.get("check_existing"), default=True)
        replace_existing = _as_bool(params.get("replace_existing"), default=False)
        keep_going = _as_bool(params.get("keep_going"), default=False)
        verify = _as_bool(params.get("verify"), default=True)
        strict = _as_bool(params.get("strict"), default=True)
        verify_radius = params.get("verify_radius")
        pos_tolerance = _number(params.get("pos_tolerance"), restore.POS_TOLERANCE, "pos_tolerance")
        vel_tolerance = _number(params.get("vel_tolerance"), restore.VEL_TOLERANCE, "vel_tolerance")
        collision_radius = _number(
            params.get("collision_radius"), restore.COLLISION_RADIUS, "collision_radius"
        )
        ignore_nbt_keys = _as_list(params.get("ignore_nbt_keys"), "ignore_nbt_keys")

        meta, entities = fork.read_snapshot(directory)
        issues = restore.validate_snapshot(meta, entities)
        blockers = restore.blocking_issues(issues)
        if blockers:
            raise restore.RestoreError(restore.describe_blockers(directory, blockers))
        plan = restore.restore_plan(entities)

        endpoint = await self._destination_endpoint()
        result: dict[str, Any] = {
            "directory": directory,
            "target": target,
            "targetIsLabel": True,
            "orderHash": meta.get("orderHash"),
            "dimension": meta.get("dimension"),
            "levelName": meta.get("levelName"),
            "count": len(entities),
            "summonable": len(plan["items"]),
            "skipped": plan["skipped"],
            "playersExcluded": meta.get("playersSkipped"),
            "warnings": [issue.as_dict() for issue in issues if not issue.blocking],
            "endpoint": endpoint,
            "checks": {
                "endpoint": restore.endpoint_checks(
                    endpoint,
                    expect_instance=expect_instance,
                    expect_world_dir=expect_world_dir,
                    expect_level=expect_level,
                ),
                "dimension": None,
                "existing": None,
                "tick": None,
            },
        }
        if dry_run:
            result["dryRun"] = True
            result["commands"] = plan["commands"]
            result["ok"] = True
            return result

        box = restore.bounding_box(entities)
        try:
            if forceload and box is not None:
                await self._call_mod(f"CMD {restore.forceload_add_command(box)}")
            _, baseline_meta, baseline_entities, baseline_dir = await self._take_snapshot(
                restore.snapshot_name("pre", target)
            )
            result["baselineSnapshotDir"] = baseline_dir

            dimension = restore.dimension_check(meta, baseline_meta, expect_dimension)
            result["checks"]["dimension"] = dimension
            if not dimension["ok"]:
                raise restore.RestoreError(
                    "destination dimension check failed: "
                    f"{dimension.get('reason')}. Nothing was restored - the summons would land in "
                    "the wrong dimension. Point the bridge at the intended instance, or pass "
                    "expect_dimension explicitly if that is really the destination you mean."
                )

            found = restore.collisions(entities, baseline_entities, collision_radius)
            existing = {
                "destinationEntities": len(baseline_entities),
                "collisions": found["count"],
                "uuidCollisions": found["uuidCollisions"],
                "spatialCollisions": found["spatialCollisions"],
                "action": "none",
            }
            result["checks"]["existing"] = existing
            if found["count"] and check_existing and not replace_existing:
                raise restore.RestoreError(
                    f"destination already holds {found['count']} entity/entities that the restore "
                    f"would duplicate (uuid collisions: {found['uuidCollisions']['count']}, "
                    f"same-type-at-recorded-position: {found['spatialCollisions']['count']}). "
                    "Nothing was restored. Clear the copied world first (kill in the recorded box, "
                    "then save-all flush) or pass replace_existing=true to let the bridge do it."
                )
            if found["count"] and check_existing and replace_existing:
                # Two passes: the first kill of a chest minecart drops its
                # inventory as item entities, and the second removes those
                # drops. The box is supposed to hold the recording, not the
                # recording plus its popped-out contents.
                await self._call_mod(f"CMD {restore.clear_box_command(box)}")
                await self._call_mod(f"CMD {restore.clear_box_command(box)}")
                await self._call_mod("CMD /save-all flush")
                _, recheck_meta, recheck_entities, recheck_dir = await self._take_snapshot(
                    restore.snapshot_name("pre-cleared", target)
                )
                remaining = restore.collisions(entities, recheck_entities, collision_radius)
                inside = restore.entities_in_box(recheck_entities, box)
                existing["action"] = "cleared+flushed"
                existing["clearedSnapshotDir"] = recheck_dir
                existing["remainingCollisions"] = remaining["count"]
                existing["remainingInBox"] = len(inside)
                if remaining["count"] or inside:
                    raise restore.RestoreError(
                        "replace_existing cleared the recorded box but "
                        f"{len(inside) or remaining['count']} entity/entities are still there after "
                        "save-all flush; refusing a restore that would duplicate or collide. "
                        "Leftovers: "
                        f"{inside[:5] or remaining['uuidCollisions']['sample'] or remaining['spatialCollisions']['sample']}"
                    )
                if recheck_entities:
                    existing["note"] = (
                        f"{len(recheck_entities)} pre-existing entity/entities outside the recording "
                        "were left in place"
                    )
                baseline_meta, baseline_entities = recheck_meta, recheck_entities
            elif baseline_entities:
                if not found["count"]:
                    existing["action"] = "none (no collisions)"
                    existing["note"] = "destination entities do not collide with the recording"
                else:
                    existing["action"] = "allowed (check_existing=false)"
                    existing["note"] = (
                        f"{found['count']} duplicate(s) allowed by check_existing=false and left in place"
                    )
        except BaseException:
            if release_forceload and box is not None:
                await self._forceload_remove_best_effort(box)
            raise

        tick: dict[str, Any] = {"prior": "unknown", "frozenByBridge": False, "restored": None}
        frozen_by_bridge = False
        if freeze:
            prior = await self._tick_query()
            tick["prior"] = "frozen" if prior is True else "running" if prior is False else "unknown"
            if prior is not True:
                await self._call_mod("CMD /tick freeze")
                frozen_by_bridge = True
            tick["frozenByBridge"] = frozen_by_bridge
        result["checks"]["tick"] = tick

        issued = 0
        attempted: list[dict[str, Any]] = []
        failed: list[dict[str, Any]] = []
        outputs: dict[int, list[str]] = {}
        verification: dict[str, Any] | None = None
        try:
            for item in plan["items"]:
                if failed and not keep_going:
                    break
                attempted.append(item)
                try:
                    ack = await self._call_mod(f"CMD {_one_line(str(item['command']))}")
                except Exception as error:  # noqa: BLE001 - one entity must not hide the rest
                    failed.append({**_failed_item(item), "error": str(error)})
                    continue
                issued += 1
                # The game reports both success ("Summoned new ...") and
                # failure through the collected output, and the text is
                # locale-dependent, so output is evidence, not a verdict: the
                # verification snapshot decides whether the entity is there.
                output = ack.get("output") if isinstance(ack, dict) else None
                messages = (
                    [str(line) for line in output if str(line).strip()]
                    if isinstance(output, list)
                    else []
                )
                if messages:
                    outputs[int(item["index"])] = messages

            if verify:
                try:
                    _, actual_meta, actual, actual_dir = await self._take_snapshot(
                        restore.snapshot_name("check", target), verify_radius
                    )
                    result["verifiedSnapshotDir"] = actual_dir
                    verification = restore.compare_snapshots(
                        meta,
                        entities,
                        actual_meta,
                        actual,
                        strict=strict,
                        pos_tolerance=pos_tolerance,
                        vel_tolerance=vel_tolerance,
                        ignore_nbt_keys=ignore_nbt_keys,
                    )
                    actual_uuids = {str(entity.get("uuid") or "") for entity in actual}
                    already_failed = {int(failure["index"]) for failure in failed}
                    for item in attempted:
                        if int(item["index"]) in already_failed:
                            continue
                        if str(item["uuid"]) in actual_uuids:
                            continue
                        failure = {
                            **_failed_item(item),
                            "error": "the entity is not present in the post-restore snapshot",
                        }
                        if outputs.get(int(item["index"])):
                            failure["gameOutput"] = outputs[int(item["index"])]
                        failed.append(failure)
                except Exception as error:  # noqa: BLE001 - report, do not lose the issued count
                    verification = {"ok": False, "error": f"verification snapshot failed: {error}"}
        finally:
            if frozen_by_bridge:
                try:
                    await self._call_mod("CMD /tick unfreeze")
                    tick["restored"] = True
                except Exception as error:  # noqa: BLE001 - the issued/failed counts matter more
                    tick["restored"] = False
                    tick["unfreezeError"] = str(error)
            if release_forceload and box is not None:
                tick["forceloadReleased"] = await self._forceload_remove_best_effort(box)

        result["dryRun"] = False
        result["issued"] = issued
        result["failed"] = failed
        result["notAttempted"] = len(plan["items"]) - len(attempted)
        result["partial"] = bool(failed)
        result["verification"] = verification
        result["tick"] = tick
        result["ok"] = not failed and bool((verification or {}).get("ok", True))
        return result

    async def _m_verify(self, params: dict[str, Any]) -> dict[str, Any]:
        """Compare a fork with the connected destination, full state and all.

        This is the post-restore check without the restore: take a fresh
        snapshot of the connected instance and compare UUID order, counts,
        positions, velocities and NBT - not just ``orderHash``, because an
        inventory can change while the order hash stays identical.
        """
        directory = str(params.get("directory") or params.get("dir") or "").strip()
        if not directory:
            raise ValueError("verify needs a directory: the snapshot mc_fork returned")
        target = str(params.get("target") or "").strip() or None
        strict = _as_bool(params.get("strict"), default=True)
        radius = params.get("snapshot_radius")
        pos_tolerance = _number(params.get("pos_tolerance"), restore.POS_TOLERANCE, "pos_tolerance")
        vel_tolerance = _number(params.get("vel_tolerance"), restore.VEL_TOLERANCE, "vel_tolerance")
        ignore_nbt_keys = _as_list(params.get("ignore_nbt_keys"), "ignore_nbt_keys")

        meta, entities = fork.read_snapshot(directory)
        issues = restore.validate_snapshot(meta, entities)
        blockers = restore.blocking_issues(issues)
        if blockers:
            raise restore.RestoreError(restore.describe_blockers(directory, blockers))
        _, actual_meta, actual, actual_dir = await self._take_snapshot(
            restore.snapshot_name("check", target), radius
        )
        report = restore.compare_snapshots(
            meta,
            entities,
            actual_meta,
            actual,
            strict=strict,
            pos_tolerance=pos_tolerance,
            vel_tolerance=vel_tolerance,
            ignore_nbt_keys=ignore_nbt_keys,
        )
        return {
            "directory": directory,
            "target": target,
            "targetIsLabel": True,
            "snapshotDir": actual_dir,
            "expected": {
                "orderHash": report["orderHash"]["expected"],
                "count": len(entities),
                "dimension": meta.get("dimension"),
            },
            "actual": {
                "orderHash": report["orderHash"]["actual"],
                "count": len(actual),
                "dimension": actual_meta.get("dimension"),
            },
            "warnings": [issue.as_dict() for issue in issues if not issue.blocking],
            "verification": report,
            "ok": report["ok"],
        }

    async def _take_snapshot(
        self, name: str, radius: Any = None
    ) -> tuple[dict[str, Any], dict[str, Any], list[dict[str, Any]], str]:
        """``SNAPSHOT`` on the connected instance, read back from disk.

        The reply is only a pointer: the file the mod wrote is what a later
        restore would consume, so the meta and entities come from disk.
        """
        ack = await self._call_mod(fork.snapshot_line(radius, name), timeout=SNAPSHOT_TIMEOUT)
        snapshot_dir = str(ack.get("dir") or "").strip()
        if not snapshot_dir:
            raise RuntimeError(f"SNAPSHOT {name!r} answered without a dir: {ack!r}")
        meta, entities = fork.read_snapshot(snapshot_dir)
        return ack, meta, entities, snapshot_dir

    async def _destination_endpoint(self) -> dict[str, Any]:
        """What the connected instance reports about itself, for endpoint proofs."""
        endpoint: dict[str, Any] = {"instance": self.instance, "modPort": self.mod_port}
        try:
            state = await self._call_mod("STATE")
        except Exception as error:  # noqa: BLE001 - a missing STATE is reported, not fatal
            endpoint["error"] = str(error)
            return endpoint
        if not isinstance(state, dict):
            endpoint["error"] = f"STATE answered {type(state).__name__}, not an object"
            return endpoint
        endpoint.update(
            {
                "worldDir": state.get("worldDir"),
                "levelName": state.get("levelName"),
                "serverVersion": state.get("serverVersion"),
                "tick": state.get("tick"),
                "players": state.get("players"),
                "dimensions": [
                    entry.get("dimension")
                    for entry in (state.get("levels") or [])
                    if isinstance(entry, dict)
                ],
            }
        )
        return endpoint

    async def _tick_query(self) -> bool | None:
        """``/tick query`` as a tri-state: frozen, running, or unknown."""
        try:
            reply = await self._call_mod("CMD /tick query")
        except Exception:  # noqa: BLE001 - unknown is a valid, reportable answer
            return None
        output = reply.get("output") if isinstance(reply, dict) else None
        if not isinstance(output, list):
            return None
        return restore.tick_state_from_output(output)

    async def _forceload_remove_best_effort(self, box: tuple[float, ...]) -> bool:
        try:
            await self._call_mod(f"CMD {restore.forceload_remove_command(box)}")
            return True
        except Exception as error:  # noqa: BLE001 - releasing the box is cleanup, not the result
            print(f"[mc-agent-bridge] warning: could not release forceload: {error}", flush=True)
            return False

    async def _m_order(self, params: dict[str, Any]) -> dict[str, Any]:
        """Re-snapshot the connected instance and compare its order hash with a fork.

        The hash is the acceptance test for the whole track: equal hashes mean
        the entities tick in the same order the fork recorded, which is the part
        a save file cannot preserve.
        """
        directory = str(params.get("directory") or params.get("dir") or "").strip()
        if not directory:
            raise ValueError("order needs a directory: the snapshot to compare against")
        target = str(params.get("target") or "").strip() or None

        expected = str(fork.read_meta(directory).get("orderHash") or "")
        if not expected:
            raise fork.SnapshotError(f"{directory}: meta.json has no orderHash to compare with")

        name = fork.order_check_name(target)
        ack = await self._call_mod(fork.snapshot_line(None, name), timeout=SNAPSHOT_TIMEOUT)
        snapshot_dir = str(ack.get("dir") or "").strip()
        if not snapshot_dir:
            raise RuntimeError(f"SNAPSHOT {name!r} answered without a dir: {ack!r}")
        # Read the fresh hash off disk rather than trusting the ack: the file is
        # what a later restore would actually consume.
        fresh = fork.read_meta(snapshot_dir)
        actual = str(fresh.get("orderHash") or "")
        entities = fresh.get("entities")
        if not isinstance(entities, int):
            entities = len(fork.read_entities(snapshot_dir))
        return {
            "match": bool(actual) and actual == expected,
            "expected": expected,
            "actual": actual or None,
            "entities": entities,
            "directory": directory,
            "snapshotDir": snapshot_dir,
            "target": target,
        }

    async def _world_dir(self, explicit: str | None) -> str:
        """Where the instance keeps the world we are about to copy."""
        if explicit:
            return explicit
        state = await self._call_mod("STATE")
        found = str(state.get("worldDir") or "").strip()
        if not found:
            raise RuntimeError(
                "cannot resolve the world directory to copy: the instance reported no "
                'STATE "worldDir" (the mod fills it only when the server vantage has a '
                "world loaded); pass world_dir=<path> to fork a world it cannot see"
            )
        return found

    async def _unfreeze_best_effort(self) -> None:
        try:
            await self._call_mod("CMD /tick unfreeze")
        except Exception as error:  # noqa: BLE001 - the original failure matters more
            print(f"[mc-agent-bridge] warning: could not /tick unfreeze: {error}", flush=True)

    async def _m_context(self, params: dict[str, Any]) -> dict[str, Any]:
        """Retrieve a chat-time context bundle by its stable reference.

        The mod API is still open work (mc-agent-interface-mod#2); like
        ``player``, this handler is only reachable when CAPS advertises the
        capability. An unknown or expired id comes back structured, not as a
        different player's context.
        """
        context_id = (
            str(
                params.get("id")
                or params.get("context_id")
                or params.get("contextId")
                or ""
            ).strip()
        )
        if not context_id:
            raise ValueError('context needs an id from a chat event: {"id": "<context-id>"}')
        reply = await self._call_mod(adapters.context_line(_one_line(context_id)))
        return adapters.context_bundle(reply, context_id)

    async def _m_events(self, params: dict[str, Any]) -> dict[str, Any]:
        return self.recent_events(
            since=params.get("since"),
            limit=int(params.get("limit") or 200),
            category=params.get("category"),
        )

    async def _m_stop(self, _params: dict[str, Any]) -> dict[str, Any]:
        loop = asyncio.get_running_loop()
        loop.call_later(0.2, lambda: loop.create_task(self.stop()))
        return {"stopping": True}


def _one_line(text: str) -> str:
    """The mod protocol is line based, so collapse anything that could break framing."""
    return text.replace("\r", " ").replace("\n", " ")


def _text(value: Any) -> str | None:
    text = str(value or "").strip()
    return text or None


def _number(value: Any, default: float, name: str) -> float:
    if value is None or value == "":
        return float(default)
    try:
        number = float(value)
    except (TypeError, ValueError) as error:
        raise ValueError(f"{name} must be a number, got {value!r}") from error
    if not math.isfinite(number):
        raise ValueError(f"{name} must be finite, got {value!r}")
    return number


def _as_list(value: Any, name: str) -> list[str]:
    if value is None or value == "":
        return []
    if isinstance(value, str):
        return [value]
    if isinstance(value, (list, tuple)):
        return [str(item) for item in value]
    raise ValueError(f"{name} must be a string or a list of strings, got {value!r}")


def _failed_item(item: dict[str, Any]) -> dict[str, Any]:
    """The stable identity of one planned summon, for a failure record."""
    return {"index": item["index"], "uuid": item["uuid"], "command": item["command"]}


def _snapshot_name(value: Any) -> str:
    """Snapshot names become directory names on the instance, so keep them one token."""
    name = _one_line(str(value or "")).strip()
    if any(character.isspace() for character in name):
        raise ValueError(f"snapshot names cannot contain spaces: {name!r}")
    return name


def _as_bool(value: Any, default: bool) -> bool:
    """JSON callers send real booleans, CLI callers send "true"/"false"."""
    if value is None or value == "":
        return default
    if isinstance(value, bool):
        return value
    text = str(value).strip().lower()
    if text in ("true", "1", "yes", "on"):
        return True
    if text in ("false", "0", "no", "off"):
        return False
    raise ValueError(f"expected a boolean, got {value!r}")
