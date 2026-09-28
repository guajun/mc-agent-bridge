# Harness-neutral Toolkit guide

This is the legacy Python guide for `mc-agent-bridge` as a Toolkit. The
product CLI/daemon is the Go binary in [go/README.md](../go/README.md); MCP was
removed in bridge #12 and this package remains for the legacy daemon and the
local `fork`/`restore` tooling until it is migrated or retired.

A Toolkit process runs next to a Minecraft server and exposes the game over a
small JSON surface. It contains no model calls, no agent loop, no conversation
state and no Harness-specific behavior, so the same install serves Hermes,
Codex, Claude Code, a shell script, or anything else that can run a command.

The structured call surface is the Go CLI (`mc-agent ...`); the Python
`mc-bridge` commands below are the legacy equivalent and call the same daemon
methods with the same JSON shape.

```
    Go CLI / legacy CLI  <->  bridge daemon  <->  mod  <->  server
    (any Harness)              (no SDKs)        mc-agent-interface (Fabric)
```

## 1. Requirements

* Python 3.11 or newer on the machine that runs the game server (legacy path
  only; the Go binary has no Python requirement).
* The `mc-agent-interface` Fabric mod, version 0.6.x+ for the legacy loopback
  protocol, or 0.8.0+ for the authenticated same-port control transport used
  by the Go runtime.
* Nothing else. No model API key, no Harness SDK, no agent framework.

## 2. Install

```bash
# legacy Python daemon, CLI, local JSON-lines API and discovery
pip install -e /path/to/mc-agent-bridge
```

The Go runtime is a single binary; see [go/README.md](../go/README.md).

## 3. Start the game endpoint

Start the server with the mod installed. The server entrypoint writes the port
it actually bound to into

```
<gameDir>/mc-agent-server/port.txt
```

(default directory `mc-agent-server`, configurable with `-Dmcagent.serverDir`,
default port 25581 via `-Dmcagent.serverPort`; it takes the next free port if
that one is busy).

Check that the file exists before starting the daemon:

```bash
cat mc-agent-server/port.txt
# 25581
```

If the game runs elsewhere, point the daemon at the right directory or file;
see the next section.

## 4. Start the daemon

```bash
mc-bridge run
```

The daemon owns the one connection to the mod and re-serves it on loopback
(JSON-lines) for the CLI and agent loops. It reads the port file on every
(re)connect, so starting it before the game is fine: it prints a hint and keeps
watching until the file appears.

`mc-bridge run` defaults to the server vantage. It never guesses a port and
never silently falls back to the client vantage.

### Endpoint discovery

| Option / variable | Use |
| --- | --- |
| `--mod-port N` | explicit port; wins over every file |
| `--port-file PATH`, `MC_AGENT_PORT_FILE` | read the port from this exact file |
| `--server-dir DIR`, `MC_AGENT_SERVER_DIR` | game/server directory; reads `<DIR>/mc-agent-server/port.txt` (also `<DIR>/port.txt`) |
| *(default)* | `./mc-agent-server/port.txt` relative to the current directory |
| `--vantage client` | explicit legacy client-vantage setup: `./port.txt`, `./mc-agent/port.txt`, else 25580 |

Check discovery without starting the daemon; `discover` exits non-zero when it
cannot resolve:

```bash
mc-bridge discover --server-dir /srv/minecraft
# {"vantage": "server", "port": 25581, "source": "port-file:/srv/minecraft/mc-agent-server/port.txt", ...}

mc-bridge discover --server-dir /wrong/place
# {"vantage": "server", "port": null, "source": "unresolved",
#  "error": "cannot find the server-vantage port file: no readable .../mc-agent-server/port.txt
#            ... --server-dir ... --port-file ... --mod-port ... --vantage client ..."}
```

A missing file is not an error if the game is simply not up yet: the daemon
logs the same actionable message and retries. It does not connect to the client
port behind your back.

### Keep it local

The daemon binds its local API to `127.0.0.1` and the mod binds to loopback.
There is no public listener and nothing to expose to the Internet. Anyone who
can reach the local API can run commands as the server's command source, so
treat the machine as trusted.

## 5. Connect a Harness

MCP was removed in bridge #12. Point the Harness at the Go CLI (see
[go/README.md](../go/README.md)) - a Harness only needs to run commands and
read JSON - or, for the legacy daemon, at `mc-bridge call ...` / the local
JSON-lines API. Nothing needs a Harness SDK or an API key.

If the legacy daemon runs in another shell or as a service, point the caller at
its loopback API; if the Harness owns the daemon too, start `mc-bridge run` as
a separate long-lived process first.

The Go runtime filters the operation surface by the connected mod's advertised
capabilities; the legacy daemon does the same in `capabilities`. Client-vantage
operations (`chat`, `screen`, `connect`, `world`, `lan`, `record_*`) are never
advertised against a server vantage; use `--vantage client` only for an
explicit legacy client endpoint.

### Suggested first call

```text
mc-agent capabilities
# or, against the legacy daemon: mc-bridge call capabilities
```

returns the raw CAPS list from the mod plus the filtered
`surface.supported` / `surface.unsupported` view. Check it before assuming an
operation exists. A typical server-vantage flow:

