-- The catalogue, plus the products only the transactions know about.
--
-- Only the web store publishes a catalogue. POS emits a product_id on every
-- order and no catalogue at all, and the ERP hierarchy names departments above
-- a category without listing products. So a product dimension built from the
-- catalogue alone would silently drop every POS-only product — the join would
-- succeed and the revenue would go missing.
--
-- Building it from the UNION of what the catalogue lists and what the facts
-- reference is what makes the fact-to-dimension join total. Products seen only
-- in transactions arrive with a null name and are marked, rather than being
-- excluded or given an invented one.

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
