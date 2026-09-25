from __future__ import annotations

import math
import tempfile
import unittest
from pathlib import Path

from mc_agent_bridge import fork, restore
from mc_agent_bridge.fork import SnapshotError
from mc_agent_bridge.restore import RestoreError

CART_A = {
    "order": 0,
    "uuid": "11111111-1111-4111-8111-111111111111",
    "type": "minecraft:chest_minecart",
    "pos": [10.5, 64.0, -3.5],
    "vel": [0.0, 0.0, 0.0],
    "nbt": (
        '{"Items":[{"Slot":0b,"id":"minecraft:diamond","count":5}],'
        '"Motion":[0.0d,0.0d,0.0d],"UUID":[I;1,2,3,4]}'
    ),
    "restorable": True,
}

CART_B = {
    "order": 1,
    "uuid": "22222222-2222-4222-8222-222222222222",
    "type": "minecraft:chest_minecart",
    "pos": [10.5, 65.0, -3.5],
    "vel": [0.0, -0.08, 0.0],
    "nbt": (
        '{"Items":[{"Slot":0b,"id":"minecraft:emerald","count":2}],'
        '"Motion":[0.0d,-0.08d,0.0d],"UUID":[I;5,6,7,8]}'
    ),
    "restorable": True,
}

PASSENGER = {
    "order": 2,
    "uuid": "33333333-3333-4333-8333-333333333333",
    "type": "minecraft:zombie",
    "pos": [10.5, 66.0, -3.5],
    "vel": [0.0, 0.0, 0.0],
    "nbt": '{"UUID":[I;9,10,11,12]}',
    "restorable": False,
    "vehicle": CART_B["uuid"],
}

ENTITIES = [CART_A, CART_B]


def meta_for(records, **extra):
    meta = {
        "protocol": 1,
        "entities": len(records),
        "orderHash": fork.order_hash([entity["uuid"] for entity in records]),
    }
    meta.update(extra)
    return meta


def copy(entity, **changes):
    updated = dict(entity)
    updated.update(changes)
    return updated


class ValidateSnapshotTests(unittest.TestCase):
    def test_a_faithful_snapshot_has_no_blocking_issues(self) -> None:
        issues = restore.validate_snapshot(meta_for(ENTITIES), ENTITIES)
        self.assertEqual(restore.blocking_issues(issues), [])

    def test_hash_mismatch_and_bad_hash_format_are_blocking(self) -> None:
        issues = restore.validate_snapshot(meta_for(ENTITIES, orderHash="0" * 16), ENTITIES)
        self.assertIn("order_hash", [issue.code for issue in restore.blocking_issues(issues)])

        issues = restore.validate_snapshot(meta_for(ENTITIES, orderHash="nope"), ENTITIES)
        self.assertIn("order_hash_format", [issue.code for issue in restore.blocking_issues(issues)])

    def test_count_mismatch_and_missing_protocol_are_blocking(self) -> None:
        issues = restore.validate_snapshot(meta_for(ENTITIES, entities=99), ENTITIES)
        self.assertIn("entity_count", [issue.code for issue in restore.blocking_issues(issues)])

        meta = meta_for(ENTITIES)
        del meta["protocol"]
        issues = restore.validate_snapshot(meta, ENTITIES)
        self.assertIn("meta_missing", [issue.code for issue in restore.blocking_issues(issues)])

        issues = restore.validate_snapshot(meta_for(ENTITIES, protocol=2), ENTITIES)
        self.assertIn("meta_protocol", [issue.code for issue in restore.blocking_issues(issues)])

    def test_an_unsummonable_record_is_blocking_before_any_command(self) -> None:
        for broken, expected in (
            (copy(CART_A, type=None), "type"),
            (copy(CART_A, pos=[1.0, 2.0]), "pos"),
            (copy(CART_A, nbt=""), "NBT"),
        ):
            issues = restore.validate_snapshot(meta_for([broken]), [broken])
            blocking = restore.blocking_issues(issues)
            self.assertTrue(blocking, broken)
            self.assertIn(expected, " ".join(issue.message for issue in blocking))

    def test_an_order_mismatch_is_a_warning_not_a_blocker(self) -> None:
        scrambled = copy(CART_A, order=5)
        issues = restore.validate_snapshot(meta_for([scrambled]), [scrambled])
        codes = {issue.code for issue in issues}
        self.assertIn("order_mismatch", codes)
        self.assertEqual(restore.blocking_issues(issues), [])

    def test_duplicate_uuids_are_blocking(self) -> None:
        twin = copy(CART_A, uuid=CART_A["uuid"])
        issues = restore.validate_snapshot(meta_for([CART_A, twin]), [CART_A, twin])
        self.assertIn("uuid_duplicate", [issue.code for issue in restore.blocking_issues(issues)])

    def test_describe_blockers_names_the_file_and_says_nothing_was_sent(self) -> None:
        issues = restore.validate_snapshot(meta_for([copy(CART_A, type=None)]), [copy(CART_A, type=None)])
        text = restore.describe_blockers("C:/snap", restore.blocking_issues(issues))
        self.assertIn("C:/snap", text)
        self.assertIn("Nothing was sent to the game", text)