```text
mc_status()                          # connected? which port/vantage?
mc_capabilities()                    # what can this instance do?
mc_state()                           # players, levels, world directory
mc_save()                            # world-save metadata (level name, worldDir)
mc_entities(radius=32, limit=30)     # summarised, nearest first
mc_command_output("data get entity <name> Motion")
mc_events(since=0, category="game")  # replay what happened
```

### Adaptive operations

Two operations are gated on the connected mod's capability (shipped in
mc-agent-interface-mod 0.6.0):

| Tool | Needs | Origin |
| --- | --- | --- |
| `mc_player(player)` | server-side player context: identity, dimension, position, rotation, eye, view direction and target | [mc-agent-interface-mod#1](https://github.com/guajun/mc-agent-interface-mod/issues/1) |
| `mc_context(context_id)` | chat-time player context bundle lookup | [mc-agent-interface-mod#2](https://github.com/guajun/mc-agent-interface-mod/issues/2) |

They are hidden until the connected mod advertises the capability. If a caller
tries one anyway (for example through a stale tool list), the daemon refuses it
*before* sending anything and says which capability is missing. The adapter
boundary - request line, reply normalization, aliases - lives in
`src/mc_agent_bridge/adapters.py`. The released mod reports the entity record
under `player` and the ray under a separate top-level `view`; the adapter merges
the ray into `player.view` so one accessor works for every reply shape. The
raw mod reply is authoritative; a `capabilities` call confirms what the
connection serves.

## 6. Legacy CLI

Every daemon method has a `mc-bridge` call that prints machine-readable JSON
(the Go CLI is the product path; the tables in `go/README.md` map each command):

```bash
mc-bridge call status
mc-bridge call capabilities
mc-bridge call state
mc-bridge call save
mc-bridge call entities '{"radius": 32}'
mc-bridge call command_output '{"command": "time set day"}'
mc-bridge call events '{"since": 0, "limit": 50, "category": "game"}'
mc-bridge call player '{"player": "<uuid-or-name>"}'   # adaptive
mc-bridge call context '{"id": "<context-id>"}'        # adaptive
```

Stream events as JSON lines, with an optional replay cursor:

```bash
mc-bridge watch --events chat,game,mark,error
mc-bridge watch --since 120
```

Target a daemon on a non-default loopback port by putting `--api-port` before
or after the sub-command:

```bash
mc-bridge --api-port 8766 call status
mc-bridge call --api-port 8766 status
```

`capabilities`, `status`, `player`, `context` and `save` are the discovery
calls. `mc-bridge call capabilities` is the single source of truth for what
the current connection supports.

## 7. Tool shapes at a glance

| Method | Returns |
| --- | --- |
| `status` | connection, vantage, resolved port/source, discovery error, API clients, buffer |
| `capabilities` | raw mod CAPS + `surface.supported` / `surface.unsupported` + per-operation reasons |
| `state` | tick, player list, levels, worldDir, server version |
| `player` | `{found, player: {uuid, name, dimension, position, rotation, view}}` (adaptive) |
| `entities` | tick-ordered entities; `radius` needs a player on the server vantage, 0 = all |
| `command` / `command_output` | command ack / `{command, output: [...], source: "ack"|"events"}` |
| `events` | `{events: [...], next, dropped}`; feed `next` back as `since` |
| `context` | `{found, context: {...}}` or `{found: false, status: "expired"}` (adaptive) |
| `save` | `{levelName, worldDir, worldDirExistsOnBridgeHost, players, levels, tick}` |
| `snapshot` / `snapshots` | entity set in tick order (`orderHash`, `dir`) / what is on disk |
| `fork` / `restore` | world copy + manifest / guarded dry-run-or-apply summon with full-state verification |
| `verify` / `order` | full-state comparison (UUID order, counts, pos, vel, NBT) / `orderHash` comparison |

## 8. Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `cannot find the server-vantage port file` | The game is not up yet, or the daemon runs in the wrong directory. Wait, or pass `--server-dir`, `--port-file` / `MC_AGENT_PORT_FILE`, or `--mod-port`. |
| `connected: false` in `status` | The daemon is up but the mod is unreachable. Check the `discovery` block in `status` and the daemon log; confirm the game has the mod and the port file. |
| `toolkit operation 'chat' is not available` | You are on the server vantage, which has no client chat/screen/connect/world/lan/record. Use `command_output` with `/say`, or run the legacy client endpoint with `--vantage client`. |
| `requires mc-agent-interface-mod#1/#2` | The mod build predates the per-player context / context-bundle capability. Use `mc_state` for a global player list, and `mc_events` for chat; the tool appears when the mod updates. |
| `no bridge daemon on 127.0.0.1:8765` | Start `mc-bridge run` first, or point the call at the right `--api-port`. |
| MCP tool list lacks a tool | This doc's MCP front-end no longer exists (bridge #12). Use the Go CLI's `mc-agent schema`/`capabilities` and read `surface.unsupported`; see the adaptive-operations note above. |
| `dropped: true` in an `events` reply | The ring buffer discarded events you never read. Poll more often or raise the daemon's `--buffer`. |
| Port moves after a game restart | Expected: the mod picks the next free port. The daemon re-reads `port.txt` on every reconnect; nothing to do. |

## 9. Tests

The suite runs against a fake mod - no game required:

```bash
PYTHONPATH=src python -m unittest discover -s tests -t .
```

Port-file selection, server capability filtering and the adaptive
context-bundle delegation each have dedicated tests (`test_discovery.py`,
`test_capabilities.py`, `test_adapters.py`).
