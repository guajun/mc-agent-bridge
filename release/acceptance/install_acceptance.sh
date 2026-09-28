#!/bin/sh
# Release-installed acceptance for one supported platform (Linux amd64,
# macOS arm64). Runs the real install script against a staged release
# directory, then exercises version/doctor/upgrade/uninstall/skill safety.
#
#   sh release/acceptance/install_acceptance.sh \
#       --assets dist --version 0.5.0 --work /tmp/mc-agent-acceptance
set -eu

ASSETS=""
VERSION=""
WORK=""
INSTALLER=""
while [ $# -gt 0 ]; do
    case "$1" in
        --assets) ASSETS="$2"; shift 2 ;;
        --version) VERSION="$2"; shift 2 ;;
        --work) WORK="$2"; shift 2 ;;
        --installer) INSTALLER="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done
[ -n "$ASSETS" ] || { echo "--assets is required" >&2; exit 2; }
[ -n "$VERSION" ] || { echo "--version is required" >&2; exit 2; }
[ -n "$WORK" ] || { echo "--work is required" >&2; exit 2; }
[ -n "$INSTALLER" ] || INSTALLER="install/install.sh"
ASSETS="$(cd "$ASSETS" && pwd)"

rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/home" "$WORK/skills" "$WORK/staging-next"
BIN_DIR="$WORK/bin"
BINARY="$BIN_DIR/mc-agent"
MANIFEST="$BIN_DIR/mc-agent.installed"
export MC_AGENT_HOME="$WORK/home"

failures=0
check() {
    if [ "$2" = "0" ]; then
        echo "PASS  $1"
    else
        echo "FAIL  $1" >&2
        failures=$((failures + 1))
    fi
}
hash_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
VERSION_OUTPUT="$("$BINARY" version)"
printf '%s' "$VERSION_OUTPUT" | grep -q "mc-agent $VERSION" && check "installed binary reports version $VERSION" 0 || check "installed binary reports version $VERSION ($VERSION_OUTPUT)" 1

# Doctor against the in-process fake mod. A clean machine has no configured
# target, so the overall exit code is 1; the version and daemon checks must be ok.
MC_AGENT_HOME="$WORK/home" "$BINARY" daemon start --fake >/dev/null
MC_AGENT_HOME="$WORK/home" "$BINARY" capabilities >"$WORK/capabilities.json"
grep -q '"state"' "$WORK/capabilities.json" && check "capabilities lists state through the daemon" 0 || check "capabilities lists state through the daemon" 1
MC_AGENT_HOME="$WORK/home" "$BINARY" state >"$WORK/state.json"
grep -q '"stub":true' "$WORK/state.json" && check "state answers through the fake mod" 0 || check "state answers through the fake mod" 1
MC_AGENT_HOME="$WORK/home" "$BINARY" doctor >"$WORK/doctor.json" || true
grep -q '"check":"version","detail":"mc-agent '"$VERSION" "$WORK/doctor.json" && check "doctor reports the installed version" 0 || check "doctor reports the installed version" 1
grep -q '"check":"daemon","detail":"running"' "$WORK/doctor.json" && check "doctor sees the running daemon" 0 || check "doctor sees the running daemon" 1
MC_AGENT_HOME="$WORK/home" "$BINARY" daemon stop >/dev/null

# Skill install, overwrite refusal and explicit update.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" >/dev/null
[ -f "$WORK/skills/minecraft-toolkit/SKILL.md" ] && check "skill installed to the explicit directory" 0 || check "skill installed to the explicit directory" 1
printf '\nuser edit marker\n' >>"$WORK/skills/minecraft-toolkit/SKILL.md"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" >/dev/null 2>&1; then
    check "existing skill is not overwritten without --update-skill" 1
else
    check "existing skill is not overwritten without --update-skill" 0
