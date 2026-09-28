#!/bin/sh
# mc-agent release installer for Linux (glibc amd64) and macOS (arm64).
#
# Installs the Go CLI/daemon binary from a versioned GitHub release, verifies
# the published checksum and the embedded version, and can install the matching
# Toolkit Skill into an explicit harness or custom directory. It never installs
# Python, Go or MCP, and it never silently overwrites user content.
#
#   curl -fsSL https://raw.githubusercontent.com/guajun/mc-agent-bridge/v0.5.0/install/install.sh \
#       | sh -s -- --version 0.5.0 --skill-harness codex
#
# See docs/install.md for the full matrix and examples. The undocumented
# MC_AGENT_INSTALL_FAULT environment variable injects a commit-stage failure
# for the negative acceptance tests (after-binary, after-skill, at-manifest).
set -eu

PRODUCT="mc-agent"
REPO="guajun/mc-agent-bridge"
PRODUCT_VERSION="0.5.0"
ASSET_BINARY="mc-agent"
MANIFEST_NAME="mc-agent.installed"
PREVIOUS_NAME="mc-agent.previous"
MANIFEST_VERSION="2"
STATE_FILES="daemon.json targets.json secrets.json unknown_writes.json daemon.log"
STATE_IDENTITY_FILES="daemon.json targets.json secrets.json"

die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

info() {
    printf '%s\n' "$*" >&2
}

usage() {
    cat >&2 <<EOF
mc-agent installer (Linux glibc amd64, macOS arm64)

Usage: install.sh [options]

Install or upgrade:
  --version VERSION       Product version to install (default: ${PRODUCT_VERSION})
  --install-dir DIR       Binary directory (default: \$HOME/.mc-agent/bin)
  --base-url URL          Release base URL (default: GitHub release for --version)
  --from-dir DIR          Install from a local staged asset directory (no network)
  --archive FILE          Install this local archive instead of downloading
  --sha256 HEX            Expected sha256 of --archive (recommended with --archive)
  --skill-harness NAME    codex | claude-code | universal | hermes
  --skill-dir DIR         Explicit skill parent directory (installs <DIR>/minecraft-toolkit)
  --update-skill          Replace an existing skill directory (a backup is kept)
  --no-skill              Do not install any skill
  --add-to-path           Append the install directory to \$HOME/.profile once
  --dry-run               Print the plan without writing anything
  --force                 Replace a modified managed binary / unowned file

Uninstall:
  --uninstall             Remove the installed binary
  --remove-skill          With --uninstall: remove unchanged installed skill copies
  --purge-state           With --uninstall: remove known product state files
  --force                 With --uninstall: allow a missing/invalid manifest

Every requested component (binary, skill) is staged and verified before the
installation is touched; checksum, version, manifest and conflict failures
leave the current installation unchanged. The commit stage snapshots the
binary, manifest and skill and rolls all of them back together if any step
fails. The skill bundle comes from the release's pinned mc-agent commit.
EOF
    exit 2
}

# ---------------------------------------------------------------- arguments

VERSION="$PRODUCT_VERSION"
INSTALL_DIR=""
BASE_URL=""
FROM_DIR=""
ARCHIVE=""
EXPECTED_SHA=""
SKILL_HARNESS=""
SKILL_DIR=""
UPDATE_SKILL=0
NO_SKILL=0
ADD_TO_PATH=0
DRY_RUN=0
UNINSTALL=0
REMOVE_SKILL=0
PURGE_STATE=0
FORCE=0

while [ $# -gt 0 ]; do
    case "$1" in
        --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
        --install-dir) [ $# -ge 2 ] || die "--install-dir needs a value"; INSTALL_DIR="$2"; shift 2 ;;
        --base-url) [ $# -ge 2 ] || die "--base-url needs a value"; BASE_URL="$2"; shift 2 ;;
        --from-dir) [ $# -ge 2 ] || die "--from-dir needs a value"; FROM_DIR="$2"; shift 2 ;;
        --archive) [ $# -ge 2 ] || die "--archive needs a value"; ARCHIVE="$2"; shift 2 ;;
        --sha256) [ $# -ge 2 ] || die "--sha256 needs a value"; EXPECTED_SHA="$2"; shift 2 ;;
        --skill-harness) [ $# -ge 2 ] || die "--skill-harness needs a value"; SKILL_HARNESS="$2"; shift 2 ;;
        --skill-dir) [ $# -ge 2 ] || die "--skill-dir needs a value"; SKILL_DIR="$2"; shift 2 ;;
        --update-skill) UPDATE_SKILL=1; shift ;;
        --no-skill) NO_SKILL=1; shift ;;
        --add-to-path) ADD_TO_PATH=1; shift ;;
        --dry-run) DRY_RUN=1; shift ;;
        --uninstall) UNINSTALL=1; shift ;;
        --remove-skill) REMOVE_SKILL=1; shift ;;
        --purge-state) PURGE_STATE=1; shift ;;
        --force) FORCE=1; shift ;;
        -h|--help) usage ;;
        *) die "unknown option: $1 (see --help)" ;;
    esac
