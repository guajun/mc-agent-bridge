# Guarded restore: commands issued are not state restored

`fork` records a live world's entities in tick order and copies its blocks.
`restore` puts the entities back. The dangerous gap is between *sending* the
``/summon`` lines and the world actually being the recording again: a command
can fail silently, land in the wrong dimension, duplicate a leftover copy that
was still in the region files, or restore every entity while one chest's
inventory changed. `order` catches none of that - it compares one hash.

This page describes the guarded workflow the bridge now runs (and what callers
must still do themselves). It is the bridge side of
`docs/protocol-snapshot.md` in the meta repository and shares its
`orderHash`/`restorable` definitions with `tools/fork_verify.py` so all three
speak one contract.

## The endpoint contract: `target` is a label

The mod protocol identifies a connection, not a server by name. A bridge daemon
owns exactly one mod connection, so `target` cannot select an instance - it is a
label carried through into snapshot names and replies. Routing is proven
instead:

| Parameter | What it proves |
| --- | --- |
| `expect_instance` | The connected mod's hello/CAPS instance (`server`/`client`). |
| `expect_world_dir` | `STATE.worldDir` - the save directory the connected server actually owns. Best proof that a lab is not the live world. |
| `expect_level` | `STATE.levelName`, a weaker but cheap label. |

Every one of these is checked against the live endpoint report *before* a
command is sent; `expect_instance` comes from the mod's hello/CAPS frame (the
only source of the instance name) and the other two from `STATE`. An
expectation that cannot be verified (STATE has no `worldDir`, say) is a
refusal - "cannot verify" is not "verified". If the values do not match, the
error names the field, the expected value, the actual value, and says nothing
was sent and that a label does not route.

For an apply (`dry_run=false`) at least one of the three is **required**:
no proof means no mutation. `allow_unproven_destination=true` is the explicit
override for a caller who knows what it is writing to; it is recorded in
`checks.endpoint.overridden` and `checks.endpoint.action`. A failed `STATE`
refuses an apply with or without an expectation unless that override is given.
Dry runs never mutate, so they may be called without a proof.

`expect_world_dir` is the guard against the original failure mode: pointing a
bridge at the source world while passing the lab's name as a label. For a lab
that loaded a copied `world/`, pass the *destination* directory, e.g.
`F:/.../labs/bridge6-dst/world`.

## What `restore` automates

With `dry_run=false` (the default is a dry run), the daemon performs this
sequence and refuses at the first unsafe step:

1. **Read and validate** `meta.json` + `entities.jsonl`: protocol, declared
   count, `orderHash` recomputed from the file, unique UUIDs, and per-record
   `type`/`pos`/`nbt`. Blocking issues refuse before any game command; order
   field inconsistencies are warnings because the file's line order drives the
   restore. Passengers (`"restorable": false`) are skipped and reported, never
   summoned twice.
2. **Prove the endpoint** with the `expect_*` parameters above; an apply
   without a proof, or with a failed `STATE`, refuses before any other call.
3. **Resolve the prior tick state, and freeze, before taking any evidence.**
   `prior_tick_state=auto` reads `/tick query`; if that answer cannot be read
   the restore refuses before sending anything else, because guessing would
   either change an already-frozen world or leave a running one uncontrolled.
   Pass `prior_tick_state=frozen`/`running` to state it, or `freeze=false` to
   skip tick control. When running, the bridge freezes here, before the
   baseline snapshot, so pre-restore evidence cannot age while the game ticks.
4. **Load the recorded box** with `forceload add` (a headless lab has no player,
   so nothing is loaded otherwise). `forceload=false` skips it;
   `release_forceload=true` removes the box again afterwards.
5. **Baseline snapshot** of the destination (frozen, so it is stable), then
   **dimension check**: the destination's primary dimension must equal the
   recording's (`expect_dimension` overrides explicitly).
6. **Duplicate check**: same UUID, or the same entity type within
   `collision_radius` (default 0.75 blocks) of a recorded position. Each
   leftover is counted once. A collision refuses the restore with samples of
   the leftovers. `replace_existing=true` issues one narrow `kill` per detected
   collision - `@e[type=<leftover type>,x=<leftover pos>,distance=..<r>]` - plus
   a `minecraft:item` kill at the same spots for the inventory a killed chest
   minecart drops. It never kills a padded volume, so unrelated entities are
   left alone. The clear runs with ticks *running* (frozen kills leave
   dying-but-present entities), `save-all flush`es, re-freezes, re-snapshots
   and refuses if anything is still at a recorded position or collides.
   `check_existing=false` is the escape hatch for callers who manage leftovers
   themselves; combining it with `replace_existing=true` is a parameter error,
   not a silent no-op.
7. **Summon sequentially** in recorded order, still frozen. A command-level
   error (the mod refuses the line) stops the batch unless `keep_going=true`.
   A command that answers but whose entity never appears - the failure mode
   acks hide - is detected from the post-restore snapshot and reported as a
   failure with its index, UUID and the game's own output as evidence. The
   game's text is locale-dependent, so it is never parsed for success/failure.
