from __future__ import annotations

import hashlib
import json
import os
import tempfile
import unittest

from mc_agent_bridge import fork
from mc_agent_bridge.fork import SnapshotError

META = {
    "protocol": 1,
    "mod": "mc-agent-interface",
    "modVersion": "0.5.0",
    "minecraft": "26.2",
    "tick": 104233,
    "dimension": "minecraft:overworld",
    "radius": 64.0,
    "entities": 2,
    "orderHash": "9f2c0a1b2c3d4e5f",
    "createdAt": 1790145600000,
    "frozen": True,
    "worldDir": "C:/worlds/live",
    "instance": "server",
}

ENTITIES = [
    {
        "order": 0,
        "uuid": "466e11e1-1111-2222-3333-444455556666",
        "type": "minecraft:sulfur_cube",
        "entityId": 2096,
        "pos": [103.5, 56.0, 52.5],
        "vel": [0.0, -0.0784, 0.0],
        "yaw": 180.0,
        "pitch": 0.0,
        "nbt": "{Motion:[0.0d,-0.0784d,0.0d]}",
        "passengers": [],
        "vehicle": None,
    },
    {
        "order": 1,
        "uuid": "777e11e1-aaaa-bbbb-cccc-ddddeeeeffff",
        "type": "minecraft:tnt",
        "entityId": 2097,
        "pos": [12.0, 70.0, -3.25],
        "vel": [0.0, 0.0, 0.0],
        "yaw": 0.0,
        "pitch": 0.0,
        "nbt": "{fuse:80s}",
        "passengers": [],
        "vehicle": None,
    },
]

#: A 26.2 world directory the way the game leaves it: dimension data under
#: `dimensions/<namespace>/<dimension>/`, save metadata under `data/`, and the
#: files a lab must not inherit.
WORLD_FILES = {
    "level.dat": b"level",
    "level.dat_old": b"old",
    "data/minecraft/game_rules.dat": b"rules",
    "datapacks/keep/keep.mcfunction": b"say hi",
    "dimensions/minecraft/overworld/region/r.0.0.mca": b"r" * 4096,
    "dimensions/minecraft/overworld/region/r.0.-1.mca": b"s" * 1024,
    "dimensions/minecraft/overworld/entities/r.0.0.mca": b"e" * 64,
    "dimensions/minecraft/overworld/poi/r.0.0.mca": b"p" * 32,
    "dimensions/minecraft/the_nether/region/r.0.0.mca": b"n" * 512,
    "dimensions/minecraft/the_end/region/r.0.0.mca": b"o" * 256,
    "scripts/main.mcfunction": b"say world",
    "levelname.txt": b"Live World",
}

SKIP_FILES = {
    "session.lock": b"lock",
    "players/data/466e11e1-1111-2222-3333-444455556666.dat": b"player",
    "playerdata/abc.dat": b"player",
    "stats/abc.json": b"{}",
    "advancements/abc.json": b"{}",
    "logs/latest.log": b"log",
    "dimensions/minecraft/overworld/logs/latest.log": b"nested logs go too",
}


def write_snapshot(
    directory: str,
    meta: dict | None = None,
    entities: list[dict] | None = None,
    entity_lines: list[str] | None = None,
) -> str:
    os.makedirs(directory, exist_ok=True)
    if meta is not None:
        with open(os.path.join(directory, "meta.json"), "w", encoding="utf-8") as handle:
            json.dump(meta, handle, ensure_ascii=False)
    if entity_lines is None and entities is not None:
        entity_lines = [json.dumps(entity, ensure_ascii=False) for entity in entities]
    if entity_lines is not None:
        with open(os.path.join(directory, "entities.jsonl"), "w", encoding="utf-8") as handle:
            handle.write("\n".join(entity_lines) + "\n")
    return directory


