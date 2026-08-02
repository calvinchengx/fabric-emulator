-- Nothing was lost between silver and the fact, per source system.
--
-- fct_order_lines.sql joins the POS side to silver_customer_xref on an INNER
-- join, deliberately: an order whose customer did not resolve has no place in a
-- star keyed by customer. But "deliberately dropped" and "silently dropped"
-- look identical downstream — the star still builds, every dimension still
-- joins, and the revenue is simply smaller. This test is what makes the choice
-- honest rather than merely stated.
--
-- Both sides are checked, not just POS. The web side selects straight from
-- silver_web_order_lines with no join, so nothing CAN drop there today — which
-- is exactly why it is worth pinning, because the day someone adds a join it
-- will look like the POS side and nobody will re-derive this argument.
--
-- The LEFT JOIN plus coalesce is load-bearing: if a source system stopped
-- contributing entirely, an inner join here would drop the row that proves it
-- and the test would pass by having nothing to compare.
--
-- This is NOT the same check as gold_star.py's assertion on the same counts.
-- That one compares against the FIXTURE constants and catches "silver produced
-- the wrong number of rows". This one compares against the silver tables as
-- they actually are, and catches "gold lost rows relative to whatever silver
-- produced". Either can fail with the other passing.

with expected as (

    select
        'contoso_pos'   as source_system,
        count(*)        as n
    from {{ source('silver', 'silver_orders') }}

    union all

    select
        'contoso_web'   as source_system,
        count(*)        as n
    from {{ source('silver', 'silver_web_order_lines') }}

),

actual as (

    select
        source_system,
        count(*) as n
    from {{ ref('fct_order_lines') }}
    group by source_system

)

select
    e.source_system,
    e.n                     as in_silver,
    coalesce(a.n, 0)        as in_star,
    e.n - coalesce(a.n, 0)  as dropped

from expected e
left join actual a
  on a.source_system = e.source_system

where coalesce(a.n, 0) <> e.n
