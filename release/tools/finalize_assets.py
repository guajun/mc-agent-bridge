#!/usr/bin/env python3
"""Finalize a staged release: verify archives, add metadata and the skill.

Reads the per-platform archives produced by ``build_archive.py``, checks that
the set exactly matches the advertised supported matrix, then writes:

* ``version.json`` - product, control protocol, compatibility, per-platform
  asset hashes and the pinned skill record;
* ``mc-agent-skill-<skillVersion>.tar.gz`` - the single Skill bundle taken from
  the pinned meta repository commit (never from a working tree copy);
* ``checksums.txt`` - sha256 of every published asset;
* ``skill-pin.json`` - the reviewed meta commit that the bundle was built from.

Example:

    python release/tools/finalize_assets.py \
        --assets dist --version 0.5.0 --tag v0.5.0 \
        --commit "$GITHUB_SHA" --go-version "$(go version)" \
        --skill-dir staging-meta/skills/minecraft-toolkit \
        --skill-pin release/skill-pin.json
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import re
import sys
import tarfile
import zipfile
from datetime import datetime, timezone
from io import BytesIO
from pathlib import Path

FIXED_MTIME = 0


def sha256_bytes(payload: bytes) -> str:
    return hashlib.sha256(payload).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def asset_name(version: str, goos: str, goarch: str, kind: str) -> str:
    suffix = "zip" if kind == "zip" else "tar.gz"
    return f"mc-agent-{version}-{goos}-{goarch}.{suffix}"


def inspect_archive(path: Path) -> str:
    """Return the single member name, failing on anything unexpected."""
    if path.suffix == ".zip":
        with zipfile.ZipFile(path) as archive:
            names = [info.filename for info in archive.infolist() if not info.is_dir()]
    else:
        with tarfile.open(path) as archive:
            names = [member.name for member in archive.getmembers() if member.isfile()]
    if len(names) != 1:
        raise SystemExit(f"{path.name}: expected exactly one file, found {names}")
    return names[0]


def read_skill_version(skill_md: Path) -> str:
    text = skill_md.read_text(encoding="utf-8")
    match = re.search(r'^\s*version:\s*"?([0-9][^"\s]*)"?\s*$', text, re.MULTILINE)
    if not match:
        raise SystemExit(f"cannot find a version in {skill_md}")
    return match.group(1)


def build_skill_bundle(skill_dir: Path, version: str, output: Path) -> tuple[str, str]:
    """Package the skill directory as minecraft-toolkit/... deterministically."""
    members: list[tuple[str, bytes]] = []
    for path in sorted(skill_dir.rglob("*")):
        if path.is_dir() or path.name == ".DS_Store":
            continue
        relative = path.relative_to(skill_dir.parent).as_posix()
        members.append((relative, path.read_bytes()))
    if not members:
        raise SystemExit(f"skill directory is empty: {skill_dir}")
    if not any(name == "minecraft-toolkit/SKILL.md" for name, _ in members):
        raise SystemExit(f"{skill_dir} must contain SKILL.md")

    buffer = BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.GNU_FORMAT) as archive:
        for name, payload in members:
            info = tarfile.TarInfo(name)
            info.size = len(payload)
            info.mode = 0o644
            info.mtime = FIXED_MTIME
            info.uid = 0
            info.gid = 0
            info.uname = ""
            info.gname = ""
            archive.addfile(info, BytesIO(payload))
    archive_name = f"mc-agent-skill-{version}.tar.gz"
    target = output / archive_name
    with target.open("wb") as handle:
        with gzip.GzipFile(fileobj=handle, mode="wb", mtime=FIXED_MTIME) as compressed:
            compressed.write(buffer.getvalue())
    return archive_name, sha256_file(target)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--assets", default="dist", help="directory with staged archives")
    parser.add_argument("--out", default="", help="output directory (defaults to --assets)")
    parser.add_argument("--version", required=True)
    parser.add_argument("--tag", default="", help="release tag; must equal v<version>")
    parser.add_argument("--commit", default="")
    parser.add_argument("--go-version", default="")
    parser.add_argument("--build-time", default="")
    parser.add_argument("--compat", default="release/compatibility.json")
    parser.add_argument("--repo", default="guajun/mc-agent-bridge")
    parser.add_argument("--skill-repo", default="guajun/mc-agent")
    parser.add_argument("--skill-dir", required=True, help="path to the pinned skills/minecraft-toolkit tree")
    parser.add_argument("--skill-pin", default="release/skill-pin.json")
    args = parser.parse_args()

    assets = Path(args.assets).resolve()
    output = Path(args.out).resolve() if args.out else assets
    output.mkdir(parents=True, exist_ok=True)

    compatibility = json.loads(Path(args.compat).read_text(encoding="utf-8"))
    if compatibility["productVersion"] != args.version:
        raise SystemExit(
            f"compatibility.json version {compatibility['productVersion']} != --version {args.version}"
        )
    if args.tag and args.tag != f"v{args.version}":
        raise SystemExit(f"tag {args.tag} does not match product version {args.version}")

    platforms = []
    expected_files = set()
    for entry in compatibility["supportedPlatforms"]:
        name = asset_name(args.version, entry["goos"], entry["goarch"], entry["archive"])
        expected_files.add(name)
        path = assets / name
        if not path.is_file():
            raise SystemExit(f"missing archive for {entry['goos']}/{entry['goarch']}: {name}")
        member = inspect_archive(path)
        wanted = "mc-agent.exe" if entry["goos"] == "windows" else "mc-agent"
        if member != wanted:
            raise SystemExit(f"{name}: expected member {wanted!r}, found {member!r}")
        platforms.append(
            {
                "os": entry["goos"],
                "arch": entry["goarch"],
                "label": entry["label"],
                "asset": name,
                "bytes": path.stat().st_size,
                "sha256": sha256_file(path),
            }
        )

    staged = {
        candidate.name
        for candidate in assets.glob(f"mc-agent-{args.version}-*")
        if candidate.is_file()
    }
    unexpected = staged - expected_files
    if unexpected:
        raise SystemExit(
            "staged archives for unadvertised platforms: " + ", ".join(sorted(unexpected))
        )

    pin = json.loads(Path(args.skill_pin).read_text(encoding="utf-8"))
    skill_version = read_skill_version(Path(args.skill_dir) / "SKILL.md")
    if pin.get("skillVersion") != skill_version:
        raise SystemExit(
            f"skill-pin.json version {pin.get('skillVersion')} != SKILL.md version {skill_version}"
        )
    skill_asset, skill_sha256 = build_skill_bundle(Path(args.skill_dir), skill_version, output)

    if args.build_time:
        build_time = args.build_time
    else:
        build_time = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    version_info = {
        "product": compatibility["product"],
        "version": args.version,
        "controlProtocol": compatibility["controlProtocol"],
        "modMinVersion": compatibility["modMinVersion"],
        "compatibility": compatibility["compatibility"],
        "supportedPlatforms": platforms,
        "build": {
            "tag": args.tag or f"v{args.version}",
            "commit": args.commit,
            "go": args.go_version,
            "builtAt": build_time,
        },
        "skill": {
            "repository": args.skill_repo,
            "path": pin["path"],
            "commit": pin["commit"],
            "version": skill_version,
            "asset": skill_asset,
            "sha256": skill_sha256,
        },
        "unsupported": compatibility.get("unsupported", {}),
        "runtimeRequirements": {
            "python": False,
            "go": False,
            "mcp": False,
        },
    }
    version_path = output / "version.json"
    version_path.write_text(json.dumps(version_info, indent=2, sort_keys=True) + "\n",
                            encoding="utf-8", newline="\n")

    published = [platform["asset"] for platform in platforms] + [skill_asset, "version.json"]
    checksums = output / "checksums.txt"
    lines = []
    for name in sorted(published):
        lines.append(f"{sha256_file(output / name)}  {name}")
    checksums.write_text("\n".join(lines) + "\n", encoding="utf-8", newline="\n")

    skill_pin = {
        "repository": args.skill_repo,
        "path": pin["path"],
        "commit": pin["commit"],
        "skillVersion": skill_version,
        "asset": skill_asset,
        "sha256": skill_sha256,
        "bundleSource": (
            f"https://github.com/{args.skill_repo}/archive/{pin['commit']}.tar.gz"
        ),
    }
    (output / "skill-pin.json").write_text(
        json.dumps(skill_pin, indent=2, sort_keys=True) + "\n", encoding="utf-8", newline="\n"
    )

    summary = {
        "version": args.version,
        "assets": sorted(published),
        "checksums": str(checksums),
        "versionInfo": str(version_path),
        "skill": skill_pin,
    }
    print(json.dumps(summary, indent=2, sort_keys=True))
    print(f"finalized {len(platforms)} platform archives in {output}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
