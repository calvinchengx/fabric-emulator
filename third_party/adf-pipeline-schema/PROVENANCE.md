# ADF/Synapse `entityTypes/Pipeline.json` — copied in full

The published schema for Data Factory / Synapse pipeline **activities**: the
`x-ms-discriminator-value` of every activity type ADF defines. The emulator
accepts this vocabulary as a compatibility surface beside Fabric's own
(third_party/fabric-activity-types), so it is an oracle in exactly the same way
and is now checked in exactly the same way.

## Provenance

- **Upstream:** https://github.com/Azure/azure-rest-api-specs
- **File:** `specification/synapse/data-plane/Microsoft.Synapse/preview/2021-06-01-preview/entityTypes/Pipeline.json`
- **Pinned revision:** `e773d51070bcccd61bafc30ccfedb96b261a0754`
- **Integrity:** `sha256:d4d1b9cadf9b8f17ec3bef9b0eb7a8cfcfa0d6eb4ccd48a63bf11cd14a4ea951`, 270562 bytes (`Pipeline.json`)
- **License:** MIT (`LICENSE`) — © Microsoft Corporation.
- **Used by:** `scripts/check_adf_activity_types.py` (in `make check` and CI),
  which asserts every CONCRETE ADF activity type is handled by the dispatch,
  the pipeline interpreter, or the refusal map — never the success stub.

## Concrete vs abstract, and why the checker derives it

Three of the 41 discriminators are not authorable activity types: `Container`
(`ControlActivity`) and `Execution` (`ExecutionActivity`) are **base classes**
other definitions inherit from. The checker excludes them by DERIVING which
definitions are `allOf` bases, rather than carrying an exemption list — an
exemption list is the thing this whole exercise exists to remove.

## Refresh

    scripts/vendor_adf_pipeline_schema.py --sha <newer-commit>

Bump deliberately, in its own commit, and diff the schema: a change here is a
change to the golden truth the dispatch is measured against.
