"""Forking a live world: parse snapshots, hash the tick order, copy world files.

Half of a fork cannot be read off disk: a save file has blocks and entity NBT
but no *tick order*, and the order decides the result of anything computed
entity by entity (see ``docs/protocol-snapshot.md`` in the mc-agent meta
repository). The mod owns that half - ``entities.jsonl`` plus ``meta.json``
under ``<mcagent.dir>/snapshots/<name>/``. The other half is the world
directory, and copying it is plain file plumbing.

Everything here is a pure function of the filesystem, so the daemon stays a thin
translator and the interesting behaviour is testable without a game.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
import shutil
from collections.abc import Iterable
from typing import Any

#: The snapshot protocol this bridge understands.
PROTOCOL_VERSION = 1

#: Names a lab instance must not inherit from the live world: the lock the live
#: game holds, the per-instance bookkeeping (player progress, stats,
#: advancements) and logs. Everything else *is* copied - a lab that loads the
#: forked world needs its `dimensions/`, `data/`, `datapacks/` and level
#: metadata too, so the skip list is the filter rather than an include list.
#: Player data is spelled `playerdata/` before 1.21 and `players/` in 26.2's
#: world layout; both are skipped.
SKIP_NAMES = frozenset(
    {"session.lock", "playerdata", "players", "stats", "advancements", "logs"}
)


class SnapshotError(ValueError):
    """A snapshot directory is missing, unreadable, or not protocol v1.

    Subclasses :class:`ValueError` so a caller at the CLI or the local API gets
    a plain message instead of a traceback.
    """


def read_meta(directory: str | os.PathLike[str]) -> dict[str, Any]:
    """Read ``<directory>/meta.json``, the description of one snapshot."""
    path = os.path.join(os.fspath(directory), "meta.json")
    try:
        with open(path, encoding="utf-8") as handle:
            meta = json.load(handle)
    except FileNotFoundError as error:
        raise SnapshotError(
            f"{path} is not a snapshot: no meta.json (expected the mod's SNAPSHOT layout)"
        ) from error
    except OSError as error:
        raise SnapshotError(f"cannot read {path}: {error}") from error
    except UnicodeDecodeError as error:
        raise SnapshotError(f"{path} is not valid UTF-8: {error}") from error
    except json.JSONDecodeError as error:
        raise SnapshotError(f"{path} is not valid JSON: {error}") from error
    if not isinstance(meta, dict):
        raise SnapshotError(f"{path} must hold a JSON object, not {type(meta).__name__}")
    protocol = meta.get("protocol")
    if protocol is not None and protocol != PROTOCOL_VERSION:
        raise SnapshotError(
            f"{path} declares snapshot protocol {protocol!r}; this bridge speaks "
            f"protocol {PROTOCOL_VERSION}"
        )
    return meta


def read_entities(directory: str | os.PathLike[str]) -> list[dict[str, Any]]:
    """Read ``<directory>/entities.jsonl``: one entity per line, in tick order."""
    path = os.path.join(os.fspath(directory), "entities.jsonl")
    try:
        handle = open(path, encoding="utf-8")
    except FileNotFoundError as error:
        raise SnapshotError(
            f"{path} is not a snapshot: no entities.jsonl (expected the mod's SNAPSHOT layout)"
        ) from error
    except OSError as error:
        raise SnapshotError(f"cannot read {path}: {error}") from error

    entities: list[dict[str, Any]] = []
    try:
        with handle:
            for number, line in enumerate(handle, start=1):
                text = line.strip()
                if not text:
                    continue
                try:
                    record = json.loads(text)
                except json.JSONDecodeError as error:
                    raise SnapshotError(
                        f"{path} line {number} is not valid JSON: {error}"
                    ) from error
                if not isinstance(record, dict):
                    raise SnapshotError(
                        f"{path} line {number} must be a JSON object, not {type(record).__name__}"
                    )
                entities.append(record)
    except UnicodeDecodeError as error:
        raise SnapshotError(f"{path} is not valid UTF-8: {error}") from error
    return entities


def read_snapshot(directory: str | os.PathLike[str]) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    """Read one snapshot: ``(meta, entities)``, entities in recorded tick order.

    A truncated ``entities.jsonl`` is the failure worth catching: a restore that
    silently summons half a world looks like a working restore.
    """
    meta = read_meta(directory)
    entities = read_entities(directory)
    reported = meta.get("entities")
    if isinstance(reported, int) and reported != len(entities):
        raise SnapshotError(
            f"{os.fspath(directory)}: meta.json says {reported} entities but "
            f"entities.jsonl holds {len(entities)}"
        )
    return meta, entities


def order_hash(uuids: Iterable[Any]) -> str:
    """The tick order as one value: ``sha256(":".join(uuids))[:16]``.

    The mod computes the same digest over the same UUID strings, so a re-snapshot
    that reports this value has reproduced the order the fork was taken from.
    """
    joined = ":".join(str(uuid) for uuid in uuids)
    return hashlib.sha256(joined.encode("utf-8")).hexdigest()[:16]


def snapshot_line(radius: Any = None, name: str | None = None) -> str:
    """The mod request line for one ``SNAPSHOT``.

    ``SNAPSHOT [radius] [name]`` is positional, so a name without a radius is
    sent with an explicit ``0`` (which protocol v1 defines as "every entity the
    level ticks"): the mod must never have to guess whether the token after
    ``SNAPSHOT`` is a radius or a name.
    """
    line = "SNAPSHOT"
    has_radius = radius is not None and radius != ""
    if has_radius:
        try:
            line += f" {float(radius)}"
        except (TypeError, ValueError) as error:
            raise ValueError(f"snapshot radius must be a number, got {radius!r}") from error
    if name:
        if not has_radius:
            line += " 0"
        line += f" {name}"
    return line


def order_check_name(target: str | None = None) -> str:
    """Name for the throwaway snapshot ``mc_order`` takes to compare against."""
    base = f"order-{target}" if target else "order-check"
    safe = re.sub(r"[^A-Za-z0-9_-]+", "-", base)
    safe = re.sub(r"-{2,}", "-", safe).strip("-")
    return (safe or "order-check")[:64]


def copy_world(
    source_dir: str | os.PathLike[str], target_dir: str | os.PathLike[str]
) -> dict[str, Any]:
    """Copy the world files a lab instance needs, and nothing it must not have.

    Returns ``{"files": n, "bytes": n, "skipped": [...]}``: how many files were
    copied, how many bytes they hold, and the relative paths left behind (see
    :data:`SKIP_NAMES`). The counts describe the copy, so a manifest says what
    actually landed in the fork directory.
    """
    source = os.path.abspath(os.fspath(source_dir))
    target = os.path.abspath(os.fspath(target_dir))
    if not os.path.isdir(source):
        raise SnapshotError(f"world directory does not exist: {source}")
    if target == source or target.startswith(source + os.sep):
        raise SnapshotError(f"refusing to copy the world into itself: {source} -> {target}")

    os.makedirs(target, exist_ok=True)
    files = 0
    total = 0
    skipped: list[str] = []

    for root, dirs, names in os.walk(source):
        relative_root = os.path.relpath(root, source)
        dirs[:] = sorted(name for name in dirs if not _skip(name, relative_root, skipped))
        for name in sorted(names):
            if _skip(name, relative_root, skipped):
                continue
            path = os.path.join(root, name)
            if os.path.islink(path):
                # A link would be copied as a link by some tools and followed by
                # others; neither is what a lab wants from a live world.
                skipped.append(_relative(relative_root, name))
                continue
            destination = os.path.join(target, relative_root, name)
            os.makedirs(os.path.dirname(destination), exist_ok=True)
            shutil.copy2(path, destination)
            files += 1
            total += os.path.getsize(path)

    return {"files": files, "bytes": total, "skipped": sorted(skipped)}


def summon_commands(entities: Iterable[dict[str, Any]]) -> list[str]:
    """One ``/summon`` per entity, in file order: vanilla rebuilds the tick list
    in the order the entities appear, so the re-snapshot's ``orderHash`` matches.

    An entity queued for a command but missing its type, position or NBT is
    reported instead of being summoned wrong, because a restore that drops
    records still looks successful.
    """
    commands: list[str] = []
    for index, entity in enumerate(entities):
        if not isinstance(entity, dict):
            raise SnapshotError(f"entity {index} is not a JSON object")
        where = entity.get("uuid") or f"record {index}"
        kind = entity.get("type")
        position = entity.get("pos")
        nbt = entity.get("nbt")
        if not isinstance(kind, str) or not kind:
            raise SnapshotError(f"entity {where} has no type, so it cannot be summoned")
        if not isinstance(position, (list, tuple)) or len(position) != 3:
            raise SnapshotError(f"entity {where} has no [x, y, z] position")
        if not isinstance(nbt, str) or not nbt.strip():
            raise SnapshotError(f"entity {where} has no NBT, so it cannot be summoned faithfully")
        x, y, z = (_coordinate(value, where) for value in position)
        commands.append(f"/summon {kind} {x} {y} {z} {nbt}")
    return commands


def _coordinate(value: Any, where: Any) -> str:
    try:
        number = float(value)
    except (TypeError, ValueError) as error:
        raise SnapshotError(f"entity {where} has a non-numeric position: {value!r}") from error
    if not math.isfinite(number):
        raise SnapshotError(f"entity {where} has a non-finite position: {value!r}")
    # repr() of a float always carries a fraction or an exponent ("103.5",
    # "56.0", "1e-05"), which is what the mod's own example shows and what the
    # command parser expects; it also never loses precision on the way out.
    return repr(number)


def _skip(name: str, relative_root: str, skipped: list[str]) -> bool:
    if name.casefold() not in SKIP_NAMES:
        return False
    skipped.append(_relative(relative_root, name))
    return True


def _relative(relative_root: str, name: str) -> str:
    """Manifest paths read the same on every platform."""
    if relative_root in ("", "."):
        return name
    return f"{relative_root.replace(os.sep, '/')}/{name}"
