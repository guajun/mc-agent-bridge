#!/bin/sh
# mc-agent release installer for Linux (amd64) and macOS (arm64).
#
# Installs the Go CLI/daemon binary from a versioned GitHub release, verifies
# the published checksum, and can install the matching Toolkit Skill into an
# explicit harness or custom directory. It never installs Python, Go or MCP,
# and it never silently overwrites user content.
#
#   curl -fsSL https://raw.githubusercontent.com/guajun/mc-agent-bridge/v0.5.0/install/install.sh \
#       | sh -s -- --version 0.5.0 --skill-harness codex
#
# See docs/install.md for the full matrix and examples.
set -eu

PRODUCT="mc-agent"
REPO="guajun/mc-agent-bridge"
PRODUCT_VERSION="0.5.0"
ASSET_BINARY="mc-agent"

die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

info() {
    printf '%s\n' "$*" >&2
}

usage() {
    cat >&2 <<EOF
mc-agent installer (Linux amd64, macOS arm64)

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
  --dry-run               Print the actions without writing anything

Uninstall:
  --uninstall             Remove the installed binary
  --remove-skill          With --uninstall: remove unchanged installed skill copies
  --purge-state           With --uninstall: remove the mc-agent state directory
  --force                 Allow replacing/deleting files without a matching manifest

The skill bundle comes from the release's pinned mc-agent commit, not from the
current working tree. Supported platforms are checked at start-up; unsupported
OS/CPU pairs are refused instead of guessed.
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
    *) die "unsupported OS '$OS': this installer supports Linux amd64 and macOS arm64 only" ;;
esac
case "$MACHINE" in
    x86_64|amd64) GOARCH="amd64" ;;
    arm64|aarch64) GOARCH="arm64" ;;
    *) die "unsupported CPU '$MACHINE'" ;;
esac
if [ "$GOOS" = "linux" ] && [ "$GOARCH" != "amd64" ]; then
    die "unsupported platform linux/$GOARCH: Linux arm64 is not built or tested by this release line"
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

fetch() {
    _url="$1"
    _dest="$2"
    if [ "$DRY_RUN" = "1" ]; then
        info "dry-run: would download $_url"
        return 0
    fi
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
        [ "$DRY_RUN" = "1" ] && { info "dry-run: would copy $FROM_DIR/$_name"; return 0; }
        cp "$FROM_DIR/$_name" "$_dest"
    else
        fetch "$BASE_URL/$_name" "$_dest"
    fi
}

verify_checksum() {
    _file="$1"
    _name="$2"
    if [ -n "$FROM_DIR" ] && [ ! -f "$FROM_DIR/checksums.txt" ]; then
        [ -n "$EXPECTED_SHA" ] || die "no checksums.txt in $FROM_DIR; pass --sha256 for $_name"
    fi
    if [ "$DRY_RUN" = "1" ]; then
        return 0
    fi
    if [ -f "$WORK/checksums.txt" ]; then
        _expected="$(awk -v name="$_name" '$2 == name || $2 == "./" name || $2 == name "\r" {print $1; exit}' "$WORK/checksums.txt")"
    fi
    if [ -z "${_expected:-}" ] && [ -n "$EXPECTED_SHA" ]; then
        _expected="$EXPECTED_SHA"
    fi
    [ -n "${_expected:-}" ] || die "no published checksum found for $_name"
    _actual="$(hash_file "$_file")"
    [ "$_actual" = "$_expected" ] || die "checksum mismatch for $_name: expected $_expected, got $_actual"
}

manifest_get() {
    _file="$1"
    _key="$2"
    [ -f "$_file" ] || return 0
    sed -n "s/^${_key}=//p" "$_file" | head -n 1
}

write_manifest() {
    _version="$1"
    _sha="$2"
    {
        printf 'product=%s\n' "$PRODUCT"
        printf 'version=%s\n' "$_version"
        printf 'platform=%s\n' "$PLATFORM"
        printf 'binary_sha256=%s\n' "$_sha"
        printf 'installed_at=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        printf 'source=%s\n' "${SOURCE_DESCRIPTION:-unknown}"
        if [ -n "$SKILL_TARGET" ] && [ -d "$SKILL_TARGET" ]; then
            printf 'skill.0.dir=%s\n' "$SKILL_TARGET"
            printf 'skill.0.version=%s\n' "$SKILL_VERSION"
            printf 'skill.0.commit=%s\n' "$SKILL_COMMIT"
            printf 'skill.0.tree_sha256=%s\n' "$(tree_hash "$SKILL_TARGET")"
        fi
    } >"$INSTALL_DIR/$PRODUCT.installed"
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

skill_root_for_harness() {
    case "$1" in
        codex) printf '%s\n' "${CODEX_HOME:-$HOME/.codex}/skills" ;;
        claude|claude-code) printf '%s\n' "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/skills" ;;
        universal) printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/agents/skills" ;;
        hermes) printf '%s\n' "${HERMES_HOME:-$HOME/.hermes}/skills" ;;
        *) die "unknown harness '$1'; use --skill-dir DIR for a custom location" ;;
    esac
}