class RestorePlanTests(unittest.TestCase):
    def test_commands_keep_recorded_order_and_name_each_entity(self) -> None:
        plan = restore.restore_plan(ENTITIES)
        self.assertEqual(len(plan["commands"]), 2)
        self.assertTrue(plan["commands"][0].startswith("/summon minecraft:chest_minecart 10.5 64.0 -3.5 "))
        self.assertEqual([item["uuid"] for item in plan["items"]], [CART_A["uuid"], CART_B["uuid"]])
        self.assertEqual(plan["skipped"], [])

    def test_passengers_are_skipped_and_reported_not_silently_dropped(self) -> None:
        plan = restore.restore_plan([CART_A, PASSENGER, CART_B])
        self.assertEqual([item["uuid"] for item in plan["items"]], [CART_A["uuid"], CART_B["uuid"]])
        self.assertEqual(len(plan["skipped"]), 1)
        self.assertEqual(plan["skipped"][0]["uuid"], PASSENGER["uuid"])
        self.assertEqual(plan["skipped"][0]["index"], 1)
        self.assertIn(CART_B["uuid"], plan["skipped"][0]["reason"])

    def test_a_record_that_cannot_be_summoned_raises_before_planning_succeeds(self) -> None:
        with self.assertRaises(SnapshotError):
            restore.restore_plan([copy(CART_A, nbt="")])

    def test_records_without_the_restorable_flag_are_treated_as_restorable(self) -> None:
        without_flag = {key: value for key, value in CART_A.items() if key != "restorable"}
        plan = restore.restore_plan([without_flag])
        self.assertEqual(len(plan["commands"]), 1)


class BoxCommandTests(unittest.TestCase):
    def test_box_pads_the_recorded_area(self) -> None:
        box = restore.bounding_box(ENTITIES, margin=16.0)
        self.assertEqual(box, (10.5 - 16.0, 64.0 - 16.0, -3.5 - 16.0, 10.5 + 16.0, 65.0 + 16.0, -3.5 + 16.0))

    def test_no_entities_is_no_box(self) -> None:
        self.assertIsNone(restore.bounding_box([]))

    def test_forceload_uses_the_xz_footprint(self) -> None:
        box = restore.bounding_box(ENTITIES, margin=16.0)
        self.assertEqual(restore.forceload_add_command(box), "forceload add -6 -20 27 13")
        self.assertEqual(restore.forceload_remove_command(box), "forceload remove -6 -20 27 13")

    def test_clear_command_preserves_players_and_covers_the_box(self) -> None:
        box = restore.bounding_box(ENTITIES, margin=16.0)
        command = restore.clear_box_command(box)
        self.assertIn("type=!player", command)
        self.assertIn("x=-6,y=48,z=-20", command)
        self.assertIn("dx=34,dy=34,dz=34", command)

    def test_entities_in_box_flags_only_what_is_inside(self) -> None:
        box = restore.bounding_box(ENTITIES, margin=16.0)
        drop = copy(CART_A, uuid="77777777-7777-4777-8777-777777777777", type="minecraft:item", pos=[10.5, 63.0, -3.5])
        outside = copy(CART_A, uuid="88888888-8888-4888-8888-888888888888", pos=[100.0, 64.0, 100.0])
        found = restore.entities_in_box([drop, outside], box)
        self.assertEqual([entity["uuid"] for entity in found], [drop["uuid"]])
        self.assertEqual(found[0]["type"], "minecraft:item")


