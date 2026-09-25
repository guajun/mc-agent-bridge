"""Guarded restore: validate a fork, plan the summons, compare it with the result.

A restore reports two different things that are easy to confuse: *commands
issued* and *state faithfully restored*. The daemon's ``restore`` handler issues
the commands; this module supplies the pure half - what makes a snapshot safe to
consume, which records can become ``/summon`` lines, and whether a fresh
snapshot of the destination really reproduces the recording.

The definitions here mirror the consumer half in the meta repository's
``tools/fork_verify.py`` (``orderHash`` = ``sha256(":".join(uuids))[:16]``,
``restorable:false`` passengers are skipped, validation issue codes) so the two
tools do not drift into different contracts. Nothing here talks to a game: the
daemon composes these functions around its mod client, and the tests exercise
them without one.

The comparison is deliberately not hash-only. ``orderHash`` proves the tick
order; it says nothing about an inventory that changed while the UUID order did
not. :func:`compare_snapshots` compares UUID order, per-type counts, positions,
velocities and the full NBT string (with the ``Items``/``components`` fragment
quoted in the report), and its ``ok`` verdict requires all of them.
"""

from __future__ import annotations

import json
import math
import os
import re
from collections import Counter
from collections.abc import Iterable, Sequence
from dataclasses import dataclass
from typing import Any

from . import fork

#: Keys every restorable record must carry (fork_verify's REQUIRED_KEYS).
REQUIRED_KEYS = ("order", "uuid", "type", "pos", "nbt")

#: The wire format of ``meta.json``'s ``orderHash``.
HASH_RE = re.compile(r"[0-9a-f]{16}")

#: Default tolerances for "the same state" after a restore. A frozen restore
#: reproduces the recorded doubles exactly; the slack absorbs float rendering
#: through SNBT without accepting a moved entity.
POS_TOLERANCE = 0.01
VEL_TOLERANCE = 0.01

#: Two entities of the same type closer than this are treated as a collision:
#: a leftover copy that would be duplicated by the restore. Stacked chest
#: minecarts sit ~1.0 block apart, so the default only fires on a real overlap.
COLLISION_RADIUS = 0.75


class RestoreError(ValueError):
    """A guarded restore was refused before it could mutate the world.

    Subclasses :class:`ValueError` so the local API, CLI and MCP layers turn it
    into the same plain error response they already return for bad parameters.
    """


@dataclass(frozen=True)
class Issue:
    """One validation problem, with a stable code shared with fork_verify."""

    code: str
    message: str
    line: int | None = None
    #: Blocking issues refuse the restore before any command is sent; the rest
    #: are warnings (the file's line order is what drives a restore).
    blocking: bool = False

    def as_dict(self) -> dict[str, Any]:
        return {"code": self.code, "message": self.message, "line": self.line}


# ------------------------------------------------------------------- validation


def _uuid_order(entities: Iterable[dict[str, Any]]) -> list[str]:
    return [str(entity.get("uuid") or "") for entity in entities]