# ---------------------------------------------------------------- uninstall

if [ "$UNINSTALL" = "1" ]; then
    MANIFEST="$INSTALL_DIR/$PRODUCT.installed"
    BINARY="$INSTALL_DIR/$ASSET_BINARY"
    [ -f "$BINARY" ] || die "no $PRODUCT binary at $BINARY"
    if [ -f "$MANIFEST" ] && [ "$FORCE" != "1" ]; then
        _recorded="$(manifest_get "$MANIFEST" binary_sha256)"
        if [ -n "$_recorded" ] && [ "$(hash_file "$BINARY")" != "$_recorded" ]; then
            die "$BINARY differs from the recorded install; pass --force to remove it anyway"
        fi
    fi
    if [ "$DRY_RUN" = "1" ]; then
        info "dry-run: would remove $BINARY"
    else
        rm -f "$BINARY" "$INSTALL_DIR/$PRODUCT.previous"
        info "removed $BINARY"
    fi
    if [ "$REMOVE_SKILL" = "1" ] && [ -f "$MANIFEST" ]; then
        _index=0
        while :; do
            _dir="$(manifest_get "$MANIFEST" "skill.$_index.dir")"
            [ -n "$_dir" ] || break
            _recorded_tree="$(manifest_get "$MANIFEST" "skill.$_index.tree_sha256")"
            if [ ! -d "$_dir" ]; then
                info "skill directory is already gone: $_dir"
            elif [ "$FORCE" = "1" ] || [ "$(tree_hash "$_dir")" = "$_recorded_tree" ]; then
                if [ "$DRY_RUN" = "1" ]; then info "dry-run: would remove $_dir"; else rm -rf "$_dir"; info "removed skill $_dir"; fi
            else
                info "keeping modified skill directory (pass --force to remove): $_dir"
            fi
            _index=$((_index + 1))
        done
    elif [ "$REMOVE_SKILL" = "1" ]; then
        info "no install manifest: not removing any skill directory"
    fi
    [ "$DRY_RUN" = "1" ] || rm -f "$MANIFEST"
    if [ "$PURGE_STATE" = "1" ]; then
        _state="$(state_dir)"
        if [ -d "$_state" ]; then
            if [ "$DRY_RUN" = "1" ]; then info "dry-run: would remove state $_state"; else rm -rf "$_state"; info "removed state $_state"; fi
        else
            info "state directory not present: $_state"
        fi
    fi
    info "uninstall complete"
    exit 0
fi

# ---------------------------------------------------------------- install

[ -n "$VERSION" ] || die "--version is required"
if [ -n "$SKILL_HARNESS" ] && [ -n "$SKILL_DIR" ]; then
    die "use either --skill-harness or --skill-dir, not both"
fi
if [ "$NO_SKILL" = "1" ] && { [ -n "$SKILL_HARNESS" ] || [ -n "$SKILL_DIR" ]; }; then
    die "use either --no-skill or a skill target, not both"
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/mc-agent-install.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT INT TERM
ARCHIVE_NAME="$PRODUCT-$VERSION-$GOOS-$GOARCH.tar.gz"

if [ -n "$ARCHIVE" ]; then
    [ -f "$ARCHIVE" ] || die "archive not found: $ARCHIVE"
    [ "$DRY_RUN" = "1" ] || cp "$ARCHIVE" "$WORK/$ARCHIVE_NAME"
    SOURCE_DESCRIPTION="$ARCHIVE"
else
    source_asset checksums.txt "$WORK/checksums.txt"
    source_asset "$ARCHIVE_NAME" "$WORK/$ARCHIVE_NAME"
    SOURCE_DESCRIPTION="$BASE_URL/$ARCHIVE_NAME"
fi
if [ "$ARCHIVE" = "" ]; then
    verify_checksum "$WORK/$ARCHIVE_NAME" "$ARCHIVE_NAME"
elif [ -n "$EXPECTED_SHA" ]; then
    [ "$DRY_RUN" = "1" ] || verify_checksum "$WORK/$ARCHIVE_NAME" "$ARCHIVE_NAME"
fi

mkdir -p "$INSTALL_DIR"
EXISTING_BINARY="$INSTALL_DIR/$ASSET_BINARY"
EXISTING_MANIFEST="$INSTALL_DIR/$PRODUCT.installed"
if [ -f "$EXISTING_BINARY" ] && [ ! -f "$EXISTING_MANIFEST" ] && [ "$FORCE" != "1" ]; then
    die "$EXISTING_BINARY exists without an install manifest; pass --force to replace it"
