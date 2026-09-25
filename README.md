# mc-agent-bridge

📖 Part of **mc-agent**; the guide lives at <https://guajun.github.io/mc-agent/>.

A small, Harness-neutral Minecraft Toolkit. It runs next to the game server,
owns one connection to the `mc-agent-interface` mod, and exposes the game as a
stable JSON surface any Harness can call over MCP or the CLI: health and
capabilities, player context, entities, commands, chat, events, context
bundles, snapshots and world-save metadata.

The Toolkit contains no model calls, no agent loop and no conversation state,
and it does not know what any particular Harness wants to do. It also does not
decide what an Agent should do: it answers questions and executes requested
primitives, and gets out of the way.

The default target is the mod's **server vantage** - the authoritative one, and
the only one that can snapshot entity tick order.

```
 MCP client / CLI / loop  <->  bridge daemon  <->  server-vantage mod  <->  server
 (any Harness, no SDKs)        this repo          mc-agent-interface      (Fabric)
```

See **[docs/toolkit.md](docs/toolkit.md)** for the install-and-use guide and
the full tool reference.

## Why a separate daemon

The mod accepts exactly one kind of client: a TCP line connection. Rather than
letting every agent session open its own socket to the game, a single long-lived
daemon owns that connection and re-serves it on loopback as a JSON-lines API.

That buys three things:

* **Swappable Harnesses.** Hermes today, Codex tomorrow, a shell script next
  week. They all speak the same API and none of them touch the game.
* **Survivable Harness sessions.** A Harness that restarts, or an MCP server
  that is spawned per session, does not disturb the game connection.
* **Event replay.** The daemon keeps a ring buffer of recent events, so an agent
  can ask "what happened while I was thinking?" with a cursor instead of
  needing to be alive at the exact moment something happened.

MCP is offered as an optional front-end (`mc-bridge mcp`), not as the core. MCP
is a pull-based tool interface: a client spawns the server, so the game cannot
wake an agent through it. Waking an agent on a game event is the job of an agent
loop that subscribes to the daemon's event stream - see `mc-agent-loop`.

## Requirements

* Python 3.11+
* The `mc-agent-interface` Fabric mod running in the same environment as the
  game server (a dedicated server, or the integrated server inside a
  single-player client)

## Install

```bash
pip install -e .            # core: daemon, local API, CLI
pip install -e ".[mcp]"     # plus the MCP front-end
```

## Quick start

```bash
# 1. Start the daemon (keep it running while the game is open)
mc-bridge run

# 2. In another shell, check the connection and what this instance can do
mc-bridge call status
mc-bridge call capabilities

# 3. Use only the operations the instance advertises
mc-bridge call state
mc-bridge call save
mc-bridge call entities '{"radius": 32}'
mc-bridge call command_output '{"command": "data get entity <name> Motion"}'

# 4. Watch the event stream
mc-bridge watch --events game,chat,mark
```

### Finding the game

The default target is the server vantage. Its entrypoint writes the port it
bound to into `<gameDir>/mc-agent-server/port.txt`; the daemon reads the file
on every reconnect, so a port that moves is picked up automatically.

Resolution order for the server vantage:

1. `--mod-port` (an explicit port always wins)
2. `--port-file` or `MC_AGENT_PORT_FILE` (the exact `port.txt`)
3. `<--server-dir | $MC_AGENT_SERVER_DIR | cwd>/mc-agent-server/port.txt`
4. `<--server-dir>/port.txt` when a server directory was named explicitly

If nothing resolves, the daemon prints an actionable error and keeps watching -
it never guesses a port and never falls back to client-vantage. You can check
discovery without starting the daemon:

```bash
mc-bridge discover
# {"vantage": "server", "port": null, "source": "unresolved",
#  "error": "cannot find the server-vantage port file ... --port-file ... --vantage client"}
```

Legacy client-vantage setups (the mod in a Minecraft client) opt in explicitly:

