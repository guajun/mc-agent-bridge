# mc-agent-bridge

📖 Part of **mc-agent**; the guide lives at <https://guajun.github.io/mc-agent/>.

An agent-agnostic bridge between a Minecraft client and **any** agent runtime.

The bridge does not contain an agent, and it does not know anything about what
you want to do in the game. It exposes the game as a small set of primitives -
state, entities, commands, chat, recording, waiting - and gets out of the way.

```
 agent runtime  <->  bridge daemon  <->  interface mod  <->  Minecraft
 (Hermes, Codex,      this repo        mc-agent-interface      (Fabric)
  your own loop)                        (Java, in-game)
```

## Why a separate daemon

The mod accepts exactly one kind of client: a TCP line connection. Rather than
letting every agent session open its own socket to the game, a single long-lived
daemon owns that connection and re-serves it on loopback as a JSON-lines API.

That buys three things:

* **Swappable agent runtimes.** Hermes today, Codex tomorrow, a shell script
  next week. They all speak the same API and none of them touch the game.
* **Survivable agent sessions.** An agent that restarts, or an MCP server that
  is spawned per session, does not disturb the game connection.
* **Event replay.** The daemon keeps a ring buffer of recent events, so an agent
  can ask "what happened while I was thinking?" with a cursor instead of
  needing to be alive at the exact moment something happened.

MCP is offered as an optional front-end (`mc-bridge mcp`), not as the core. MCP
is a pull-based tool interface: a client spawns the server, so the game cannot
wake an agent through it. Waking an agent on a game event is the job of an agent
loop that subscribes to the daemon's event stream - see `mc-agent-loop`.

## Requirements

* Python 3.11+
* The `mc-agent-interface` Fabric mod running in the client

## Install

```bash
pip install -e .            # core: daemon, local API, CLI
pip install -e ".[mcp]"     # plus the MCP front-end
```

## Quick start

```bash
# 1. Start the daemon (keep it running while the game is open)
mc-bridge run

# 2. In another shell, talk to the game
mc-bridge call state
mc-bridge call entities '{"radius": 32}'
mc-bridge call command '{"command": "time set day"}'
mc-bridge call chat '{"message": "hello from outside"}'

# 3. Watch the event stream
mc-bridge watch --events chat,game
```

### Finding the game

The daemon needs the port the mod actually bound to. The mod writes it to
`port.txt` inside its data directory (`<gameDir>/mc-agent/port.txt`) and falls
back to the next free port when the preferred one is busy.

Resolution order:

1. `--mod-port`
2. `MC_AGENT_PORT_FILE`, `./port.txt`, `./mc-agent/port.txt`
3. `25580`

For a fully deterministic setup, launch the game with
`-Dmcagent.port=25580 -Dmcagent.dir=<path>` so that the port never moves.

## The local API

Newline-delimited JSON over loopback. Requests carry an `id`; events are pushed
to subscribed connections.

```jsonc
{"id": "1", "method": "state", "params": {}}
{"type": "response", "id": "1", "ok": true, "result": {"type": "state", "tick": 4211, "x": 12.5}}
{"type": "event", "event": "chat", "data": {"type": "chat", "text": "hi", "sender": "someone", "seq": 7}}
```

