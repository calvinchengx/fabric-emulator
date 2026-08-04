package tsql

import (
	"reflect"
	"testing"
)

// The shapes below are the statements dbt-fabric actually ships, captured from
// a real `dbt run` against the emulator (the medallion-advanced example's
// gold_star build) — not invented SQL.

func one(t *testing.T, sql string) Flow {
	t.Helper()
	flows := DataFlows(sql)
	if len(flows) != 1 {
		t.Fatalf("DataFlows(%q) = %d flows (%+v); want 1", sql, len(flows), flows)
	}
	return flows[0]
}

func TestDataFlowCTASInsideExec(t *testing.T) {
	// dbt's table materialization: EXEC-wrapped CTAS from its temp view.
	sql := `EXEC('CREATE TABLE [f590f0f7].[dbo].[dim_customer_360__dbt_temp]  AS SELECT * FROM [f590f0f7].[dbo].[dim_customer_360__dbt_temp__dbt_tmp_vw] OPTION (LABEL = ''dbt-fabric-dw'');')`
	f := one(t, sql)
	if f.Kind != FlowCTAS ||
		!reflect.DeepEqual(f.Target, []string{"f590f0f7", "dbo", "dim_customer_360__dbt_temp"}) ||
		!reflect.DeepEqual(f.Sources, [][]string{{"f590f0f7", "dbo", "dim_customer_360__dbt_temp__dbt_tmp_vw"}}) {
		t.Fatalf("exec ctas = %+v", f)
	}
}

func TestDataFlowCreateViewAcrossDatabases(t *testing.T) {
	// The temp view holds the model body: three-part refs into the reflected
	// lakehouse database — the cross-item edges the graph needs.
	sql := `create view dbo.fct_orders__dbt_tmp_vw as
	  with orders as (select * from [lake-guid].[dbo].[silver_orders]),
	       cust as (select customer_id from [lake-guid].[dbo].[silver_customers])
	  select o.*, c.customer_id
	  from orders o
	  join cust c on o.customer_id = c.customer_id
	  left join [wh-guid].[dbo].[dim_date] d on o.order_date = d.date_key`
	f := one(t, sql)
	if f.Kind != FlowCreateView || !reflect.DeepEqual(f.Target, []string{"dbo", "fct_orders__dbt_tmp_vw"}) {
		t.Fatalf("view flow = %+v", f)
	}
	want := [][]string{
		{"lake-guid", "dbo", "silver_orders"},
		{"lake-guid", "dbo", "silver_customers"},
		{"wh-guid", "dbo", "dim_date"},
	}
	if !reflect.DeepEqual(f.Sources, want) {
		t.Fatalf("view sources = %v; want %v (CTE aliases excluded, bodies scanned)", f.Sources, want)
	}
}

func TestDataFlowRename(t *testing.T) {
	// dbt's swap: EXEC sp_rename over the temp name. Positional and named
	// argument spellings, and the object-kind guard.
	f := one(t, `EXEC sp_rename 'dbo.dim_customer_360__dbt_temp', 'dim_customer_360'`)
	if f.Kind != FlowRename ||
		!reflect.DeepEqual(f.Target, []string{"dbo", "dim_customer_360__dbt_temp"}) ||
		f.NewName != "dim_customer_360" {
		t.Fatalf("rename = %+v", f)
	}
	f = one(t, `sp_rename @objname = 'dbo.t_old', @newname = 't_new', @objtype = 'OBJECT'`)
	if f.Kind != FlowRename || f.NewName != "t_new" {
		t.Fatalf("named-args rename = %+v", f)
	}
	// A COLUMN rename is not an identity move for lineage.
	if flows := DataFlows(`EXEC sp_rename 'dbo.t.old_col', 'new_col', 'COLUMN'`); flows != nil {
		t.Fatalf("column rename produced flows: %+v", flows)
	}
}

func TestDataFlowSelectIntoAndInsert(t *testing.T) {
	f := one(t, `SELECT a, b INTO dbo.dst FROM dbo.src WHERE x = 1`)
	if f.Kind != FlowSelectInto ||
		!reflect.DeepEqual(f.Target, []string{"dbo", "dst"}) ||
		!reflect.DeepEqual(f.Sources, [][]string{{"dbo", "src"}}) {
		t.Fatalf("select into = %+v", f)
	}
	f = one(t, `INSERT INTO dbo.dst (a, b) SELECT a, b FROM dbo.src JOIN dbo.ref ON src.k = ref.k`)
	if f.Kind != FlowInsert || len(f.Sources) != 2 {
		t.Fatalf("insert select = %+v", f)
	}
	// INSERT … VALUES moves no table.
	if flows := DataFlows(`INSERT INTO dbo.dst (a) VALUES (1)`); flows != nil {
		t.Fatalf("insert values produced flows: %+v", flows)
	}
}

func TestDataFlowDrop(t *testing.T) {
	flows := DataFlows(`drop view if exists dbo.fct_orders__dbt_tmp_vw`)
	if len(flows) != 1 || flows[0].Kind != FlowDropView {
		t.Fatalf("drop view = %+v", flows)
	}
	flows = DataFlows(`DROP TABLE IF EXISTS dbo.a, dbo.b`)
	if len(flows) != 2 || flows[0].Kind != FlowDropTable || flows[1].Target[1] != "b" {
		t.Fatalf("drop table list = %+v", flows)
	}
}

