package store

// The lineage_edges upgrade path. relaxLineageJobFK rebuilds the table so
// job_id may be NULL — the change that made a warehouse write recordable at all,
// since dbt builds gold over a TDS connection with no Fabric job behind it.
//
// It sat at 36% coverage with the REBUILD never executed: every test opens a
// fresh database, where the schema is already relaxed and the function returns
// at its first check. So the branch that only runs against a database created
// by an older build — the one real users upgrade — was untested. A broken
// migration there does not fail a test, it fails someone's startup.

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	_ "modernc.org/sqlite"
)

// oldSchema is the pre-migration shape: job_id NOT NULL, and no partial index.
const oldSchema = `
CREATE TABLE workspaces (id TEXT PRIMARY KEY, display_name TEXT NOT NULL);
CREATE TABLE job_instances (id TEXT PRIMARY KEY);
CREATE TABLE lineage_edges (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	job_id TEXT NOT NULL REFERENCES job_instances(id) ON DELETE CASCADE,
	activity_name TEXT NOT NULL,
	source_workspace_id TEXT NOT NULL,
	source_item_id TEXT NOT NULL,
	source_path TEXT NOT NULL,
	target_workspace_id TEXT NOT NULL,
	target_item_id TEXT NOT NULL,
	target_path TEXT NOT NULL,
	producer TEXT NOT NULL DEFAULT 'Copy',
	created_at INTEGER NOT NULL,
	UNIQUE(job_id, activity_name, source_item_id, source_path, target_item_id, target_path)
);`

func TestRelaxLineageJobFKUpgradesAnOlderDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fabric-emulator.db")

	// A database as an older build left it: the strict schema plus one edge
	// that must survive the rebuild.
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO workspaces VALUES ('ws-1','w')`,
		`INSERT INTO job_instances VALUES ('job-1')`,
		`INSERT INTO lineage_edges VALUES ('e-1','ws-1','job-1','Copy',
			'ws-1','src-item','Tables/a','ws-1','dst-item','Tables/b','Copy',1)`,
	} {
		if _, err := seed.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening the store runs migrate(), and with it the rebuild.
	s, err := Open(dir, clock.New())
	if err != nil {
		t.Fatalf("opening a pre-migration database failed: %v", err)
	}
	defer s.Close()

	// 1. The existing edge survived, with its columns intact.
	var activity, srcPath, producer string
	err = s.db.QueryRow(`SELECT activity_name, source_path, producer FROM lineage_edges WHERE id='e-1'`).
		Scan(&activity, &srcPath, &producer)
	if err != nil {
		t.Fatalf("the pre-existing edge did not survive the rebuild: %v", err)
	}
	if activity != "Copy" || srcPath != "Tables/a" || producer != "Copy" {
		t.Fatalf("edge came back as %q/%q/%q; the rebuild's column order is wrong",
			activity, srcPath, producer)
	}

	// 2. The point of the migration: a job-less edge is now insertable.
	if err := s.CreateLineageEdge(&LineageEdge{
		WorkspaceID: "ws-1", ActivityName: "dbt", SourceWorkspaceID: "ws-1",
		SourceItemID: "wh", SourcePath: "Tables/silver",
		TargetWorkspaceID: "ws-1", TargetItemID: "wh", TargetPath: "Tables/gold",
		Producer: ProducerWarehouse,
	}); err != nil {
		t.Fatalf("a job-less warehouse edge is still rejected after the migration: %v", err)
	}

	// 3. And the partial index the rebuild adds still dedupes those, since the
	// table's own UNIQUE cannot: SQL treats NULLs as distinct.
	if err := s.CreateLineageEdge(&LineageEdge{
		WorkspaceID: "ws-1", ActivityName: "dbt", SourceWorkspaceID: "ws-1",
		SourceItemID: "wh", SourcePath: "Tables/silver",
		TargetWorkspaceID: "ws-1", TargetItemID: "wh", TargetPath: "Tables/gold",
		Producer: ProducerWarehouse,
	}); err != nil {
		t.Fatalf("re-recording the same job-less edge errored: %v", err)
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM lineage_edges WHERE job_id IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d job-less edges after recording the same movement twice; want 1 "+
			"— the partial index is what dedupes them", n)
	}
}

// TestRelaxLineageJobFKIsIdempotent: startup runs it every time, so a second
// open of an already-migrated database must be a no-op rather than a rebuild.
func TestRelaxLineageJobFKIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir, clock.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.CreateLineageEdge(&LineageEdge{
		WorkspaceID: "ws-x", ActivityName: "a", SourceWorkspaceID: "ws-x",
		SourceItemID: "s", SourcePath: "Tables/s",
		TargetWorkspaceID: "ws-x", TargetItemID: "d", TargetPath: "Tables/d",
	}); err != nil {
		// A workspace FK may reject this; the edge is not the point of this test.
		t.Logf("seed edge not inserted (%v); the reopen below is what matters", err)
	}
	s1.Close()

	s2, err := Open(dir, clock.New())
	if err != nil {
		t.Fatalf("reopening an already-migrated database failed: %v", err)
	}
	defer s2.Close()
	if err := s2.relaxLineageJobFK(); err != nil {
		t.Fatalf("running the migration a third time errored: %v", err)
	}
}
