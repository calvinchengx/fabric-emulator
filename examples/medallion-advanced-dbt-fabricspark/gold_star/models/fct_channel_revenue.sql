-- What the star is for: revenue compared ACROSS channels.
--
-- This is the query the single-source example could not express at all. It
-- needs the customer dimension to be one row per person rather than per source
-- system, or a customer who buys in both channels is counted as two people and
-- the country split is wrong.

select
    f.order_date,
    f.channel,
    c.country,
    count(*)            as lines,
    sum(f.quantity)     as units,
    sum(f.amount)       as revenue

from {{ ref('fct_order_lines') }} f
join {{ ref('dim_customer') }} c
  on c.customer_key = f.customer_key

group by f.order_date, f.channel, c.country
