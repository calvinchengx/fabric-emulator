-- The rows silver_orders refused, kept whole.
--
-- This model exists because silently dropping bad rows is the failure mode the
-- quarantine pattern prevents. Its row count is asserted, so a change to the
-- predicate that quietly discards more data fails the build instead of merely
-- shrinking the fact table.
--
-- The column list is asked of the warehouse at compile time rather than written
-- as `SELECT * EXCEPT (_rn)`: star-modifier syntax is not uniformly available
-- across Spark builds, and this project has to run on Sail.
--
-- TWO CTEs, not one. The one-CTE form — narrowing to the explicit column list
-- while filtering on `_rn` in the same select — failed with
--
--     attribute ObjectName([Identifier("_rn")]) is missing from the schema
--
-- This was first attributed to Sail resolving WHERE against the projected
-- schema. That attribution was WRONG: a probe ran this exact shape against Sail
-- over Spark Connect, including wrapped in a view, and every form passed. The
-- fault lies somewhere on the Livy path or in dbt's generated SQL and is not
-- yet localised.
--
-- The rewrite stays regardless. Standard SQL evaluates WHERE before projection,
-- so both forms are legal; this one keeps `_rn` in scope at the point it is
-- filtered, which is portable and costs nothing.
{% set src = source('bronze', 'bronze_orders') %}
{% set cols = adapter.get_columns_in_relation(src) %}

with latest as (

    select
        *,
        row_number() over (
            partition by order_id
            order by event_seq desc
        ) as _rn

    from {{ src }}

),

quarantined as (

    -- `_rn` is still in scope here, and stays out of the final projection.
    select * from latest
    where _rn = 1
      and (quantity <= 0 or unit_price is null)

)

select
    {% for c in cols -%}
    {{ c.name }}{% if not loop.last %},{% endif %}
    {% endfor %}

from quarantined