def validate_snapshot(
    meta: dict[str, Any], entities: Sequence[dict[str, Any]]
) -> list[Issue]:
    """Everything wrong with a snapshot, in the same shape fork_verify reports.

    Meta problems, a count or hash that does not match the file, duplicated
    UUIDs and records that cannot become a faithful ``/summon`` are blocking;
    order-field inconsistencies are warnings because the file order drives the
    restore. ``fork.read_snapshot`` already refuses missing files and a count
    mismatch; this catches the semantic corruption it does not.
    """
    issues: list[Issue] = []
    count = len(entities)

    protocol = meta.get("protocol")
    if protocol is None:
        issues.append(Issue("meta_missing", "meta.json: protocol is missing", blocking=True))
    elif protocol != fork.PROTOCOL_VERSION:
        issues.append(
            Issue(
                "meta_protocol",
                f"meta.json: protocol={protocol!r}, expected {fork.PROTOCOL_VERSION}",
                blocking=True,
            )
        )

    declared = meta.get("entities")
    if isinstance(declared, bool) or not isinstance(declared, int):
        issues.append(
            Issue("meta_missing", "meta.json: entities is missing or not an integer", blocking=True)
        )
    elif declared != count:
        issues.append(
            Issue(
                "entity_count",
                f"meta.json: entities={declared} but entities.jsonl holds {count}",
                blocking=True,
            )
        )

    computed = fork.order_hash(_uuid_order(entities))
    recorded = meta.get("orderHash")
    if not isinstance(recorded, str) or not HASH_RE.fullmatch(recorded):
        issues.append(
            Issue(
                "order_hash_format",
                f"meta.json: orderHash={recorded!r} is not 16 lowercase hex characters",
                blocking=True,
            )
        )
    elif recorded != computed:
        issues.append(
            Issue(
                "order_hash",
                f"meta.json: orderHash={recorded} but the file hashes to {computed}",
                blocking=True,
            )
        )

    order_values: list[int] = []
    uuid_counts: Counter[str] = Counter()
    for index, entity in enumerate(entities):
        if not isinstance(entity, dict):
            issues.append(Issue("bad_record", f"record {index} is not a JSON object", blocking=True))
            continue
        line = index + 1
        absent = [key for key in REQUIRED_KEYS if key not in entity]
        if absent:
            issues.append(
                Issue(
                    "missing_field",
                    f"record {line}: missing required field(s): {', '.join(absent)}",
                    line,
                    blocking=True,
                )
            )
        uuid = entity.get("uuid")
        if not isinstance(uuid, str) or not uuid:
            issues.append(
                Issue(
                    "bad_field",
                    f"record {line}: uuid must be a non-empty string, got {uuid!r}",
                    line,
                    blocking=True,
                )
            )
        else:
            uuid_counts[uuid] += 1
        entity_type = entity.get("type")
        if not isinstance(entity_type, str) or not entity_type:
            issues.append(
                Issue(
                    "bad_field",
                    f"record {line}: type must be a non-empty string, got {entity_type!r}",
                    line,
                    blocking=True,
                )
            )
        if _vec(entity.get("pos")) is None:
            issues.append(
                Issue(
                    "bad_field",
                    f"record {line}: pos must be three finite numbers, got {entity.get('pos')!r}",
                    line,
                    blocking=True,
                )
            )
        if "vel" in entity and _vec(entity.get("vel")) is None:
            issues.append(
                Issue("bad_field", f"record {line}: vel must be three finite numbers", line)
            )
        nbt = entity.get("nbt")
        if not isinstance(nbt, str) or not nbt.strip():
            issues.append(
                Issue(
                    "bad_field",
                    f"record {line}: no NBT, so the entity cannot be summoned faithfully",
                    line,
                    blocking=True,
                )
            )
        order = entity.get("order")
        if isinstance(order, bool) or not isinstance(order, int):
            issues.append(
                Issue("order_type", f"record {line}: order must be an integer, got {order!r}", line)
            )
            continue
        order_values.append(order)
        if order != index:
            issues.append(
                Issue(
                    "order_mismatch",
                    f"record {line}: order={order}, expected {index} "
                    "(the file's line order is the tick order)",
                    line,
                )
            )

    duplicated_orders = sorted(value for value, times in Counter(order_values).items() if times > 1)
    if duplicated_orders:
        issues.append(Issue("order_duplicate", f"order values repeat: {_short(duplicated_orders)}"))
    missing_orders = sorted(set(range(count)) - set(order_values))
    if missing_orders:
        issues.append(
            Issue(
                "order_missing",
                f"order values unused: {_short(missing_orders)} (a faithful file uses 0..{count - 1})",
            )
        )
    repeated = sorted(uuid for uuid, times in uuid_counts.items() if times > 1)
    if repeated:
        issues.append(
            Issue(
                "uuid_duplicate",
                f"{len(repeated)} uuid(s) appear more than once: {_short(repeated)}",
                blocking=True,
            )
        )
    return issues


def blocking_issues(issues: Sequence[Issue]) -> list[Issue]:
    return [issue for issue in issues if issue.blocking]


