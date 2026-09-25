"""A stand-in for the interface mod that speaks the same line protocol.

Used by the test-suite so the bridge can be exercised without launching
Minecraft.
"""

from __future__ import annotations

import asyncio
import json
import os
import time
from typing import Any

from mc_agent_bridge.fork import order_hash

#: What the client vantage advertises in the released mod (InterfaceConstants).
CLIENT_CAPABILITIES = [
    "state",
    "entities",
    "command",
    "chat",
    "record",
    "wait",
    "screen",
    "mark",
    "connect",
    "world",
    "lan",
    "events:chat",
    "events:game",
]

#: What the server vantage advertises in the released mod.
SERVER_CAPABILITIES = ["state", "entities", "command", "wait", "mark", "snapshot", "events:game"]


class FakeMod:
    """One connection, the same line protocol, and the files the real mod writes.

    The snapshot primitives are real enough to be useful: ``SNAPSHOT`` writes
    ``entities.jsonl`` and ``meta.json`` under ``snapshots_dir`` and answers with
    the on-disk order hash, so the bridge's fork path is exercised end to end
    without a game.
    """

    def __init__(
        self,
        port: int = 0,
        world_dir: str | os.PathLike[str] | None = None,
        snapshots_dir: str | os.PathLike[str] | None = None,
        entities: list[dict[str, Any]] | None = None,
        tick: int = 1,
        instance: str | None = "client",
        capabilities: list[str] | None = None,
        player_reply: dict[str, Any] | None = None,
        context_reply: dict[str, Any] | None = None,
        cmd_output: list[str] | None = None,
        caps_delay: float = 0.0,
    ) -> None:
        self.query_port = port
        self.port: int | None = None
        self.server: asyncio.AbstractServer | None = None
        self.lines: list[str] = []
        self._writers: list[asyncio.StreamWriter] = []
        #: ``instance``/``capabilities`` as the real mod reports them in hello
        #: and CAPS. ``None`` omits the field (a legacy mod shape).
        self.instance = instance
        self.capabilities = (
            list(CLIENT_CAPABILITIES) if capabilities is None else list(capabilities)
        )
        #: Replies for the adaptive operations (capability-gated mod APIs).
        self.player_reply = player_reply
        self.context_reply = context_reply
        #: When set, CMD acks carry the output array the server vantage sends.
        self.cmd_output = list(cmd_output) if cmd_output is not None else None
        #: Seconds to hold the CAPS reply, to exercise the negotiation window.
        self.caps_delay = caps_delay
        #: What STATE reports as "worldDir": only the server vantage knows it.
        self.world_dir = os.fspath(world_dir) if world_dir is not None else None
        #: Where SNAPSHOT writes, mirroring <mcagent.dir>/snapshots.
        self.snapshots_dir = os.fspath(snapshots_dir) if snapshots_dir is not None else None
        #: Entity records, in tick order, that SNAPSHOT writes out.
        self.entity_records = list(entities or [])
        self.tick = tick
        #: Set to make SNAPSHOT answer with an error, for failure-path tests.
        self.snapshot_error: str | None = None
        #: The mod may name the listing reply either way; the bridge must take both.
        self.snapshots_reply_type = "snapshots"

    @property
    def commands(self) -> list[str]:
        """The commands the bridge ran, in order, with the leading slash."""
        return [line[4:].strip() for line in self.lines if line.upper().startswith("CMD ")]

    @property
    def requests(self) -> list[str]:
        """Request lines minus the connection handshake (hello/CAPS)."""
        return [line for line in self.lines if line != "CAPS"]

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
        hello: dict[str, Any] = {
            "type": "hello",
            "mod": "mc-agent-interface",
            "version": "0.1.0",
            "protocol": 1,
            "minecraft": "test",
            "port": self.port,
        }
        if self.instance is not None:
            hello["instance"] = self.instance
        if self.capabilities is not None:
            hello["capabilities"] = self.capabilities
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
                if self.caps_delay and line.upper() == "CAPS":
                    await asyncio.sleep(self.caps_delay)
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
            state: dict[str, Any] = {
                "type": "state",
                "tick": self.tick,
                "inWorld": True,
                "name": "Bot",
                "instance": "server",
                "entities": len(self.entity_records),
            }
            if self.world_dir is not None:
                state["worldDir"] = self.world_dir
            return state
        if upper == "CAPS":
            caps: dict[str, Any] = {"type": "capabilities", "protocol": 1}
            if self.instance is not None:
                caps["instance"] = self.instance
            if self.capabilities is not None:
                caps["capabilities"] = self.capabilities
            return caps
        if upper.startswith("ENTITIES"):
            return {"type": "entities", "entities": []}
        if upper == "SNAPSHOT" or upper.startswith("SNAPSHOT "):
            if self.snapshot_error is not None:
                return {"type": "error", "message": self.snapshot_error}
            return self.take_snapshot(line[len("SNAPSHOT") :].strip())
        if upper == "SNAPSHOTS":
            return {"type": self.snapshots_reply_type, "snapshots": self.list_snapshots()}
        if upper.startswith("BIG"):
            # >64 KiB of JSON, like an entity snapshot of a busy world.
            return {
                "type": "entities",
                "entities": [
                    {"id": index, "type": "minecraft:tnt", "x": index * 1.5, "z": index * -0.25}
                    for index in range(4000)
                ],
            }
        if upper.startswith("PLAYER"):
            if self.player_reply is None:
                return {"type": "error", "message": f"unknown command: {line}"}
            return dict(self.player_reply)
        if upper.startswith("CONTEXT"):
            if self.context_reply is None:
                return {"type": "error", "message": f"unknown command: {line}"}
            return dict(self.context_reply)
        if upper.startswith("CHAT "):
            return {"type": "chat_ack", "detail": line[5:]}
        if upper.startswith("CMD "):
            ack: dict[str, Any] = {"type": "cmd_ack", "detail": line[4:]}
            if self.cmd_output is not None:
                ack["output"] = list(self.cmd_output)
            return ack
        if upper.startswith("SAMPLE_START"):
            return {"type": "sample_start_ack", "detail": "ticks=10"}
        if upper == "SAMPLE_STOP":
            return {"type": "sample_stop_ack", "detail": "samples=0"}
        if upper.startswith("WAIT "):
            return {"type": "wait_ack", "detail": line[5:]}
        return {"type": "error", "message": f"unknown command: {line}"}

    # ------------------------------------------------------------- snapshots

    def take_snapshot(self, arguments: str) -> dict[str, Any]:
        """`SNAPSHOT [radius] [name]`: write the files the real mod would write."""
        if not self.snapshots_dir:
            return {"type": "error", "message": "SNAPSHOT is not enabled on this fake"}
        radius: float | None = None
        tokens = arguments.split()
        if tokens and _is_number(tokens[0]):
            radius = float(tokens.pop(0))
        name = tokens[0] if tokens else "snapshot"
        directory = os.path.join(self.snapshots_dir, name)
        os.makedirs(directory, exist_ok=True)
        replaced = os.path.isfile(os.path.join(directory, "meta.json"))

        entities_path = os.path.join(directory, "entities.jsonl")
        with open(entities_path, "w", encoding="utf-8") as handle:
            for record in self.entity_records:
                handle.write(json.dumps(record, ensure_ascii=False) + "\n")

        hash_value = order_hash(str(record.get("uuid") or "") for record in self.entity_records)
        meta = {
            "protocol": 1,
            "mod": "mc-agent-interface",
            "modVersion": "0.1.0",
            "minecraft": "test",
            "tick": self.tick,
            "dimension": "minecraft:overworld",
            "radius": radius,
            "entities": len(self.entity_records),
            "orderHash": hash_value,
            "createdAt": int(time.time() * 1000),
            "frozen": True,
            "worldDir": self.world_dir,
            "instance": "server",
        }
        with open(os.path.join(directory, "meta.json"), "w", encoding="utf-8") as handle:
            json.dump(meta, handle, ensure_ascii=False)

        return {
            "type": "snapshot_ack",
            "id": name,
            "dir": directory,
            "entities": len(self.entity_records),
            "orderHash": hash_value,
            "tick": self.tick,
            "dimension": "minecraft:overworld",
            "bytes": os.path.getsize(entities_path),
            "replaced": replaced,
        }

    def list_snapshots(self) -> list[dict[str, Any]]:
        found: list[dict[str, Any]] = []
        if not self.snapshots_dir or not os.path.isdir(self.snapshots_dir):
            return found
        for name in sorted(os.listdir(self.snapshots_dir)):
            directory = os.path.join(self.snapshots_dir, name)
            try:
                with open(os.path.join(directory, "meta.json"), encoding="utf-8") as handle:
                    meta = json.load(handle)
            except (OSError, ValueError):
                continue
            found.append({"name": name, "dir": directory, **meta})
        return found


def _is_number(token: str) -> bool:
    try:
        float(token)
    except ValueError:
        return False
    return True
