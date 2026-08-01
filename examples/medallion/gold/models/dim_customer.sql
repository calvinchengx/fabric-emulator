select customer_id, name, email, country
from {{ source('silver', 'silver_customers') }}