8. **Verify** (unless `verify=false`): a fresh snapshot, still frozen, is
   compared with the recording - UUID order and `orderHash`, per-type counts,
   dimension, positions, velocities and the full NBT string (including
   `Items`). Malformed destination records fail the comparison instead of
   quietly dropping a field, and a position or velocity present on one side
   only is a mismatch. Extra destination entities fail `strict=true` (default)
   and are reported as incidental with `strict=false`.
9. **Restore the tick state** found in step 3: unfreeze if and only if the
   bridge froze a running world; a world that was frozen is left frozen (after
   the temporary unfreeze a clear needs, it is re-frozen). A failure to put the
   tick state back is reported in `tick.unfreezeError`/`tick.refreezeError` and
   makes the verdict fail rather than hiding it. A `freeze_timeout_seconds`
   watchdog releases the world if the restore never reaches its handlers; its
   firing also fails the verdict, because the comparison no longer ran under a
   controlled tick.

The result carries a state verdict, not an "it did not throw" flag:

- `verdict: "ok"` + `ok: true` + `verified: true` - the post-restore
  comparison ran and matched, and the prior tick state is back.
- `verdict: "failed"` + `ok: false` - a command failed, the comparison
  mismatched, the verification snapshot could not be taken, or the tick state
  could not be restored.
- `verdict: "unverified"` + `ok: null` + `verified: false` - `verify=false`.
  Commands were issued, but the bridge makes no claim that the world was
  faithfully restored; transport-level failures are still reported in
  `failed`.
- `verdict: "dry-run"` + `ok: null` - nothing was sent.

A failed summon or a mismatching inventory therefore never comes back as
`ok: true`, no matter how many acks arrived. `partial: true` means some
commands failed or were not attempted after a failure.

## Parameters at a glance