| Method | Params | Maps to mod line |
| --- | --- | --- |
| `ping` | - | local only |
| `status` | - | local only: connection, port, buffer, clients |
| `capabilities` | - | `CAPS` |
| `state` | - | `STATE` |
| `entities` | `radius` | `ENTITIES [radius]` |
| `command` | `command` | `CMD <command>` |
| `chat` | `message` | `CHAT <message>` |
| `record_start` | `ticks`, `radius`, `interval` | `SAMPLE_START <ticks> <radius> <interval>` |
| `record_stop` | - | `SAMPLE_STOP` |
| `wait` | `ticks` | `WAIT <ticks>` |
| `screen` | - | `SCREEN` |
| `mark` | `text` | `MARK <text>` |
| `connect` | `address` | `CONNECT <address>` |
| `world` | `level` | `WORLD <level>` (open a single-player save) |
| `lan` | `port`, `mode` | `LAN [port] [online\|offline]` (publish the world to the LAN) |
| `snapshot` | `radius`, `name` | `SNAPSHOT [radius] [name]` (entities + tick order, on the instance's disk) |
| `snapshots` | - | `SNAPSHOTS` (what is already on the instance) |
| `fork` | `name`, `radius`, `regions`, `world_dir`, `freeze` | freeze → `save-all flush` → `SNAPSHOT` → copy the world → unfreeze |
| `restore` | `directory`, `dry_run`, `target` | `/summon` per entity, in recorded order (dry run by default) |
| `order` | `directory`, `target` | re-snapshot and compare `orderHash` with a fork |
| `events` | `since`, `limit`, `category` | replay from the buffer |
| `stop` | - | shut the daemon down |

Every client connection can also call `subscribe` / `unsubscribe` with a list of
event categories: `hello`, `chat`, `game`, `mark`, `sample`, `error`, `other`,
or `*` for everything.

`events` returns `{"events": [...], "next": <cursor>, "dropped": <bool>}`.
Feed `next` back as `since` to poll incrementally; `dropped` warns that the
ring buffer discarded events you never saw.

## MCP front-end

```bash
mc-bridge mcp                      # stdio, for MCP clients that spawn servers
mc-bridge mcp --transport streamable-http
```

Tools: `mc_status`, `mc_capabilities`, `mc_state`, `mc_entities`, `mc_command`,
`mc_command_output`, `mc_chat`, `mc_record_start`, `mc_record_stop`, `mc_wait`,
`mc_screen`, `mc_mark`, `mc_connect`, `mc_world`, `mc_lan`, `mc_events`,
`mc_snapshot`, `mc_snapshots`, `mc_fork`, `mc_restore`, `mc_order`.

`mc_command` and `mc_command_output` exist because a command's *answer* is chat,
not a return value: the first one just sends it, the second sends it and collects
the feedback. Anything that reports data - `data get entity <name> Motion`,
`player <name> ...`, mod commands - should use the second.

Verified against `mcp` 2.x (where the SDK renamed `FastMCP` to `MCPServer`) and
1.x; the front-end picks whichever class the installed SDK provides.

The equivalent of the older `mc-codex-bridge` design was one special-purpose
daemon per agent. Here the daemon is neutral and each agent attaches however it
likes: MCP, the JSON-lines API, or a loop built on this package.

## Forking a live world

A save file has the blocks and the entity NBT, but not the **tick order**:
entities are appended to the level's tick list as chunks load, and that order
decides the result of anything computed entity by entity - pushes, cramming,
explosions. `fork` therefore freezes the game, asks the mod for the order, and
copies the world files; `restore` puts the entities back in that order.

```bash
# 1. What has already been snapshotted on this instance?
mc-bridge call snapshots

# 2. Fork: freeze -> save-all flush -> SNAPSHOT -> copy the world -> unfreeze
mc-bridge call fork '{"name": "before-fight", "radius": 64}'
# {
#   "snapshotDir": "C:/mc-agent/snapshots/before-fight",
#   "forkDir":     "C:/mc-agent/snapshots/before-fight/world",
#   "worldDir":    "C:/.../saves/量子硫方怪",
#   "manifest":    {"files": 42, "bytes": 88123456, "skipped": ["advancements", "logs", ...]},
#   "orderHash":   "9f2c0a1b2c3d4e5f",
#   "entities":    521
# }

# 3. Move forkDir into the lab instance's saves/, launch it with the mod, and
#    point a bridge at that instance (here on a second API port):
mc-bridge --api-port 8799 call restore '{"directory": "C:/mc-agent/snapshots/before-fight"}'
mc-bridge --api-port 8799 call restore '{"directory": "C:/mc-agent/snapshots/before-fight", "dry_run": false}'

# 4. The acceptance test: the re-snapshot must report the same order.
mc-bridge --api-port 8799 call order '{"directory": "C:/mc-agent/snapshots/before-fight"}'
# {"match": true, "expected": "9f2c0a1b2c3d4e5f", "actual": "9f2c0a1b2c3d4e5f", "entities": 521}
```

`restore` is a dry run until you say otherwise: it returns the commands it would
send, which is the cheap way to check the count and the destination first. Both
`restore` and `order` take a `target` label naming the lab instance they are
pointed at; a bridge owns one mod connection, so the label is carried through
for the record rather than used for routing.

The copy is the instance's whole world directory minus the things a lab must not
inherit - `session.lock`, the player data (`playerdata/` before 1.21, `players/`
in 26.2), `stats/`, `advancements/`, `logs/` - so a fork keeps `level.dat`, the
world's `data/` and `datapacks/`, and every dimension's region, entity and POI
files (26.2 keeps those under `dimensions/<namespace>/<dimension>/`). The
returned manifest says how many files and bytes landed where. The live world is
unfrozen even when a step fails, so a failed fork cannot leave the game stopped;
if only the entities are interesting, pass `"regions": false`.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `MC_AGENT_PORT_FILE` | - | explicit path to the mod's `port.txt` |
| `MC_AGENT_API_HOST` | `127.0.0.1` | host the MCP front-end dials |
| `MC_AGENT_API_PORT` | `8765` | port the MCP front-end dials |

The API binds to loopback only. It can run arbitrary commands as the player in
the connected world, so treat the machine it runs on as trusted.

## Embedding the bridge

```python
import asyncio
from mc_agent_bridge.local_api import LocalApiClient

async def main() -> None:
    client = LocalApiClient(port=8765)
    await client.connect()
    await client.call("subscribe", {"events": ["chat", "error"]})
    print(await client.call("state"))
    queue = await client.events()
    while True:
        print(await queue.get())

asyncio.run(main())
```

## Tests

The test-suite runs against a fake mod, so no game is required:

```bash
PYTHONPATH=src python -m unittest discover -s tests -t .
```

## Protocol details

See `docs/rfc/0001-agent-interface.md` in the `mc-agent` meta repository for the
rationale, the wire format, and the open questions (event callbacks, richer
subscriptions, non-chat triggers).

## License

MIT