def describe_blockers(directory: str | os.PathLike[str], issues: Sequence[Issue]) -> str:
    """One actionable sentence plus the first few issues, for the refusal error."""
    shown = "; ".join(f"[{issue.code}] {issue.message}" for issue in issues[:8])
    more = f" (+{len(issues) - 8} more)" if len(issues) > 8 else ""
    return (
        f"{os.fspath(directory)} cannot be restored: {len(issues)} blocking issue(s). "
        f"Nothing was sent to the game. {shown}{more}"
    )


def _short(values: Sequence[Any], limit: int = 5) -> str:
    shown = ", ".join(str(value) for value in values[:limit])
    return f"{shown}, +{len(values) - limit} more" if len(values) > limit else shown


def _vec(value: Any) -> tuple[float, float, float] | None:
    if not isinstance(value, (list, tuple)) or len(value) != 3:
        return None
    numbers: list[float] = []
    for item in value:
        if isinstance(item, bool) or not isinstance(item, (int, float)):
            return None
        number = float(item)
        if not math.isfinite(number):
            return None
        numbers.append(number)
    return (numbers[0], numbers[1], numbers[2])


# ----------------------------------------------------------------------- planning


def is_restorable(entity: dict[str, Any]) -> bool:
    """Whether this record can be summoned on its own.

    A passenger's NBT is nested inside its vehicle's, so summoning the vehicle
    already brings the passenger back; the mod marks those lines
    ``"restorable": false`` and a restore must skip them. Records without the
    flag (protocol drift, hand-written fixtures) are treated as restorable.
    """
    flag = entity.get("restorable")
    return True if flag is None else bool(flag)


def restore_plan(entities: Sequence[dict[str, Any]]) -> dict[str, Any]:
    """Turn a validated recording into summon commands, in recorded order.

    ``items`` pairs each command with the source index and uuid so a command
    failure can be reported against the entity it was meant to restore.
    Passengers are skipped and reported in ``skipped``, never silently dropped.
    A record that cannot become a faithful summon raises
    :class:`~mc_agent_bridge.fork.SnapshotError` before anything is sent.
    """
    commands: list[str] = []
    items: list[dict[str, Any]] = []
    skipped: list[dict[str, Any]] = []
    for index, entity in enumerate(entities):
        uuid = str(entity.get("uuid") or "")
        if not is_restorable(entity):
            vehicle = entity.get("vehicle")
            reason = "passenger of %s" % vehicle if vehicle else "restorable=false"
            skipped.append({"index": index, "uuid": uuid, "reason": reason})
            continue
        command = fork.summon_commands([entity])[0]
        items.append({"index": index, "uuid": uuid, "command": command})
        commands.append(command)
    return {"commands": commands, "items": items, "skipped": skipped}


# --------------------------------------------------------------- destination boxes


def bounding_box(
    entities: Sequence[dict[str, Any]], margin: float = 16.0
) -> tuple[float, float, float, float, float, float] | None:
    """The recorded area in blocks, padded - the box a restore has to load.

    A headless lab has no player, so no chunks are loaded, and a ``/summon`` at
    an unloaded position fails. The box comes from the recorded positions, not
    from a guess. Returns ``(minX, minY, minZ, maxX, maxY, maxZ)``.
    """
    points = [point for point in (_vec(entity.get("pos")) for entity in entities) if point]
    if not points:
        return None
    xs = [point[0] for point in points]
    ys = [point[1] for point in points]
    zs = [point[2] for point in points]
    return (
        min(xs) - margin,
        min(ys) - margin,
        min(zs) - margin,
        max(xs) + margin,
        max(ys) + margin,
        max(zs) + margin,
    )


def forceload_add_command(box: tuple[float, float, float, float, float, float]) -> str:
    """``forceload add`` the box's X/Z footprint (the command has no Y)."""
    return "forceload add %d %d %d %d" % (
        math.floor(box[0]),
        math.floor(box[2]),
        math.ceil(box[3]),
        math.ceil(box[5]),
    )


def forceload_remove_command(box: tuple[float, float, float, float, float, float]) -> str:
    return "forceload remove %d %d %d %d" % (
        math.floor(box[0]),
        math.floor(box[2]),
        math.ceil(box[3]),
        math.ceil(box[5]),
    )