fi
grep -q "user edit marker" "$WORK/skills/minecraft-toolkit/SKILL.md" && check "user edit survived the refused install" 0 || check "user edit survived the refused install" 1
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" --update-skill >/dev/null
grep -q "user edit marker" "$WORK/skills/minecraft-toolkit/SKILL.md" && check "--update-skill replaced the edited copy" 1 || check "--update-skill replaced the edited copy" 0
ls "$WORK/skills"/minecraft-toolkit.backup-* >/dev/null 2>&1 && check "--update-skill kept a backup" 0 || check "--update-skill kept a backup" 1

# Upgrade from a mechanically staged next version.
NEXT_VERSION="$VERSION-upgradetest"
PLATFORM="$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
SOURCE_ARCHIVE="$(ls "$ASSETS"/mc-agent-"$VERSION"-"$PLATFORM".tar.gz)"
cp "$SOURCE_ARCHIVE" "$WORK/staging-next/mc-agent-$NEXT_VERSION-$PLATFORM.tar.gz"
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$WORK/staging-next" && sha256sum "mc-agent-$NEXT_VERSION-$PLATFORM.tar.gz" >checksums.txt)
else
    (cd "$WORK/staging-next" && shasum -a 256 "mc-agent-$NEXT_VERSION-$PLATFORM.tar.gz" >checksums.txt)
fi
sh "$INSTALLER" --version "$NEXT_VERSION" --from-dir "$WORK/staging-next" --install-dir "$BIN_DIR" --no-skill >/dev/null
grep -q "^version=$NEXT_VERSION$" "$MANIFEST" && check "upgrade updated the install manifest" 0 || check "upgrade updated the install manifest" 1
[ -f "$BIN_DIR/mc-agent.previous" ] && check "upgrade kept the previous binary" 0 || check "upgrade kept the previous binary" 1
"$BINARY" version | grep -q "mc-agent $VERSION" && check "upgraded binary still runs" 0 || check "upgraded binary still runs" 1

# A corrupted archive must fail verification and leave the install untouched.
BEFORE_HASH="$(hash_file "$BINARY")"
cp "$SOURCE_ARCHIVE" "$WORK/staging-next/mc-agent-$NEXT_VERSION-$PLATFORM.tar.gz"
printf 'corruption' >>"$WORK/staging-next/mc-agent-$NEXT_VERSION-$PLATFORM.tar.gz"
if sh "$INSTALLER" --version "$NEXT_VERSION" --from-dir "$WORK/staging-next" --install-dir "$BIN_DIR" --no-skill >/dev/null 2>&1; then
    check "corrupted archive is refused" 1
else
    check "corrupted archive is refused" 0
fi
AFTER_HASH="$(hash_file "$BINARY")"
[ "$BEFORE_HASH" = "$AFTER_HASH" ] && check "refused upgrade left the binary unchanged" 0 || check "refused upgrade left the binary unchanged" 1

# Uninstall preserves state and skills unless asked explicitly.
touch "$WORK/home/sentinel"
sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" >/dev/null
[ ! -f "$BINARY" ] && check "uninstall removed the binary" 0 || check "uninstall removed the binary" 1
[ -f "$WORK/home/sentinel" ] && check "uninstall preserved the state directory" 0 || check "uninstall preserved the state directory" 1
[ -d "$WORK/skills/minecraft-toolkit" ] && check "uninstall preserved the skill unless asked" 0 || check "uninstall preserved the skill unless asked" 1
# Reinstall, then purge explicitly.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" --update-skill >/dev/null
sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --remove-skill --purge-state >/dev/null
[ ! -f "$BINARY" ] && check "second uninstall removed the binary" 0 || check "second uninstall removed the binary" 1
[ ! -d "$WORK/skills/minecraft-toolkit" ] && check "--remove-skill removed the unchanged skill" 0 || check "--remove-skill removed the unchanged skill" 1
[ ! -d "$WORK/home" ] && check "--purge-state removed the state directory" 0 || check "--purge-state removed the state directory" 1

if [ "$failures" -ne 0 ]; then
    echo "$failures check(s) failed" >&2
    exit 1
fi
echo "all release-install acceptance checks passed"