done

# ---------------------------------------------------------------- platform

OS="$(uname -s)"
MACHINE="$(uname -m)"
case "$OS" in
    Linux) GOOS="linux" ;;
    Darwin) GOOS="darwin" ;;
    *) die "unsupported OS '$OS': this installer supports Linux glibc amd64 and macOS arm64 only" ;;
esac
case "$MACHINE" in
    x86_64|amd64) GOARCH="amd64" ;;
    arm64|aarch64) GOARCH="arm64" ;;
    *) die "unsupported CPU '$MACHINE'" ;;
esac
if [ "$GOOS" = "linux" ]; then
    [ "$GOARCH" = "amd64" ] || die "unsupported platform linux/$GOARCH: Linux arm64 is not built or tested by this release line"
    if { command -v ldd >/dev/null 2>&1 && ldd --version 2>&1 | grep -qi musl; } \
        || ls /lib/ld-musl-*.so.1 >/dev/null 2>&1; then
        die "unsupported libc: the Linux release is built for glibc and not tested on musl/Alpine"
    fi
fi
if [ "$GOOS" = "darwin" ] && [ "$GOARCH" != "arm64" ]; then
    die "unsupported platform darwin/$GOARCH: macOS amd64 is not built or tested by this release line"
fi
PLATFORM="$GOOS/$GOARCH"

if [ -z "$INSTALL_DIR" ]; then
    INSTALL_DIR="$HOME/.mc-agent/bin"
fi
if [ -z "$BASE_URL" ]; then
    BASE_URL="https://github.com/$REPO/releases/download/v$VERSION"
fi
MANIFEST="$INSTALL_DIR/$MANIFEST_NAME"
BINARY="$INSTALL_DIR/$ASSET_BINARY"
PREVIOUS="$INSTALL_DIR/$PREVIOUS_NAME"
FAULT="${MC_AGENT_INSTALL_FAULT:-}"

# ---------------------------------------------------------------- helpers

hash_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        die "need sha256sum or shasum to verify downloads"
    fi
}

hash_stdin() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 | awk '{print $1}'
    else
        die "need sha256sum or shasum to verify downloads"
    fi
}

tree_hash() {
    _dir="$1"
    (cd "$_dir" && find . -type f | LC_ALL=C sort | while IFS= read -r _file; do
        printf '%s %s\n' "$(hash_file "$_file")" "${_file#./}"
    done) | hash_stdin
}

