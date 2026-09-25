"""A stand-in for the interface mod that speaks the same line protocol.

Used by the test-suite so the bridge can be exercised without launching
Minecraft.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import time
import uuid as uuid_module
from typing import Any, Callable

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
        #: Replies for the adaptive operations (unmerged mod APIs).
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
        #: The dimension the fake reports in STATE/SNAPSHOT; tests flip it to
        #: exercise the bridge's wrong-dimension refusal.
        self.dimension = "minecraft:overworld"
        #: ``/tick freeze`` state, so ``/tick query`` answers truthfully.
        self.tick_frozen = False
        #: When set, ``/tick query`` answers with exactly these lines (tests use
        #: an unreadable answer to exercise the unknown-prior-state refusal).
        self.tick_query_output: list[str] | None = None
        #: Snapshot names are recorded here with the tick state they were taken
        #: in, so tests can prove evidence was collected while frozen.
        self.snapshot_tick_states: list[bool] = []
        #: CMD substring -> message: the fake answers that command with the
        #: message in its output instead of a silent success (server vantage).
        self.failing_commands: dict[str, str] = {}
        #: CMD substring -> message: the fake answers with a protocol error, so
        #: the bridge's ``_call_mod`` raises (transport-level failure).
        self.raising_commands: dict[str, str] = {}
        #: UUIDs handed to summons whose NBT carries no UUID (test fixtures).
        self.summon_uuids: list[str] = []
        #: Called with each CMD body before the reply is built; tests use it to
        #: mutate ``entity_records`` mid-restore.
        self.on_command: Callable[[str], None] | None = None
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
            return self.command_reply(line[4:].strip())
        if upper.startswith("SAMPLE_START"):
            return {"type": "sample_start_ack", "detail": "ticks=10"}
        if upper == "SAMPLE_STOP":
            return {"type": "sample_stop_ack", "detail": "samples=0"}
        if upper.startswith("WAIT "):
            return {"type": "wait_ack", "detail": line[5:]}
        return {"type": "error", "message": f"unknown command: {line}"}

    # ------------------------------------------------------------- commands

    def command_reply(self, command: str) -> dict[str, Any]:
        """A CMD ack, with the fake game's tick/summon side effects applied.

        The bridge sends ``CMD /line``; the real mod strips the slash before
        running the vanilla command, so the fake does too.
        """
        detail = command
        if command.startswith("/"):
            command = command[1:]
        if self.on_command is not None:
            self.on_command(command)
        if command.startswith("tick query"):
            output = self.tick_query_output
            if output is None:
                output = [
                    "The game is frozen." if self.tick_frozen else "The game is running normally."
                ]
            return {"type": "cmd_ack", "detail": detail, "output": list(output)}
        if command.startswith("tick freeze"):
            self.tick_frozen = True
        elif command.startswith("tick unfreeze"):
            self.tick_frozen = False
        for marker, message in self.raising_commands.items():
            if marker in command:
                return {"type": "error", "message": message}
        for marker, message in self.failing_commands.items():
            if marker in command:
                return {"type": "cmd_ack", "detail": detail, "output": [message]}
        if command.startswith("summon "):
            self._simulate_summon(command)
        ack: dict[str, Any] = {"type": "cmd_ack", "detail": detail}
        if self.cmd_output is not None:
            ack["output"] = list(self.cmd_output)
        return ack

    def _simulate_summon(self, command: str) -> None:
        """Append what a successful ``/summon <type> <x> <y> <z> <nbt>`` created."""
        tokens = command.split(" ", 5)
        if len(tokens) < 6:
            return
        entity_type = tokens[1]
        try:
            x, y, z = (float(value) for value in tokens[2:5])
        except ValueError:
            return
        nbt = tokens[5].strip()
        entity_uuid = _uuid_from_nbt(nbt)
        if entity_uuid is None and self.summon_uuids:
            entity_uuid = self.summon_uuids.pop(0)
        if entity_uuid is None:
            entity_uuid = str(uuid_module.uuid4())
        self.entity_records.append(
            {
                "order": len(self.entity_records),
                "uuid": entity_uuid,
                "type": entity_type,
                "pos": [x, y, z],
                "vel": _motion_from_nbt(nbt),
                "nbt": nbt,
                "passengers": [],
                "vehicle": None,
                "restorable": True,
            }
        )

    # ------------------------------------------------------------- snapshots

    def take_snapshot(self, arguments: str) -> dict[str, Any]:
        """`SNAPSHOT [radius] [name]`: write the files the real mod would write."""
        self.snapshot_tick_states.append(self.tick_frozen)
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
            "dimension": self.dimension,
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
            "dimension": self.dimension,
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


def _uuid_from_nbt(nbt: str) -> str | None:
    """The canonical UUID string for an SNBT ``"UUID":[I;a,b,c,d]``, if present."""
    match = re.search(r'"?UUID"?:\[I;(-?\d+),(-?\d+),(-?\d+),(-?\d+)\]', nbt)
    if not match:
        return None
    parts = [int(value) & 0xFFFFFFFF for value in match.groups()]
    hexed = "".join(f"{value:08x}" for value in parts)
    return f"{hexed[0:8]}-{hexed[8:12]}-{hexed[12:16]}-{hexed[16:20]}-{hexed[20:32]}"


def _motion_from_nbt(nbt: str) -> list[float]:
    match = re.search(
        r'"?Motion"?:\[(-?[\d.eE+]+)d,(-?[\d.eE+]+)d,(-?[\d.eE+]+)d\]', nbt
    )
    if not match:
        return [0.0, 0.0, 0.0]
    try:
        return [float(match.group(index)) for index in (1, 2, 3)]
    except ValueError:
        return [0.0, 0.0, 0.0]
