-- The join. Two channels, two grains, one fact.
--
-- POS sells one product per order and keys the customer by its own
-- `customer_id`; the web store sells several per order and knows nobody by
-- anything but email. Neither can be turned into the other, so both are
-- projected onto a common line grain and the customer is addressed by the
-- surrogate `customer_key` that star_silver.py resolved.
--
-- The POS side has to travel through the crosswalk to get that key. The web
-- side already carries it, because the web fact grain was built after
-- resolution. That asymmetry is real and worth leaving visible.

with pos as (

    select
        o.order_id                      as order_id,
        1                               as line_no,
        x.customer_key                  as customer_key,
        o.product_id                    as product_id,
        o.order_date                    as order_date,
        o.channel                       as channel,
        o.quantity                      as quantity,
        o.unit_price                    as unit_price,
        o.amount                        as amount,
        'contoso_pos'                   as source_system

    from {{ source('silver', 'silver_orders') }} o

    -- inner join, deliberately: a POS order whose customer did not resolve has
    -- no place in a star keyed by customer. Dropping it here would be silent,
    -- so tests/assert_no_order_lines_dropped.sql counts what survives against
    -- silver, per source system.
    --
    -- That sentence used to end "so the schema test below counts what survives
    -- against silver", and there was no such test — not below, not anywhere.
    -- The comment had been standing in for the check it described.
    join {{ source('silver', 'silver_customer_xref') }} x
      on  x.source_id     = o.customer_id
      and x.source_system = 'contoso_pos'

),

web as (

    select
        w.web_order_id                  as order_id,
        w.line_no                       as line_no,
        w.customer_key                  as customer_key,
        w.product_id                    as product_id,
        w.order_date                    as order_date,
        w.channel                       as channel,
        w.quantity                      as quantity,
        w.unit_price                    as unit_price,
        w.amount                        as amount,
        'contoso_web'                   as source_system

    from {{ source('silver', 'silver_web_order_lines') }} w

)

select * from pos
union all
select * from web
