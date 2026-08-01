select
    o.order_date,
    c.country,
    count(*)        as orders,
    sum(o.quantity) as units,
    sum(o.amount)   as revenue
from {{ ref('fct_orders') }} o
join {{ ref('dim_customer') }} c
  on o.customer_id = c.customer_id
group by o.order_date, c.country
