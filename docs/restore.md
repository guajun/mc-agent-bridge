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

Every one of these is checked against a live `STATE` reply *before* a command is
sent, and an expectation that cannot be verified (STATE has no `worldDir`, say)
is a refusal - "cannot verify" is not "verified". If the values do not match,
the error names the field, the expected value, the actual value, and says
nothing was sent and that a label does not route.

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
2. **Prove the endpoint** with the `expect_*` parameters above.
3. **Load the recorded box** with `forceload add` (a headless lab has no player,
   so nothing is loaded otherwise). `forceload=false` skips it;
   `release_forceload=true` removes the box again afterwards.
4. **Baseline snapshot** of the destination, then **dimension check**: the
   destination's primary dimension must equal the recording's (`expect_dimension`
   overrides explicitly).
5. **Duplicate check**: same UUID, or the same entity type within
   `collision_radius` (default 0.75 blocks) of a recorded position. A collision
   refuses the restore with samples of the leftovers. `replace_existing=true`
   clears the recorded box (`kill @e[type=!player,...]`), `save-all flush`es so
   the clear survives a chunk reload, re-snapshots and refuses if anything
   still collides. `check_existing=false` is the escape hatch for callers who
   manage leftovers themselves.
6. **Freeze the tick**: `/tick query` records the prior state (frozen / running /
   unknown); the bridge freezes only if the game was not frozen, and unfreezes
   only if it froze. A frozen world is left frozen.
7. **Summon sequentially** in recorded order. A command-level error (the mod
   refuses the line) stops the batch unless `keep_going=true`. A command that
   answers but whose entity never appears - the failure mode acks hide - is
   detected from the post-restore snapshot and reported as a failure with its
   index, UUID and the game's own output as evidence. The game's text is
   locale-dependent, so it is never parsed for success/failure.
8. **Verify** (unless `verify=false`): a fresh snapshot is compared with the
   recording - UUID order and `orderHash`, per-type counts, positions,
   velocities and the full NBT string (including `Items`). Extra destination
   entities fail `strict=true` (default) and are reported as incidental with
   `strict=false`.
9. **Unfreeze** if and only if the bridge froze the tick; a failure there is
   reported in `tick.unfreezeError`, never hidden.

The result's `ok` is the verdict: `not failed and verification.ok`. A failed
summon or a mismatching inventory means `ok: false`, no matter how many acks
came back. `partial: true` means some commands failed or were not attempted.

## Parameters at a glance

| Parameter | Default | Meaning |
| --- | --- | --- |
| `directory` | - | The fork directory (`mc_fork`'s `snapshotDir`). |
| `dry_run` | `true` | Return validation, endpoint report and commands; send nothing. |
| `target` | - | Label only; names the baseline/check snapshots. |
| `expect_instance` / `expect_world_dir` / `expect_level` | - | Destination proofs (see above). |
| `expect_dimension` | recording's | Override the dimension requirement explicitly. |
| `freeze` | `true` | Controlled tick state for the restore. |
| `forceload` / `release_forceload` | `true` / `false` | Load the recorded box; release it afterwards. |
| `check_existing` / `replace_existing` | `true` / `false` | Refuse duplicates, or clear and re-check them. |
| `keep_going` | `false` | Continue after a command failure (default fail fast). |
| `verify` / `strict` | `true` / `true` | Full-state comparison; `strict=false` tolerates extra entities. |
| `verify_radius` | every entity | Radius for the verification snapshot. |
| `pos_tolerance` / `vel_tolerance` | `0.01` | Movement slack; a frozen restore reproduces the doubles. |
| `ignore_nbt_keys` | `[]` | Explicit opt-out fields (e.g. tick counters); off by default. |

`verify` (and the MCP `mc_verify` tool) runs the same comparison without
restoring - use it after an externally driven restore, or to prove "the order
hash matches but an inventory did not".

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
   or the destination bridge at the source.
4. After a successful restore, if you need to prove the copied disk entities do
   not respawn, stop the lab, start it again (a real chunk reload) and run
   `verify` again: the entity *set* must still be the recording. Tick order is
   rebuilt at load time, so a restart is a set/count check, not an order check.

## Live acceptance (26.2 Fabric lab, generic stacked chest minecarts)

The E2E run for this change used two fresh dedicated servers (no player, server
vantage, interface mod 0.5.2, Fabric loader 0.19.5, Java 25), built a 3-cart
stacked chest-minecart fixture in the source lab with distinct items in each
cart, forked it, loaded the copy into a destination lab, and restored it under
guard.

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
| guarded apply | `ok: true`, 3/3 summoned, order hash `a663c5c0dfd7ec6f` identical, positions/velocities/NBT/items identical, tick frozen and unfrozen |
| second restore without replacement | refused with 3 UUID + 3 spatial collisions and no mutation |
| `replace_existing` | two-pass kill + `save-all flush`, recheck `remainingInBox: 0`, then exact restore |
| live inventory mutation (`Items[0].count` 5/2/7 -> 64) | `verify` `ok: false`, order hash unchanged, 3 `nbtMismatches` with both inventory fragments; guarded repair `ok: true` |
| server stop/start (real chunk reload) | 3 entities, 0 missing, 0 unexpected, exact items, order hash unchanged |
| source world after the whole run | full-state comparison against `source-before`: unchanged |

The fork manifest also confirmed the 26.2 layout live: entity storage under
`dimensions/minecraft/overworld/entities/*.mca` was stripped from the copy
(`"stripped"` in the manifest), and after the reload no copied entity respawned
alongside the restored ones.
