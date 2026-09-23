"""A stand-in for the interface mod that speaks the same line protocol.

Used by the test-suite so the bridge can be exercised without launching
Minecraft.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any


class FakeMod:
    def __init__(self, port: int = 0) -> None:
        self.query_port = port
        self.port: int | None = None
        self.server: asyncio.AbstractServer | None = None
        self.lines: list[str] = []
        self._writers: list[asyncio.StreamWriter] = []

    async def start(self) -> int:
        self.server = await asyncio.start_server(self._on_client, "127.0.0.1", self.query_port)
        assert self.server.sockets
        self.port = self.server.sockets[0].getsockname()[1]
        return self.port

    async def stop(self) -> None:
        for writer in list(self._writers):
            writer.close()
            try:
                await writer.wait_closed()
            except (ConnectionError, OSError):
                pass
        self._writers.clear()
        if self.server is not None:
            self.server.close()
            await self.server.wait_closed()

    async def push(self, message: dict[str, Any]) -> None:
        line = json.dumps(message, ensure_ascii=False) + "\n"
        for writer in list(self._writers):
            writer.write(line.encode("utf-8"))
            await writer.drain()

    async def _on_client(self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
        self._writers.append(writer)
        hello = {
            "type": "hello",
            "mod": "mc-agent-interface",
            "version": "0.1.0",
            "protocol": 1,
            "minecraft": "test",
            "port": self.port,
            "capabilities": ["state", "entities", "command", "chat"],
        }
        writer.write((json.dumps(hello) + "\n").encode("utf-8"))
        await writer.drain()
        try:
            while True:
                raw = await reader.readline()
                if not raw:
                    break
                line = raw.decode("utf-8").strip()
                if not line:
                    continue
                self.lines.append(line)
                reply = self.reply_for(line)
                writer.write((json.dumps(reply, ensure_ascii=False) + "\n").encode("utf-8"))
                await writer.drain()
        except (ConnectionError, OSError):
            pass
        finally:
            if writer in self._writers:
                self._writers.remove(writer)
            writer.close()
            try:
                await writer.wait_closed()
            except (ConnectionError, OSError):
                pass

    def reply_for(self, line: str) -> dict[str, Any]:
        upper = line.upper()
        if upper == "STATE":
            return {"type": "state", "tick": 1, "inWorld": True, "name": "Bot"}
        if upper == "CAPS":
            return {"type": "capabilities", "protocol": 1}
        if upper.startswith("ENTITIES"):
            return {"type": "entities", "entities": []}
        if upper.startswith("BIG"):
            # >64 KiB of JSON, like an entity snapshot of a busy world.
            return {
                "type": "entities",
                "entities": [
                    {"id": index, "type": "minecraft:tnt", "x": index * 1.5, "z": index * -0.25}
                    for index in range(4000)
                ],
            }
        if upper.startswith("CHAT "):
            return {"type": "chat_ack", "detail": line[5:]}
        if upper.startswith("CMD "):
            return {"type": "cmd_ack", "detail": line[4:]}
        if upper.startswith("SAMPLE_START"):
            return {"type": "sample_start_ack", "detail": "ticks=10"}
        if upper == "SAMPLE_STOP":
            return {"type": "sample_stop_ack", "detail": "samples=0"}
        if upper.startswith("WAIT "):
            return {"type": "wait_ack", "detail": line[5:]}
        return {"type": "error", "message": f"unknown command: {line}"}
