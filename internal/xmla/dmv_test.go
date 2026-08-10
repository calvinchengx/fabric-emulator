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
	rs, err := DMV(goldenModel(t), goldenData(t), `
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
	// Ids live in a MODEL-WIDE space, not a per-rowset one: TOM assembles a
	// single object graph from every rowset and refuses duplicates
	// (`Duplicate object ID 1, first in 'Tabular.Model' ...`). Tables are based
	// at 1000, so the first table is 1001. What matters is that the SELECT and
	// Discover grammars agree, which TestDiscoverModelledTypes asserts.
	if rs.Rows[0][0] != objID("TMSCHEMA_TABLES", 0) || rs.Rows[0][1] != "Store" {
		t.Errorf("first row = %v, want [%s Store]", rs.Rows[0], objID("TMSCHEMA_TABLES", 0))
	}
	parses(t, rs.ExecuteResponse())
}

// Partitions join back to tables on TableID; sempy does exactly this merge, so
// the ids must agree across the two rowsets.
func TestPartitionsJoinToTables(t *testing.T) {
	m := goldenModel(t)
	tables, err := DMV(m, goldenData(t), `SELECT [ID] AS [SemPyTableID], [Name] FROM $SYSTEM.TMSCHEMA_TABLES`)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := DMV(m, goldenData(t), `SELECT [ID] AS [SemPyPartitionID], [TableID] AS [SemPyTableID], [Name] FROM $SYSTEM.TMSCHEMA_PARTITIONS`)
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
	cols, err := DMV(m, goldenData(t), `SELECT [ID], [TableID], [ExplicitName] FROM $SYSTEM.TMSCHEMA_COLUMNS`)
	if err != nil {
		t.Fatal(err)
	}
	// Counted from the model: the golden fixture is shared and grows.
	wantCols := 0
	for _, tb := range m.Tables {
		wantCols += len(tb.Columns)
	}
	if len(cols.Rows) != wantCols {
		t.Errorf("columns = %d, want %d (every column in the model)", len(cols.Rows), wantCols)
	}
	// Column IDs must be unique across the whole model, not per table.
	seen := map[string]bool{}
	for _, r := range cols.Rows {
		if seen[r[0]] {
			t.Errorf("duplicate column ID %q — ids must be model-wide", r[0])
		}
		seen[r[0]] = true
	}
	rels, err := DMV(m, goldenData(t), `SELECT [ID], [Name] FROM $SYSTEM.TMSCHEMA_RELATIONSHIPS`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels.Rows) != len(m.Relationships) {
		t.Errorf("relationships = %d, want %d", len(rels.Rows), len(m.Relationships))
	}
}

// An empty rowset is a legitimate answer and must still carry its schema —
// the client reads the shape before the rows.
func TestHierarchiesEmptyButWellFormed(t *testing.T) {
	rs, err := DMV(goldenModel(t), goldenData(t), `SELECT [ID], [Name], [TableID] FROM $SYSTEM.TMSCHEMA_HIERARCHIES`)
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
		"unknown dmv":    `SELECT [ID] FROM $SYSTEM.TMSCHEMA_NOPE`,
		"unknown column": `SELECT [Nonexistent] FROM $SYSTEM.TMSCHEMA_TABLES`,
		"select star":    `SELECT * FROM $SYSTEM.TMSCHEMA_TABLES`,
		"not a dmv":      `EVALUATE 'Store'`,
	}
	for name, q := range cases {
		if _, err := DMV(m, goldenData(t), q); err == nil {
			t.Errorf("%s: expected an error, got a rowset", name)
		}
	}
	// COLUMN_STORAGES is deliberately NOT in that list any more: it is derived
	// exactly from the rows we hold, so refusing it withheld an answer we can
	// give. Asserted positively in TestStorageRowsetsAreDerivedExactly.
	if _, err := DMV(m, goldenData(t), `SELECT [ColumnID] FROM $SYSTEM.TMSCHEMA_COLUMN_STORAGES`); err != nil {
		t.Errorf("COLUMN_STORAGES is derivable and must be answered, got %v", err)
	}
	if _, err := DMV(nil, nil, `SELECT [ID] FROM $SYSTEM.TMSCHEMA_TABLES`); err == nil {
		t.Error("nil model should error")
	}
}

func goldenData(t *testing.T) semanticmodel.Data {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "e2e", "semantic-model", "fixtures", "seed_data.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := semanticmodel.ParseData(raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The storage rowsets are DERIVED from rows we hold, so they must be exact —
// this is the test that would fail if they were stubbed to 0.
func TestStorageRowsetsAreDerivedExactly(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)

	seg, err := DMV(m, d, `SELECT [PartitionStorageID], [RecordCount], [SegmentCount] FROM $SYSTEM.TMSCHEMA_SEGMENT_MAP_STORAGES`)
	if err != nil {
		t.Fatal(err)
	}
	// Sales has 8 rows in the golden fixture; one segment (far under 8,388,608).
	var sales []string
	for _, r := range seg.Rows {
		if r[0] == "3" { // Sales is the third table
			sales = r
		}
	}
	if sales == nil || sales[1] != "8" || sales[2] != "1" {
		t.Fatalf("Sales segment row = %v, want RecordCount 8 / SegmentCount 1", sales)
	}

	cs, err := DMV(m, d, `SELECT [ColumnID], [Statistics_DistinctStates] FROM $SYSTEM.TMSCHEMA_COLUMN_STORAGES`)
	if err != nil {
		t.Fatal(err)
	}
	wantCS := 0
	for _, tb := range m.Tables {
		wantCS += len(tb.Columns)
	}
	if len(cs.Rows) != wantCS {
		t.Fatalf("column storages = %d, want %d (one per column)", len(cs.Rows), wantCS)
	}
	// Store[Territory] is the 4th column: West, East, Central, West -> 3 distinct.
	if cs.Rows[3][1] != "3" {
		t.Errorf("Territory distinct states = %s, want 3", cs.Rows[3][1])
	}
	parses(t, cs.ExecuteResponse())
}

// The documented rule: "every table partition has at least one segment", and
// the default is 8,388,608 rows per segment.
func TestSegmentCountFormula(t *testing.T) {
	cases := map[int]int{0: 1, 1: 1, 8388608: 1, 8388609: 2, 16777216: 2, 16777217: 3}
	for rows, want := range cases {
		if got := segmentCount(rows); got != want {
			t.Errorf("segmentCount(%d) = %d, want %d", rows, got, want)
		}
	}
}

// The one that stays refused, and the refusal must name the narrow reason.
func TestDeltaMetadataStillRefused(t *testing.T) {
	_, err := DMV(goldenModel(t), goldenData(t),
		`SELECT [FallbackReason], [TableName] FROM $SYSTEM.TMSCHEMA_DELTA_TABLE_METADATA_STORAGES`)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "FallbackReason") {
		t.Errorf("refusal should name the unverified field, got %v", err)
	}
}

// PARTITION_STORAGES joins to partitions, which join to tables — the chain
// sempy walks to attribute segment stats back to a table name.
func TestPartitionStoragesChain(t *testing.T) {
	m, d := goldenModel(t), goldenData(t)
	ps, err := DMV(m, d, `SELECT [ID], [PartitionID] FROM $SYSTEM.TMSCHEMA_PARTITION_STORAGES`)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := DMV(m, d, `SELECT [ID] FROM $SYSTEM.TMSCHEMA_PARTITIONS`)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Rows) != len(parts.Rows) {
		t.Fatalf("partition storages = %d, partitions = %d; one each", len(ps.Rows), len(parts.Rows))
	}
	ids := map[string]bool{}
	for _, r := range parts.Rows {
		ids[r[0]] = true
	}
	for _, r := range ps.Rows {
		if !ids[r[1]] {
			t.Errorf("storage PartitionID %q joins to no partition", r[1])
		}
	}
	parses(t, ps.ExecuteResponse())
}