# canonical_path resolves an existing or not-yet-existing path to an absolute,
# symlink-resolved form without creating anything.
canonical_path() {
    _path="$1"
    case "$_path" in
        /*) : ;;
        *) _path="$(pwd)/$_path" ;;
    esac
    if [ -d "$_path" ]; then
        (cd "$_path" && pwd -P)
        return 0
    fi
    _parent="$(dirname "$_path")"
    _base="$(basename "$_path")"
    if [ "$_parent" = "/" ]; then
        printf '/%s\n' "$_base"
        return 0
    fi
    printf '%s/%s\n' "$(canonical_path "$_parent")" "$_base"
}

# assert_no_symlink_ancestors refuses to touch a path whose existing ancestors
# include a symlink, so a forged path cannot escape through a link.
assert_no_symlink_ancestors() {
    _parent="$(dirname "$1")"
    while [ -n "$_parent" ] && [ "$_parent" != "/" ] && [ "$_parent" != "." ]; do
        [ -L "$_parent" ] && die "refusing to touch a path through a symlinked ancestor: $_parent"
        _next="$(dirname "$_parent")"
        [ "$_next" = "$_parent" ] && break
        _parent="$_next"
    done
}

# safe_remove_tree refuses to recursively delete roots, home, home ancestors,
# symlinks, symlinked ancestors or the install directory.
safe_remove_tree() {
    _target="$1"
    [ -n "$_target" ] || die "refusing to remove an empty path"
    [ -L "$_target" ] && die "refusing to remove a symlink: $_target"
    _canon="$(canonical_path "$_target")"
    [ -d "$_canon" ] || return 0
    [ "$_canon" = "/" ] && die "refusing to remove the filesystem root"
    [ "$_canon" = "$HOME" ] && die "refusing to remove the home directory"
    case "$HOME/" in
        "$_canon"/*) die "refusing to remove an ancestor of the home directory" ;;
    esac
    [ "$_canon" = "$(canonical_path "$INSTALL_DIR")" ] && die "refusing to remove the install directory"
    assert_no_symlink_ancestors "$_canon"
    rm -rf "$_target"
}

# purge_state_dir removes only known product-owned state files and keeps the
# directory (and any unrelated files) unless it becomes empty.
purge_state_dir() {
    _dir="$1"
    _found=0
    for _name in $STATE_FILES; do
        _file="$_dir/$_name"
        if [ -L "$_file" ]; then
            die "refusing to remove symlinked state file: $_file"
        elif [ -d "$_file" ]; then
            die "refusing to remove directory-valued state file: $_file"
        elif [ -f "$_file" ]; then
            rm -f "$_file"
            info "removed state file $_file"
            _found=1
        fi
    done
    [ "$_found" = 1 ] || info "no known product state files found in $_dir"
    if rmdir "$_dir" 2>/dev/null; then
        info "removed empty state directory $_dir"
    else
        info "kept state directory with unrelated files: $_dir"
    fi
}

fetch() {
    _url="$1"
    _dest="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$_url" -o "$_dest"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$_dest" "$_url"
    else
        die "need curl or wget to download release assets"
    fi
}

source_asset() {
    _name="$1"
    _dest="$2"
    if [ -n "$FROM_DIR" ]; then
        [ -f "$FROM_DIR/$_name" ] || die "missing staged asset: $FROM_DIR/$_name"
        cp "$FROM_DIR/$_name" "$_dest"
    else
        fetch "$BASE_URL/$_name" "$_dest"
    fi
}

verify_checksum() {
    _file="$1"
    _name="$2"
    if [ -f "$WORK/checksums.txt" ]; then
        _expected="$(awk -v name="$_name" '
            {
                file = $2
                sub(/^\*/, "", file)
                sub(/\r$/, "", file)
                if (file == name) { print $1; exit }
            }' "$WORK/checksums.txt")"
    fi
    if [ -z "${_expected:-}" ] && [ -n "$EXPECTED_SHA" ]; then
        _expected="$EXPECTED_SHA"
    fi
    [ -n "${_expected:-}" ] || die "no published checksum found for $_name"
    _actual="$(hash_file "$_file")"
    [ "$_actual" = "$_expected" ] || die "checksum mismatch for $_name: expected $_expected, got $_actual"
}

verify_binary_version() {
    _binary="$1"
    _want="$2"
    _output="$("$_binary" version 2>&1)" || die "cannot run the extracted binary: $_output"
    set -- $_output
    [ "${1:-}" = "$PRODUCT" ] || die "extracted binary did not report $PRODUCT: $_output"
    [ "${2:-}" = "$_want" ] || die "archive contains version ${2:-unknown}, requested $_want"
}

manifest_get() {
    _file="$1"
    _key="$2"
    [ -f "$_file" ] || return 0
    sed -n "s/^${_key}=//p" "$_file" | head -n 1
}

# manifest_valid requires the product, format version and a well-formed
# binary hash; an empty or unrelated file is never treated as ownership.
manifest_valid() {
    _file="$1"
    [ -f "$_file" ] && [ -s "$_file" ] || return 1
    [ "$(manifest_get "$_file" manifest_version)" = "$MANIFEST_VERSION" ] || return 1
    [ "$(manifest_get "$_file" product)" = "$PRODUCT" ] || return 1
    [ -n "$(manifest_get "$_file" version)" ] || return 1
    [ -n "$(manifest_get "$_file" platform)" ] || return 1
    _hash="$(manifest_get "$_file" binary_sha256)"
    [ ${#_hash} -eq 64 ] || return 1
    case "$_hash" in
        *[!0-9a-f]*) return 1 ;;
    esac
    return 0
}

manifest_skill_dirs() {
    _file="$1"
    [ -f "$_file" ] || return 0
    sed -n 's/^skill\.[0-9][0-9]*\.dir=//p' "$_file"
}

manifest_skill_tree() {
    awk -v dir="$2" '
        /^skill\.[0-9][0-9]*\.dir=/ {
            value = substr($0, index($0, "=") + 1)
            index_part = substr($0, 7); sub(/\..*/, "", index_part)
            if (value == dir) current = index_part
            next
        }
        /^skill\.[0-9][0-9]*\.tree_sha256=/ && current != "" {
            print substr($0, index($0, "=") + 1)
            exit
        }
    ' "$1"
}

manifest_max_skill_index() {
    _file="$1"
    _max=0
    [ -f "$_file" ] || { printf '0\n'; return 0; }
    while IFS= read -r _line; do
        case "$_line" in
            skill.*.dir=*)
                _index="${_line#skill.}"
                _index="${_index%%.dir=*}"
                case "$_index" in
                    ''|*[!0-9]*) continue ;;
                esac
                [ "$_index" -gt "$_max" ] && _max="$_index"
                ;;
        esac
    done <"$_file"
    printf '%s\n' "$_max"
}

