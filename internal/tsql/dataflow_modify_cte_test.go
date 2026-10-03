package tsql

import (
	"reflect"
	"testing"
)

// A WITH clause can lead any DML statement. Before it was read through, the
// statement classified as a query, produced no flow, and a Warehouse table
// changed by it silently got no version (docs/35-warehouse-time-travel.md).
func TestDataFlowCTELedDML(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want Flow
	}{
		{"insert",
			"WITH c AS (SELECT id FROM dbo.src) INSERT INTO dbo.t (id) SELECT id FROM c",
			Flow{Kind: FlowInsert, Target: []string{"dbo", "t"}, Sources: [][]string{{"dbo", "src"}}}},
		{"delete",
			"WITH c AS (SELECT id FROM dbo.src) DELETE FROM dbo.t WHERE id IN (SELECT id FROM c)",
			Flow{Kind: FlowModify, Target: []string{"dbo", "t"}}},
		{"update",
			"WITH c AS (SELECT id FROM dbo.src), d AS (SELECT 1 AS x) UPDATE dbo.t SET v = 1 WHERE id IN (SELECT id FROM c)",
			Flow{Kind: FlowModify, Target: []string{"dbo", "t"}}},
		{"merge",
			"WITH c AS (SELECT id FROM dbo.src) MERGE INTO dbo.t AS tgt USING c ON tgt.id = c.id WHEN MATCHED THEN DELETE;",
			Flow{Kind: FlowModify, Target: []string{"dbo", "t"}}},
		{"delete through the cte",
			"WITH c AS (SELECT * FROM dbo.t WHERE v > 1) DELETE FROM c",
			Flow{Kind: FlowModify, Target: []string{"dbo", "t"}}},
		{"update through the cte",
			"WITH c AS (SELECT * FROM dbo.t) UPDATE c SET v = 2",
			Flow{Kind: FlowModify, Target: []string{"dbo", "t"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flows := DataFlows(tc.sql)
			if len(flows) != 1 || !reflect.DeepEqual(flows[0], tc.want) {
				t.Fatalf("DataFlows(%q) = %+v; want [%+v]", tc.sql, flows, tc.want)
			}
		})
	}
}

// A WITH-led query that writes nothing stays a non-flow, and a CTE-led
// SELECT … INTO keeps its CTAS reading.
func TestDataFlowCTELedQueryUnchanged(t *testing.T) {
	if f := DataFlows("WITH c AS (SELECT 1 AS x) SELECT x FROM c"); len(f) != 0 {
		t.Fatalf("plain WITH query produced flows %+v", f)
	}
	f := one(t, "WITH c AS (SELECT id FROM dbo.src) SELECT id INTO dbo.t FROM c")
	if f.Kind != FlowSelectInto || !reflect.DeepEqual(f.Target, []string{"dbo", "t"}) {
		t.Fatalf("with + select into = %+v", f)
	}
}

// A WITH clause that does not parse reads as no flow rather than a guess.
func TestDataFlowCTEMalformedClause(t *testing.T) {
	for _, sql := range []string{
		"WITH",
		"WITH (NOLOCK) DELETE FROM dbo.t",
		"WITH c (a, b AS (SELECT 1) DELETE FROM dbo.t",
		"WITH c SELECT 1",
		"WITH c AS (SELECT 1 DELETE FROM dbo.t",
		"WITH c AS (SELECT 1),",
	} {
		if f := DataFlows(sql); len(f) != 0 {
			t.Errorf("DataFlows(%q) = %+v; want none", sql, f)
		}
	}
}

// A CTE with a column list, and a write through a CTE that reads no table,
// keep the target as written.
func TestDataFlowCTEColumnListAndSourcelessTarget(t *testing.T) {
	f := one(t, "WITH c (a) AS (SELECT 1) UPDATE c SET a = 2")
	if f.Kind != FlowModify || !reflect.DeepEqual(f.Target, []string{"c"}) {
		t.Fatalf("flow = %+v", f)
	}
}