def entities_in_box(
    entities: Sequence[dict[str, Any]], box: tuple[float, float, float, float, float, float]
) -> list[dict[str, Any]]:
    """Entities whose position lies inside the recorded box.

    Used after ``replace_existing`` cleared and flushed: the box is supposed
    to be empty, so anything still here is a leftover the restore would place
    next to (most commonly the item drops a killed chest minecart leaves).
    """
    min_x, min_y, min_z, max_x, max_y, max_z = box
    inside: list[dict[str, Any]] = []
    for entity in entities:
        position = _vec(entity.get("pos"))
        if position is None:
            continue
        if (
            min_x <= position[0] <= max_x
            and min_y <= position[1] <= max_y
            and min_z <= position[2] <= max_z
        ):
            inside.append(
                {
                    "uuid": str(entity.get("uuid") or ""),
                    "type": str(entity.get("type") or ""),
                    "pos": [round(value, 3) for value in position],
                }
            )
    return inside


def clear_box_command(box: tuple[float, float, float, float, float, float]) -> str:
    """``/kill`` every non-player entity inside the recorded box.

    Selector coordinates are floors with a one-block pad, and the ``dx/dy/dz``
    select the volume inclusively enough that an entity exactly on the boundary
    is still cleared. Players are preserved: they are not restorable and the
    restore must not touch them.
    """
    x = math.floor(box[0])
    y = math.floor(box[1])
    z = math.floor(box[2])
    dx = math.ceil(box[3]) - x + 1
    dy = math.ceil(box[4]) - y + 1
    dz = math.ceil(box[5]) - z + 1
    return f"kill @e[type=!player,x={x},y={y},z={z},dx={dx},dy={dy},dz={dz}]"


# ------------------------------------------------------------------- comparison


def _motion(entity: dict[str, Any]) -> tuple[float, float, float] | None:
    velocity = _vec(entity.get("vel"))
    if velocity is not None:
        return velocity
    nbt = entity.get("nbt")
    if isinstance(nbt, str):
        match = re.search(
            r'"?Motion"?:\[(-?[\d.eE+]+)d,(-?[\d.eE+]+)d,(-?[\d.eE+]+)d\]', nbt
        )
        if match:
            try:
                return (float(match.group(1)), float(match.group(2)), float(match.group(3)))
            except ValueError:
                return None
    return None


def _nbt_of(entity: dict[str, Any]) -> str:
    nbt = entity.get("nbt")
    return nbt if isinstance(nbt, str) else ""


def _split_top_level(nbt: str) -> list[tuple[str, str]] | None:
    """Split a ``{...}`` SNBT object into ``(key, raw value)`` at depth 1."""
    text = nbt.strip()
    if len(text) < 2 or not text.startswith("{") or not text.endswith("}"):
        return None
    body = text[1:-1]
    items: list[tuple[str, str]] = []
    index = 0
    while index < len(body):
        while index < len(body) and body[index] in " \t\r\n":
            index += 1
        if index >= len(body):
            break
        if body[index] != '"':
            return None
        end = _skip_value(body, index)
        try:
            key = json.loads(body[index:end])
        except ValueError:
            return None
        cursor = end
        while cursor < len(body) and body[cursor] in " \t\r\n":
            cursor += 1
        if cursor >= len(body) or body[cursor] != ":":
            return None
        cursor += 1
        while cursor < len(body) and body[cursor] in " \t\r\n":
            cursor += 1
        if cursor >= len(body):
            return None
        value_end = _skip_value(body, cursor)
        items.append((str(key), body[cursor:value_end]))
        index = value_end
        while index < len(body) and body[index] in " \t\r\n":
            index += 1
        if index < len(body):
            if body[index] != ",":
                return None
            index += 1
    return items


