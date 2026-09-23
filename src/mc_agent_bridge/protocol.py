"""Client for the mc-agent-interface mod protocol (TCP, JSON lines)."""

from __future__ import annotations

import asyncio
import inspect
import json
from collections.abc import Awaitable, Callable
from typing import Any

#: Everything the mod can answer a request with. Replies are a *closed* set:
#: they all come from one place in the mod. Events are open-ended (new ones get
#: added as the mod grows), so routing must not depend on knowing every event
#: name - getting that backwards silently shifts the whole request/response
#: stream by one, which is exactly what happened when the mod grew a "lan" event.
#: The snapshot protocol (docs/protocol-snapshot.md in the mc-agent meta
#: repository) adds two: SNAPSHOT answers ``snapshot_ack`` (which the ``*_ack``
#: rule below already covers) and SNAPSHOTS answers ``snapshots`` - a name that
#: does not end in ``_ack``, so it has to be listed here or the listing would be
#: routed to the event stream and the caller would wait for a reply that had
#: already arrived.
REPLY_TYPES = {
    "state",
    "entities",
    "screen",
    "capabilities",
    "error",
    "pong",
    "snapshot_ack",
    "snapshots",
}

#: Kept for readability and for mod builds that predate the event marker.
EVENT_TYPES = {
    "hello",
    "chat",
    "game",
    "mark",
    "sample_start",
    "sample_progress",
    "sample_done",
    "lan",
    "auto_connect",
    "world_failed",
}

#: One entity snapshot of a busy world is hundreds of kilobytes of JSON, and
#: asyncio's default line limit is 64 KiB: without this, readline() raises and
#: the connection silently dies mid-request.
MAX_LINE_BYTES = 16 * 1024 * 1024


def is_event(message: dict[str, Any]) -> bool:
    """Decide whether a mod line is a push event rather than a request reply.

    Newer mod builds mark events with ``"event": true``; anything that is not a
    known reply type and not an ``*_ack`` counts as an event as well, so an event
    type this bridge has never heard of cannot corrupt the reply stream.
    """
    if message.get("event") is True:
        return True
    kind = message.get("type")
    if not isinstance(kind, str):
        return False
    if kind in REPLY_TYPES or kind.endswith("_ack"):
        return False
    return True


async def _maybe_await(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value


class ModClient:
    """One persistent connection to the interface mod.

    Requests are serialized (one in flight at a time). Events are dispatched to
    ``on_event`` and never mixed into the request/response queue.
    """

    def __init__(
        self,
        host: str = "127.0.0.1",
        port: int = 25580,
        on_event: Callable[[dict[str, Any]], Any] | None = None,
    ) -> None:
        self.host = host
        self.port = port
        self.on_event = on_event
        self.hello: dict[str, Any] | None = None
        self.reader: asyncio.StreamReader | None = None
        self.writer: asyncio.StreamWriter | None = None
        self._responses: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self._request_lock = asyncio.Lock()
        self._closed = asyncio.Event()
        self._reader_task: asyncio.Task[None] | None = None

    async def connect(self, retry: bool = True) -> dict[str, Any]:
        while True:
            try:
                self.reader, self.writer = await asyncio.open_connection(
                    self.host, self.port, limit=MAX_LINE_BYTES
                )
                self.hello = None
                self._closed.clear()
                self._reader_task = asyncio.create_task(self._read_loop())
                for _ in range(200):
                    if self.hello is not None:
                        return self.hello
                    await asyncio.sleep(0.025)
                raise TimeoutError("mod did not send a hello message")
            except OSError:
                if not retry:
                    raise
                await asyncio.sleep(2)

    async def request(self, line: str, timeout: float = 15.0) -> dict[str, Any]:
        async with self._request_lock:
            if self.writer is None or self.writer.is_closing():
                raise ConnectionError("mod connection is not available")
            self.writer.write((line + "\n").encode("utf-8"))
            await self.writer.drain()
            try:
                return await asyncio.wait_for(self._responses.get(), timeout=timeout)
            except asyncio.TimeoutError as error:
                raise TimeoutError(f"no response for {line!r} within {timeout}s") from error

    async def _read_loop(self) -> None:
        assert self.reader is not None
        try:
            while True:
                raw = await self.reader.readline()
                if not raw:
                    break
                try:
                    message = json.loads(raw.decode("utf-8"))
                except (UnicodeDecodeError, json.JSONDecodeError):
                    continue
                event_type = message.get("type")
                if event_type == "hello":
                    self.hello = message
                    if self.on_event is not None:
                        await self._dispatch_event(message)
                elif is_event(message):
                    if self.on_event is not None:
                        await self._dispatch_event(message)
                else:
                    await self._responses.put(message)
        except (ConnectionError, OSError):
            pass
        except Exception as error:  # noqa: BLE001 - a broken line must not kill the link
            print(f"[mc-agent-bridge] mod read loop stopped: {error!r}")
        finally:
            self._closed.set()

    async def _dispatch_event(self, message: dict[str, Any]) -> None:
        try:
            await _maybe_await(self.on_event(message))  # type: ignore[misc]
        except Exception as error:  # noqa: BLE001 - a bad listener must not kill the link
            print(f"[mc-agent-bridge] event listener failed: {error!r}")

    @property
    def connected(self) -> bool:
        return self.writer is not None and not self.writer.is_closing() and not self._closed.is_set()

    async def wait_closed(self) -> None:
        await self._closed.wait()

    async def close(self) -> None:
        if self.writer is not None:
            self.writer.close()
            try:
                await self.writer.wait_closed()
            except OSError:
                pass
        if self._reader_task is not None:
            self._reader_task.cancel()
            try:
                await self._reader_task
            except (asyncio.CancelledError, Exception):
                pass


def read_port_file(path: str) -> int | None:
    try:
        with open(path, "r", encoding="utf-8") as handle:
            return int(handle.read().strip())
    except (OSError, ValueError):
        return None