# skill_entry_removable accepts only an absolute path whose leaf is the
# install-owned minecraft-toolkit directory; forged manifest lines are refused
# even with --force.
skill_entry_removable() {
    _dir="$1"
    case "$_dir" in
        /*) : ;;
        *) return 1 ;;
    esac
    [ "$(basename "$_dir")" = "minecraft-toolkit" ] || return 1
    return 0
}

state_dir() {
    if [ -n "${MC_AGENT_HOME:-}" ]; then
        printf '%s\n' "$MC_AGENT_HOME"
    elif [ "$GOOS" = "darwin" ]; then
        printf '%s\n' "$HOME/Library/Application Support/mc-agent"
    else
        printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/mc-agent"
    fi
}

default_state_dir() {
    if [ "$GOOS" = "darwin" ]; then
        printf '%s\n' "$HOME/Library/Application Support/mc-agent"
    else
        printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/mc-agent"
    fi
}

is_product_state_dir() {
    _dir="$1"
    [ "$_dir" = "$(default_state_dir)" ] && return 0
    for _marker in $STATE_IDENTITY_FILES; do
        [ -f "$_dir/$_marker" ] && return 0
    done
    return 1
}

skill_root_for_harness() {
    case "$1" in
        codex) printf '%s\n' "${CODEX_HOME:-$HOME/.codex}/skills" ;;
        claude|claude-code) printf '%s\n' "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/skills" ;;
        universal) printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/agents/skills" ;;
        hermes)
            [ -n "${HERMES_HOME:-}" ] || die "the hermes harness has no verified default directory; set HERMES_HOME or pass --skill-dir DIR"
            printf '%s\n' "$HERMES_HOME/skills"
            ;;
        *) die "unknown harness '$1'; use --skill-dir DIR for a custom location" ;;
    esac
}

# ---------------------------------------------------------------- plan

[ -n "$VERSION" ] || die "--version is required"
if [ -n "$SKILL_HARNESS" ] && [ -n "$SKILL_DIR" ]; then
    die "use either --skill-harness or --skill-dir, not both"
fi
if [ "$NO_SKILL" = "1" ] && { [ -n "$SKILL_HARNESS" ] || [ -n "$SKILL_DIR" ]; }; then
    die "use either --no-skill or a skill target, not both"
fi
if [ "$UNINSTALL" = "1" ] && { [ -n "$ARCHIVE" ] || [ -n "$FROM_DIR" ] || [ -n "$SKILL_HARNESS" ] || [ -n "$SKILL_DIR" ]; }; then
    die "--uninstall cannot be combined with install-only options"
fi
case "$VERSION" in
    ""|*[!A-Za-z0-9._+-]*) die "invalid version string: $VERSION" ;;
esac

INSTALL_DIR="$(canonical_path "$INSTALL_DIR")"
MANIFEST="$INSTALL_DIR/$MANIFEST_NAME"
BINARY="$INSTALL_DIR/$ASSET_BINARY"
PREVIOUS="$INSTALL_DIR/$PREVIOUS_NAME"
ARCHIVE_NAME="$PRODUCT-$VERSION-$GOOS-$GOARCH.tar.gz"

if [ "$DRY_RUN" = "1" ]; then
    info "dry-run: platform $PLATFORM, version $VERSION"
    info "dry-run: would install $BINARY (from $BASE_URL/$ARCHIVE_NAME or staged input)"
    if [ -n "$SKILL_DIR" ]; then
        info "dry-run: would install the pinned skill into $(canonical_path "$SKILL_DIR")/minecraft-toolkit"
    elif [ -n "$SKILL_HARNESS" ]; then
        info "dry-run: would install the pinned skill into $(skill_root_for_harness "$SKILL_HARNESS")/minecraft-toolkit"
    fi
    info "dry-run: would write $MANIFEST"
    [ "$ADD_TO_PATH" = "1" ] && info "dry-run: would add $INSTALL_DIR to PATH in $HOME/.profile"
    exit 0
fi

# ---------------------------------------------------------------- uninstall

if [ "$UNINSTALL" = "1" ]; then
    [ -f "$BINARY" ] || [ -f "$MANIFEST" ] || die "no $PRODUCT installation found at $INSTALL_DIR"
    [ -L "$BINARY" ] && [ "$FORCE" != "1" ] && die "refusing to remove a symlinked binary: $BINARY"
    if [ -f "$MANIFEST" ]; then
        if ! manifest_valid "$MANIFEST" && [ "$FORCE" != "1" ]; then
            die "$MANIFEST is not a valid $PRODUCT manifest; pass --force to remove anyway"
        fi
        _recorded="$(manifest_get "$MANIFEST" binary_sha256)"
        if [ -f "$BINARY" ] && [ -n "$_recorded" ] && [ "$FORCE" != "1" ] \
            && [ "$(hash_file "$BINARY")" != "$_recorded" ]; then
            die "$BINARY differs from the recorded install; pass --force to remove it anyway"
        fi
    elif [ "$FORCE" != "1" ]; then
        die "$INSTALL_DIR has no product manifest; pass --force to remove $BINARY"
    fi
    _purge_target=""
    if [ "$PURGE_STATE" = "1" ]; then
        _purge_target="$(canonical_path "$(state_dir)")"
        if [ -d "$_purge_target" ] && ! is_product_state_dir "$_purge_target"; then
            die "refusing --purge-state: $_purge_target is not a recognized $PRODUCT state directory"
        fi
    fi
    if [ "$REMOVE_SKILL" = "1" ] && [ -f "$MANIFEST" ]; then
        _skill_list="$(mktemp "${TMPDIR:-/tmp}/mc-agent-skills.XXXXXX")"
        manifest_skill_dirs "$MANIFEST" >"$_skill_list"
        while IFS= read -r _dir; do
            [ -n "$_dir" ] || continue
            if ! skill_entry_removable "$_dir"; then
                info "refusing to remove a non-skill path from the manifest: $_dir"
                continue
            fi
            _recorded_tree="$(manifest_skill_tree "$MANIFEST" "$_dir")"
            if [ ! -d "$_dir" ]; then
                info "skill directory is already gone: $_dir"
            elif [ "$(canonical_path "$_dir")" != "$_dir" ]; then
                info "refusing to remove a skill path that resolves through a symlink: $_dir"
                continue
            elif [ "$FORCE" = "1" ] || { [ -n "$_recorded_tree" ] && [ "$(tree_hash "$_dir")" = "$_recorded_tree" ]; }; then
                safe_remove_tree "$_dir"
                info "removed skill $_dir"
            else
                info "keeping modified skill directory (pass --force to remove): $_dir"
            fi
        done <"$_skill_list"
        rm -f "$_skill_list"
    elif [ "$REMOVE_SKILL" = "1" ]; then
        info "no install manifest: not removing any skill directory"
    fi
    [ -f "$BINARY" ] && rm -f "$BINARY"
    rm -f "$PREVIOUS" "$MANIFEST"
    info "removed the $PRODUCT binary from $INSTALL_DIR"
    if [ "$PURGE_STATE" = "1" ] && [ -d "$_purge_target" ]; then
        purge_state_dir "$_purge_target"
    fi
    info "uninstall complete"
    exit 0
fi

# ---------------------------------------------------------------- stage

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mc-agent-install.XXXXXX")"
IN_TRANSACTION=0
RECOVERY_ROOT="${MC_AGENT_RECOVERY_ROOT:-${TMPDIR:-/tmp}}"

preserve_snapshot() {
    [ -d "$SNAPSHOT" ] || return 1
    _recovery="$(mktemp -d "$RECOVERY_ROOT/mc-agent-recovery.XXXXXX" 2>/dev/null)" || return 1
    if cp -R "$SNAPSHOT"/. "$_recovery"/ 2>/dev/null; then
        info "recovery snapshot preserved at $_recovery"
        return 0
    fi
    info "snapshot directory is at $SNAPSHOT"
    return 1
}

# Any exit while the commit is in progress rolls the installation back; an
# unexpected set -e failure takes exactly the same path as an injected one.
transaction_cleanup() {
    _status=$?
    if [ "${IN_TRANSACTION:-0}" = "1" ]; then
        IN_TRANSACTION=0
        info "the commit did not complete; rolling back"
        if ! rollback_commit; then
            preserve_snapshot || info "no recovery snapshot could be saved"
        fi
    fi
    rm -rf "$WORK"
    exit "$_status"
}
trap transaction_cleanup EXIT INT TERM

# 1. Stage and verify the binary before touching anything.
if [ -n "$ARCHIVE" ]; then
    [ -f "$ARCHIVE" ] || die "archive not found: $ARCHIVE"
    cp "$ARCHIVE" "$WORK/$ARCHIVE_NAME"
    SOURCE_DESCRIPTION="$ARCHIVE"
else
    source_asset checksums.txt "$WORK/checksums.txt"
    source_asset "$ARCHIVE_NAME" "$WORK/$ARCHIVE_NAME"
    SOURCE_DESCRIPTION="$BASE_URL/$ARCHIVE_NAME"
fi
if [ -n "$ARCHIVE" ] && [ -z "$EXPECTED_SHA" ]; then
    info "warning: --archive without --sha256; skipping checksum verification"
else
    verify_checksum "$WORK/$ARCHIVE_NAME" "$ARCHIVE_NAME"
fi
mkdir -p "$WORK/extract"
tar -xzf "$WORK/$ARCHIVE_NAME" -C "$WORK/extract"
[ -f "$WORK/extract/$ASSET_BINARY" ] || die "archive does not contain $ASSET_BINARY"
chmod 755 "$WORK/extract/$ASSET_BINARY"
verify_binary_version "$WORK/extract/$ASSET_BINARY" "$VERSION"

# 2. Validate the existing installation before replacing it.
if [ -L "$BINARY" ] && [ "$FORCE" != "1" ]; then
    die "refusing to replace a symlinked binary: $BINARY (pass --force to replace the link)"
fi
if [ -f "$BINARY" ]; then
    if [ -f "$MANIFEST" ] && manifest_valid "$MANIFEST"; then
        _recorded="$(manifest_get "$MANIFEST" binary_sha256)"
        if [ "$FORCE" != "1" ] && [ "$(hash_file "$BINARY")" != "$_recorded" ]; then
            die "$BINARY was modified after installation; pass --force to replace it"
        fi
    elif [ "$FORCE" != "1" ]; then
        die "$BINARY exists without a valid product manifest; pass --force to replace it"
    fi
fi

# 3. Stage and validate the skill (conflicts included) before the binary moves.
SKILL_ROOT=""
SKILL_TARGET=""
SKILL_VERSION=""
SKILL_COMMIT=""
if [ "$NO_SKILL" != "1" ]; then
    if [ -n "$SKILL_DIR" ]; then
        SKILL_ROOT="$(canonical_path "$SKILL_DIR")"
    elif [ -n "$SKILL_HARNESS" ]; then
        SKILL_ROOT="$(canonical_path "$(skill_root_for_harness "$SKILL_HARNESS")")"
    fi
    if [ -z "$SKILL_ROOT" ]; then
        info "no skill target given; pass --skill-harness NAME or --skill-dir DIR to install the Toolkit Skill"
    else
        SKILL_TARGET="$SKILL_ROOT/minecraft-toolkit"
        if [ -d "$SKILL_TARGET" ] && [ "$UPDATE_SKILL" != "1" ]; then
            die "$SKILL_TARGET already exists; pass --update-skill to replace it (a backup is kept)"
        fi
        if [ -L "$SKILL_TARGET" ]; then
            die "refusing to replace a symlinked skill directory: $SKILL_TARGET"
        fi
        assert_no_symlink_ancestors "$SKILL_TARGET"
        source_asset skill-pin.json "$WORK/skill-pin.json"
        SKILL_ASSET="$(sed -n 's/.*"asset"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$WORK/skill-pin.json" | head -n 1)"
        SKILL_COMMIT="$(sed -n 's/.*"commit"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$WORK/skill-pin.json" | head -n 1)"
        [ -n "$SKILL_ASSET" ] || die "skill-pin.json does not name a skill asset"
        source_asset "$SKILL_ASSET" "$WORK/$SKILL_ASSET"
        verify_checksum "$WORK/$SKILL_ASSET" "$SKILL_ASSET"
        mkdir -p "$WORK/skill"
        tar -xzf "$WORK/$SKILL_ASSET" -C "$WORK/skill"
        [ -f "$WORK/skill/minecraft-toolkit/SKILL.md" ] || die "skill bundle is missing minecraft-toolkit/SKILL.md"
        SKILL_VERSION="$(tr -d '\r"' <"$WORK/skill/minecraft-toolkit/SKILL.md" | awk '/^[[:space:]]*version:[[:space:]]*[0-9]/{sub(/^[[:space:]]*version:[[:space:]]*/,""); print; exit}')"
        [ -n "$SKILL_VERSION" ] || die "cannot read the skill version from the bundle"
    fi
