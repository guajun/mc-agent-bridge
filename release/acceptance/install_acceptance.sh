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
tree_hash() {
    _dir="$1"
    (cd "$_dir" && find . -type f | LC_ALL=C sort | while IFS= read -r _file; do
        printf '%s %s\n' "$(hash_file "$_file")" "${_file#./}"
    done) | (command -v sha256sum >/dev/null 2>&1 && sha256sum || shasum -a 256) | awk '{print $1}'
}
wait_binary_idle() {
    _tries=0
    while [ "$_tries" -lt 10 ]; do
        command -v pgrep >/dev/null 2>&1 || break
        pgrep -f "$BINARY" >/dev/null 2>&1 || break
        sleep 1
        _tries=$((_tries + 1))
    done
    return 0
}
write_checksums() {
    _dir="$1"
    (
        cd "$_dir"
        for f in *; do
            [ -f "$f" ] || continue
            if command -v sha256sum >/dev/null 2>&1; then
                sha256sum "$f"
            else
                shasum -a 256 "$f"
            fi
        done
    ) >"$_dir/checksums.txt"
}

# The previous-version fixtures are stage-only; give them the checksums and
# skill files the installer expects without mutating the input directory.
if [ -n "$PREVIOUS_ASSETS" ]; then
    mkdir -p "$WORK/previous-assets"
    for f in "$PREVIOUS_ASSETS"/*; do
        [ -f "$f" ] || continue
        cp "$f" "$WORK/previous-assets/"
    done
    if [ -f "$ASSETS/skill-pin.json" ]; then
        cp "$ASSETS/skill-pin.json" "$WORK/previous-assets/"
        _skill_asset="$(sed -n 's/.*"asset"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$ASSETS/skill-pin.json" | head -n 1)"
        if [ -n "$_skill_asset" ] && [ -f "$ASSETS/$_skill_asset" ]; then
            cp "$ASSETS/$_skill_asset" "$WORK/previous-assets/"
        fi
    fi
    write_checksums "$WORK/previous-assets"
    PREVIOUS_ASSETS="$WORK/previous-assets"
fi

# ---------------------------------------------------------- baseline install

if [ -n "$PREVIOUS_ASSETS" ]; then
    sh "$INSTALLER" --version "$PREVIOUS_VERSION" --from-dir "$PREVIOUS_ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" >/dev/null
    BASELINE_VERSION="$PREVIOUS_VERSION"
else
    sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" >/dev/null
    BASELINE_VERSION="$VERSION"
fi
"$BINARY" version | grep -q "mc-agent $BASELINE_VERSION" && check "installed binary reports version $BASELINE_VERSION" 0 || check "installed binary reports version $BASELINE_VERSION" 1
[ -f "$WORK/skills/minecraft-toolkit/SKILL.md" ] && check "baseline skill installed" 0 || check "baseline skill installed" 1
BASE_BINARY_HASH="$(hash_file "$BINARY")"
BASE_MANIFEST_HASH="$(hash_file "$MANIFEST")"
BASE_SKILL_HASH="$(tree_hash "$WORK/skills/minecraft-toolkit")"

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

# ------------------------------------------------- injected commit failures

if [ -n "$PREVIOUS_ASSETS" ]; then
    for fault in after-binary after-skill at-manifest; do
        if MC_AGENT_INSTALL_FAULT=$fault sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" \
            --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" --update-skill >/dev/null 2>&1; then
            check "injected $fault failure aborts the install" 1
        else
            check "injected $fault failure aborts the install" 0
        fi
        [ "$(hash_file "$BINARY")" = "$BASE_BINARY_HASH" ] \
            && check "$fault: original binary preserved" 0 || check "$fault: original binary preserved" 1
        [ "$(hash_file "$MANIFEST")" = "$BASE_MANIFEST_HASH" ] \
            && check "$fault: original manifest preserved" 0 || check "$fault: original manifest preserved" 1
        [ "$(tree_hash "$WORK/skills/minecraft-toolkit")" = "$BASE_SKILL_HASH" ] \
            && check "$fault: original skill tree preserved" 0 || check "$fault: original skill tree preserved" 1
    done

    # A fresh failed install must not leave an unmanaged binary or manifest.
    if MC_AGENT_INSTALL_FAULT=after-binary sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" \
        --install-dir "$WORK/fresh/bin" --no-skill >/dev/null 2>&1; then
        check "fresh injected failure aborts the install" 1
    else
        check "fresh injected failure aborts the install" 0
    fi
    [ ! -e "$WORK/fresh/bin/mc-agent" ] && [ ! -e "$WORK/fresh/bin/mc-agent.installed" ] \
        && check "fresh failed install left no binary or manifest" 0 || check "fresh failed install left no binary or manifest" 1
else
    skip "injected commit-failure tests need --previous-assets"
fi

# ------------------------------------------------------------------- upgrade

if [ -n "$PREVIOUS_ASSETS" ]; then
    sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills" --update-skill >/dev/null
    grep -q "^version=$VERSION$" "$MANIFEST" && check "upgrade updated the install manifest" 0 || check "upgrade updated the install manifest" 1
    "$BINARY" version | grep -q "mc-agent $VERSION" && check "upgraded binary reports the new version" 0 || check "upgraded binary reports the new version" 1
    [ -f "$BIN_DIR/mc-agent.previous" ] && check "upgrade kept the previous binary" 0 || check "upgrade kept the previous binary" 1

    # An archive whose binary does not match the requested version must be
    # refused before the installation changes.
    mkdir -p "$WORK/staging-mismatch"
    cp "$PREVIOUS_ASSETS/mc-agent-$PREVIOUS_VERSION-$PLAT_SUFFIX.tar.gz" \
        "$WORK/staging-mismatch/mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz"
    write_checksums "$WORK/staging-mismatch"
    HASH_BEFORE="$(hash_file "$BINARY")"
    if sh "$INSTALLER" --version "$VERSION" --from-dir "$WORK/staging-mismatch" --install-dir "$BIN_DIR" --no-skill >/dev/null 2>&1; then
        check "mismatched binary version is refused" 1
    else
        check "mismatched binary version is refused" 0
    fi
    [ "$(hash_file "$BINARY")" = "$HASH_BEFORE" ] && check "refused mismatch left the binary unchanged" 0 || check "refused mismatch left the binary unchanged" 1
else
    skip "version mismatch refusal needs --previous-assets"
fi

# A modified managed binary must be refused without --force, then replaced
# with --force. An empty/invalid manifest is never treated as ownership.
printf 'modified' >>"$BINARY"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null 2>&1; then
    check "modified managed binary is refused" 1
else
    check "modified managed binary is refused" 0
fi
: >"$MANIFEST"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null 2>&1; then
    check "empty manifest is not treated as ownership" 1
else
    check "empty manifest is not treated as ownership" 0
fi
if sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" >/dev/null 2>&1; then
    check "uninstall with an empty manifest is refused" 1
else
    check "uninstall with an empty manifest is refused" 0
fi
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill --force >/dev/null
"$BINARY" version | grep -q "mc-agent $VERSION" && check "--force replaced the modified binary and manifest" 0 || check "--force replaced the modified binary and manifest" 1

# ---------------------------------------------------------------- skill safety

SKILL_EDIT="$WORK/skills-edit"
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$SKILL_EDIT" >/dev/null
[ -f "$SKILL_EDIT/minecraft-toolkit/SKILL.md" ] && check "skill installed to the explicit directory" 0 || check "skill installed to the explicit directory" 1
printf '\nuser edit marker\n' >>"$SKILL_EDIT/minecraft-toolkit/SKILL.md"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$SKILL_EDIT" >/dev/null 2>&1; then
    check "existing skill is not overwritten without --update-skill" 1
else
    check "existing skill is not overwritten without --update-skill" 0
fi
grep -q "user edit marker" "$SKILL_EDIT/minecraft-toolkit/SKILL.md" && check "user edit survived the refused install" 0 || check "user edit survived the refused install" 1

# A rejected skill conflict must not touch the binary.
if [ -n "$PREVIOUS_ASSETS" ]; then
    HASH_BEFORE="$(hash_file "$BINARY")"
    if sh "$INSTALLER" --version "$PREVIOUS_VERSION" --from-dir "$PREVIOUS_ASSETS" --install-dir "$BIN_DIR" --skill-dir "$SKILL_EDIT" >/dev/null 2>&1; then
        check "skill conflict aborts the upgrade before the binary changes" 1
    else
        check "skill conflict aborts the upgrade before the binary changes" 0
    fi
    [ "$(hash_file "$BINARY")" = "$HASH_BEFORE" ] && check "aborted upgrade left the binary unchanged" 0 || check "aborted upgrade left the binary unchanged" 1
    grep -q "^version=$VERSION$" "$MANIFEST" && check "aborted upgrade left the manifest unchanged" 0 || check "aborted upgrade left the manifest unchanged" 1
else
    skip "transaction-order test needs --previous-assets"
fi

sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$SKILL_EDIT" --update-skill >/dev/null
grep -q "user edit marker" "$SKILL_EDIT/minecraft-toolkit/SKILL.md" && check "--update-skill replaced the edited copy" 1 || check "--update-skill replaced the edited copy" 0
ls "$SKILL_EDIT"/minecraft-toolkit.backup-* >/dev/null 2>&1 && check "--update-skill kept a backup" 0 || check "--update-skill kept a backup" 1

# A binary-only upgrade must preserve the existing skill record, and a later
# --remove-skill must still find and remove it.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
grep -Fq "dir=$SKILL_EDIT/minecraft-toolkit" "$MANIFEST" && check "binary-only reinstall preserved the skill record" 0 || check "binary-only reinstall preserved the skill record" 1
sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --remove-skill >/dev/null
[ ! -d "$SKILL_EDIT/minecraft-toolkit" ] && check "--remove-skill removed the preserved skill" 0 || check "--remove-skill removed the preserved skill" 1

# Several explicit targets, including a relative path and non-ASCII/space
# names, stay recorded across installs and are removed on uninstall.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skillsA" >/dev/null
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir "$WORK/skills dir/üñí" >/dev/null
(cd "$WORK" && sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --skill-dir ./relskills) >/dev/null
[ "$(grep -c '^skill\.[0-9]*\.dir=' "$MANIFEST")" -eq 3 ] && check "three skill targets are recorded" 0 || check "three skill targets are recorded" 1
grep -Fq "dir=$WORK/skills dir/üñí/minecraft-toolkit" "$MANIFEST" && check "non-ASCII/space skill path stored canonically" 0 || check "non-ASCII/space skill path stored canonically" 1
grep -Fq "dir=$WORK/relskills/minecraft-toolkit" "$MANIFEST" && check "relative skill path stored as absolute" 0 || check "relative skill path stored as absolute" 1
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

# --purge-state removes known product files, preserves unrelated ones, and
# keeps a nonempty directory.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
mkdir -p "$WORK/state-mixed"
printf '{}\n' >"$WORK/state-mixed/daemon.json"
printf 'keep me\n' >"$WORK/state-mixed/notes.txt"
MC_AGENT_HOME="$WORK/state-mixed" sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --purge-state >/dev/null
[ ! -f "$WORK/state-mixed/daemon.json" ] && check "purge removed the known state file" 0 || check "purge removed the known state file" 1
[ -f "$WORK/state-mixed/notes.txt" ] && check "purge preserved an unrelated file" 0 || check "purge preserved an unrelated file" 1
[ -d "$WORK/state-mixed" ] && check "purge kept the nonempty directory" 0 || check "purge kept the nonempty directory" 1

sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
mkdir -p "$WORK/state-empty"
printf '{}\n' >"$WORK/state-empty/daemon.json"
MC_AGENT_HOME="$WORK/state-empty" sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --purge-state >/dev/null
[ ! -d "$WORK/state-empty" ] && check "purge removed the empty product state directory" 0 || check "purge removed the empty product state directory" 1

# A directory with no product identity files is refused.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
mkdir -p "$WORK/state-unowned"
printf 'keep me\n' >"$WORK/state-unowned/notes.txt"
if MC_AGENT_HOME="$WORK/state-unowned" sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --purge-state >/dev/null 2>&1; then
    check "purge-state refuses a directory without product ownership" 1
else
    check "purge-state refuses a directory without product ownership" 0
fi
[ -f "$WORK/state-unowned/notes.txt" ] && check "refused purge-state left the directory untouched" 0 || check "refused purge-state left the directory untouched" 1

# A forged skill entry (not leaf minecraft-toolkit) is refused even with --force.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
mkdir -p "$WORK/evil"
printf 'do not delete\n' >"$WORK/evil/file.txt"
{
    printf 'manifest_version=2\nproduct=mc-agent\nversion=%s\nplatform=linux/amd64\n' "$VERSION"
    printf 'binary_sha256=%s\n' "$(hash_file "$BINARY")"
    printf 'skill.1.dir=%s\n' "$WORK/evil"
    printf 'skill.1.version=0.3.0\nskill.1.commit=deadbeef\n'
    printf 'skill.1.tree_sha256=%s\n' "$(tree_hash "$WORK/evil")"
} >"$MANIFEST"
sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --remove-skill --force >/dev/null 2>&1 || true
[ -f "$WORK/evil/file.txt" ] && check "forged non-skill manifest entry is never removed" 0 || check "forged non-skill manifest entry is never removed" 1

# A skill entry whose path resolves through a symlink is refused.
sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$BIN_DIR" --no-skill >/dev/null
mkdir -p "$WORK/real-skills/minecraft-toolkit"
printf 'real\n' >"$WORK/real-skills/minecraft-toolkit/SKILL.md"
ln -sfn "$WORK/real-skills" "$WORK/link-skills"
{
    printf 'manifest_version=2\nproduct=mc-agent\nversion=%s\nplatform=linux/amd64\n' "$VERSION"
    printf 'binary_sha256=%s\n' "$(hash_file "$BINARY")"
    printf 'skill.1.dir=%s\n' "$WORK/link-skills/minecraft-toolkit"
    printf 'skill.1.version=0.3.0\nskill.1.commit=deadbeef\n'
    printf 'skill.1.tree_sha256=%s\n' "$(tree_hash "$WORK/real-skills/minecraft-toolkit")"
} >"$MANIFEST"
sh "$INSTALLER" --uninstall --install-dir "$BIN_DIR" --remove-skill --force >/dev/null 2>&1 || true
[ -f "$WORK/real-skills/minecraft-toolkit/SKILL.md" ] && check "symlinked skill path is never removed" 0 || check "symlinked skill path is never removed" 1

# PATH entries must be shell-quoted: a path with quotes, $ and spaces must not
# execute anything when the profile is sourced later.
PATH_HOME="$WORK/path-home"
mkdir -p "$PATH_HOME"
EVIL_DIR="$WORK/bin \$(touch $WORK/pwned) it's"
HOME="$PATH_HOME" sh "$INSTALLER" --version "$VERSION" --from-dir "$ASSETS" --install-dir "$EVIL_DIR" --no-skill --add-to-path >/dev/null
[ -f "$PATH_HOME/.profile" ] && check "add-to-path wrote the profile" 0 || check "add-to-path wrote the profile" 1
SOURCED="$(HOME="$PATH_HOME" sh -c '. "$HOME/.profile"; printf "%s" "${PATH%%:*}"')"
[ "$SOURCED" = "$EVIL_DIR" ] && check "PATH entry preserves the literal directory" 0 || check "PATH entry preserves the literal directory ($SOURCED)" 1
[ ! -f "$WORK/pwned" ] && check "PATH entry did not execute embedded shell" 0 || check "PATH entry did not execute embedded shell" 1

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
write_checksums "$WORK/staging-corrupt"
printf 'corruption' >>"$WORK/staging-corrupt/mc-agent-$VERSION-$PLAT_SUFFIX.tar.gz"
HASH_BEFORE="$(hash_file "$BINARY")"
if sh "$INSTALLER" --version "$VERSION" --from-dir "$WORK/staging-corrupt" --install-dir "$BIN_DIR" --no-skill >/dev/null 2>&1; then
    check "corrupted archive is refused" 1
else
    check "corrupted archive is refused" 0
fi
[ "$(hash_file "$BINARY")" = "$HASH_BEFORE" ] && check "refused checksum left the binary unchanged" 0 || check "refused checksum left the binary unchanged" 1

if [ "$failures" -ne 0 ]; then
    echo "$failures check(s) failed" >&2
    exit 1
fi
echo "all release-install acceptance checks passed"
