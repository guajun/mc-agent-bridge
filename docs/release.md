# Release matrix, versioning and boundaries

This page is for operators deciding whether a release fits their setup and for
maintainers preparing a release. The user instructions are in
[install.md](install.md).

## Version model

Four version numbers are independent. `mc-agent version` and the published
`version.json` report the first three:

| Item | Current | Meaning |
| --- | --- | --- |
| Product version | `0.5.0` | The Go CLI/daemon release tag (`v0.5.0`). |
| Control protocol | `1` | The TLS protocol between daemon and mod. |
| Mod minimum | `0.8.0` | Oldest `mc-agent-interface` release with the required capability set. |
| Mod / Minecraft stack | mod `0.8.0`, Minecraft `26.2`, Fabric loader `0.19.5`, Fabric API `0.161.0`, Java 25 | The tested game-side combination. |

A mismatched protocol is reported by `doctor` and by connect errors instead of
being silently downgraded. `version.json` in every release lists the exact
compatibility block and the pinned Skill commit.

## Supported matrix

Only the platforms below are built, packaged and install-verified by the
release workflows. Each advertised runner executes install, `version`,
`doctor`, upgrade, uninstall and Skill install/uninstall against the staged
release assets.

| OS | CPU | Runner | Archive |
| --- | --- | --- | --- |
| Windows 10/11 | amd64 | `windows-latest` | `mc-agent-<version>-windows-amd64.zip` |
| Linux glibc | amd64 | `ubuntu-latest` | `mc-agent-<version>-linux-amd64.tar.gz` |
| macOS 14+ | arm64 (Apple silicon) | `macos-14` | `mc-agent-<version>-darwin-arm64.tar.gz` |

Everything else - Windows arm64, Linux arm64, Linux musl/Alpine, macOS amd64,
other OS/CPU pairs - is **not tested and not claimed**. The installers refuse
those platforms explicitly rather than installing an incompatible binary.
Building the Go binary for another platform is possible from source, but that
is a source build, not a supported release.

The mod is plain Java and runs wherever Minecraft 26.2 with the listed Fabric
components runs; the binary matrix above is only about the daemon host.

## Release assets

Every `v<productVersion>` release contains:

- one archive per supported platform, containing the Go binary only;
- `checksums.txt` - SHA-256 of every published asset;
- `version.json` - product version, control protocol, mod minimum,
  compatibility block, per-platform hashes, build metadata, pinned Skill
  record and the unsupported-boundary text;
- `mc-agent-skill-<skillVersion>.tar.gz` - the Toolkit Skill bundle;
- `skill-pin.json` - repository, path, commit and hash of the Skill source.

There is no Python wheel, Go module tarball, MCP server or client mod in a
release.

## Skill source pinning

`release/skill-pin.json` in this repository names the reviewed
`guajun/mc-agent` commit and the expected `SKILL.md` version. The release
workflow downloads that exact commit archive, validates the version, packages
`skills/minecraft-toolkit/` and records the resulting hash. A release is never
built from the meta repository's current `main` or from a working tree copy.

To move the pin (after the meta change is merged and reviewed):

1. Set `commit` to the merged commit and `skillVersion` to the version in
   `skills/minecraft-toolkit/SKILL.md`.
2. Run the release check workflow and confirm the packaged bundle hash.
3. Tag the product release; `finalize_assets.py` fails if the pin version and
   the file disagree.

## Proxy and platform boundary

The control transport is a TLS connection accepted on the same port as the
Minecraft listener. A **byte-transparent TCP relay** does not need to
understand Minecraft or the control protocol: it forwards the byte stream and
the client still verifies the server certificate pin/CA.

Not tested and not claimed:

- Minecraft-aware proxies (BungeeCord/Velocity-style handoff, protocol
  rewriting, player-info forwarding) and TLS-terminating or inspecting
  proxies. Terminating the TLS session or rewriting the stream breaks the
  certificate pin and connection classification.
- Network address translation beyond ordinary TCP reachability, and proxies
  that only forward specific Minecraft packets.
- Filesystem access across hosts. A remote server's world directory is never a
  local path; `snapshot` writes on the game host, and `fork`/`restore` remain
  refused for remote targets.

