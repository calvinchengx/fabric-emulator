{#-
  An atomic cutover for gold.

  WHY THIS OVERRIDE EXISTS

  dbt-fabric's stock `table` materialization builds into a temp table and then
  swaps it in with two SEPARATE statements:

      EXEC sp_rename 'dbo.fct_orders',            'fct_orders__dbt_backup'
      EXEC sp_rename 'dbo.fct_orders__dbt_temp',  'fct_orders'

  Both are `adapter.rename_relation()` calls, so each lands on its own round
  trip with nothing spanning them. Between the two, `dbo.fct_orders` DOES NOT
  EXIST. A reader querying in that window does not get stale data — it gets
  `Invalid object name 'fct_orders'`. The window is milliseconds, and
  milliseconds is not never: gold is the layer a semantic model and Power BI
  read, which is exactly the layer that must not disappear mid-refresh.

  The build itself was never the problem. The temp table is fully populated
  before either rename, so this is already blue-green in shape — a fresh copy
  built beside the live one, then a metadata cutover. What was missing is that
  the cutover was two steps instead of one.

  This override changes ONLY the swap. Both renames go into one explicit
  transaction, so a reader sees the old table or the new one and never the gap.

  WHY IN THE dbt PROJECT AND NOT IN THE EMULATOR

  The emulator could have rewritten the swap on the wire — it already rewrites
  CTAS to SELECT … INTO in the TDS dialect layer, so the machinery is there.
  That would have been the wrong place. The emulator's job is to behave like
  Fabric, and real Fabric running stock dbt-fabric HAS this gap. Closing it
  emulator-side would make a pipeline look atomic locally and still tear in
  production — the failure mode the parity map exists to prevent. Fixed here,
  the fix travels with the project and behaves identically against real Fabric.

  THE TRADE-OFF, STATED

  Fabric Warehouse supports DDL inside an explicit transaction, and Microsoft
  documents the cost: DDL in a transaction holds locks that can block
  concurrent DML, SELECTs on the affected tables, and queries against catalog
  views like sys.tables. So this trades a brief "table is missing" window for a
  brief "reader waits" window. A reader that blocks and then succeeds is
  strictly better than one that fails, which is why the trade is worth making —
  but it is a trade, not a free win, and long transactions here would be worse
  than the gap they fix. The transaction below spans two metadata renames and
  nothing else, deliberately: no data movement, no user SQL.

  https://learn.microsoft.com/en-us/fabric/data-warehouse/transactions
  https://learn.microsoft.com/en-us/sql/relational-databases/system-stored-procedures/sp-rename-transact-sql
-#}

{% macro fabric__atomic_swap(existing_relation, backup_relation, temp_relation, target_relation) -%}
  {#- One statement, one transaction, both renames. XACT_ABORT makes any
      failure roll the whole thing back rather than leaving the table renamed
      out and not renamed back — which would turn a missing-for-milliseconds
      table into a missing-permanently one. -#}
  {% call statement('atomic_swap') -%}
    SET XACT_ABORT ON;
    BEGIN TRANSACTION;
      EXEC sp_rename '{{ existing_relation.schema }}.{{ existing_relation.identifier }}', '{{ backup_relation.identifier }}', 'OBJECT';
      EXEC sp_rename '{{ temp_relation.schema }}.{{ temp_relation.identifier }}', '{{ target_relation.identifier }}', 'OBJECT';
    COMMIT TRANSACTION;
  {%- endcall %}
{%- endmacro %}


{% materialization table, adapter='fabric' %}

  {%- set target_relation = this.incorporate(type='table') %}
  {%- set existing_relation = adapter.get_relation(database=this.database, schema=this.schema, identifier=this.identifier) -%}

  {#- A view being converted to a table still has to be dropped first; there is
      no rename that turns one into the other. Unchanged from stock. -#}
  {% if existing_relation is not none and not existing_relation.is_table %}
    {{ log("Dropping relation " ~ existing_relation ~ " because it is of type " ~ existing_relation.type) }}
    {{ adapter.drop_relation(existing_relation) }}
  {% endif %}

  {% set grant_config = config.get('grants') %}

  {% set temp_relation = make_temp_relation(target_relation, '__dbt_temp') %}
  {{ adapter.drop_relation(temp_relation) }}

  {% set tmp_vw_relation = temp_relation.incorporate(path={"identifier": temp_relation.identifier ~ '__dbt_tmp_vw'}, type='view')-%}
  {{ adapter.drop_relation(tmp_vw_relation) }}

  {{ run_hooks(pre_hooks, inside_transaction=False) }}
  {{ run_hooks(pre_hooks, inside_transaction=True) }}

  {#- Build the new copy beside the live one. The live table is untouched and
      still serving for the whole of this. -#}
  {% call statement('main') -%}
    {{ create_table_as(False, temp_relation, sql) }}
  {% endcall %}

  {% if existing_relation is not none and existing_relation.is_table %}

    {%- set set_backup_relation = adapter.get_relation(database=this.database, schema=this.schema, identifier=this.identifier) -%}
    {% if (set_backup_relation != none and set_backup_relation.type == "table") %}
      {%- set backup_relation = make_backup_relation(target_relation, 'table') -%}
    {% elif (set_backup_relation != none and set_backup_relation.type == "view") %}
      {%- set backup_relation = make_backup_relation(target_relation, 'view') -%}
    {% endif %}

    {{ adapter.drop_relation(backup_relation) }}

    {#- THE ONE CHANGE: stock dbt-fabric calls adapter.rename_relation() twice
        here, leaving a window with no target table. One transaction instead. -#}
    {{ fabric__atomic_swap(existing_relation, backup_relation, temp_relation, target_relation) }}

    {{ adapter.drop_relation(backup_relation) }}

  {%- else %}

    {#- First build: nothing is live yet, so there is no gap to close and a
        single rename is already atomic enough. -#}
    {{ adapter.rename_relation(temp_relation, target_relation) }}

  {% endif %}

  {{ adapter.drop_relation(tmp_vw_relation) }}

  {{ run_hooks(post_hooks, inside_transaction=True) }}

  {% do apply_grants(target_relation, grant_config, should_revoke=should_revoke) %}
  {% do persist_docs(target_relation, model) %}
  {{ adapter.commit() }}

  {{ build_model_constraints(target_relation) }}
  {{ run_hooks(post_hooks, inside_transaction=False) }}
  {{ return({'relations': [target_relation]}) }}

{% endmaterialization %}
