#!/bin/sh
# Release-installed acceptance for one supported platform (Linux amd64,
# macOS arm64). Runs the real install script against staged release assets and
# exercises install/version/doctor/upgrade/uninstall/skill behavior, including
# negative transaction, ownership and filesystem-safety cases.
#
#   sh release/acceptance/install_acceptance.sh \
#       --assets dist --previous-assets dist-previous \
#       --version 0.5.0 --work /tmp/mc-agent-acceptance
set -eu

ASSETS=""
PREVIOUS_ASSETS=""
VERSION=""
WORK=""
INSTALLER=""
while [ $# -gt 0 ]; do
    case "$1" in
        --assets) ASSETS="$2"; shift 2 ;;
        --previous-assets) PREVIOUS_ASSETS="$2"; shift 2 ;;
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
INSTALLER="$(cd "$(dirname "$INSTALLER")" && pwd)/$(basename "$INSTALLER")"
ASSETS="$(cd "$ASSETS" && pwd)"
if [ -n "$PREVIOUS_ASSETS" ]; then
    PREVIOUS_ASSETS="$(cd "$PREVIOUS_ASSETS" && pwd)"
fi
PREVIOUS_VERSION="0.4.9-ci"
PLAT_SUFFIX="$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"

# The work directory is deleted recursively; only accept an empty directory or
# one carrying our own marker.
if [ -e "$WORK" ]; then
    if [ ! -d "$WORK" ] || { [ -n "$(ls -A "$WORK" 2>/dev/null)" ] && [ ! -f "$WORK/.mc-agent-acceptance" ]; }; then
        echo "error: --work $WORK is not empty and has no .mc-agent-acceptance marker" >&2
        exit 2
    fi
fi
rm -rf "$WORK"
mkdir -p "$WORK/bin" "$WORK/home" "$WORK/skills"
: >"$WORK/.mc-agent-acceptance"
BIN_DIR="$WORK/bin"
BINARY="$BIN_DIR/mc-agent"
MANIFEST="$BIN_DIR/mc-agent.installed"
export MC_AGENT_HOME="$WORK/home"

