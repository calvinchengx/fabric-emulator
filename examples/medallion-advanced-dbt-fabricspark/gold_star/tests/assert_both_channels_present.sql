-- A business rule, not a schema shape: the star exists to compare channels, so
-- a build in which one channel contributed nothing is a broken pipeline, not an
-- empty quarter. Without this the union could silently degrade to POS-only —
-- every other test would still pass, and the cross-channel report would simply
-- show one bar.
select 1 as failed
from (
    select count(distinct channel) as channels
    from {{ ref('fct_order_lines') }}
) c
where c.channels < 2
