-- A business rule, not a schema shape: revenue is never zero or negative.
select order_date, country, revenue
from {{ ref('fct_daily_revenue') }}
where revenue <= 0