Minecraft servers behind a plain TCP proxy are expected to work when the
daemon's address reaches the proxy and the proxy forwards to the game port,
but the combined setup has not been executed in CI, so treat it as untested
until you run the acceptance below yourself.

## Release process

The workflows are:

- `.github/workflows/release-check.yml` - pull requests and branch pushes:
  builds the three archives, packages the pinned Skill, verifies checksums and
  runs the install acceptance on all three advertised runners.
- `.github/workflows/release-publish.yml` - tag `vX.Y.Z` (or manual dispatch
  with an existing tag): the same build/acceptance, then creates the GitHub
  release with the verified assets. The tag must match
  `release/compatibility.json`.

Maintainer sequence:

1. Update `go/internal/version/version.go` and
   `release/compatibility.json` together; update `release/skill-pin.json` if
   the Skill changed.
2. Open the PR and let `release-check` prove the staged assets on all three
   runners.
3. Merge, tag `vX.Y.Z` from the merged commit, and watch `release-publish`.
4. Confirm the published `checksums.txt` and `version.json` match the workflow
   artifact, then run the real-game acceptance below with the published
   binary.

## Real-game acceptance with a released binary

The install acceptance in CI uses the in-process fake mod. The real-game
acceptance uses the same released binary and a throwaway lab. Development
tooling (`python`, `lab_server.py`, Java 25, a Minecraft 26.2 installation)
provisions the environment; the product itself does not require them.

```bash
# 1. Install the released binary as a user would (staged or published assets).
sh install.sh --version 0.5.0 --from-dir ./dist --install-dir "$PWD/bin" --no-skill
BIN="$PWD/bin/mc-agent"; "$BIN" version

# 2. Dedicated server on the real game port with this exact binary.
python /path/to/mc-agent-interface-mod/e2e/control_e2e.py dedicated \
    --lab-server /path/to/mc-agent/tools/lab_server.py \
    --meta-root /path/to/mc-agent \
    --mod-jar /path/to/mc-agent-interface-0.8.0.jar \
    --go-binary "$BIN" \
    --jdk25 /path/to/jdk-25 \
    --minecraft-dir /path/to/minecraft \
    --work-dir /tmp/mc-agent-dedicated --evidence-dir /tmp/mc-agent-dedicated/evidence

# 3. LAN host: the mod's actual published port, no fixed 25565.
python /path/to/mc-agent-interface-mod/e2e/control_e2e.py lan \
    --lab-server /path/to/mc-agent/tools/lab_server.py \
    --meta-root /path/to/mc-agent \
    --mod-jar /path/to/mc-agent-interface-0.8.0.jar \
    --go-binary "$BIN" \
    --jdk25 /path/to/jdk-25 \
    --minecraft-dir /path/to/minecraft \
    --work-dir /tmp/mc-agent-lan --evidence-dir /tmp/mc-agent-lan/evidence

# 4. Linux/WSL daemon against a remote Windows/LAN server (optional).
python /path/to/mc-agent-interface-mod/e2e/control_e2e.py wsl \
    --wsl-linux-binary "$BIN" --host-ip <server-ip> \
    --remote-port <port> --remote-pin sha256:<hex> --remote-token <token> \
    --lab-server /path/to/mc-agent/tools/lab_server.py --meta-root /path/to/mc-agent \
    --mod-jar /path/to/mc-agent-interface-0.8.0.jar \
    --jdk25 /path/to/jdk-25 --minecraft-dir /path/to/minecraft \
    --work-dir /tmp/mc-agent-wsl --evidence-dir /tmp/mc-agent-wsl/evidence
```

The dedicated run must show two independent daemon processes, normal player
join/leave, writes with the unknown-write ledger, reconnect and game restart;
the LAN run must show the host-only-mod path on the actual published port.
Evidence for the current pre-release heads is recorded under
`.worktrees/artifacts/issue-39/runtime/` in the mc-agent workspace; the release
acceptance must be repeated with the published binary before calling a release
verified.

## WSL, containers and remote hosts

The binary runs anywhere the harness executes commands. In WSL and containers
use the Linux archive; keep loopback addresses and state paths local to that
environment. `MC_AGENT_HOME` and `MC_AGENT_DAEMON_ADDR`/`MC_AGENT_IPC_TOKEN`
let a CLI in one process namespace reach a daemon in another. No SSH, extra
public port, proxy or client-mod relay is required.