fi

# ---------------------------------------------------------------- commit
#
# The commit stage snapshots the binary, previous binary, manifest and touched
# skill, then applies every change. Any failure restores all four, so a failed
# upgrade never leaves a half-installed product and a fresh failed install
# never leaves an unmanaged binary.

SNAPSHOT="$WORK/snapshot"
mkdir -p "$SNAPSHOT"
HAD_BINARY=0
HAD_MANIFEST=0
HAD_PREVIOUS=0
HAD_SKILL=0
CREATED_INSTALL_DIR=0
[ -d "$INSTALL_DIR" ] || CREATED_INSTALL_DIR=1
[ -f "$BINARY" ] && { cp -p "$BINARY" "$SNAPSHOT/binary"; HAD_BINARY=1; }
[ -f "$MANIFEST" ] && { cp -p "$MANIFEST" "$SNAPSHOT/manifest"; HAD_MANIFEST=1; }
[ -f "$PREVIOUS" ] && { cp -p "$PREVIOUS" "$SNAPSHOT/previous"; HAD_PREVIOUS=1; }
if [ -n "$SKILL_TARGET" ] && [ -d "$SKILL_TARGET" ]; then
    cp -R "$SKILL_TARGET" "$SNAPSHOT/skill"
    HAD_SKILL=1
