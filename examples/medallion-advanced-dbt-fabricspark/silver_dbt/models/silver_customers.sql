-- Silver customers, declaratively: dedupe, lower-case the email, conform the
-- country. The same three rules ../medallion-pyspark/silver.py applies
-- imperatively, so the two builds can be compared row for row.
--
-- The record stays WIDE — silver is the conformed customer-360, and the star's
-- dimensions are a projection of it rather than the other way round. That is
-- awkward in SQL when the source has ~100 generated columns, so the column list
-- is asked of the warehouse at compile time instead of being typed out or
-- papered over with `SELECT * EXCEPT`, which not every Spark build supports.
{% set src = source('bronze', 'bronze_customers') %}
{% set cols = adapter.get_columns_in_relation(src) %}

with ranked as (

    select
        *,
        -- The vendor repeats a share of its rows verbatim. row_number over the
        -- key keeps exactly one; a bare DISTINCT would also collapse rows that
        -- differ in some other column, which is a different and wrong rule.
        row_number() over (partition by customer_id order by customer_id) as _rn

    from {{ src }}

)

select
    {% for c in cols if c.name not in ['email', 'country'] -%}
    {{ c.name }},
    {% endfor -%}

    -- Case-folded, and '' rather than NULL for "the vendor sent none": the
    -- missing-email cohort has to stay identifiable, because it is the cohort
    -- that can never be matched to an email-keyed system.
    lower(trim(coalesce(email, ''))) as email,

    -- Silver's own business rule, written out rather than derived from the
    -- generator's COUNTRY_VARIANTS. Importing that mapping would make the
    -- conformance assertion agree with itself, and a new variant appearing
    -- upstream would silently conform instead of failing.
    case upper(trim(country))
        when 'US' then 'US'
        when 'USA' then 'US'
        when 'U.S.' then 'US'
        when 'UNITED STATES' then 'US'
        when 'GB' then 'GB'
        when 'U.K.' then 'GB'
        when 'UK' then 'GB'
        when 'GBR' then 'GB'
        when 'UNITED KINGDOM' then 'GB'
        when 'SG' then 'SG'
        when 'SGP' then 'SG'
        when 'SINGAPORE' then 'SG'
        else upper(trim(country))
    end as country

from ranked
where _rn = 1