func TestDataFlowIgnoresWhatItShould(t *testing.T) {
	for _, sql := range []string{
		`SELECT * FROM dbo.t`,                          // a read moves nothing
		`CREATE TABLE dbo.t (a int)`,                   // DDL without AS
		`SELECT name FROM sys.tables`,                  // catalog read
		`SELECT x INTO #tmp FROM dbo.t`,                // temp target
		`UPDATE dbo.t SET a = 1`,                       // no table-to-table movement modelled
		`EXEC(@dynamic_sql)`,                           // unknowable content
		`EXEC('EXEC(''SELECT 1 INTO dbo.two_deep'')')`, // second level stays alone
		`this is not sql at all`,
	} {
		if flows := DataFlows(sql); flows != nil {
			t.Errorf("DataFlows(%q) = %+v; want nil", sql, flows)
		}
	}
}

func TestDataFlowTVFAndDerivedTables(t *testing.T) {
	f := one(t, `SELECT v.x INTO dbo.dst
	  FROM (SELECT x FROM dbo.real_src) v
	  CROSS APPLY string_split(v.x, ',')
	  JOIN openrowset('x') r ON 1=1`)
	if !reflect.DeepEqual(f.Sources, [][]string{{"dbo", "real_src"}}) {
		t.Fatalf("sources = %v; want only dbo.real_src (TVFs and derived aliases excluded)", f.Sources)
	}
}

// The gold materialization wraps dbt's two renames in ONE transaction (see
// examples/*/gold_star/macros/table_atomic_swap.sql), so the batch the TDS
// front observes now carries SET/BEGIN/COMMIT around them. The observer must
// still see both renames — otherwise gold's lineage lands on __dbt_temp
// scaffolding and the real table appears to come from nowhere.
func TestDataFlowsFollowsRenamesInsideATransaction(t *testing.T) {
	batch := `SET XACT_ABORT ON;
BEGIN TRANSACTION;
  EXEC sp_rename 'dbo.fct_orders', 'fct_orders__dbt_backup', 'OBJECT';
  EXEC sp_rename 'dbo.fct_orders__dbt_temp', 'fct_orders', 'OBJECT';
COMMIT TRANSACTION;`
	var renames []Flow
	for _, f := range DataFlows(batch) {
		if f.Kind == FlowRename {
			renames = append(renames, f)
		}
	}
	if len(renames) != 2 {
		t.Fatalf("got %d rename(s) from the transactional swap, want 2", len(renames))
	}
	if renames[0].NewName != "fct_orders__dbt_backup" || renames[1].NewName != "fct_orders" {
		t.Errorf("renames = %q, %q", renames[0].NewName, renames[1].NewName)
	}
	if got := renames[1].Target[len(renames[1].Target)-1]; got != "fct_orders__dbt_temp" {
		t.Errorf("second rename target = %q; want the temp table", got)
	}
}

// TestDataFlowExcludesNestedCTENames pins the fix for a PHANTOM lineage edge.
//
// CTE names were collected from the leading WITH only, while FROM/JOIN are read
// at every depth — so a CTE defined in a nested WITH was reported as a source
// TABLE. Nothing downstream rejects it: warehouseLineage.resolve does not verify
// the table exists, so a single-part name resolves against the connection's own
// item and a real lineage edge is written for a table that never existed. A
// catalog inventing provenance is worse than one missing it.
//
// Nested CTEs are a supported, documented shape here, so this is expected
// traffic rather than an exotic input.
func TestDataFlowExcludesNestedCTENames(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			name: "CTE nested inside a CTE",
			sql: `CREATE TABLE t AS
			  WITH a AS (WITH b AS (SELECT * FROM src) SELECT * FROM b)
			  SELECT * FROM a`,
		},
		{
			name: "CTE inside a derived table",
			sql:  `SELECT x INTO t FROM (WITH b AS (SELECT * FROM src) SELECT * FROM b) q`,
		},
		{
			name: "two levels of nesting",
			sql: `CREATE TABLE t AS
			  WITH a AS (WITH b AS (WITH c AS (SELECT * FROM src) SELECT * FROM c) SELECT * FROM b)
			  SELECT * FROM a`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := one(t, tc.sql)
			if !reflect.DeepEqual(f.Sources, [][]string{{"src"}}) {
				t.Errorf("Sources = %+v; want only [[src]] — a CTE alias was "+
					"reported as a table, which becomes a lineage edge to a "+
					"table that does not exist", f.Sources)
			}
		})
	}
}

// A `WITH` that is not a CTE clause must not swallow the real source: the
// table-hint and CTAS table-option forms both put `(` where a CTE name goes.
func TestDataFlowWithClauseThatIsNotACTE(t *testing.T) {
	for _, sql := range []string{
		`CREATE TABLE t AS SELECT a FROM src WITH (NOLOCK)`,
		`CREATE TABLE t WITH (DISTRIBUTION = ROUND_ROBIN) AS SELECT a FROM src`,
	} {
		f := one(t, sql)
		if !reflect.DeepEqual(f.Sources, [][]string{{"src"}}) {
			t.Errorf("DataFlows(%q) sources = %+v; want [[src]]", sql, f.Sources)
		}
	}
}
