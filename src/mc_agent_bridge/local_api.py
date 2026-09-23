"""Local JSON-lines API used by the daemon, MCP server, agent loop and CLI."""

from __future__ import annotations

import asyncio
import itertools
import json
from collections.abc import Awaitable, Callable
from typing import Any

Handler = Callable[[str, dict[str, Any]], Awaitable[Any]]

#: Payloads can be large (an entity snapshot of a busy world). See protocol.py.
MAX_LINE_BYTES = 16 * 1024 * 1024


class LocalApiServer:
    def __init__(self, host: str, port: int, handler: Handler) -> None:
        self.host = host
        self.port = port
        self.handler = handler
        self.server: asyncio.AbstractServer | None = None
        self.clients: set[LocalClient] = set()

    async def start(self) -> None:
        self.server = await asyncio.start_server(self._on_client, self.host, self.port)
        if self.port == 0 and self.server.sockets:
            # Callers may ask the OS to pick a port; record what we actually got.
            self.port = self.server.sockets[0].getsockname()[1]

    async def stop(self) -> None:
        """Stop listening and hang up on every connected client."""
        server, self.server = self.server, None
        if server is not None:
            server.close()
        for client in list(self.clients):
            await client.close()
        if server is not None:
            await server.wait_closed()

    async def _on_client(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        client = LocalClient(reader, writer, self.handler)
        self.clients.add(client)
        try:
            await client.run()
        finally:
            self.clients.discard(client)

    def broadcast(self, event: str, data: dict[str, Any]) -> None:
        for client in list(self.clients):
            if client.subscribed_to(event):
                client.send_event(event, data)


class LocalClient:
    def __init__(
        self,
        reader: asyncio.StreamReader,
        writer: asyncio.StreamWriter,
        handler: Handler,
    ) -> None:
        self.reader = reader
        self.writer = writer
        self.handler = handler
        self.events: set[str] = set()

    def subscribed_to(self, event: str) -> bool:
        return event in self.events or "*" in self.events

    async def run(self) -> None:
        try:
            while True:
                raw = await self.reader.readline()
                if not raw:
                    break
                try:
                    request = json.loads(raw.decode("utf-8"))
                except (UnicodeDecodeError, json.JSONDecodeError):
                    continue
                await self._handle(request)
        except (ConnectionError, OSError):
            pass
        except Exception as error:  # noqa: BLE001 - keep the daemon alive
            print(f"[mc-agent-bridge] client read loop stopped: {error!r}")
        finally:
            self.writer.close()
            try:
                await self.writer.wait_closed()
            except OSError:
                pass

    async def _handle(self, request: dict[str, Any]) -> None:
        request_id = request.get("id")
        method = request.get("method")
        params = request.get("params") or {}
        try:
            if method == "subscribe":
                self.events.update(params.get("events") or [])
                result: Any = {"subscribed": sorted(self.events)}
            elif method == "unsubscribe":
                self.events.difference_update(params.get("events") or [])
                result = {"subscribed": sorted(self.events)}
            elif method == "ping":
                result = {"pong": True}
            else:
                result = await self.handler(method, params)
            self._send({"type": "response", "id": request_id, "ok": True, "result": result})
        except Exception as error:  # noqa: BLE001
            self._send({"type": "response", "id": request_id, "ok": False, "error": str(error)})

    def send_event(self, event: str, data: dict[str, Any]) -> None:
        self._send({"type": "event", "event": event, "data": data})

    def _send(self, payload: dict[str, Any]) -> None:
        try:
            self.writer.write((json.dumps(payload, ensure_ascii=False) + "\n").encode("utf-8"))
        except (ConnectionError, OSError):
            pass

    async def close(self) -> None:
        self.writer.close()
        try:
            await self.writer.wait_closed()
        except (ConnectionError, OSError):
            pass


class LocalApiClient:
    """Client for the local bridge API (used by CLI, MCP and agent loops)."""

    def __init__(self, host: str = "127.0.0.1", port: int = 8765) -> None:
        self.host = host
        self.port = port
        self.reader: asyncio.StreamReader | None = None
        self.writer: asyncio.StreamWriter | None = None
        self._ids = itertools.count(1)
        self._pending: dict[str, asyncio.Future[Any]] = {}
        self._events: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self._reader_task: asyncio.Task[None] | None = None

    async def connect(self, retry: bool = True) -> None:
        while True:
            try:
                self.reader, self.writer = await asyncio.open_connection(
                    self.host, self.port, limit=MAX_LINE_BYTES
                )
                self._reader_task = asyncio.create_task(self._read_loop())
                return
            except OSError:
                if not retry:
                    raise
                await asyncio.sleep(2)

    async def call(self, method: str, params: dict[str, Any] | None = None, timeout: float = 30.0) -> Any:
        if self.writer is None:
            raise ConnectionError("local API connection is not available")
        request_id = str(next(self._ids))
        future: asyncio.Future[Any] = asyncio.get_running_loop().create_future()
        self._pending[request_id] = future
        payload = {"id": request_id, "method": method, "params": params or {}}
        self.writer.write((json.dumps(payload, ensure_ascii=False) + "\n").encode("utf-8"))
        await self.writer.drain()
        try:
            return await asyncio.wait_for(future, timeout=timeout)
        finally:
            self._pending.pop(request_id, None)

    async def events(self) -> asyncio.Queue[dict[str, Any]]:
        return self._events

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
                if message.get("type") == "event":
                    await self._events.put(message)
                elif message.get("type") == "response":
                    future = self._pending.get(str(message.get("id")))
                    if future is not None and not future.done():
                        if message.get("ok"):
                            future.set_result(message.get("result"))
                        else:
                            future.set_exception(RuntimeError(message.get("error") or "bridge error"))
        except (ConnectionError, OSError):
            pass
        except Exception as error:  # noqa: BLE001
            print(f"[mc-agent-bridge] local API read loop stopped: {error!r}")

    async def close(self) -> None:
        if self.writer is not None:
            self.writer.close()
            try:
                await self.writer.wait_closed()
            except OSError:
                pass
