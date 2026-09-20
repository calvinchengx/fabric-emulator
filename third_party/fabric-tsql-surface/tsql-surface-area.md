---
title: T-SQL Surface Area in Fabric Data Warehouse
description: T-SQL surface area of the SQL analytics endpoint and warehouse in Microsoft Fabric.
ms.reviewer: jovanpop, twinklecyril, prlangad
ms.date: 08/26/2026
ms.topic: concept-article
ms.search.form: T-SQL Surface area # This article's title should not change. If so, contact engineering.
---
# T-SQL surface area in Fabric Data Warehouse

**Applies to:** [!INCLUDE [fabric-se-dw](includes/applies-to-version/fabric-se-and-dw.md)]

This article covers the T-SQL language syntax capabilities of [!INCLUDE [product-name](../includes/product-name.md)], when querying the [!INCLUDE [fabric-se](includes/fabric-se.md)] or [!INCLUDE [fabric-dw](includes/fabric-dw.md)].

For SQL database in Fabric, see [Limitations in SQL database (preview)](../database/sql/limitations.md).

> [!NOTE]
> For more information on upcoming feature development for Fabric Data Warehouse, see the [Fabric Data Warehouse release plan](https://aka.ms/fabricrm).

## T-SQL surface area

Fabric Data Warehouse supports T-SQL tables, views, stored procedures, functions, permissions, and security roles. 

- For more information about `CREATE` and `DROP` `TABLE` support in [!INCLUDE [fabric-dw](includes/fabric-dw.md)], see [Tables](tables.md).
    - [Identity columns](identity.md) are supported in Fabric Data Warehouse.
- For more information about supported data types in [!INCLUDE [fabric-dw](includes/fabric-dw.md)], see [Data types](data-types.md).
- You can also create T-SQL views, functions, and procedures on top of the tables that reference your Delta Lake data in the [!INCLUDE [fabric-se](includes/fabric-se.md)] of the Lakehouse.
   - Creating, altering, and dropping tables, and insert, update, and delete operations are only supported in [!INCLUDE [fabric-dw](includes/fabric-dw.md)] in [!INCLUDE [product-name](../includes/product-name.md)], not in the [!INCLUDE [fabric-se](includes/fabric-se.md)] of the Lakehouse.
- Fabric Warehouse and SQL analytics endpoint both support *standard*, *sequential*, and *nested* common table expressions (CTEs). While CTEs are generally available in Microsoft Fabric, nested CTEs are currently a preview feature. For more information, see [Nested Common Table Expression (CTE) in Fabric Data Warehouse (Transact-SQL)](/sql/t-sql/queries/nested-common-table-expression?view=fabric&preserve-view=true).
- [TRUNCATE TABLE](/sql/t-sql/statements/truncate-table-transact-sql?view=fabric&preserve-view=true) is supported in [!INCLUDE [fabric-dw](includes/fabric-dw.md)].
- To change the name of the column in a user table in [!INCLUDE [fabric-dw](includes/fabric-dw.md)], use the `sp_rename` stored procedure. 
- A subset of query and join hints are supported in [!INCLUDE [fabric-dw](includes/fabric-dw.md)]. For more information, see [Hints (Transact-SQL)](/sql/t-sql/queries/hints-transact-sql-query?view=fabric&preserve-view=true).
- Session-scoped #temp tables are supported in [!INCLUDE [fabric-dw](includes/fabric-dw.md)] in [!INCLUDE [product-name](../includes/product-name.md)]. For more information, see [Temp tables in Fabric Data Warehouse](temp-tables.md).
- Currently, only the following subset of `ALTER TABLE` operations in [!INCLUDE [fabric-dw](includes/fabric-dw.md)] in [!INCLUDE [product-name](../includes/product-name.md)] are supported:
   - You can `ADD` nullable columns of supported column data types.
   - `DROP COLUMN` is supported.
   - For more information on primary keys, unique constraints, and foreign keys, see [Primary keys, foreign keys, and unique keys in Warehouse in Microsoft Fabric](table-constraints.md).
      - `ADD` `PRIMARY KEY`, `UNIQUE`, and `FOREIGN_KEY` column constraints are supported, but only if the `NOT ENFORCED` option is specified. All other `ALTER TABLE` operations are blocked. There are limitations with adding table constraints or columns when using [Git Integration for source control](git-integration.md#limitations-in-git-integration).
       - `DROP` `PRIMARY KEY`, `UNIQUE`, and `FOREIGN_KEY` column constraints are supported.
   - `ALTER TABLE` on distributed temporary tables is supported.
   - `ALTER TABLE ... ALTER COLUMN` is in preview. For more information, see [ALTER COLUMN in Fabric Data Warehouse](/sql/t-sql/statements/alter-table-transact-sql?view=fabric&preserve-view=true#syntax-for-warehouse-in-fabric).
   - You can execute supported `ALTER TABLE` statements inside an explicit user-defined transaction in [!INCLUDE [fabric-dw](includes/fabric-dw.md)] in [!INCLUDE [product-name](../includes/product-name.md)].
- `MERGE` syntax is supported and is a generally available feature. For more information, see [MERGE T-SQL syntax](/sql/t-sql/statements/merge-transact-sql?view=fabric&preserve-view=true).
- While Fabric Data Warehouse supports many [AI functions (preview)](ai-functions.md) to enable advanced text processing without leaving your warehouse, the **vector** data type isn't supported.

### Limitations

Currently, the following commands aren't supported. Don't try to use these commands. Even though they might appear to succeed, they could cause problems for your warehouse.

- `BULK LOAD`, though `bcp` is supported as a preview feature.
- `CREATE USER`
- `FOR JSON` must be the last operator in the query, so you can't use it inside subqueries.
- Manually created multi-column stats
- Materialized views
- `PREDICT`
- Queries targeting system and user tables
- Recursive queries
- Schema and table names can't contain `/` or `\`
- `SELECT` - `FOR XML`
- `SET ROWCOUNT`
- `SET TRANSACTION ISOLATION LEVEL`
- `sp_showspaceused`
- Synonyms
- Triggers
- Vector data type and search functions

## Related content

- [Query insights in Fabric Data Warehouse](query-insights.md)
- [What is Fabric Data Warehouse?](data-warehousing.md)
- [Data types in Microsoft Fabric](data-types.md)
- [Limitations of Microsoft Fabric Data Warehouse](limitations.md)