class TickStateTests(unittest.TestCase):
    def test_reads_the_frozen_state_from_command_output(self) -> None:
        self.assertIs(restore.tick_state_from_output(["The game is frozen."]), True)
        self.assertIs(restore.tick_state_from_output(["The game is running normally."]), False)
        self.assertIs(restore.tick_state_from_output(["The game is frozen (frozen at tick 42)"]), True)
        self.assertIsNone(restore.tick_state_from_output([]))
        self.assertIsNone(restore.tick_state_from_output(["参数错误"]))


class NbtTests(unittest.TestCase):
    def test_extracts_the_inventory_fragment(self) -> None:
        fragment = restore.nbt_fragment(CART_A["nbt"])
        self.assertEqual(
            fragment,
            '[{"Slot":0b,"id":"minecraft:diamond","count":5}]',
        )

    def test_missing_inventory_is_none(self) -> None:
        self.assertIsNone(restore.nbt_fragment('{"Motion":[0.0d,0.0d,0.0d]}'))

    def test_strip_top_level_keys_drops_only_the_named_field(self) -> None:
        stripped = restore.strip_top_level_keys(CART_A["nbt"], ["UUID"])
        self.assertNotIn('"UUID"', stripped)
        self.assertIn('"Items"', stripped)
        self.assertIn('"Motion"', stripped)
        self.assertEqual(restore.strip_top_level_keys(CART_A["nbt"], []), CART_A["nbt"])

    def test_strip_handles_nested_braces_and_quoted_strings(self) -> None:
        nbt = '{"Name":"a,b}c","DeathTime":3s,"Items":[{"id":"minecraft:stick","count":1}]}'
        stripped = restore.strip_top_level_keys(nbt, ["DeathTime"])
        self.assertEqual(
            stripped, '{"Name":"a,b}c","Items":[{"id":"minecraft:stick","count":1}]}'
        )


