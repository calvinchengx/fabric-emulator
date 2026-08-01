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

)

select
    {% for c in cols -%}
    {{ c.name }}{% if not loop.last %},{% endif %}
    {% endfor %}

from latest
where _rn = 1
  and (quantity <= 0 or unit_price is null)