```bash
mc-bridge run --vantage client                      # ./port.txt, ./mc-agent/port.txt, else 25580
mc-bridge run --vantage client --port-file /path/to/mc-agent/port.txt
```

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
| `status` | - | local only: connection, vantage, port source, discovery, buffer, clients |
| `capabilities` | - | `CAPS` plus the filtered toolkit surface |
| `state` | - | `STATE` |
| `player` | `player` (UUID, name as convenience) | `PLAYER <id>` (server vantage; adaptive) |
| `entities` | `radius` | `ENTITIES [radius]` |
| `command` | `command` | `CMD <command>` |
| `command_output` | `command`, `wait` | `CMD` plus its answer (ack output, else events) |
| `chat` | `message` | `CHAT <message>` (client vantage) |
| `record_start` | `ticks`, `radius`, `interval` | `SAMPLE_START <ticks> <radius> <interval>` (client vantage) |
| `record_stop` | - | `SAMPLE_STOP` (client vantage) |
| `wait` | `ticks` | `WAIT <ticks>` |
| `screen` | - | `SCREEN` (client vantage) |
| `mark` | `text` | `MARK <text>` |
| `connect` | `address` | `CONNECT <address>` (client vantage) |
| `world` | `level` | `WORLD <level>` (client vantage; open a single-player save) |
| `lan` | `port`, `mode` | `LAN [port] [online\|offline]` (client vantage) |
| `events` | `since`, `limit`, `category` | replay from the buffer |
| `context` | `id` | `CONTEXT <id>` (server vantage; adaptive) |
| `save` | - | local composition of `STATE`: world-save metadata |
| `snapshot` | `radius`, `name` | `SNAPSHOT [radius] [name]` (entities + tick order, on the instance's disk) |
| `snapshots` | - | `SNAPSHOTS` (what is already on the instance) |
| `fork` | `name`, `radius`, `regions`, `world_dir`, `freeze` | freeze → `save-all flush` → `SNAPSHOT` → copy the world → unfreeze |
| `restore` | `directory`, `dry_run`, `target` | `/summon` per entity, in recorded order (dry run by default) |
| `order` | `directory`, `target` | re-snapshot and compare `orderHash` with a fork |
| `stop` | - | shut the daemon down |

Methods are checked against the connected mod's CAPS before anything is sent. A
client-only method on a server-vantage connection (or a server-only method on a
client connection) returns a structured error naming the missing capability -
and, for the two adaptive operations, the still-open mod issue it depends on.
`player` and `context` are the boundaries for mc-agent-interface-mod#1
(server-side player context) and #2 (chat context bundles); they light up as
soon as the connected mod advertises the capability, with no code change here.

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

```bash
mc-bridge mcp --vantage client     # legacy client surface only
```

The tool list is filtered by the connected instance's CAPS, so a server-vantage
session gets only the operations it can serve: health, capabilities, state,
entities, commands, command output, wait, mark, events, save, snapshots, fork,
restore and order - and `mc_player`/`mc_context` when the mod advertises
them - and never `mc_chat`, `mc_screen`, `mc_connect` or the other client-only
tools.
Before the daemon answers, the front-end registers the documented default
server surface; once it answers, the live CAPS reply wins. Callers should
still start with `mc_capabilities`, which returns the raw CAPS list plus the
filtered `surface.supported` / `surface.unsupported` view with reasons.

`mc_command` and `mc_command_output` exist because a command's *answer* is chat,
not a return value: the first one just sends it, the second sends it and returns
the answer - from the command reply on the server vantage, or from game/chat
events on the client vantage. Anything that reports data - `data get entity
<name> Motion`, `player <name> ...`, mod commands - should use the second.

Verified against `mcp` 2.x (where the SDK renamed `FastMCP` to `MCPServer`) and
1.x; the front-end picks whichever class the installed SDK provides. It imports
only the MCP SDK - no agent-framework SDK and no model client.

The equivalent of the older `mc-codex-bridge` design was one special-purpose
daemon per agent. Here the daemon is neutral and each agent attaches however it
likes: MCP, the JSON-lines API, or a loop built on this package.

## Forwarding events to a webhook

`mc-bridge forward` is an optional, receiver-neutral event forwarder. It
subscribes to the daemon's event stream over loopback and POSTs selected events
to one HTTP(S) URL. It contains no agent runtime and makes no model calls, and
only *outbound* requests leave the machine, so the mod and local API keep their
loopback bindings.

```bash
export MC_AGENT_WEBHOOK_URL="https://listener.example/hooks/mc-agent"
export MC_AGENT_WEBHOOK_SECRET="$(python -c 'import secrets; print(secrets.token_urlsafe(32))')"
mc-bridge forward --events chat,game,mark,error
```

Forwarding is **off by default**: without both a URL and a secret the command
refuses to start. Credentials come from the environment or a JSON config file,
never from command-line flags, so they cannot land in shell history. Queue and
retry state live in memory only - there is no durable delivery across machine
restarts, and a restarted forwarder does not replay what it missed. Receivers
should therefore treat `eventId` as the idempotency key.

### Request contract

Every delivery is an HTTP `POST` with a JSON body:

```jsonc
{
  "eventId": "9f2c0a1b...:7",        // stable; unchanged across retries
  "sequence": 7,                       // the daemon's buffered sequence
  "streamId": "9f2c0a1b...",          // identifies the daemon run
  "event": "chat",                     // bridge category: chat, game, mark, error, ...
  "type": "chat",                      // the raw server event type
  "category": "chat",
  "timestamp": 1730000000123,          // event receipt time, epoch milliseconds
  "tick": 4211,                        // game tick when available, else null
  "sender": "Alice",                   // sender identity when the event has one
  "context_id": "ctx-42",              // server-vantage chat context reference
  "data": { "...": "the original server event, verbatim" }
}
```

| Header | Meaning |
| --- | --- |
| `X-MC-Agent-Signature` | `sha256=<hex>` HMAC-SHA256 over `"<timestamp>.<raw body>"`, key = shared secret |
| `X-MC-Agent-Timestamp` | Unix seconds used in the signature |
| `X-MC-Agent-Event-Id` | Same `eventId` as the body; use it as the idempotency key |
| `X-MC-Agent-Event-Type` | Raw event type, for routing without parsing the body first |
| `X-MC-Agent-Attempt` | 1 for the first try, 2 for the first retry, ... |

A receiver verifies a request by checking that the signing timestamp is inside
its replay window (300 seconds is the documented default) and comparing the
signature in constant time. A minimal Python receiver side check:

```python
import hashlib, hmac, time

def verify(secret, header_timestamp, raw_body, signature, window=300):
    if abs(time.time() - int(header_timestamp)) > window:
        return False
    expected = "sha256=" + hmac.new(
        secret.encode(), f"{header_timestamp}.".encode() + raw_body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)
```

### Retries and status

Network failures, truncated or malformed HTTP responses, HTTP 5xx, 408, 425
and 429 are retried with bounded exponential backoff (default 1s base, 30s
cap, 5 attempts); other statuses are reported as permanent and not retried.
Redirects are never followed - following one would turn the signed POST into
a GET and drop the body - so point the forwarder at the final receiver URL;
a 3xx is a permanent failure. A retry re-signs with a fresh timestamp but
reuses the same body and `eventId`. The forwarder logs every failure,
permanent rejection and queue overflow, and `WebhookForwarder.status()` exposes
`queued`, `delivered`, `retries`, `failed`, `dropped`, `lastEventId` and a
sanitized `lastError`. Logs never contain the shared secret or the full
receiver URL - the URL's path and query are redacted too, because hosted
webhook URLs often carry a token there.

Configuring a particular receiver or Harness to consume the webhook (routes,
skills, credentials) is deliberately out of scope; the contract above is all a
receiver needs. The event source, this forwarder and the mod are expected to
run on the same machine.

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
| `MC_AGENT_SERVER_DIR` | - | game/server directory for server-vantage discovery |
| `MC_AGENT_API_HOST` | `127.0.0.1` | host the MCP front-end dials |
| `MC_AGENT_API_PORT` | `8765` | port the MCP front-end dials |
| `MC_AGENT_WEBHOOK_URL` | - | receiver URL; forwarding needs this and a secret |
| `MC_AGENT_WEBHOOK_SECRET` | - | shared secret for HMAC-SHA256 signing |
| `MC_AGENT_WEBHOOK_EVENTS` | `chat,game,mark,error` | comma separated categories, `*` for all |
| `MC_AGENT_WEBHOOK_QUEUE` | `256` | bounded in-memory delivery queue |
| `MC_AGENT_WEBHOOK_MAX_ATTEMPTS` | `5` | delivery attempts per event, first try included |
| `MC_AGENT_WEBHOOK_BACKOFF` | `1.0` | base retry backoff in seconds |
| `MC_AGENT_WEBHOOK_MAX_BACKOFF` | `30.0` | retry delay cap in seconds |
| `MC_AGENT_WEBHOOK_TIMEOUT` | `10.0` | per-request network timeout in seconds |
| `MC_AGENT_WEBHOOK_CONFIG` | - | JSON file with `url`, `secret`, `events`, ... (env overrides it) |

The API binds to loopback only. It can run arbitrary commands as the server's
command source, so treat the machine it runs on as trusted.

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