fi

rollback_commit() {
    _rollback_failed=0
    if [ "$HAD_BINARY" = 1 ]; then
        rm -f "$BINARY" || _rollback_failed=1
        cp -p "$SNAPSHOT/binary" "$BINARY" || _rollback_failed=1
    else
        rm -f "$BINARY" || _rollback_failed=1
    fi
    if [ "$HAD_MANIFEST" = 1 ]; then
        rm -f "$MANIFEST" || _rollback_failed=1
        cp -p "$SNAPSHOT/manifest" "$MANIFEST" || _rollback_failed=1
    else
        rm -f "$MANIFEST" || _rollback_failed=1
    fi
    if [ "$HAD_PREVIOUS" = 1 ]; then
        rm -f "$PREVIOUS" || _rollback_failed=1
        cp -p "$SNAPSHOT/previous" "$PREVIOUS" || _rollback_failed=1
    else
        rm -f "$PREVIOUS" || _rollback_failed=1
    fi
    if [ -n "$SKILL_TARGET" ]; then
        rm -rf "$SKILL_TARGET" || _rollback_failed=1
        if [ "$HAD_SKILL" = 1 ]; then
            mkdir -p "$SKILL_ROOT" || _rollback_failed=1
            cp -R "$SNAPSHOT/skill" "$SKILL_TARGET" || _rollback_failed=1
        fi
    fi
    [ "$CREATED_INSTALL_DIR" = 1 ] && rmdir "$INSTALL_DIR" 2>/dev/null || true
    return "$_rollback_failed"
}