class CompareSnapshotTests(unittest.TestCase):
    def test_identical_recordings_match_including_inventory(self) -> None:
        report = restore.compare_snapshots(meta_for(ENTITIES), ENTITIES, meta_for(ENTITIES), ENTITIES)
        self.assertTrue(report["ok"])
        self.assertTrue(report["orderHash"]["match"])
        self.assertTrue(report["counts"]["match"])
        self.assertEqual(report["nbtMismatches"]["count"], 0)
        self.assertEqual(report["matched"], 2)

    def test_inventory_mutation_with_unchanged_order_hash_fails_on_nbt(self) -> None:
        mutated = copy(
            CART_A,
            nbt=CART_A["nbt"].replace('"count":5', '"count":64'),
        )
        report = restore.compare_snapshots(meta_for(ENTITIES), ENTITIES, meta_for([mutated, CART_B]), [mutated, CART_B])
        self.assertFalse(report["ok"])
        self.assertTrue(report["orderHash"]["match"], "the order hash stays the same by construction")
        self.assertEqual(report["uuidOrder"]["match"], True)
        self.assertEqual(report["nbtMismatches"]["count"], 1)
        mismatch = report["nbtMismatches"]["sample"][0]
        self.assertEqual(mismatch["uuid"], CART_A["uuid"])
        self.assertIn('"count":5', mismatch["expectedInventory"])
        self.assertIn('"count":64', mismatch["actualInventory"])

    def test_reordered_entities_fail_the_order_and_hash(self) -> None:
        swapped = [CART_B, CART_A]
        report = restore.compare_snapshots(meta_for(ENTITIES), ENTITIES, meta_for(swapped), swapped)
        self.assertFalse(report["ok"])
        self.assertFalse(report["orderHash"]["match"])
        self.assertFalse(report["uuidOrder"]["match"])
        self.assertEqual(report["uuidOrder"]["divergence"]["index"], 0)

    def test_position_and_velocity_drift_beyond_tolerance_fail(self) -> None:
        moved = copy(CART_A, pos=[10.5, 66.0, -3.5])
        report = restore.compare_snapshots(meta_for(ENTITIES), ENTITIES, meta_for([moved, CART_B]), [moved, CART_B])
        self.assertFalse(report["ok"])
        self.assertEqual(report["positionDeltas"]["count"], 1)
        self.assertAlmostEqual(report["positionDeltas"]["sample"][0]["distance"], 2.0)

        pushed = copy(CART_B, vel=[1.0, -0.08, 0.0])
        report = restore.compare_snapshots(
            meta_for(ENTITIES), ENTITIES, meta_for([CART_A, pushed]), [CART_A, pushed]
        )
        self.assertFalse(report["ok"])
        self.assertEqual(report["velocityDeltas"]["count"], 1)

    def test_a_missing_entity_fails_strict_and_lenient(self) -> None:
        actual = [CART_A]
        for strict in (True, False):
            report = restore.compare_snapshots(
                meta_for(ENTITIES), ENTITIES, meta_for(actual), actual, strict=strict
            )
            self.assertFalse(report["ok"], strict)
            self.assertEqual(report["missing"]["count"], 1)
            self.assertEqual(report["missing"]["sample"], [CART_B["uuid"]])

    def test_extra_entities_only_fail_in_strict_mode(self) -> None:
        extra = copy(CART_B, uuid="99999999-9999-4999-8999-999999999999")
        actual = [CART_A, CART_B, extra]
        strict = restore.compare_snapshots(meta_for(ENTITIES), ENTITIES, meta_for(actual), actual)
        self.assertFalse(strict["ok"])
        self.assertEqual(strict["unexpected"]["count"], 1)
        self.assertFalse(strict["counts"]["match"])

        lenient = restore.compare_snapshots(
            meta_for(ENTITIES), ENTITIES, meta_for(actual), actual, strict=False
        )
        self.assertTrue(lenient["ok"], lenient["failures"])
        self.assertEqual(lenient["unexpected"]["count"], 1)

    def test_empty_expected_destination_with_expected_entities_fails(self) -> None:
        report = restore.compare_snapshots(meta_for(ENTITIES), ENTITIES, meta_for([]), [])
        self.assertFalse(report["ok"])
        self.assertEqual(report["missing"]["count"], 2)
        self.assertFalse(report["counts"]["match"])

    def test_ignore_nbt_keys_is_explicit_opt_in(self) -> None:
        mutated = copy(CART_A, nbt=CART_A["nbt"].replace("[I;1,2,3,4]", "[I;9,9,9,9]"))
        report = restore.compare_snapshots(
            meta_for(ENTITIES),
            ENTITIES,
            meta_for([mutated, CART_B]),
            [mutated, CART_B],
            ignore_nbt_keys=["UUID"],
        )
        self.assertTrue(report["ok"], report["failures"])

    def test_players_excluded_are_reported_from_meta(self) -> None:
        report = restore.compare_snapshots(
            meta_for(ENTITIES, playersSkipped=1),
            ENTITIES,
            meta_for(ENTITIES, playersSkipped=0),
            ENTITIES,
        )
        self.assertEqual(report["playersExcluded"], {"expected": 1, "actual": 0})


class CollisionTests(unittest.TestCase):
    def test_same_uuid_is_an_exact_collision(self) -> None:
        found = restore.collisions(ENTITIES, [copy(CART_A, pos=[0.0, 0.0, 0.0])])
        self.assertEqual(found["uuidCollisions"]["count"], 1)
        self.assertIn(CART_A["uuid"], found["uuidCollisions"]["sample"])

    def test_same_type_at_the_same_position_with_a_different_uuid_collides(self) -> None:
        leftover = copy(CART_A, uuid="44444444-4444-4444-8444-444444444444", pos=[10.5, 64.05, -3.5])
        found = restore.collisions(ENTITIES, [leftover])
        self.assertEqual(found["uuidCollisions"]["count"], 0)
        self.assertEqual(found["spatialCollisions"]["count"], 1)
        self.assertEqual(found["count"], 1)

    def test_different_type_or_far_away_entity_is_incidental(self) -> None:
        far = copy(CART_A, uuid="55555555-5555-4555-8555-555555555555", pos=[100.0, 64.0, 100.0])
        other = copy(CART_A, uuid="66666666-6666-4666-8666-666666666666", type="minecraft:tnt", pos=[10.5, 64.5, -3.5])
        found = restore.collisions(ENTITIES, [far, other])
        self.assertEqual(found["count"], 0)