# The previous-version fixtures are stage-only; give them the checksums.txt the
# installer expects without mutating the input directory.
if [ -n "$PREVIOUS_ASSETS" ]; then
    mkdir -p "$WORK/previous-assets"
    for f in "$PREVIOUS_ASSETS"/*; do
        [ -f "$f" ] || continue
        cp "$f" "$WORK/previous-assets/"
    done
    (
        cd "$WORK/previous-assets"
        for f in *.tar.gz *.zip; do
            [ -f "$f" ] || continue
            if command -v sha256sum >/dev/null 2>&1; then
                sha256sum "$f"
            else
                shasum -a 256 "$f"
            fi
        done
    ) >"$WORK/previous-assets/checksums.txt"
    PREVIOUS_ASSETS="$WORK/previous-assets"
fi

failures=0
check() {
    if [ "$2" = "0" ]; then
        echo "PASS  $1"
    else
        echo "FAIL  $1" >&2
        failures=$((failures + 1))
    fi
}
skip() {
    echo "SKIP  $1"
}
hash_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}
wait_binary_idle() {
    # daemon start spawns a child that may outlive daemon stop by a moment;
    # Linux refuses to rewrite a binary that is still being executed.
    _tries=0
    while [ "$_tries" -lt 10 ]; do
        command -v pgrep >/dev/null 2>&1 || break
        pgrep -f "$BINARY" >/dev/null 2>&1 || break
        sleep 1
        _tries=$((_tries + 1))
    done
    return 0
}

# ---------------------------------------------------------- baseline install

if [ -n "$PREVIOUS_ASSETS" ]; then
    sh "$INSTALLER" --version "$PREVIOUS_VERSION" --from-dir "$PREVIOUS_ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
    BASELINE_VERSION="$PREVIOUS_VERSION"
else
    sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
    BASELINE_VERSION="$VERSION"
fi
"$BINARY" version | grep -q "mc-agent $BASELINE_VERSION" && check "installed binary reports version $BASELINE_VERSION" 0 || check "installed binary reports version $BASELINE_VERSION" 1

# Doctor against the in-process fake mod. A clean machine has no configured
# target, so the overall exit code is 1; the version and daemon checks must be ok.
"$BINARY" daemon start --fake >/dev/null
"$BINARY" capabilities >"$WORK/capabilities.json"
grep -q '"state"' "$WORK/capabilities.json" && check "capabilities lists state through the daemon" 0 || check "capabilities lists state through the daemon" 1
"$BINARY" state >"$WORK/state.json"
grep -q '"stub":true' "$WORK/state.json" && check "state answers through the fake mod" 0 || check "state answers through the fake mod" 1
"$BINARY" doctor >"$WORK/doctor.json" || true
grep -q '"check":"version","detail":"mc-agent '"$BASELINE_VERSION" "$WORK/doctor.json" && check "doctor reports the installed version" 0 || check "doctor reports the installed version" 1
grep -q '"check":"daemon","detail":"running"' "$WORK/doctor.json" && check "doctor sees the running daemon" 0 || check "doctor sees the running daemon" 1
"$BINARY" daemon stop >/dev/null
wait_binary_idle

# ------------------------------------------------------------------- upgrade

if [ -n "$PREVIOUS_ASSETS" ]; then
    sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
    grep -q "^version=$VERSION$" "$MANIFEST" && check "upgrade updated the install manifest" 0 || check "upgrade updated the install manifest" 1
    "$BINARY" version | grep -q "mc-agent $VERSION" && check "upgraded binary reports the new version" 0 || check "upgraded binary reports the new version" 1
    [ -f "$BIN_DIR/mc-agent.previous" ] && check "upgrade kept the previous binary" 0 || check "upgrade kept the previous binary" 1

    # An archive whose binary does not match the requested version must be
    # refused before the installation changes.
    mkdir -p "$WORK/staging-mismatch"
    cp "$PREVIOUS_ASSETS/mc-agent-$PREVIOUS_VERSION-$PLAT_SUFFIX.tar.gz" \
        "$WORK/staging-mismatch/mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz"
    (cd "$WORK/staging-mismatch" && (command -v sha256sum >/dev/null 2>&1 && sha256sum "mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz" || shasum -a 256 "mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz") >checksums.txt)
    HASH_BEFORE="$(hash_file "$BINARY")"
    if sh "$INSTALLER" --version "$VERSION" --from-dir "$WORK/staging-mismatch" --install-dir "$BIN_DIR" --no-skill >/dev/null 2>&1; then
        check "mismatched binary version is refused" 1
    else
        check "mismatched binary version is refused" 0
    fi
    HASH_AFTER="$(hash_file "$BINARY")"
    [ "$HASH_BEFORE" = "$HASH_AFTER" ] && check "refused mismatch left the binary unchanged" 0 || check "refused mismatch left the binary unchanged" 1
else
    skip "version mismatch refusal needs --previous-assets"
fi

# A modified managed binary must be refused without --force, then replaced
# with --force.
printf 'modified' >>"$BINARY"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null 2>&1; then
    check "modified managed binary is refused" 1
else
    check "modified managed binary is refused" 0
fi
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill --force >/dev/null
"$BINARY" version | grep -q "mc-agent $VERSION" && check "--force replaced the modified binary" 0 || check "--force replaced the modified binary" 1

# ---------------------------------------------------------------- skill safety

sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" >/dev/null
[ -f "$WORK/skills/minecraft-toolkit/SKILL.md" ] && check "skill installed to the explicit directory" 0 || check "skill installed to the explicit directory" 1
printf '\nuser edit marker\n' >>"$WORK/skills/minecraft-toolkit/SKILL.md"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" >/dev/null 2>&1; then
    check "existing skill is not overwritten without --update-skill" 1
else
    check "existing skill is not overwritten without --update-skill" 0
fi
grep -q "user edit marker" "$WORK/skills/minecraft-toolkit/SKILL.md" && check "user edit survived the refused install" 0 || check "user edit survived the refused install" 1

# A rejected skill conflict must not touch the binary: ask for the previous
# version while the edited skill conflicts with the install.
if [ -n "$PREVIOUS_ASSETS" ]; then
    HASH_BEFORE="$(hash_file "$BINARY")"
    if sh "$INSTALLER" --version "$PREVIOUS_VERSION" --from-dir "$PREVIOUS_ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" >/dev/null 2>&1; then
        check "skill conflict aborts the upgrade before the binary changes" 1
    else
        check "skill conflict aborts the upgrade before the binary changes" 0
    fi
    HASH_AFTER="$(hash_file "$BINARY")"
    [ "$HASH_BEFORE" = "$HASH_AFTER" ] && check "aborted upgrade left the binary unchanged" 0 || check "aborted upgrade left the binary unchanged" 1
    grep -q "^version=$VERSION$" "$MANIFEST" && check "aborted upgrade left the manifest unchanged" 0 || check "aborted upgrade left the manifest unchanged" 1
else
    skip "transaction-order test needs --previous-assets"
fi

sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" --update-skill >/dev/null
grep -q "user edit marker" "$WORK/skills/minecraft-toolkit/SKILL.md" && check "--update-skill replaced the edited copy" 1 || check "--update-skill replaced the edited copy" 0
ls "$WORK/skills"/minecraft-toolkit.backup-* >/dev/null 2>&1 && check "--update-skill kept a backup" 0 || check "--update-skill kept a backup" 1

# A binary-only upgrade must preserve the existing skill record, and a later
# --remove-skill must still find and remove it.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
grep -Fq "dir=$WORK/skills/minecraft-toolkit" "$MANIFEST" && check "binary-only reinstall preserved the skill record" 0 || check "binary-only reinstall preserved the skill record" 1
sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --remove-skill >/dev/null
[ ! -d "$WORK/skills/minecraft-toolkit" ] && check "--remove-skill removed the preserved skill" 0 || check "--remove-skill removed the preserved skill" 1

# Several explicit targets, including a relative path and non-ASCII/space
# names, stay recorded across installs and are removed on uninstall.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skillsA" >/dev/null
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills dir/üñí" >/dev/null
(cd "$WORK" && sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir ./relskills) >/dev/null
[ "$(grep -c '^skill\.[0-9]*\.dir=' "$MANIFEST")" -eq 3 ] && check "three skill targets are recorded" 0 || check "three skill targets are recorded" 1
grep -Fq "dir=$WORK/skills dir/üñí" "$MANIFEST" && check "non-ASCII/space skill path stored canonically" 0 || check "non-ASCII/space skill path stored canonically" 1
grep -Fq "dir=$WORK/relskills" "$MANIFEST" && check "relative skill path stored as absolute" 0 || check "relative skill path stored as absolute" 1
sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --remove-skill >/dev/null
[ ! -d "$WORK/skillsA/minecraft-toolkit" ] && [ ! -d "$WORK/skills dir/üñí/minecraft-toolkit" ] && [ ! -d "$WORK/relskills/minecraft-toolkit" ] \
    && check "uninstall removed all recorded skill targets" 0 || check "uninstall removed all recorded skill targets" 1

# ------------------------------------------------------------- uninstall safety

sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
rm -f "$MANIFEST"
if sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" >/dev/null 2>&1; then
    check "uninstall without a manifest is refused" 1
else
    check "uninstall without a manifest is refused" 0
fi
sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --force >/dev/null
[ ! -f "$BINARY" ] && check "--force uninstall removed the unowned binary" 0 || check "--force uninstall removed the unowned binary" 1

# --purge-state only removes a recognized product state directory.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
mkdir -p "$WORK/empty-state"
: >"$WORK/empty-state/sentinel"
if MC_AGENT_HOME="$WORK/empty-state" sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --purge-state >/dev/null 2>&1; then
    check "purge-state refuses a directory without product ownership" 1
else
    check "purge-state refuses a directory without product ownership" 0
fi
[ -f "$WORK/empty-state/sentinel" ] && check "refused purge-state left the directory untouched" 0 || check "refused purge-state left the directory untouched" 1
printf '{}\n' >"$WORK/empty-state/daemon.json"
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
MC_AGENT_HOME="$WORK/empty-state" sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --purge-state >/dev/null
[ ! -d "$WORK/empty-state" ] && check "purge-state removed a product-owned state directory" 0 || check "purge-state removed a product-owned state directory" 1

# --dry-run must not create install/skill/state directories or the profile entry.
rm -rf "$WORK/dry"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$WORK/dry/bin" \
    --skill-dir "$WORK/dry/skills" --add-to-path --dry-run >/dev/null 2>&1; then
    check "dry-run exits successfully" 0
else
    check "dry-run exits successfully" 1
fi
[ ! -d "$WORK/dry" ] && check "dry-run created nothing" 0 || check "dry-run created nothing" 1

# A corrupted archive must fail verification and leave the install untouched.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
mkdir -p "$WORK/staging-corrupt"
cp "$ASSETS/mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz" \
    "$WORK/staging-corrupt/mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz"
(cd "$WORK/staging-corrupt" && (command -v sha256sum >/dev/null 2>&1 && sha256sum "mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz" || shasum -a 256 "mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz") >checksums.txt)
printf 'corruption' >>"$WORK/staging-corrupt/mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz"
HASH_BEFORE="$(hash_file "$BINARY")"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$WORK/staging-corrupt" --install-dir "$BIN_DIR" --no-skill >/dev/null 2>&1; then
    check "corrupted archive is refused" 1
else
    check "corrupted archive is refused" 0
fi
HASH_AFTER="$(hash_file "$BINARY")"
[ "$HASH_BEFORE" = "$HASH_AFTER" ] && check "refused checksum left the binary unchanged" 0 || check "refused checksum left the binary unchanged" 1

if [ "$failures" -ne 0 ]; then
    echo "$failures check(s) failed" >&2
    exit 1
fi
echo "all release-install acceptance checks passed"