die_rollback() {
    IN_TRANSACTION=0
    info "rolling back the installation: $1"
    if ! rollback_commit; then
        preserve_snapshot || info "no recovery snapshot could be saved"
    fi
    die "$1"
}

inject_fault() {
    if [ "$FAULT" = "$1" ]; then
        # An injected failure exits like any unexpected commit failure: the
        # EXIT trap dispatches the shared rollback path.
        die "injected failure: $1"
    fi
}

IN_TRANSACTION=1
mkdir -p "$INSTALL_DIR" || die "cannot create $INSTALL_DIR"
if [ "$HAD_BINARY" = 1 ]; then
    rm -f "$PREVIOUS"
    cp -p "$BINARY" "$PREVIOUS" || die_rollback "cannot keep a copy of the previous binary"
fi
mv -f "$WORK/extract/$ASSET_BINARY" "$BINARY" || die_rollback "cannot replace $BINARY"
inject_fault after-binary
NEW_SHA="$(hash_file "$BINARY")"
info "installed $PRODUCT $VERSION ($PLATFORM) to $BINARY"

if [ -n "$SKILL_TARGET" ]; then
    mkdir -p "$SKILL_ROOT" || die_rollback "cannot create $SKILL_ROOT"
    rm -rf "$SKILL_TARGET" || die_rollback "cannot replace $SKILL_TARGET"
    if [ "$FAULT" = "skill-copy" ]; then
        # A genuine command failure at the skill-copy site, dispatched by the
        # shared EXIT trap like any other unexpected commit failure.
        cp -R "$WORK/missing-skill-source" "$SKILL_TARGET"
    fi
    cp -R "$WORK/skill/minecraft-toolkit" "$SKILL_TARGET" || die_rollback "cannot install the skill to $SKILL_TARGET"
    inject_fault after-skill
    info "installed skill $SKILL_VERSION to $SKILL_TARGET"
