"""Bridge daemon: the single process that owns the connection to the interface mod.

The daemon is deliberately the *only* component that talks to the game. The CLI,
the MCP server and any agent loop are all thin clients of the small JSON-lines
API it serves on loopback, so agent runtimes can be added, restarted or swapped
without ever touching the Minecraft process.

    agent runtime  <->  bridge daemon  <->  interface mod  <->  game
"""

from __future__ import annotations

import asyncio
import os
import time
from collections import deque
from typing import Any

from .local_api import LocalApiServer
from .protocol import ModClient, read_port_file

DEFAULT_MOD_PORT = 25580
DEFAULT_API_PORT = 8765

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


def port_file_candidates(explicit: str | None = None) -> list[str]:
    """Where to look for the port the mod actually bound to.

    The mod writes ``port.txt`` into ``<gameDir>/mc-agent`` and falls back to the
    next free port when the preferred one is taken, so reading that file is more
    reliable than assuming a fixed port.
    """
    if explicit:
        return [explicit]
    candidates: list[str] = []
    from_env = os.environ.get("MC_AGENT_PORT_FILE")
    if from_env:
        candidates.append(from_env)
    candidates.append(os.path.join(os.getcwd(), "port.txt"))
    candidates.append(os.path.join(os.getcwd(), "mc-agent", "port.txt"))
    return candidates


def resolve_mod_port(explicit_port: int | None, port_file: str | None) -> tuple[int, str]:
    if explicit_port:
        return int(explicit_port), "argument"
    for candidate in port_file_candidates(port_file):
        found = read_port_file(candidate)
        if found:
            return found, candidate
    return DEFAULT_MOD_PORT, "default"


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
    ) -> None:
        self.host = host
        self.explicit_port = mod_port
        self.port_file = port_file
        self.api_host = api_host
        self.api_port = api_port
        self.reconnect_delay = reconnect_delay

        self.mod_port, self.port_source = resolve_mod_port(mod_port, port_file)
        self.hello: dict[str, Any] | None = None
        self.client: ModClient | None = None
        self.connected = False
        self.last_error: str | None = None
        self.started_at = time.time()

        self._buffer: deque[dict[str, Any]] = deque(maxlen=buffer_size)
        self._seq = 0
        self._running = False
        self._stop = asyncio.Event()
        self.server = LocalApiServer(api_host, api_port, self.handle)

    # ----------------------------------------------------------------- lifecycle

    async def run(self) -> None:
        await self.server.start()
        self._running = True
        print(f"[mc-agent-bridge] local API on {self.api_host}:{self.server.port}", flush=True)
        while self._running:
            self.mod_port, self.port_source = resolve_mod_port(self.explicit_port, self.port_file)
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
            self.connected = True
            self.last_error = None
            print(
                f"[mc-agent-bridge] connected to interface mod on port {self.mod_port}",
                flush=True,
            )
            # ModClient already forwards the hello frame to on_event, so it is
            # in the buffer and in front of every later event by now.
            await client.wait_closed()
            await client.close()  # release the socket instead of waiting for the GC

            self.connected = False
            self.client = None
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
        payload["category"] = category
        payload["receivedAt"] = int(time.time() * 1000)
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
        handler = getattr(self, f"_m_{method}", None)
        if handler is None:
            raise ValueError(f"unknown bridge method: {method}")
        return await handler(params)

    async def _call_mod(self, line: str, timeout: float = 15.0) -> dict[str, Any]:
        client = self.client
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
            "mod": {"host": self.host, "port": self.mod_port, "portSource": self.port_source},
            "hello": self.hello,
            "lastError": self.last_error,
            "api": {
                "host": self.api_host,
                "port": self.server.port,
                "clients": len(self.server.clients),
            },
            "events": {"buffered": len(self._buffer), "lastSeq": self._seq},
            "uptimeSeconds": round(time.time() - self.started_at, 3),
        }

    async def _m_capabilities(self, _params: dict[str, Any]) -> dict[str, Any]:
        return await self._call_mod("CAPS")

    async def _m_state(self, _params: dict[str, Any]) -> dict[str, Any]:
        return await self._call_mod("STATE")

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
