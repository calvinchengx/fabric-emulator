-- Country must be one of the three conformed codes silver produces.
-- (The CTE-free spelling of `accepted_values` — see models/schema.yml.)
select customer_id, country
from {{ ref('dim_customer') }}
where country not in ('US', 'GB', 'SG')
