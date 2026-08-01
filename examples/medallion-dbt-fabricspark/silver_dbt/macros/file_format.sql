{#
  Force `using delta` into the DDL.

  dbt-fabricspark's own file_format_clause treats `delta` as the ONE value that
  emits nothing:

      {%- if file_format is not none and file_format != 'delta' %}
          using {{ file_format }}
      {%- elif liquid and (file_format is none or file_format == 'delta') %}
          using delta
      {%- endif %}

  That is correct FOR FABRIC, where Delta is the default table format and saying
  so is redundant. Sail's default is not Delta, so the model is created at the
  location without being a Delta table — dbt reports success, the rows are
  queryable through the engine's catalog, and OneLake never receives a
  _delta_log. Nothing downstream that reads the lakehouse can see it.

  Overriding here rather than patching the adapter keeps the workaround where a
  reader of this example will find it. Delete it the day the engine defaults to
  Delta, and check `create or replace table` still carries `using delta` first.
#}
{% macro fabricspark__file_format_clause() %}
  {%- set file_format = config.get('file_format') -%}
  {%- if file_format is not none %}
    using {{ file_format }}
  {%- endif %}
{%- endmacro %}
