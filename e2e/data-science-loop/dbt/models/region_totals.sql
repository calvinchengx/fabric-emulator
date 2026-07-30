select
    region,
    sum(amount) as total_amount,
    count(*) as order_count
from {{ source('onelake', 'sales') }}
group by region
