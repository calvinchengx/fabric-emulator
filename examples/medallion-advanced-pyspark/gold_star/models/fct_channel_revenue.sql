-- What the star is for: revenue compared ACROSS channels.
--
-- This is the query the single-source example could not express at all. It
-- needs the customer dimension to be one row per person rather than per source
-- system, or a customer who buys in both channels is counted as two people and
-- the country split is wrong.

-- GROUPED BY source_system AS WELL AS channel, and that is not defensive
-- padding — the two systems use one word for different things.
--
-- Contoso POS records a `channel` per order and "web" is one of its five
-- (store, web, app, phone, partner). The Contoso Web store is a separate
-- SYSTEM whose lines are all, trivially, web. Grouping on channel alone puts
-- 50,871 POS web-channel orders in the same bucket as every web-store line and
-- reports the sum as "web": 69,131,193.28 where the web store alone made
-- 54,037,650.48. The difference, 15,093,542.80, is POS's own web channel.
--
-- Collapsing them would be averaging over the exact thing this example is
-- about. A word meaning different things in two systems is what conformance
-- has to survive, and the honest answer is to keep both and say which is which
-- rather than pick one meaning and lose the other.
select
    f.order_date,
    f.source_system,
    f.channel,
    c.country,
    count(*)            as lines,
    sum(f.quantity)     as units,
    sum(f.amount)       as revenue

from {{ ref('fct_order_lines') }} f
join {{ ref('dim_customer_360') }} c
  on c.customer_key = f.customer_key

group by f.order_date, f.source_system, f.channel, c.country
