"""Local JSON-lines API used by the daemon, the agent loop and the CLI."""

from __future__ import annotations

import asyncio
import itertools
import json
import os
import sys
from collections.abc import Awaitable, Callable
from typing import Any

Handler = Callable[[str, dict[str, Any]], Awaitable[Any]]

#: Payloads can be large (an entity snapshot of a busy world). See protocol.py.
MAX_LINE_BYTES = 16 * 1024 * 1024

DEFAULT_API_HOST = "127.0.0.1"
DEFAULT_API_PORT = 8765
ENV_HOME = "MC_AGENT_HOME"
ENV_DAEMON_ADDR = "MC_AGENT_DAEMON_ADDR"
ENV_IPC_TOKEN = "MC_AGENT_IPC_TOKEN"


def default_home() -> str:
    """The mc-agent state directory, matching the Go daemon's config.Home()."""
    value = os.environ.get(ENV_HOME)
    if value:
        return os.path.abspath(value)
    if os.name == "nt":
        base = os.environ.get("APPDATA") or os.path.expanduser("~")
        return os.path.join(base, "mc-agent")
    if sys.platform == "darwin":
        return os.path.expanduser("~/Library/Application Support/mc-agent")
    base = os.environ.get("XDG_CONFIG_HOME") or os.path.expanduser("~/.config")
    return os.path.join(base, "mc-agent")


def daemon_endpoint() -> tuple[str, int, str] | None:
    """The Go daemon's loopback address and IPC token, if one is running.

    The token is read from the private state file (0600) or from
    ``MC_AGENT_IPC_TOKEN``; it is never logged and never sent anywhere except
    the daemon's own loopback listener.
    """
    address = os.environ.get(ENV_DAEMON_ADDR)
    if address:
        token = os.environ.get(ENV_IPC_TOKEN)
        if token:
            host, _, port = address.rpartition(":")
            try:
                return host or DEFAULT_API_HOST, int(port), token
            except ValueError:
                return None
    path = os.path.join(default_home(), "daemon.json")
    try:
        with open(path, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, ValueError):
        return None
    address = str(data.get("address") or "")
    token = str(data.get("token") or "")
    if not address or not token:
        return None
    host, _, port = address.rpartition(":")
    try:
        return host or DEFAULT_API_HOST, int(port), token
    except ValueError:
        return None


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
    """Client for the local bridge API (used by CLI, agent loops and legacy front-ends).

    When the Go daemon is running, its loopback address and IPC token are read
    from the private state file; every request then carries the token, which the
    Go daemon requires. The Python bridge daemon ignores unknown request fields,
    so the same client keeps working against it during migration.
    """

    def __init__(
        self,
        host: str = DEFAULT_API_HOST,
        port: int = DEFAULT_API_PORT,
        token: str | None = None,
    ) -> None:
        self.host = host
        self.port = port
        self.token = token or os.environ.get(ENV_IPC_TOKEN) or None
        self.reader: asyncio.StreamReader | None = None
        self.writer: asyncio.StreamWriter | None = None
        self._ids = itertools.count(1)
        self._pending: dict[str, asyncio.Future[Any]] = {}
        self._events: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self._reader_task: asyncio.Task[None] | None = None
        self._closed = asyncio.Event()

    async def connect(self, retry: bool = True) -> None:
        # Prefer the Go daemon's advertised endpoint when the caller used the
        # historical defaults and a daemon state file is present.
        if self.host == DEFAULT_API_HOST and self.port == DEFAULT_API_PORT:
            endpoint = daemon_endpoint()
            if endpoint is not None:
                self.host, self.port, self.token = endpoint
        while True:
            try:
                self.reader, self.writer = await asyncio.open_connection(
                    self.host, self.port, limit=MAX_LINE_BYTES
                )
                self._closed.clear()
                self._reader_task = asyncio.create_task(self._read_loop())
                return
            except OSError:
                if not retry:
                    raise
                await asyncio.sleep(2)

    async def wait_closed(self) -> None:
        """Wait until the read loop ends or the connection is closed explicitly."""
        await self._closed.wait()

    @property
    def closed(self) -> bool:
        return self._closed.is_set()

    async def call(self, method: str, params: dict[str, Any] | None = None, timeout: float = 30.0) -> Any:
        if self.writer is None:
            raise ConnectionError("local API connection is not available")
        request_id = str(next(self._ids))
        future: asyncio.Future[Any] = asyncio.get_running_loop().create_future()
        self._pending[request_id] = future
        payload = {"id": request_id, "method": method, "params": params or {}}
        if self.token:
            payload["auth"] = self.token
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
        finally:
            self._closed.set()

    async def close(self) -> None:
        self._closed.set()
        if self.writer is not None:
            self.writer.close()
            try:
                await self.writer.wait_closed()
            except OSError:
                pass
