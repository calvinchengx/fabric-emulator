-- Silver orders, declaratively. Latest event wins per order, malformed events
-- diverted to quarantine, `amount` computed once here so gold cannot compute it
-- differently.
--
-- The window is the whole point: the vendor delivers AT LEAST ONCE, so an
-- order_id appears more than once and only the last event by the vendor's own
-- sequence is true. A `distinct` or a `group by` would pick an arbitrary row —
-- correct by luck until the redelivery carries a changed status.

with latest as (

    select
        *,
        row_number() over (
            partition by order_id
            order by event_seq desc
        ) as _rn

    from {{ source('bronze', 'bronze_orders') }}

),

deduped as (

    select * from latest where _rn = 1

)

select
    order_id,
    customer_id,
    product_id,
    cast(order_date as date)        as order_date,
    channel,
    store_id,
    currency,
    discount_pct,
    tax_rate,
    shipping_fee,
    payment_method,
    is_gift,
    promo_code,
    quantity,
    unit_price,
    quantity * unit_price           as amount,
    status

from deduped

-- The quarantine predicate, inverted. Rows failing it are NOT dropped: they go
-- to silver_quarantine_orders, and the two models together account for every
-- deduplicated event.
where quantity > 0
  and unit_price is not null