def _skip_value(text: str, index: int) -> int:
    """Return the index just past the SNBT value starting at ``index``."""
    if index >= len(text):
        return index
    char = text[index]
    if char == '"':
        cursor = index + 1
        while cursor < len(text):
            if text[cursor] == "\\":
                cursor += 2
                continue
            if text[cursor] == '"':
                return cursor + 1
            cursor += 1
        return len(text)
    if char in "[{":
        closer = "]" if char == "[" else "}"
        depth = 0
        cursor = index
        while cursor < len(text):
            current = text[cursor]
            if current == '"':
                cursor = _skip_value(text, cursor)
                continue
            if current == char:
                depth += 1
            elif current == closer:
                depth -= 1
                if depth == 0:
                    return cursor + 1
            cursor += 1
        return len(text)
    cursor = index
    while cursor < len(text) and text[cursor] not in ",}]":
        cursor += 1
    return cursor


def strip_top_level_keys(nbt: str, keys: Iterable[str]) -> str:
    """Drop named top-level SNBT fields, e.g. tick-dependent ``DeathTime``.

    Only an explicit caller opt-in should weaken NBT comparison; by default
    every field - including the inventory - is compared verbatim.
    """
    wanted = {str(key) for key in keys}
    if not wanted:
        return nbt
    items = _split_top_level(nbt)
    if items is None:
        return nbt
    kept = [(key, value) for key, value in items if key not in wanted]
    if len(kept) == len(items):
        return nbt
    return "{" + ",".join(f'"{key}":{value}' for key, value in kept) + "}"


def nbt_fragment(nbt: str, key: str = "Items") -> str | None:
    """The raw ``Items:[...]``/``Items:{...}`` fragment, for evidence reports.

    Extracting the inventory fragment is what lets a mismatch report say what
    changed instead of "NBT differs": a hash-only verdict would not show the
    item that vanished.
    """
    marker = f'"{key}":'
    position = nbt.find(marker)
    if position < 0:
        return None
    cursor = position + len(marker)
    while cursor < len(nbt) and nbt[cursor] in " \t":
        cursor += 1
    if cursor >= len(nbt) or nbt[cursor] not in "[{":
        return None
    end = _skip_value(nbt, cursor)
    return nbt[cursor:end]


def _truncate(value: str, limit: int = 240) -> str:
    return value if len(value) <= limit else value[:limit] + "..."


