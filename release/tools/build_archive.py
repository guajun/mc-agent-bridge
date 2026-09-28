#!/usr/bin/env python3
"""Package one built Go binary into a release archive.

The archive contains the Go binary only - no Python, no MCP server, no skill
copy and no helper scripts. Assets, checksums, version metadata and the skill
bundle are produced by ``finalize_assets.py``.

Example:

    python release/tools/build_archive.py \
        --binary staging/mc-agent.exe --version 0.5.0 \
        --goos windows --goarch amd64 --out dist
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import sys
import tarfile
import zipfile
from io import BytesIO
from pathlib import Path

FIXED_MTIME = 0  # reproducible archive metadata
BINARY_NAME = {"windows": "mc-agent.exe"}
DEFAULT_BINARY = "mc-agent"


def archive_name(version: str, goos: str, goarch: str, kind: str) -> str:
    suffix = "zip" if kind == "zip" else "tar.gz"
    return f"mc-agent-{version}-{goos}-{goarch}.{suffix}"


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_zip(path: Path, member: str, payload: bytes) -> None:
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        info = zipfile.ZipInfo(member, date_time=(1980, 1, 1, 0, 0, 0))
        info.external_attr = 0o755 << 16
        info.compress_type = zipfile.ZIP_DEFLATED
        archive.writestr(info, payload)


def write_tar_gz(path: Path, member: str, payload: bytes) -> None:
    buffer = BytesIO()
    with tarfile.open(fileobj=buffer, mode="w", format=tarfile.GNU_FORMAT) as archive:
        info = tarfile.TarInfo(member)
        info.size = len(payload)
        info.mode = 0o755
        info.mtime = FIXED_MTIME
        info.uid = 0
        info.gid = 0
        info.uname = ""
        info.gname = ""
        archive.addfile(info, BytesIO(payload))
    with path.open("wb") as handle:
        with gzip.GzipFile(fileobj=handle, mode="wb", mtime=FIXED_MTIME) as compressed:
            compressed.write(buffer.getvalue())


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="path to the built Go binary")
    parser.add_argument("--version", required=True, help="product version, for example 0.5.0")
    parser.add_argument("--goos", required=True, choices=["windows", "linux", "darwin"])
    parser.add_argument("--goarch", required=True)
    parser.add_argument("--out", default="dist", help="output directory")
    args = parser.parse_args()

    binary = Path(args.binary)
    if not binary.is_file():
        parser.error(f"binary not found: {binary}")

    member = BINARY_NAME.get(args.goos, DEFAULT_BINARY)
    payload = binary.read_bytes()
    if len(payload) < 1024 * 1024:
        print(f"warning: {binary} is only {len(payload)} bytes; expected a full Go binary", file=sys.stderr)

    use_zip = args.goos == "windows"
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    target = out / archive_name(args.version, args.goos, args.goarch, "zip" if use_zip else "tar.gz")
    if target.exists():
        target.unlink()
    if use_zip:
        write_zip(target, member, payload)
    else:
        write_tar_gz(target, member, payload)

    result = {
        "asset": target.name,
        "sha256": sha256_file(target),
        "bytes": target.stat().st_size,
        "goos": args.goos,
        "goarch": args.goarch,
        "member": member,
    }
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