fi

SKILL_INDEX="$(manifest_max_skill_index "$MANIFEST")"
if [ -n "$SKILL_TARGET" ]; then
    SKILL_INDEX=$((SKILL_INDEX + 1))
fi
MANIFEST_TMP="$INSTALL_DIR/$MANIFEST_NAME.new.$$"
{
    printf 'manifest_version=%s\n' "$MANIFEST_VERSION"
    printf 'product=%s\n' "$PRODUCT"
    printf 'version=%s\n' "$VERSION"
    printf 'platform=%s\n' "$PLATFORM"
    printf 'binary_sha256=%s\n' "$NEW_SHA"
    printf 'installed_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf 'source=%s\n' "$SOURCE_DESCRIPTION"
    if [ -f "$MANIFEST" ]; then
        awk -v target="$SKILL_TARGET" '
            /^skill\.[0-9][0-9]*\.dir=/ {
                dir = substr($0, index($0, "=") + 1)
                skip = (target != "" && dir == target)
            }
            /^skill\.[0-9][0-9]*\./ { if (!skip) print; next }
        ' "$MANIFEST"
    fi
    if [ -n "$SKILL_TARGET" ]; then
        printf 'skill.%s.dir=%s\n' "$SKILL_INDEX" "$SKILL_TARGET"
        printf 'skill.%s.version=%s\n' "$SKILL_INDEX" "$SKILL_VERSION"
        printf 'skill.%s.commit=%s\n' "$SKILL_INDEX" "$SKILL_COMMIT"
        printf 'skill.%s.tree_sha256=%s\n' "$SKILL_INDEX" "$(tree_hash "$SKILL_TARGET")"
    fi
} >"$MANIFEST_TMP" || die_rollback "cannot write the install manifest"
inject_fault at-manifest
mv -f "$MANIFEST_TMP" "$MANIFEST" || { rm -f "$MANIFEST_TMP"; die_rollback "cannot replace $MANIFEST"; }

# The commit is complete; keep a user-visible backup of the replaced skill
# (best effort: a failed backup copy must not undo a successful install).
if [ -n "$SKILL_TARGET" ] && [ "$HAD_SKILL" = 1 ]; then
    _backup="$SKILL_TARGET.backup-$(date -u +%Y%m%d%H%M%S)"
    if cp -R "$SNAPSHOT/skill" "$_backup" 2>/dev/null; then
        info "kept a backup of the previous skill at $_backup"
    else
        info "warning: could not keep a skill backup at $_backup"
    fi
fi

# The commit is complete; no further unexpected exit may roll it back.
IN_TRANSACTION=0

# ---------------------------------------------------------------- PATH

if [ "$ADD_TO_PATH" = "1" ]; then
    PROFILE="$HOME/.profile"
    MARKER="# mc-agent installer"
    if [ -f "$PROFILE" ] && grep -qF "$MARKER" "$PROFILE" 2>/dev/null; then
        info "PATH entry already present in $PROFILE"
    else
        # Shell-quote the literal directory: quotes, $ and backticks in a
        # path must never be interpreted when the profile is sourced later.
        _quoted=""
        _rest="$INSTALL_DIR"
        while :; do
            case "$_rest" in
                *"'"*)
                    _quoted="$_quoted${_rest%%\'*}'\\''"
                    _rest="${_rest#*\'}"
                    ;;
                *)
                    _quoted="$_quoted$_rest"
                    break
                    ;;
            esac
        done
        {
            printf '\n%s\n' "$MARKER"
            printf "export PATH='%s':\"\$PATH\"\n" "$_quoted"
        } >>"$PROFILE"
        info "added $INSTALL_DIR to PATH in $PROFILE (open a new shell or run: . \"$PROFILE\")"
    fi
fi

info "next: $BINARY version"
info "      $BINARY daemon start   # then: ... doctor"
