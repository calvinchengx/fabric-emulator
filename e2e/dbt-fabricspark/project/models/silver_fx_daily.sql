{{ config(materialized="table") }}

-- FX rates carried forward onto a dense daily calendar.
--
-- WHY THIS MODEL EXISTS, and why the shape matters. The vendor quotes business
-- days only, so a naive join against orders drops every weekend silently. This
-- densifies the calendar and carries the last quote forward, and `rate_is_
-- carried` says which rows are a carry rather than a fresh quote.
--
-- It is also the only model here whose result carries TYPED columns: two dates
-- and a decimal. That is not decoration. The agent used to hand a SQL result to
-- json.dumps untouched, so any row carrying a date raised inside the HTTP
-- handler, the reply was never written, and the caller saw a bare
-- RemoteDisconnected — indistinguishable from a network fault. `stg_countries`
-- could not catch it: two string columns encode fine.
--
-- The shape came from a consumer project that hit the bug for real, driving
-- this adapter against its own warehouse. A model returning dates incidentally
-- is much closer to how this reaches a user than one written to return one.
with calendar as (
    select explode(sequence(to_date('2026-01-01'), to_date('2026-01-07'), interval 1 day)) as rate_date
),

quotes as (
    select
        to_date(quoted_on) as quoted_on,
        currency,
        cast(rate_to_usd as decimal(19, 6)) as rate_to_usd
    from {{ ref('fx_rates') }}
),

currencies as (
    select distinct currency from quotes
),

grid as (
    select
        calendar.rate_date,
        currencies.currency
    from calendar
    cross join currencies
),

joined as (
    select
        grid.rate_date,
        grid.currency,
        quotes.rate_to_usd,
        quotes.quoted_on
    from grid
    left join quotes
        on grid.currency = quotes.currency
        and grid.rate_date = quotes.quoted_on
)

select
    rate_date,
    currency,
    -- The carry: the most recent quote at or before this date. `true` is
    -- ignoreNulls, which is what makes it a carry rather than a null.
    last_value(rate_to_usd, true) over (
        partition by currency order by rate_date
        rows between unbounded preceding and current row
    ) as rate_to_usd,
    last_value(quoted_on, true) over (
        partition by currency order by rate_date
        rows between unbounded preceding and current row
    ) as quoted_on,
    rate_to_usd is null as rate_is_carried
from joined