class EndpointCheckTests(unittest.TestCase):
    def endpoint(self, **extra):
        endpoint = {
            "instance": "server",
            "modPort": 25581,
            "worldDir": "F:\\labs\\dst\\world",
            "levelName": "world",
        }
        endpoint.update(extra)
        return endpoint

    def test_matching_expectations_verify(self) -> None:
        checks = restore.endpoint_checks(
            self.endpoint(),
            expect_instance="server",
            expect_world_dir="f:/labs/dst/world/",
            expect_level="world",
        )
        self.assertTrue(checks["verified"])

    def test_instance_mismatch_refuses_and_names_the_label_contract(self) -> None:
        with self.assertRaises(RestoreError) as caught:
            restore.endpoint_checks(self.endpoint(), expect_instance="lab-b")
        message = str(caught.exception)
        self.assertIn("expect_instance", message)
        self.assertIn("does not route", message)

    def test_world_dir_mismatch_refuses(self) -> None:
        with self.assertRaises(RestoreError) as caught:
            restore.endpoint_checks(self.endpoint(), expect_world_dir="F:\\labs\\src\\world")
        self.assertIn("expect_world_dir", str(caught.exception))

    def test_an_unverifiable_expectation_refuses_rather_than_pretending(self) -> None:
        with self.assertRaises(RestoreError):
            restore.endpoint_checks(self.endpoint(worldDir=None), expect_world_dir="F:\\labs\\dst\\world")
        with self.assertRaises(RestoreError):
            restore.endpoint_checks(self.endpoint(levelName=None), expect_level="world")

    def test_no_expectations_and_a_failed_state_reports_unverified_but_may_proceed(self) -> None:
        checks = restore.endpoint_checks({"error": "not connected"})
        self.assertFalse(checks["verified"])
        self.assertIn("not connected", checks["reason"])

        with self.assertRaises(RestoreError):
            restore.endpoint_checks({"error": "not connected"}, expect_instance="server")

    def test_normalize_path_folds_separators_case_and_dots(self) -> None:
        self.assertEqual(
            restore.normalize_path("F:/labs/./dst/world/"),
            restore.normalize_path("f:\\labs\\dst\\world"),
        )
        self.assertEqual(restore.normalize_path(None), "")


class DimensionCheckTests(unittest.TestCase):
    def test_recording_dimension_has_to_match_the_destination(self) -> None:
        result = restore.dimension_check(
            {"dimension": "minecraft:overworld"}, {"dimension": "minecraft:the_nether"}
        )
        self.assertFalse(result["ok"])
        self.assertIn("the_nether", result["reason"])

        result = restore.dimension_check(
            {"dimension": "minecraft:overworld"}, {"dimension": "minecraft:overworld"}
        )
        self.assertTrue(result["ok"])

    def test_explicit_expect_dimension_overrides_the_recording(self) -> None:
        result = restore.dimension_check(
            {"dimension": "minecraft:the_nether"},
            {"dimension": "minecraft:overworld"},
            expect_dimension="minecraft:overworld",
        )
        self.assertTrue(result["ok"])

    def test_a_fork_without_a_dimension_skips_the_check(self) -> None:
        result = restore.dimension_check({}, {"dimension": "minecraft:overworld"})
        self.assertTrue(result["ok"])
        self.assertFalse(result["verified"])

    def test_a_destination_without_a_dimension_refuses_an_expectation(self) -> None:
        result = restore.dimension_check({"dimension": "minecraft:overworld"}, {})
        self.assertFalse(result["ok"])
        self.assertIsNone(result["actual"])


class SnapshotNameTests(unittest.TestCase):
    def test_names_stay_one_safe_token(self) -> None:
        self.assertEqual(restore.snapshot_name("pre", None), "restore-pre")
        self.assertEqual(restore.snapshot_name("pre", "lab server"), "restore-pre-lab-server")
        self.assertEqual(restore.snapshot_name("check", "../../etc"), "restore-check-etc")

    def test_names_fit_the_mod_directory_budget(self) -> None:
        self.assertLessEqual(len(restore.snapshot_name("check", "x" * 200)), 64)


if __name__ == "__main__":
    unittest.main()
