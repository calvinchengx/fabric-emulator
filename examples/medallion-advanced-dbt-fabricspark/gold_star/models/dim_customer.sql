-- One row per resolved person, not per source record.
--
-- ../gold/models/dim_customer.sql selects straight from silver_customers and is
-- therefore a POS dimension wearing a general name. This one is keyed by the
-- surrogate that spans three systems, and carries the ERP commercial attributes
-- that neither of the other two holds.
--
-- in_pos / in_web / in_erp are kept deliberately: a dimension that cannot tell
-- you which systems know a customer makes "we have 100,000 customers" an
-- unanswerable claim.

select
    customer_key,
    name,
    email,
    country,
    account_tier,
    segment,
    credit_band,
    in_pos,
    in_web,
    in_erp,
    source_count

from {{ source('silver', 'silver_customer_conformed') }}