def write_world(directory: str) -> None:
    for relative, content in {**WORLD_FILES, **SKIP_FILES}.items():
        path = os.path.join(directory, *relative.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as handle:
            handle.write(content)


def tree(directory: str) -> set[str]:
    found: set[str] = set()
    for root, _dirs, names in os.walk(directory):
        for name in names:
            relative = os.path.relpath(os.path.join(root, name), directory)
            found.add(relative.replace(os.sep, "/"))
    return found


class ReadSnapshotTests(unittest.TestCase):
    def test_reads_meta_and_entities_in_recorded_order(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_snapshot(directory, META, ENTITIES)
            meta, entities = fork.read_snapshot(directory)
            self.assertEqual(meta["orderHash"], META["orderHash"])
            self.assertEqual(meta["tick"], 104233)
            self.assertEqual([entity["uuid"] for entity in entities], [e["uuid"] for e in ENTITIES])
            self.assertEqual(entities[0]["nbt"], "{Motion:[0.0d,-0.0784d,0.0d]}")

    def test_a_missing_snapshot_says_which_file_is_missing(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(SnapshotError) as caught:
                fork.read_snapshot(directory)
            self.assertIn("meta.json", str(caught.exception))

        with tempfile.TemporaryDirectory() as directory:
            write_snapshot(directory, META, ENTITIES)
            os.remove(os.path.join(directory, "entities.jsonl"))
            with self.assertRaises(SnapshotError) as caught:
                fork.read_snapshot(directory)
            self.assertIn("entities.jsonl", str(caught.exception))

    def test_malformed_meta_is_rejected_with_the_reason(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_snapshot(directory, entities=ENTITIES, entity_lines=[])
            with open(os.path.join(directory, "meta.json"), "w", encoding="utf-8") as handle:
                handle.write("{not json")
            with self.assertRaises(SnapshotError) as caught:
                fork.read_meta(directory)
            self.assertIn("not valid JSON", str(caught.exception))

        with tempfile.TemporaryDirectory() as directory:
            with open(os.path.join(directory, "meta.json"), "w", encoding="utf-8") as handle:
                json.dump(["not", "an", "object"], handle)
            with self.assertRaises(SnapshotError) as caught:
                fork.read_meta(directory)
            self.assertIn("JSON object", str(caught.exception))

    def test_a_future_protocol_is_refused_rather_than_guessed_at(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_snapshot(directory, {**META, "protocol": 2}, ENTITIES)
            with self.assertRaises(SnapshotError) as caught:
                fork.read_snapshot(directory)
            self.assertIn("protocol 2", str(caught.exception))

    def test_a_bad_entity_line_names_its_line_number(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            good = json.dumps(ENTITIES[0])
            write_snapshot(directory, META, entity_lines=[good, "[1, 2, 3]"])
            with self.assertRaises(SnapshotError) as caught:
                fork.read_entities(directory)
            self.assertIn("line 2", str(caught.exception))
            self.assertIn("JSON object", str(caught.exception))

        with tempfile.TemporaryDirectory() as directory:
            write_snapshot(directory, META, entity_lines=["{oops"])
            with self.assertRaises(SnapshotError) as caught:
                fork.read_entities(directory)
            self.assertIn("line 1", str(caught.exception))

    def test_a_truncated_entities_file_is_not_a_snapshot(self) -> None:
        """Half a restore looks like a restore, so the count has to be checked."""
        with tempfile.TemporaryDirectory() as directory:
            write_snapshot(directory, META, entity_lines=[json.dumps(ENTITIES[0])])
            with self.assertRaises(SnapshotError) as caught:
                fork.read_snapshot(directory)
            self.assertIn("2 entities", str(caught.exception))
            self.assertIn("holds 1", str(caught.exception))

    def test_blank_lines_are_tolerated(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_snapshot(
                directory, META, entity_lines=[json.dumps(e) for e in ENTITIES] + ["", "  "]
            )
            _meta, entities = fork.read_snapshot(directory)
            self.assertEqual(len(entities), 2)

    def test_bytes_that_are_not_utf8_are_named_as_such(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_snapshot(directory, META, entity_lines=[json.dumps(ENTITIES[0])])
            with open(os.path.join(directory, "entities.jsonl"), "ab") as handle:
                handle.write(b"\xff\xfe\n")
            with self.assertRaises(SnapshotError) as caught:
                fork.read_entities(directory)
            self.assertIn("UTF-8", str(caught.exception))


class OrderHashTests(unittest.TestCase):
    def test_matches_the_definition_the_mod_computes(self) -> None:
        uuids = [entity["uuid"] for entity in ENTITIES]
        expected = hashlib.sha256(":".join(uuids).encode("utf-8")).hexdigest()[:16]
        self.assertEqual(fork.order_hash(uuids), expected)
        self.assertEqual(len(expected), 16)

    def test_is_stable_and_order_sensitive(self) -> None:
        uuids = [entity["uuid"] for entity in ENTITIES]
        self.assertEqual(fork.order_hash(uuids), fork.order_hash(list(uuids)))
        self.assertNotEqual(fork.order_hash(uuids), fork.order_hash(list(reversed(uuids))))

    def test_an_empty_level_still_has_a_hash(self) -> None:
        self.assertEqual(fork.order_hash([]), "e3b0c44298fc1c14")


class CopyWorldTests(unittest.TestCase):
    def test_copies_the_world_and_leaves_the_live_instance_bookkeeping(self) -> None:
        with tempfile.TemporaryDirectory() as source, tempfile.TemporaryDirectory() as target:
            write_world(source)
            manifest = fork.copy_world(source, os.path.join(target, "world"))
            copied = tree(os.path.join(target, "world"))

            self.assertEqual(copied, set(WORLD_FILES))
            self.assertEqual(manifest["files"], len(WORLD_FILES))
            self.assertEqual(manifest["bytes"], sum(len(c) for c in WORLD_FILES.values()))
            self.assertEqual(
                manifest["skipped"],
                [
                    "advancements",
                    "dimensions/minecraft/overworld/logs",
                    "logs",
                    "playerdata",
                    "players",
                    "session.lock",
                    "stats",
                ],
            )

    def test_the_skipped_things_never_reach_the_target(self) -> None:
        with tempfile.TemporaryDirectory() as source, tempfile.TemporaryDirectory() as target:
            write_world(source)
            fork.copy_world(source, os.path.join(target, "world"))
            copied = tree(os.path.join(target, "world"))
            for skipped in (
                "session.lock",
                "playerdata/abc.dat",
                "players/data/466e11e1-1111-2222-3333-444455556666.dat",
                "logs/latest.log",
            ):
                self.assertNotIn(skipped, copied, skipped)

    def test_a_missing_world_is_an_error_not_an_empty_fork(self) -> None:
        with tempfile.TemporaryDirectory() as target:
            with self.assertRaises(SnapshotError) as caught:
                fork.copy_world(os.path.join(target, "nope"), os.path.join(target, "world"))
            self.assertIn("does not exist", str(caught.exception))

    def test_refuses_to_copy_a_world_into_itself(self) -> None:
        with tempfile.TemporaryDirectory() as source:
            write_world(source)
            with self.assertRaises(SnapshotError):
                fork.copy_world(source, os.path.join(source, "world"))

    def test_the_target_directory_exists_even_for_an_empty_world(self) -> None:
        with tempfile.TemporaryDirectory() as source, tempfile.TemporaryDirectory() as target:
            manifest = fork.copy_world(source, os.path.join(target, "world"))
            self.assertTrue(os.path.isdir(os.path.join(target, "world")))
            self.assertEqual(manifest, {"files": 0, "bytes": 0, "skipped": []})


class SummonCommandTests(unittest.TestCase):
    def test_one_command_per_entity_in_file_order(self) -> None:
        self.assertEqual(
            fork.summon_commands(ENTITIES),
            [
                "/summon minecraft:sulfur_cube 103.5 56.0 52.5 "
                "{Motion:[0.0d,-0.0784d,0.0d]}",
                "/summon minecraft:tnt 12.0 70.0 -3.25 {fuse:80s}",
            ],
        )

    def test_no_entities_is_no_commands(self) -> None:
        self.assertEqual(fork.summon_commands([]), [])

    def test_an_entity_that_cannot_be_summoned_faithfully_is_an_error(self) -> None:
        for broken, expected in (
            ({**ENTITIES[0], "nbt": ""}, "NBT"),
            ({**ENTITIES[0], "type": None}, "type"),
            ({**ENTITIES[0], "pos": [1.0, 2.0]}, "position"),
            ({**ENTITIES[0], "pos": [1.0, "up", 2.0]}, "non-numeric"),
        ):
            with self.assertRaises(SnapshotError) as caught:
                fork.summon_commands([broken])
            self.assertIn(expected, str(caught.exception))
            self.assertIn(ENTITIES[0]["uuid"], str(caught.exception))


class SnapshotLineTests(unittest.TestCase):
    def test_line_shapes(self) -> None:
        self.assertEqual(fork.snapshot_line(), "SNAPSHOT")
        self.assertEqual(fork.snapshot_line(0), "SNAPSHOT 0.0")
        self.assertEqual(fork.snapshot_line(64), "SNAPSHOT 64.0")
        self.assertEqual(fork.snapshot_line(64, "before"), "SNAPSHOT 64.0 before")

    def test_a_name_without_a_radius_carries_an_explicit_zero(self) -> None:
        """SNAPSHOT is positional, so the mod must not read the name as a radius."""
        self.assertEqual(fork.snapshot_line(None, "before"), "SNAPSHOT 0 before")

    def test_a_non_numeric_radius_is_refused(self) -> None:
        with self.assertRaises(ValueError):
            fork.snapshot_line("soon", "before")


class OrderCheckNameTests(unittest.TestCase):
    def test_names_stay_one_safe_token(self) -> None:
        self.assertEqual(fork.order_check_name(None), "order-check")
        self.assertEqual(fork.order_check_name("lab server"), "order-lab-server")
        self.assertEqual(fork.order_check_name("../../etc"), "order-etc")