def _view(records: Sequence[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    """uuid -> the fields a comparison reads, keeping the first occurrence."""
    found: dict[str, dict[str, Any]] = {}
    for record in records:
        uuid = str(record.get("uuid") or "")
        if not uuid or uuid in found:
            continue
        found[uuid] = record
    return found


def _subsequence(expected: Sequence[str], actual: Sequence[str]) -> bool:
    cursor = 0
    for uuid in actual:
        if cursor < len(expected) and uuid == expected[cursor]:
            cursor += 1
    return cursor == len(expected)


def compare_snapshots(
    expected_meta: dict[str, Any],
    expected: Sequence[dict[str, Any]],
    actual_meta: dict[str, Any],
    actual: Sequence[dict[str, Any]],
    *,
    strict: bool = True,
    pos_tolerance: float = POS_TOLERANCE,
    vel_tolerance: float = VEL_TOLERANCE,
    ignore_nbt_keys: Iterable[str] = (),
    limit: int = 10,
) -> dict[str, Any]:
    """Compare a recording with a fresh snapshot of the destination.

    ``strict`` (the default) requires the two recordings to be the same set in
    the same order: the order hash, per-type counts and every compared field
    must match. With ``strict=False`` extra destination entities are reported
    as incidental and the recorded UUIDs only have to appear in the same
    relative order - but every recorded entity and every recorded field still
    has to match.
    """
    expected_uuids = _uuid_order(expected)
    actual_uuids = _uuid_order(actual)
    expected_index = _view(expected)
    actual_index = _view(actual)

    expected_counts = Counter(str(record.get("type") or "(missing type)") for record in expected)
    actual_counts = Counter(str(record.get("type") or "(missing type)") for record in actual)
    rows = []
    counts_match = True
    for name in sorted(
        set(expected_counts) | set(actual_counts),
        key=lambda value: (-(expected_counts[value] + actual_counts[value]), value),
    ):
        delta = actual_counts[name] - expected_counts[name]
        counts_match = counts_match and delta == 0
        rows.append({"type": name, "expected": expected_counts[name], "actual": actual_counts[name], "delta": delta})

    missing = [uuid for uuid in expected_uuids if uuid not in actual_index]
    unexpected = [uuid for uuid in actual_uuids if uuid not in expected_index]

    type_mismatches: list[dict[str, Any]] = []
    position_deltas: list[dict[str, Any]] = []
    velocity_deltas: list[dict[str, Any]] = []
    nbt_mismatches: list[dict[str, Any]] = []
    matched = 0
    for uuid in expected_uuids:
        want = expected_index.get(uuid)
        got = actual_index.get(uuid)
        if want is None or got is None:
            continue
        matched += 1
        want_type = str(want.get("type") or "")
        got_type = str(got.get("type") or "")
        if want_type != got_type:
            type_mismatches.append({"uuid": uuid, "expected": want_type, "actual": got_type})
        want_pos = _vec(want.get("pos"))
        got_pos = _vec(got.get("pos"))
        if want_pos is not None and got_pos is not None:
            distance = math.dist(want_pos, got_pos)
            if distance > pos_tolerance:
                position_deltas.append(
                    {
                        "uuid": uuid,
                        "type": want_type or got_type,
                        "distance": round(distance, 6),
                        "components": [round(got_pos[i] - want_pos[i], 6) for i in range(3)],
                    }
                )
        want_vel = _motion(want)
        got_vel = _motion(got)
        if want_vel is not None and got_vel is not None:
            speed = math.dist(want_vel, got_vel)
            if speed > vel_tolerance:
                velocity_deltas.append(
                    {
                        "uuid": uuid,
                        "type": want_type or got_type,
                        "speed": round(speed, 6),
                        "components": [round(got_vel[i] - want_vel[i], 6) for i in range(3)],
                    }
                )
        want_nbt = strip_top_level_keys(_nbt_of(want), ignore_nbt_keys)
        got_nbt = strip_top_level_keys(_nbt_of(got), ignore_nbt_keys)
        if want_nbt != got_nbt:
            entry: dict[str, Any] = {
                "uuid": uuid,
                "type": want_type or got_type,
                "expected": _truncate(want_nbt),
                "actual": _truncate(got_nbt),
            }
            expected_inventory = nbt_fragment(_nbt_of(want))
            actual_inventory = nbt_fragment(_nbt_of(got))
            if expected_inventory != actual_inventory:
                entry["expectedInventory"] = _truncate(expected_inventory or "(none)")
                entry["actualInventory"] = _truncate(actual_inventory or "(none)")
            nbt_mismatches.append(entry)

    order_match = expected_uuids == actual_uuids
    common_prefix = 0
    for want, got in zip(expected_uuids, actual_uuids):
        if want != got:
            break
        common_prefix += 1
    divergence = None
    if not order_match:
        divergence = {
            "index": common_prefix,
            "expected": expected_uuids[common_prefix] if common_prefix < len(expected_uuids) else None,
            "actual": actual_uuids[common_prefix] if common_prefix < len(actual_uuids) else None,
        }

    failures = {
        "order": not order_match,
        "counts": not counts_match,
        "missing": bool(missing),
        "unexpected": bool(unexpected) and strict,
        "types": bool(type_mismatches),
        "positions": bool(position_deltas),
        "velocities": bool(velocity_deltas),
        "nbt": bool(nbt_mismatches),
        "subsequence": not _subsequence(expected_uuids, actual_uuids),
    }
    if strict:
        ok = not any(failures.values())
    else:
        ok = not (
            failures["missing"]
            or failures["types"]
            or failures["positions"]
            or failures["velocities"]
            or failures["nbt"]
            or failures["subsequence"]
        )

    return {
        "ok": ok,
        "strict": strict,
        "orderHash": {
            "expected": fork.order_hash(expected_uuids),
            "actual": fork.order_hash(actual_uuids),
            "match": fork.order_hash(expected_uuids) == fork.order_hash(actual_uuids),
        },
        "uuidOrder": {
            "match": order_match,
            "commonPrefix": common_prefix,
            "expectedCount": len(expected_uuids),
            "actualCount": len(actual_uuids),
            "divergence": divergence,
        },
        "counts": {"match": counts_match, "byType": rows},
        "missing": {"count": len(missing), "sample": missing[:limit]},
        "unexpected": {"count": len(unexpected), "sample": unexpected[:limit]},
        "typeMismatches": {"count": len(type_mismatches), "sample": type_mismatches[:limit]},
        "positionDeltas": {
            "count": len(position_deltas),
            "tolerance": pos_tolerance,
            "sample": position_deltas[:limit],
        },
        "velocityDeltas": {
            "count": len(velocity_deltas),
            "tolerance": vel_tolerance,
            "sample": velocity_deltas[:limit],
        },
        "nbtMismatches": {"count": len(nbt_mismatches), "sample": nbt_mismatches[:limit]},
        "matched": matched,
        "failures": failures,
        "playersExcluded": {
            "expected": _player_count(expected_meta),
            "actual": _player_count(actual_meta),
        },
    }


def _player_count(meta: dict[str, Any]) -> int | None:
    skipped = meta.get("playersSkipped")
    if isinstance(skipped, bool) or not isinstance(skipped, int):
        return None
    return skipped


# --------------------------------------------------------------------- collisions


def collisions(
    expected: Sequence[dict[str, Any]],
    existing: Sequence[dict[str, Any]],
    radius: float = COLLISION_RADIUS,
) -> dict[str, Any]:
    """Leftover entities a restore would duplicate: same UUID or same location.

    A UUID collision is exact - the destination already holds the recorded
    entity. A spatial collision is the same entity type within ``radius`` of a
    recorded position, which is what a copied-but-not-cleared world looks like
    when the UUIDs differ. Both are reported with samples so the caller can
    clear the right thing rather than rerun blind.
    """
    existing = list(existing)
    by_uuid = _view(existing)
    duplicate_uuids = [str(record.get("uuid") or "") for record in expected if str(record.get("uuid") or "") in by_uuid]

    spatial: list[dict[str, Any]] = []
    for want in expected:
        want_pos = _vec(want.get("pos"))
        if want_pos is None:
            continue
        want_type = str(want.get("type") or "")
        for got in existing:
            if str(got.get("type") or "") != want_type:
                continue
            got_pos = _vec(got.get("pos"))
            if got_pos is None:
                continue
            distance = math.dist(want_pos, got_pos)
            if distance <= radius:
                spatial.append(
                    {
                        "uuid": str(got.get("uuid") or ""),
                        "expectedUuid": str(want.get("uuid") or ""),
                        "type": want_type,
                        "distance": round(distance, 6),
                    }
                )
                break
    return {
        "count": len(set(duplicate_uuids)) + len(spatial),
        "uuidCollisions": {"count": len(duplicate_uuids), "sample": duplicate_uuids[:10]},
        "spatialCollisions": {"count": len(spatial), "sample": spatial[:10]},
    }


# ---------------------------------------------------------------- endpoint checks


def normalize_path(value: Any) -> str:
    """A comparable absolute path: separators, case and ``.``/``..`` folded.

    Windows is case-insensitive and STATE reports ``worldDir`` with a trailing
    separator, so an exact string compare would refuse a correct destination.
    Symlinks are not resolved: the point of ``expect_world_dir`` is to prove
    which directory the connected server reports, and resolving links could
    make a different path look the same.
    """
    text = str(value or "").strip()
    if not text:
        return ""
    return os.path.normcase(os.path.normpath(os.path.abspath(text)))


def endpoint_checks(
    endpoint: dict[str, Any],
    *,
    expect_instance: str | None = None,
    expect_world_dir: str | None = None,
    expect_level: str | None = None,
) -> dict[str, Any]:
    """Verify the connected destination against explicit expectations.

    ``target`` on the restore call is a *label*: a bridge owns one mod
    connection and cannot route to a server by name. Routing has to be proven
    instead, and these are the proofs a caller can state up front. Each
    expectation is checked against ``STATE`` before anything is sent; a missing
    field is a refusal too, because "cannot verify" is not "verified".
    """
    if endpoint.get("error"):
        if expect_instance or expect_world_dir or expect_level:
            raise RestoreError(
                f"cannot verify the destination endpoint: STATE failed ({endpoint['error']}); "
                "nothing was sent to the game"
            )
        return {"verified": False, "reason": f"STATE failed: {endpoint['error']}"}

    mismatches: list[str] = []
    instance = str(endpoint.get("instance") or "")
    if expect_instance:
        if not instance:
            mismatches.append(
                f"expect_instance={expect_instance!r} but the connected mod did not report an instance"
            )
        elif instance.casefold() != expect_instance.casefold():
            mismatches.append(
                f"expect_instance={expect_instance!r} but the connected instance is {instance!r}"
            )
    world_dir = str(endpoint.get("worldDir") or "")
    if expect_world_dir:
        if not world_dir:
            mismatches.append(
                f"expect_world_dir={expect_world_dir!r} but STATE did not report worldDir "
                "(a client-vantage mod cannot prove which save a server owns)"
            )
        elif normalize_path(world_dir) != normalize_path(expect_world_dir):
            mismatches.append(
                f"expect_world_dir={expect_world_dir!r} but the connected world is {world_dir!r}"
            )
    level_name = endpoint.get("levelName")
    if expect_level:
        if level_name is None:
            mismatches.append(f"expect_level={expect_level!r} but STATE did not report levelName")
        elif str(level_name) != expect_level:
            mismatches.append(
                f"expect_level={expect_level!r} but the connected level is {level_name!r}"
            )
    if mismatches:
        raise RestoreError(
            "destination endpoint check failed: "
            + "; ".join(mismatches)
            + ". Nothing was sent to the game; the label `target` does not route - "
            "point a separate bridge at the intended instance."
        )
    return {
        "verified": True,
        "instance": instance or None,
        "worldDir": world_dir or None,
        "levelName": level_name,
        "expectInstance": expect_instance,
        "expectWorldDir": expect_world_dir,
        "expectLevel": expect_level,
    }


def dimension_check(
    fork_meta: dict[str, Any],
    destination_meta: dict[str, Any],
    expect_dimension: str | None = None,
) -> dict[str, Any]:
    """The destination's primary dimension must match the recording's.

    The mod's command source summons into its primary level (the first online
    player's dimension, else the overworld). Restoring a nether recording there
    would put every entity in the wrong dimension - silently, because
    ``/summon`` succeeds. ``expect_dimension`` is the explicit override for a
    caller who means it; by default the recording's own dimension is required.
    """
    wanted = str(expect_dimension or fork_meta.get("dimension") or "").strip()
    actual = str(destination_meta.get("dimension") or "").strip()
    if not wanted:
        return {"ok": True, "verified": False, "reason": "the fork records no dimension"}
    if not actual:
        return {
            "ok": False,
            "verified": False,
            "expected": wanted,
            "actual": None,
            "reason": "the destination snapshot reports no dimension",
        }
    return {
        "ok": wanted == actual,
        "verified": True,
        "expected": wanted,
        "actual": actual,
        "reason": None if wanted == actual else f"recording is {wanted}, destination is {actual}",
    }


# --------------------------------------------------------------------------- tick


def tick_state_from_output(lines: Iterable[Any]) -> bool | None:
    """Read ``/tick query`` output: ``True`` frozen, ``False`` running, ``None`` unknown.

    Dedicated servers answer in ``en_us`` regardless of client locale, and the
    server vantage collects the command output. A client vantage that answers
    without output, or a localised server, lands on ``None`` - the caller
    should not guess a tick state from text it cannot read.
    """
    text = " ".join(str(line) for line in lines).casefold()
    if "frozen" in text:
        return True
    if "running normally" in text or "not frozen" in text:
        return False
    return None


# ------------------------------------------------------------------------- names


def snapshot_name(stage: str, target: str | None = None) -> str:
    """A one-token snapshot name for the baseline/verification snapshots.

    Snapshot names become directory names on the instance, so anything outside
    ``[A-Za-z0-9_-]`` is folded; the result stays inside the mod's 64-char
    directory budget.
    """
    base = f"restore-{stage}-{target}" if target else f"restore-{stage}"
    safe = re.sub(r"[^A-Za-z0-9_-]+", "-", base)
    safe = re.sub(r"-{2,}", "-", safe).strip("-")
    return (safe or f"restore-{stage}")[:64]
