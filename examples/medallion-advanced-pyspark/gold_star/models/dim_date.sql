-- A date dimension derived from the facts, not a generated calendar.
--
-- Fabric Warehouse does not support recursive CTEs, so the usual
-- generate-a-calendar trick needs a numbers table this example does not have.
-- Deriving the grain from the dates that actually occur is the honest version:
-- it cannot invent a day nobody traded on, and it cannot be wrong about the
-- range. The limitation is real and worth naming — there are no gap rows, so a
-- report asking "which days had no sales" gets nothing back from this table.

select distinct
    order_date                                  as date_key,
    year(order_date)                            as calendar_year,
    month(order_date)                           as calendar_month,
    day(order_date)                             as calendar_day,
    datepart(iso_week, order_date)              as iso_week

from {{ ref('fct_order_lines') }}
where order_date is not null
