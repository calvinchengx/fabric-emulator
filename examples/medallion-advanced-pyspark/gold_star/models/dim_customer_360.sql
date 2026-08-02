-- One row per resolved person, not per source record.
--
-- ../gold/models/dim_customer.sql selects straight from silver_customers and is
-- therefore a POS dimension wearing a general name. This one is keyed by the
-- surrogate that spans three systems, and carries the ERP commercial attributes
-- that neither of the other two holds.
--
-- _360 IS IN THE NAME BECAUSE BOTH STARS ARE REAL. This pipeline builds the
-- single-source star at step 8 and this one at step 20, into the same warehouse
-- and the same dbo schema. While this model was also called `dim_customer` it
-- OVERWROTE the other one — same name, different key — and the last two steps
-- then read `customer_id` off a table that now had `customer_key`. The failure
-- surfaced as `Invalid column name 'customer_id'` in contract_gates, naming
-- neither star.
--
-- It stayed hidden because step 20 had never once succeeded: the clobber cannot
-- happen until the model it belongs to builds. Fixing gold_star.py is what made
-- this reachable, which is the honest reason it appears now and not earlier.
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
