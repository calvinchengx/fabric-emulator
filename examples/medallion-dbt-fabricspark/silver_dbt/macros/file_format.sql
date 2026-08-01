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
  so is redundant. It is correct NOWHERE ELSE, and that was measured on both
  engines rather than assumed:

      Sail  Invalid table location: No commit files found in _delta_log
      JVM   [NOT_SUPPORTED_COMMAND_WITHOUT_HIVE_SUPPORT]
            CREATE Hive TABLE (AS SELECT) is not supported

  With no USING clause Spark treats the statement as a HIVE table — not Delta,
  not parquet. So this is NOT a Sail workaround waiting for a better engine: it
  is the price of running dbt-fabricspark anywhere that is not Fabric, and it
  would still be needed on the JVM overlay. Do not delete it on the grounds that
  the engine improved.

  Without it dbt reports success, the rows are queryable through the engine's
  catalog, and OneLake never receives a _delta_log — so nothing that reads the
  lakehouse can see the model. See docs/engine-matrix.md.
#}
{% macro fabricspark__file_format_clause() %}
  {%- set file_format = config.get('file_format') -%}
  {%- if file_format is not none %}
    using {{ file_format }}
  {%- endif %}
{%- endmacro %}
