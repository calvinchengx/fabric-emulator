-- Every product the facts reference, enriched by the catalogue where it has
-- something to say.
--
-- Only the web store publishes a catalogue. POS emits a product_id on every
-- order and no catalogue at all, and the ERP hierarchy names departments above
-- a category without listing products. So a product dimension built from the
-- catalogue alone would silently drop every POS-only product — the join would
-- succeed and the revenue would go missing.
--
-- The grain is therefore `referenced` — the distinct product_ids in
-- fct_order_lines — LEFT JOINed to the catalogue and the hierarchy. That is
-- what makes the fact-to-dimension join total, which is the property the
-- `relationships` test in schema.yml enforces. Products seen only in
-- transactions arrive with a null name and are marked `is_uncatalogued`,
-- rather than being excluded or given an invented one.
--
-- WHAT THIS DIMENSION IS NOT: the sellable catalogue. A product the web store
-- lists but nobody has ever bought does not appear here, because nothing in the
-- fact references it. "Products transacted" and "products offered" are
-- different questions, and this table answers the first — a dimension conformed
-- to its fact, which is what keeps the join total.
--
-- This header used to claim the model was built from the UNION of the two, and
-- it never was. The claim survived because at this fixture size the two
-- readings coincide exactly: POS and the web store transact the same eight
-- catalogue ids, so "referenced" and "catalogued" are the same eight rows and
-- no query could tell the descriptions apart. Answering "how many products do
-- we sell?" from this table would be wrong the first time a listing went
-- unsold, and nothing here would report it.

with catalogue as (

    select
        p.product_id,
        p.name          as product_name,
        p.category      as category
    from {{ source('silver', 'bronze_web_products') }} p

),

referenced as (

    select distinct product_id
    from {{ ref('fct_order_lines') }}

),

hierarchy as (

    -- Joined on PRODUCT_ID, not category. The ERP hierarchy carries one row per
    -- PRODUCT, so category is one-to-many here and joining on it fans the
    -- dimension out — a product in a category with N products came back N
    -- times. The `unique` test on product_id caught it (8 duplicates); without
    -- that test the star would have silently over-counted every measure joined
    -- through dim_product.
    select
        h.product_id,
        h.category,
        h.department,
        h.segment
    from {{ source('silver', 'bronze_product_hierarchy') }} h

)

select
    r.product_id,
    c.product_name,
    coalesce(c.category, h.category) as category,
    h.department,
    h.segment,
    case when c.product_id is null then 1 else 0 end as is_uncatalogued

from referenced r
left join catalogue c on c.product_id = r.product_id
left join hierarchy h on h.product_id = r.product_id
