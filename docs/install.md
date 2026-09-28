# Installing, upgrading and removing mc-agent

The product is one Go binary with no Python, Go or MCP runtime requirement.
The game side only needs the `mc-agent-interface` Fabric mod on the server;
the binary runs in the harness execution environment.

Supported platforms (built, packaged and install-verified by this release
line): **Windows amd64**, **Linux glibc amd64**, **macOS arm64**.
Other OS/CPU pairs are refused instead of guessed. See
[release.md](release.md) for the exact matrix, compatibility and the
proxy/platform boundary.

## Install

=== "Linux amd64 / macOS arm64"

    ```bash
    curl -fsSL https://raw.githubusercontent.com/guajun/mc-agent-bridge/v0.5.0/install/install.sh \
        | sh -s -- --version 0.5.0 --skill-harness codex
    ```

=== "Windows amd64"

    ```powershell
    iwr -useb https://raw.githubusercontent.com/guajun/mc-agent-bridge/v0.5.0/install/install.ps1 -OutFile install.ps1
    ./install.ps1 -Version 0.5.0 -SkillHarness codex
    ```

Both installers download the versioned archive and `checksums.txt`, verify the
SHA-256 before writing anything, install the binary atomically (the previous
binary, if any, is kept as `mc-agent.previous`) and write a manifest at
`<install-dir>/mc-agent.installed`.

Default install directories:

| Platform | Default | State directory |
| --- | --- | --- |
| Linux | `~/.mc-agent/bin` | `$XDG_CONFIG_HOME/mc-agent` or `~/.config/mc-agent` |
| macOS | `~/.mc-agent/bin` | `~/Library/Application Support/mc-agent` |
| Windows | `%LOCALAPPDATA%\mc-agent\bin` | `%AppData%\mc-agent` |

Use `--install-dir DIR` (`-InstallDir`) to choose another directory, and
`--add-to-path` (`-AddToPath`) to append it to your profile/user PATH. The
installer prints the exact `version` and `doctor` commands to run next.

## Verify the install

```bash
mc-agent version     # mc-agent 0.5.0 (control protocol 1, mod >= 0.8.0)
mc-agent doctor      # version + state + configured targets
```

`doctor` turns fully green once a target is configured and reachable. A clean
machine with no game has one failing `targets` check, which is expected; the
`version` and `daemon` checks must pass. To exercise the CLI/daemon without a
game, start the in-process fake mod:

```bash
mc-agent daemon start --fake
mc-agent capabilities
mc-agent state
mc-agent daemon stop
```

## Install the matching Toolkit Skill

The Skill bundle is built from a reviewed, pinned commit of
[guajun/mc-agent](https://github.com/guajun/mc-agent) (recorded in
`skill-pin.json`), not from a moving branch.

```bash
# Explicit harness target
sh install.sh --version 0.5.0 --skill-harness codex
# or a custom directory; the skill lands in <DIR>/minecraft-toolkit/
sh install.sh --version 0.5.0 --skill-dir /path/to/skills
```

| `--skill-harness` | Target directory |
| --- | --- |
| `codex` | `$CODEX_HOME/skills` or `~/.codex/skills` |
| `claude-code` (`claude`) | `$CLAUDE_CONFIG_DIR/skills` or `~/.claude/skills` |
| `universal` | `$XDG_CONFIG_HOME/agents/skills` or `~/.config/agents/skills` |
| `hermes` | `$HERMES_HOME/skills` (the installer requires `HERMES_HOME`; no default directory is guessed) |

The installer refuses to replace an existing `minecraft-toolkit/` directory
by default. It fails with the path and asks for `--update-skill`
(`-UpdateSkill`), which keeps a timestamped backup before replacing it.
`--no-skill` skips the Skill entirely.

Harnesses with native Skill support can instead use their own flow against the
meta repository, for example `gh skill install guajun/mc-agent
minecraft-toolkit --agent codex --scope user`; the installer path above is the
one pinned to the release.

## Upgrade

Run the installer again with the new version. Nothing else changes:

```bash
curl -fsSL https://raw.githubusercontent.com/guajun/mc-agent-bridge/v0.5.1/install/install.sh \
    | sh -s -- --version 0.5.1
```

The new archive is checksum-verified, the current binary is copied to
`mc-agent.previous`, and the manifest records the new version and hash. If
verification or download fails, the installed binary is left unchanged.

## Uninstall

```bash
sh install.sh --uninstall                      # removes the binary and manifest
sh install.sh --uninstall --remove-skill       # also removes unchanged skill copies
sh install.sh --uninstall --purge-state        # also removes the state directory
```

On Windows use `./install.ps1 -Uninstall` with `-RemoveSkill` / `-PurgeState`.
Uninstall refuses to delete a binary that does not match the recorded manifest
unless `--force` is given, and keeps a skill directory that has local
modifications instead of deleting user content.

## Offline / staged installs

For air-gapped machines or CI staging, point the installer at a local directory
containing the release assets:

```bash
sh install.sh --version 0.5.0 --from-dir ./dist --install-dir ./bin
sh install.sh --version 0.5.0 --archive ./mc-agent-0.5.0-linux-amd64.tar.gz \
    --sha256 <hex>
```

`--from-dir` still verifies `checksums.txt`; `--archive` without `--sha256`
warns and is intended only for a file you built yourself.

## Where the binary runs

The binary belongs in the environment that executes harness commands: a local
shell, a remote host, WSL, or a container. Installing it on a player's
computer does not make a remote harness use it. WSL is a Linux environment and
uses the Linux archive; the CLI reaches a server on the Windows LAN through the
normal address, and the state directory lives inside the WSL home unless
`MC_AGENT_HOME` says otherwise.

The daemon is the same binary and can run either next to the harness or next to
the server. A server-side daemon is optional, not a requirement; multiple
daemons connect independently to the mod. A server that only has the mod works
with a client-side daemon, and the reverse works too.
