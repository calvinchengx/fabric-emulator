package xmla

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
)

func goldenModel(t *testing.T) *semanticmodel.Model {
	t.Helper()
	bim, err := os.ReadFile(filepath.Join("..", "..", "e2e", "semantic-model", "fixtures", "retail.bim"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := semanticmodel.ParseTMSL(bim)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestIsDMV(t *testing.T) {
	if !IsDMV("SELECT [ID] FROM $SYSTEM.TMSCHEMA_TABLES") {
		t.Error("DMV statement not recognised")
	}
	if IsDMV(`EVALUATE SUMMARIZECOLUMNS('Time'[FiscalYear])`) {
		t.Error("DAX EVALUATE must not be taken for a DMV")
	}
}

// The exact query sempy issues, alias and all — the alias is what it merges
// DataFrames on, so returning the source name would break its joins silently.
func TestSempyTablesQuery(t *testing.T) {
	rs, err := DMV(goldenModel(t), `
		SELECT
			[ID]   AS [SemPyTableID],
			[Name] AS [SemPyTableName]
		FROM
			$SYSTEM.TMSCHEMA_TABLES
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rs.Columns, ","); got != "SemPyTableID,SemPyTableName" {
		t.Fatalf("columns = %q; aliases must win over source names", got)
	}
	if len(rs.Rows) != 3 {
		t.Fatalf("rows = %d, want 3 (Store, Time, Sales)", len(rs.Rows))
	}
	if rs.Rows[0][0] != "1" || rs.Rows[0][1] != "Store" {
		t.Errorf("first row = %v, want [1 Store]", rs.Rows[0])
	}
	parses(t, rs.ExecuteResponse())
}

// Partitions join back to tables on TableID; sempy does exactly this merge, so
// the ids must agree across the two rowsets.
func TestPartitionsJoinToTables(t *testing.T) {
	m := goldenModel(t)
	tables, err := DMV(m, `SELECT [ID] AS [SemPyTableID], [Name] FROM $SYSTEM.TMSCHEMA_TABLES`)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := DMV(m, `SELECT [ID] AS [SemPyPartitionID], [TableID] AS [SemPyTableID], [Name] FROM $SYSTEM.TMSCHEMA_PARTITIONS`)
	if err != nil {
		t.Fatal(err)
	}
	tableIDs := map[string]bool{}
	for _, r := range tables.Rows {
		tableIDs[r[0]] = true
	}
	if len(parts.Rows) == 0 {
		t.Fatal("no partitions")
	}
	for _, p := range parts.Rows {
		if !tableIDs[p[1]] {
			t.Errorf("partition TableID %q joins to no table — sempy's merge would drop it", p[1])
		}
	}
}

func TestColumnsAndRelationships(t *testing.T) {
	m := goldenModel(t)
	cols, err := DMV(m, `SELECT [ID], [TableID], [ExplicitName] FROM $SYSTEM.TMSCHEMA_COLUMNS`)
	if err != nil {
		t.Fatal(err)
	}
	// Store(4) + Time(3) + Sales(5) = 12 columns in the golden model.
	if len(cols.Rows) != 12 {
		t.Errorf("columns = %d, want 12", len(cols.Rows))
	}
	// Column IDs must be unique across the whole model, not per table.
	seen := map[string]bool{}
	for _, r := range cols.Rows {
		if seen[r[0]] {
			t.Errorf("duplicate column ID %q — ids must be model-wide", r[0])
		}
		seen[r[0]] = true
	}
	rels, err := DMV(m, `SELECT [ID], [Name] FROM $SYSTEM.TMSCHEMA_RELATIONSHIPS`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels.Rows) != 2 {
		t.Errorf("relationships = %d, want 2", len(rels.Rows))
	}
}

// An empty rowset is a legitimate answer and must still carry its schema —
// the client reads the shape before the rows.
func TestHierarchiesEmptyButWellFormed(t *testing.T) {
	rs, err := DMV(goldenModel(t), `SELECT [ID], [Name], [TableID] FROM $SYSTEM.TMSCHEMA_HIERARCHIES`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.Rows) != 0 {
		t.Errorf("expected no hierarchies, got %d", len(rs.Rows))
	}
	payload := rs.ExecuteResponse()
	parses(t, payload)
	if !strings.Contains(string(payload), "<xsd:schema") {
		t.Error("an empty DMV rowset must still emit its schema")
	}
}

// Refusals must be refusals. A blank or zero here would look like a real
// answer and survive indefinitely.
func TestRefusalsAreErrors(t *testing.T) {
	m := goldenModel(t)
	cases := map[string]string{
		"storage stats":  `SELECT [ColumnID], [Statistics_DistinctStates] FROM $SYSTEM.TMSCHEMA_COLUMN_STORAGES`,
		"unknown dmv":    `SELECT [ID] FROM $SYSTEM.TMSCHEMA_NOPE`,
		"unknown column": `SELECT [Nonexistent] FROM $SYSTEM.TMSCHEMA_TABLES`,
		"select star":    `SELECT * FROM $SYSTEM.TMSCHEMA_TABLES`,
		"not a dmv":      `EVALUATE 'Store'`,
	}
	for name, q := range cases {
		if _, err := DMV(m, q); err == nil {
			t.Errorf("%s: expected an error, got a rowset", name)
		}
	}
	// The storage refusal should say why, not just fail.
	_, err := DMV(m, `SELECT [ColumnID] FROM $SYSTEM.TMSCHEMA_COLUMN_STORAGES`)
	if err == nil || !strings.Contains(err.Error(), "VertiPaq") {
		t.Errorf("storage refusal should name the reason, got %v", err)
	}
	if _, err := DMV(nil, `SELECT [ID] FROM $SYSTEM.TMSCHEMA_TABLES`); err == nil {
		t.Error("nil model should error")
	}
}
