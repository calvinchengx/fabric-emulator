-- Referential integrity: every order resolves to a customer.
-- (The CTE-free spelling of `relationships` — see models/schema.yml.)
select o.order_id
from {{ ref('fct_orders') }} o
left join {{ ref('dim_customer') }} c on o.customer_id = c.customer_id
where c.customer_id is null