| Parameter | Default | Meaning |
| --- | --- | --- |
| `directory` | - | The fork directory (`mc_fork`'s `snapshotDir`). |
| `dry_run` | `true` | Return validation, endpoint report and commands; send nothing. |
| `target` | - | Label only; names the baseline/check snapshots. |
| `expect_instance` / `expect_world_dir` / `expect_level` | - | Destination proofs (see above); at least one is required for an apply. |
| `allow_unproven_destination` | `false` | Explicitly permit an apply with no proof or a failed `STATE`; recorded in `checks.endpoint.overridden`. |
| `expect_dimension` | recording's | Override the dimension requirement explicitly. |
| `prior_tick_state` | `auto` | `auto` reads `/tick query` and refuses if unreadable; `frozen`/`running` state it explicitly. |
| `freeze` | `true` | Controlled tick state for the restore. |
| `freeze_timeout_seconds` | `300` | Watchdog: unfreeze if the restore is still running this long after freezing; `0` disables. |
| `forceload` / `release_forceload` | `true` / `false` | Load the recorded box; release it afterwards. |
| `check_existing` / `replace_existing` | `true` / `false` | Refuse duplicates, or kill the detected collisions (plus their drops) at their own positions and re-check. |
| `collision_radius` | `0.75` | Same-type distance that counts as a duplicate. |
| `keep_going` | `false` | Continue after a command failure (default fail fast). |
| `verify` / `strict` | `true` / `true` | Full-state comparison; `strict=false` tolerates extra entities. |
| `verify_radius` | every entity | Radius for the verification snapshot. |
| `pos_tolerance` / `vel_tolerance` | `0.01` | Movement slack; a frozen restore reproduces the doubles. |
| `ignore_nbt_keys` | `[]` | Explicit opt-out fields (e.g. tick counters); off by default. |

`verify` (and the MCP `mc_verify` tool) runs the same comparison without
restoring - use it after an externally driven restore, or to prove "the order
hash matches but an inventory did not". It validates the destination snapshot
itself too (counts/hash/malformed records) and fails on a dimension mismatch,
so a `the_nether` snapshot with identical entities is not `ok` against an
overworld recording.

After a real server restart the tick order is rebuilt by chunk load order, so
the strict comparison may fail on order alone. Use `strict=false` and check
`missing: 0`, `unexpected: 0` and `counts.match: true` for the no-respawn
acceptance; `dimension` and the per-entity fields are still required. A
`strict=false` `ok` additionally requires the recorded UUIDs to appear in the
same relative order, which a restart does not guarantee.

## What callers must still do

The bridge does not choose the destination or copy the world for you:

1. Fork the source (`mc_fork`), keeping the source untouched. The fork strips
   the `entities/*.mca` files from the copy (`fork.copy_world`), but entity data
   also lives in region files, and copied chunks can carry it back on load -
   that is why the duplicate check and `replace_existing` exist.
2. Create a disposable lab instance from the fork's `world/` directory and
   start it with the interface mod (see `labs/` tooling in the meta repo).
3. Point a **separate** bridge daemon at the lab (its own API port and
   `--server-dir`) and call `restore` with `expect_world_dir` (and
   `expect_instance`) set to the lab. Never point the source bridge at the lab
   or the destination bridge at the source. If the connected mod cannot report
   `/tick query` (a client vantage that answers commands without output), pass
   `prior_tick_state` explicitly or `freeze=false`: the default `auto` refuses
   rather than guessing whether the world was frozen.
4. After a successful restore, if you need to prove the copied disk entities do
   not respawn, stop the lab, start it again (a real chunk reload) and run
   `verify` with `strict=false` again: check `missing: 0`, `unexpected: 0` and
   `counts.match: true`. Tick order is rebuilt at load time, so a restart is a
   set/count check, not an order promise; `strict=false` still compares every
   field of every shared entity and the dimension.

## Live acceptance (26.2 Fabric lab, generic stacked chest minecarts)

The E2E run for this change used two fresh dedicated servers (no player, server
vantage), built a 3-cart stacked chest-minecart fixture in the source lab with
distinct items in each cart, forked it, loaded the clean copy into a destination
lab, and restored it under guard.

Build under test: interface mod **0.6.0**, a clean build whose source commit is
`3b93ceb137f8e05624d9d443356c0db78c4b4751` and whose jar SHA-256 is
`45f12e16b3979be6a699ac3c744b2a68dfcf8dd2379f5987bf9b9319adf4404f`;
Fabric loader 0.19.5, Java 25.0.1, Minecraft 26.2.

Fixture and result, taken from `source-before` / `bridge6-fixture` and from the
verification snapshot after a real server restart:

```
fixture  (hash a663c5c0dfd7ec6f, 3 records)      restored after restart (same hash)
  11111111... chest_minecart y=100 diamond x5      11111111... diamond x5
  22222222... chest_minecart y=101 emerald x2      22222222... emerald x2
  33333333... chest_minecart y=102 redstone x7     33333333... redstone x7
```

What the run proved, with the evidence kept under `labs/evidence/`:

| Check | Result |
| --- | --- |
| wrong `expect_world_dir` (source world) | refused before any command: "the label `target` does not route" |
| dry run | 3 commands, endpoint verified, nothing sent |
| guarded apply | `ok: true`/`verdict: ok`/`verified: true`, 3/3 summoned, dimension/order hash `a663c5c0dfd7ec6f`/counts/positions/velocities/NBT/items identical, `tick: {prior: running, frozenByBridge: true, restored: true, preserved: true}` |
| second restore without replacement | refused with 3 UUID collisions (spatial 0, no double count), no mutation, tick state restored on the refusal |
| `replace_existing` | six narrow kill commands, one per collision position plus its item drop (`kill @e[type=minecraft:chest_minecart,x=0.5,y=100.0,z=0.5,distance=..1]` ...), `remainingCollisions: 0`, `remainingAtRecordedPositions: 0`, exact restore, `tick.preserved: true` |
| `verify=false` | `ok: null`, `verdict: "unverified"`, `verified: false`, `verification: null`; a separate `verify` afterwards is `ok: true` |
| live inventory mutation (`Items[0].count` 5/2/7 -> 64) | `verify` `ok: false`, order hash unchanged, 3 `nbtMismatches` with both inventory fragments; guarded repair `ok: true` |
| server stop/start (real chunk reload) | `verify strict=false`: 3 entities, 0 missing, 0 unexpected, counts/dimension/NBT match |
| source world after the whole run | full-state comparison against `source-before`: unchanged |

The fork manifest also confirmed the 26.2 layout live: entity storage under
`dimensions/minecraft/overworld/entities/*.mca` was stripped from the copy
(`"stripped"` in the manifest), and after the reload no copied entity respawned
alongside the restored ones.

The paths that need a real refusal rather than a live demonstration are covered
by daemon regressions because the dedicated server answers in `en_us`: an
unreadable `/tick query` (`test_unknown_prior_tick_state_refuses_before_mutation`),
an apply without an endpoint proof (`test_apply_without_an_endpoint_proof_refuses_before_any_command`),
a failed `STATE` (`test_apply_refuses_when_state_fails_and_a_proof_was_given`),
a failed unfreeze (`test_a_failed_unfreeze_fails_the_verdict_and_is_reported`),
the freeze watchdog (`test_the_freeze_watchdog_releases_a_stuck_restore`),
`verify` dimension/malformed-input failures and the narrow clear targeting.

### Reproducing and retained evidence

The driver is `labs/live_verify.py` with stages `setup`, `fork`, `restore`,
`duplicate`, `mutate`, `unverified`, `after-reload`, `source-final`. It writes
`labs/evidence/<stage>.json` plus `summary.json`, and `labs/evidence/index.json`
records the bridge commit, the interface jar path/hash and source commit, the
runtime versions, the ports, and a SHA-256 of every evidence file, so a later
reader can tell whether the retained files are the ones the report describes.
