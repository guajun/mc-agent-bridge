# Go CLI and daemon

One binary provides a short-lived CLI and a long-lived daemon. The CLI is the
harness-facing tool entry point; the daemon owns every connection to the mod
and never runs a model or manages harness sessions. There is no MCP server and
no Python runtime requirement.

Release installs (Windows amd64, Linux glibc amd64, macOS arm64), upgrades,
uninstall and the pinned Toolkit Skill are in
[../docs/install.md](../docs/install.md); the supported matrix, release assets
and boundaries are in [../docs/release.md](../docs/release.md).

```
agent runtime  <->  mc-agent CLI  --(token-protected loopback IPC)-->
                mc-agent daemon  --(TLS control protocol | legacy loopback)-->
                mc-agent-interface mod  <->  Minecraft
```

Build (Go 1.26+, no modules are downloaded):

```bash
cd go
go build -o mc-agent ./cmd/mc-agent
GOOS=linux   GOARCH=amd64 go build -o mc-agent-linux   ./cmd/mc-agent
GOOS=darwin  GOARCH=arm64 go build -o mc-agent-darwin  ./cmd/mc-agent
GOOS=windows GOARCH=amd64 go build -o mc-agent.exe     ./cmd/mc-agent
```

## Quick start

```bash
# 1. Register the server. Remote targets must pin the server identity:
#    the mod writes the fingerprint to <gameDir>/mc-agent-server/control/fingerprint.txt.
mc-agent target add dedicated --transport remote --address 127.0.0.1:25565 \
    --pin sha256:<fingerprint> --token-stdin --default
# (the credential from `/mcagent control token add ...` on the server console
#  is read from stdin; it is stored 0600 in the private state directory)

# 2. Start the daemon once; it reconnects on its own.
mc-agent daemon start
mc-agent daemon status

# 3. Use it.
mc-agent state
mc-agent command "say hello"
mc-agent player Alice
mc-agent events --follow
mc-agent daemon stop
```

`MC_AGENT_HOME` selects the state directory (defaults to the platform user
config directory + `/mc-agent`). `--home` does the same per command.
`MC_AGENT_TARGET` selects the default target; `--target` overrides it.

## Operations

| Command | Needs | Notes |
| --- | --- | --- |
| `status` | daemon | daemon + per-target connection state |
| `capabilities` | mod `state` etc. | capability tokens and the filtered surface |
| `schema [op]` | daemon | operation contract and parameter schema |
| `state` | `state` | server state and player list |
| `player <name\|uuid>` | `player` | server-side context and view target |
| `entities [--radius N]` | `entities` | tick-ordered entities |
| `context <id>` | `context` | chat-time player context bundle |
| `command <line>` | `command` | write; returns `writeSeq` |
| `command-output <line>` | `command` | command plus collected output |
| `mark <text>` | `mark` | write; annotates the event stream |
| `wait <ticks>` | `wait` | blocks until the game advanced |
| `save`, `snapshots` | `state`, `snapshot` | world/snapshot metadata |
| `snapshot [...]` | `snapshot` | write; the game host writes the files |
| `events [--since N] [--follow]` | daemon | replay buffer and live stream |
| `requests`, `request-status <id>` | daemon | unknown-write ledger and lookup |
| `exclusive-*` | daemon | leases from mc-agent#6 |
| `chat`, `screen`, `connect`, `world`, `lan`, `record-*` | legacy client vantage | client GUI operations |

Every command prints one JSON value on stdout; errors go to stderr as
`{"ok":false,"error":{...}}` with a stable `code` and process exit code
(2 usage, 3 target, 4 connection, 5 auth, 6 capability, 7 timeout/unknown).

## Transports

- **remote** (default): the authenticated TLS control protocol from
  `mc-agent-interface-mod` 0.8.0+. Verification is a SHA-256 certificate pin
  (`--pin`) or a CA/PEM file (`--ca [--server-name]`); certificate
  verification cannot be disabled.
- **legacy**: the explicit pre-#8 loopback JSON-lines adapter. Use it for a
  client vantage or a released mod; discovery reads
  `<gameDir>/mc-agent-server/port.txt` (or `--port-file`/`--server-dir`).

`--transport fake` starts an in-process fake mod and is only for tests and
smoke runs (`mc-agent daemon start --fake`).

## Credentials and the local IPC boundary

The daemon binds a random loopback port and writes `daemon.json` (0600) with
the port and a random IPC token. Every local request must carry the token;
a wrong token closes the connection. The CLI reads the file automatically, and
`MC_AGENT_DAEMON_ADDR`/`MC_AGENT_IPC_TOKEN` cover containers and remote shells.
Tokens are stored in `secrets.json` (0600) and are never printed by
`target list`/`show`/`doctor` or written to logs; the webhook URL is redacted
the same way.

## Multi-instance and reconnection

Two daemons can connect to the same server with independent credentials and
independent event streams; requests and replies never cross connections. If
more than one target is configured, every call needs `--target` unless a
default is set - the daemon never guesses a world. On reconnect the daemon
tracks the server's `runId` and event sequence and reports:

