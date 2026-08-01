select order_id, customer_id, order_date, quantity, amount
from {{ source('silver', 'silver_orders') }}