fi

if [ "$DRY_RUN" != "1" ]; then
    mkdir -p "$WORK/extract"
    tar -xzf "$WORK/$ARCHIVE_NAME" -C "$WORK/extract"
    [ -f "$WORK/extract/$ASSET_BINARY" ] || die "archive does not contain $ASSET_BINARY"
    if [ -f "$EXISTING_BINARY" ]; then
        cp "$EXISTING_BINARY" "$INSTALL_DIR/$PRODUCT.previous"
    fi
    chmod 755 "$WORK/extract/$ASSET_BINARY"
    mv -f "$WORK/extract/$ASSET_BINARY" "$EXISTING_BINARY"
else
    info "dry-run: would install $ASSET_BINARY to $INSTALL_DIR"
fi
NEW_SHA="$(hash_file "$EXISTING_BINARY" 2>/dev/null || printf 'dry-run')"

info "installed $PRODUCT $VERSION ($PLATFORM) to $INSTALL_DIR/$ASSET_BINARY"

# ---------------------------------------------------------------- skill

SKILL_TARGET=""
SKILL_VERSION=""
SKILL_COMMIT=""
if [ "$NO_SKILL" != "1" ]; then
    if [ -n "$SKILL_DIR" ]; then
        SKILL_ROOT="$SKILL_DIR"
    elif [ -n "$SKILL_HARNESS" ]; then
        SKILL_ROOT="$(skill_root_for_harness "$SKILL_HARNESS")"
    else
        SKILL_ROOT=""
    fi
    if [ -z "$SKILL_ROOT" ]; then
        info "no skill target given; pass --skill-harness NAME or --skill-dir DIR to install the Toolkit Skill"
    else
        if [ "$DRY_RUN" != "1" ]; then
            source_asset skill-pin.json "$WORK/skill-pin.json"
            SKILL_ASSET="$(sed -n 's/.*"asset"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$WORK/skill-pin.json" | head -n 1)"
            SKILL_COMMIT="$(sed -n 's/.*"commit"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$WORK/skill-pin.json" | head -n 1)"
            [ -n "$SKILL_ASSET" ] || die "skill-pin.json does not name a skill asset"
            source_asset "$SKILL_ASSET" "$WORK/$SKILL_ASSET"
            verify_checksum "$WORK/$SKILL_ASSET" "$SKILL_ASSET"
            mkdir -p "$WORK/skill"
            tar -xzf "$WORK/$SKILL_ASSET" -C "$WORK/skill"
            [ -f "$WORK/skill/minecraft-toolkit/SKILL.md" ] || die "skill bundle is missing minecraft-toolkit/SKILL.md"
            SKILL_TARGET="$SKILL_ROOT/minecraft-toolkit"
            SKILL_VERSION="$(tr -d '\r"' <"$WORK/skill/minecraft-toolkit/SKILL.md" | awk '/^[[:space:]]*version:[[:space:]]*[0-9]/{sub(/^[[:space:]]*version:[[:space:]]*/,""); print; exit}')"
            if [ -d "$SKILL_TARGET" ]; then
                [ "$UPDATE_SKILL" = "1" ] || die "$SKILL_TARGET already exists; pass --update-skill to replace it (a backup is kept)"
                _backup="$SKILL_TARGET.backup-$(date -u +%Y%m%d%H%M%S)"
                cp -R "$SKILL_TARGET" "$_backup"
                info "kept a backup at $_backup"
                rm -rf "$SKILL_TARGET"
            fi
            mkdir -p "$SKILL_ROOT"
            cp -R "$WORK/skill/minecraft-toolkit" "$SKILL_TARGET"
            info "installed skill $SKILL_VERSION to $SKILL_TARGET"
        fi
    fi
fi

if [ "$DRY_RUN" != "1" ]; then
    write_manifest "$VERSION" "$NEW_SHA"
fi

# ---------------------------------------------------------------- PATH

if [ "$ADD_TO_PATH" = "1" ]; then
    PROFILE="$HOME/.profile"
    MARKER="# mc-agent installer"
    if [ "$DRY_RUN" = "1" ]; then
        info "dry-run: would add $INSTALL_DIR to PATH in $PROFILE"
    elif [ -f "$PROFILE" ] && grep -qF "$MARKER" "$PROFILE" 2>/dev/null; then
        info "PATH entry already present in $PROFILE"
    else
        {
            printf '\n%s\n' "$MARKER"
            printf '%s\n' "export PATH=\"$INSTALL_DIR:\$PATH\""
        } >>"$PROFILE"
        info "added $INSTALL_DIR to PATH in $PROFILE (open a new shell or run: . \"$PROFILE\")"
    fi
fi

info "next: $INSTALL_DIR/$ASSET_BINARY version"
info "      $INSTALL_DIR/$ASSET_BINARY daemon start   # then: ... doctor"