- `bridge_connected` / `bridge_disconnected` per target,
- `event_gap` when the server's replay buffer can no longer reach the last
  seen sequence,
- `game_restarted` when the run id changes (no replay crosses a restart),
- `request_resolved` for a write whose outcome became known after a reconnect.

Non-idempotent writes (`command`, `mark`, `snapshot`, client `chat`/`world`)
are never resent automatically. The daemon allocates a request id that is
unique across processes, clients and reconnects (`<random nonce>-<counter>`),
persists the request in `unknown_writes.json` **before it can leave the
process**, and resolves it with the server's `request_status` when the link
returns; if the server has no record (for example a game restart) or the
credential/instance/run scope does not match, the entry stays visible as
`unresolved` and the CLI reports the request id instead of pretending it
succeeded. If the ledger cannot be persisted, the daemon refuses to send the
write rather than losing recovery data. A request that timed out before the
server claimed it is cancelled and safe to retry; one that was already running
keeps its ledger entry until the server reports the final state.

Event cursors are stored per target together with the `(instanceId, runId)`
they belong to and are only offered back to the server with that run id, so a
reconnected daemon can never present a previous run's high-water mark to a
restarted game. `mc-agent events` returns the oldest events after `since`, so
`next` (the highest delivered sequence) is a usable continuation, and reports
`truncated: true` when the caller must ask again; `dropped` is reserved for
buffer eviction. A target whose reader cannot drain its socket is closed so the
next reconnect produces a machine-readable replay/gap report.

## Webhook forwarding

Optional and off by default:

```bash
# webhook flags are accepted by `daemon run` (foreground/supervised);
# `daemon start` has no webhook flags.
mc-agent daemon run --webhook-url https://receiver.example/hook \
    --webhook-secret "$SECRET" --webhook-events chat,game,mark,error
```

`MC_AGENT_WEBHOOK_*` variables and a JSON config file (`--webhook-config`) are
also accepted. Delivery is best effort, in memory, and bounded (queue 256,
5 attempts, 1s..30s backoff, 10s timeout). Every POST carries
`X-MC-Agent-Signature` (`sha256=hex(HMAC-SHA256(secret, "<timestamp>.<body>"))`)
plus timestamp, stable event id, event type and attempt headers; the stable id
makes receiver-side de-duplication possible across retries.

## Migration matrix

Python bridge 0.4.1 -> Go runtime:

| Python bridge method / MCP tool | Go CLI | Status |
| --- | --- | --- |
| `ping`, `status` | `mc-agent status`, `daemon status` | migrated |
| `capabilities`, `mc_capabilities` | `mc-agent capabilities`, `schema` | migrated |
| `state`, `mc_state` | `mc-agent state` | migrated |
| `player`, `mc_player` | `mc-agent player` | migrated |
| `entities`, `mc_entities` | `mc-agent entities` | migrated |
| `command`, `mc_command` | `mc-agent command` | migrated |
| `command_output`, `mc_command_output` | `mc-agent command-output` | migrated |
| `mark`, `mc_mark` | `mc-agent mark` | migrated |
| `wait`, `mc_wait` | `mc-agent wait` | migrated |
| `events`, `mc_events` | `mc-agent events` | migrated |
| `context`, `mc_context` | `mc-agent context` | migrated |
| `save`, `mc_save` | `mc-agent save` | migrated |
| `snapshot`, `mc_snapshot` | `mc-agent snapshot` | migrated (remote writes happen on the game host) |
| `snapshots`, `mc_snapshots` | `mc-agent snapshots` | migrated |
| `chat`, `mc_chat` | `mc-agent chat` | migrated (legacy client vantage) |
| `screen`, `mc_screen` | `mc-agent screen` | migrated (legacy) |
| `connect`, `mc_connect` | `mc-agent connect` | migrated (legacy) |
| `world`, `mc_world` | `mc-agent world` | migrated (legacy) |
| `lan`, `mc_lan` | `mc-agent lan` | migrated (legacy) |
| `record_start`/`record_stop` | `mc-agent record-start`/`record-stop` | migrated (legacy) |
| `fork`, `restore`, `verify`, `order`, `mc_fork`, `mc_restore` | - | **not migrated**: refused with `capability_not_supported`; local file snapshots need the daemon host to read the game's world directory. The Python bridge remains the explicit legacy path. |
| `stop` | `mc-agent daemon stop` | migrated |
| webhook forwarder | `daemon run --webhook-*` | migrated (same signatures and retry semantics) |
| MCP server | - | removed: config must use the CLI |

Remote operations never interpret the mod's `worldDir` (or any server path) as
a path on the daemon host. `save` reports that the directory is remote; fork
and restore are refused accordingly.

## Tests

```bash
cd go && go test ./...
```

The suite covers the protocol surface, the TLS client (pin/CA/auth/replay/
connection loss), the legacy reply-routing rules, a full daemon with a fake
mod (calls, events, target selection, reconnect + unknown-write
reconciliation, IPC authentication, webhook signing and retry), and the
fakemod itself. `e2e/control_e2e.py` in the mod repository runs the same
daemon against a real Minecraft server.
