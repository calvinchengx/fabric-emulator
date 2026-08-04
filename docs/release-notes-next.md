# Release notes — next tag (draft)

Draft for the tag after `v0.16.0`. **Suggest `v0.16.1`:** it corrects a claim
`v0.16.0`'s notes made and its binary did not honour, plus two type-map
divergences of the same class. No new surface, no removals.

Not published. Cut the tag when you are ready; nothing here tags or publishes.

---

## The nested-column omission now actually happens

`v0.16.0` announced that `struct`/`array`/`map` columns are omitted, and shipped
a binary that served them as `varchar` full of `NULL`. Reported by
contoso-data-platform against the released image:

```
probe_nested columns: ['web_order_id', 'lines', 'addr', 'tags']
values:               web_order_id='W-1'  lines=None  addr=None  tags=None
```

The reader dropped them correctly; `ReadDeltaTable` then re-projected each part
onto the logical schema from the Delta log — which still names the nested
fields — so every one came back with a nil value, took `sqlType`'s `varchar`
default (no non-null value is ever seen) and served as NULL. The `Skipped` list
was dropped in the same step, so the "not representable … omitted" warning never
fired for exactly the tables that needed it.

**If you wrote a check against `0.16.0`'s actual behaviour, it moves.** A nested
column is now absent from `INFORMATION_SCHEMA` rather than present-and-NULL.
Assert absence: that is what Fabric does, and `SELECT lines` against Fabric
fails with an invalid column name rather than returning NULL.

The direction of the failure is the reason this is worth a patch rather than a
doc note. The emulator was **more permissive than the thing it emulates**, which
is the one asymmetry an emulator must not have: code passes locally and fails in
Fabric.

The nested set now comes from the Delta schema rather than from whichever data
file is read first. After a schema evolution that adds a nested column the
oldest file does not carry it at all, so a first-file heuristic re-adds it for
every later file — the same bug back again for one specific table history.

## Two more type-map widths

Both off Microsoft's documented Delta→SQL map, both failing in the same
direction — one width too wide, with nothing raised:

| Delta | before | now |
|---|---|---|
| `tinyint`, `byte`, `smallint`, `short` | `int` | `smallint` |
| `float`, `real` | `float` | `real` |

The integer widths are the `date` bug's exact shape: physically an INT32 like
any other, with the width living only in the `INT(8,true)` / `INT(16,true)`
annotation the reader discarded. `real` was milder in cause — FLOAT and DOUBLE
differ in the *physical* kind — and identical in effect: `real` and `double`
both reflected as `float`, so the two were indistinguishable at the endpoint.

**If you pinned `int` for a smallint column, or `float` for a real column, it
moves.** `double` is unchanged and still maps to `float`.

## Correctness

- **Pipeline expressions read every integer column as `0`.** Pre-existing and
  unrelated to the widths above, found while checking what consumes the reader's
  values: `toNumber` listed only `float64` and `int`, but a Lookup over a Delta
  table or Parquet file puts the reader's own types into the row map — `int32`
  for a Delta int, `int64` for a bigint. So
  `@activity('L').output.firstRow.amount` on a bigint column evaluated to `0`,
  silently, and every expression built on it was wrong. `toBool` had the same
  hole, where the consequence is a wrong branch rather than a wrong number.

## Known issues

- `decimal` in the mirror (SQL→Delta) direction carries precision and scale; the
  nested types have no kind at all in that direction either.
- The trigger activation bound cannot distinguish 50 nested activations from 50
  concurrent independent ones; it bounds both.
- Unsigned 8- and 16-bit Parquet annotations still reflect as `int` rather than
  `smallint`. Delta has no unsigned types, so this is unreachable in practice,
  and narrowing a `uint16` up to 65535 into an `int16` would be lossy — reflecting
  one width too wide is the safe direction when the width cannot be trusted.
