// Package store is the emulator's persistence layer: pure-Go SQLite
// (modernc.org/sqlite, no CGO), one database for workspaces, items,
// definitions, role assignments, and operations. All timestamps flow through
// Now (the controllable clock) so LRO completion is deterministic.
package store

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	_ "modernc.org/sqlite"
)

// Store wraps the database plus the emulator clock.
type Store struct {
	db    *sql.DB
	Clock *clock.Clock
	// FileEvents, when set, receives a OneLake data-plane event after every
	// committed file write, rename, or delete — whoever made it. The server
	// wires it to the event-trigger dispatcher (internal/api/triggers.go);
	// nil means nothing subscribes and the emit is a no-op.
	//
	// This hook is **synchronous and exactly-once**: a trigger must fire before
	// the write returns, so a test that writes a file sees the pipeline it
	// started. The flow bus below has the opposite contract — asynchronous and
	// lossy — which is why they are separate rather than one mechanism.
	FileEvents func(FileEvent)

	// bus fans the same events out to observers (the /_emulator/events stream).
	// See bus.go for why delivery there must never block a writer.
	bus *bus
}

// Open opens (creating if needed) the database in dataDir; an empty dataDir
// uses an in-memory database (tests, ephemeral runs).
func Open(dataDir string, ck *clock.Clock) (*Store, error) {
	dsn := ":memory:"
	if dataDir != "" {
		// "Creating if needed" has to include the DIRECTORY, not just the file.
		// SQLite will not make one, and its failure says only "unable to open
		// database file (14)" — no path, no mention of a directory, on the one
		// error a first run is most likely to hit. This mattered the moment
		// DataDir stopped defaulting to in-memory: `fabric-emulator` in any
		// directory without a ./data now opens the store before anything else
		// has had a chance to create it.
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir %q: %w", dataDir, err)
		}
		dsn = filepath.Join(dataDir, "fabric-emulator.db")
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc/sqlite serializes writes; a single conn avoids table locks.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, Clock: ck, bus: newBus()}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	go s.runBus()
	return s, nil
}

// Close stops the flow bus and closes the database. The bus is stopped first
// and waited for, so no dispatch goroutine is still holding the handle.
func (s *Store) Close() error {
	if s.bus != nil {
		s.bus.stop()
	}
	return s.db.Close()
}

// Now returns the current emulator time (epoch seconds).
func (s *Store) Now() int64 { return s.Clock.Now() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS workspaces (
	id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	capacity_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS role_assignments (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	principal_id TEXT NOT NULL,
	principal_type TEXT NOT NULL,
	role TEXT NOT NULL,
	UNIQUE (workspace_id, principal_id)
);
CREATE TABLE IF NOT EXISTS items (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	type TEXT NOT NULL,
	display_name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	-- Workspace folder the item lives in ('' = workspace root). Not a declared
	-- FK: folders are deleted independently and an item outliving its folder
	-- must fall back to the root rather than block the delete.
	folder_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS item_definitions (
	item_id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
	parts_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	complete_at INTEGER NOT NULL,
	result_ref TEXT NOT NULL DEFAULT '',
	fail_with TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS connections (
	id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	connectivity_type TEXT NOT NULL DEFAULT '',
	details_json TEXT NOT NULL DEFAULT '{}',
	credential_type TEXT NOT NULL DEFAULT '',
	sso_type TEXT NOT NULL DEFAULT '',
	encryption TEXT NOT NULL DEFAULT '',
	credentials_json TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS git_connections (
	workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
	provider_json TEXT NOT NULL,
	remote_key TEXT NOT NULL,
	branch TEXT NOT NULL,
	cred_source TEXT NOT NULL,
	connection_id TEXT NOT NULL DEFAULT '',
	initialized INTEGER NOT NULL DEFAULT 0,
	synced_commit TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS git_remote_items (
	remote_key TEXT NOT NULL,
	branch TEXT NOT NULL,
	logical_id TEXT NOT NULL,
	item_type TEXT NOT NULL,
	display_name TEXT NOT NULL,
	parts_json TEXT NOT NULL,
	PRIMARY KEY (remote_key, branch, item_type, display_name)
);
CREATE TABLE IF NOT EXISTS git_remote_heads (
	remote_key TEXT NOT NULL,
	branch TEXT NOT NULL,
	commit_hash TEXT NOT NULL,
	PRIMARY KEY (remote_key, branch)
);
CREATE TABLE IF NOT EXISTS onelake_paths (
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	rel_path TEXT NOT NULL, -- path within the item, e.g. Files/raw/a.txt
	is_dir INTEGER NOT NULL DEFAULT 0,
	content BLOB NOT NULL DEFAULT x'',
	created_at INTEGER NOT NULL,
	etag TEXT NOT NULL DEFAULT '',
	modified_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (item_id, rel_path)
);
CREATE TABLE IF NOT EXISTS workspace_identities (
	workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
	identity_id TEXT NOT NULL, -- entra service principal object id
	app_id TEXT NOT NULL,      -- the sub/appid in tokens the identity mints
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS folders (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	display_name TEXT NOT NULL,
	parent_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	UNIQUE (workspace_id, parent_id, display_name)
);
CREATE TABLE IF NOT EXISTS job_instances (
	id TEXT PRIMARY KEY,
	item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	job_type TEXT NOT NULL,
	invoke_type TEXT NOT NULL DEFAULT 'Manual',
	created_at INTEGER NOT NULL,
	complete_at INTEGER NOT NULL,
	cancelled INTEGER NOT NULL DEFAULT 0,
	fail_with TEXT NOT NULL DEFAULT ''
);
-- Fabric's native per-item Job Scheduler (POST …/jobs/{jobType}/schedules).
-- The ScheduleConfig union is stored as the JSON the caller sent, validated on
-- the way in by internal/schedule: the emulator round-trips exactly what it
-- was given rather than a lossy column-per-field decomposition.
CREATE TABLE IF NOT EXISTS item_schedules (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	job_type TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	configuration TEXT NOT NULL,   -- JSON ScheduleConfig, verbatim
	execution_data TEXT NOT NULL DEFAULT '',  -- JSON, optional
	owner_id TEXT NOT NULL,
	owner_type TEXT NOT NULL DEFAULT 'User',
	created_at INTEGER NOT NULL,
	-- The high-water mark of materialised occurrences: the next evaluation
	-- fires the window (fired_through, now]. 0 = never fired.
	fired_through INTEGER NOT NULL DEFAULT 0
);
-- Event triggers: a Reflex's subscription to OneLake file events, and the item
-- job it starts. Real Fabric binds these in the portal (an Eventstream feeding
-- a Reflex rule) with no public REST, so the flattened shape here is an
-- emulator-native control surface — see internal/store/events.go.
CREATE TABLE IF NOT EXISTS event_triggers (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	reflex_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	display_name TEXT NOT NULL DEFAULT '',
	enabled INTEGER NOT NULL DEFAULT 1,
	event_type TEXT NOT NULL,
	source_item_id TEXT NOT NULL,
	path_prefix TEXT NOT NULL DEFAULT '',
	target_workspace_id TEXT NOT NULL,
	target_item_id TEXT NOT NULL,
	target_job_type TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_event_triggers_source ON event_triggers (source_item_id);
-- Materialized lake views: a named query in a lakehouse that is re-computed
-- into a real Delta table under Tables/. Fabric defines these with Spark SQL
-- DDL inside a notebook, and no capture of that syntax exists here, so the
-- DEFINITION surface is emulator-native and labelled as such in docs/parity.md
-- — the same stance the event_triggers table above takes. What is faithful is
-- everything downstream: the query really runs on the engine, the table really
-- lands in OneLake, and staleness is measured against real Delta versions.
CREATE TABLE IF NOT EXISTS materialized_lake_views (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	lakehouse_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	query TEXT NOT NULL,
	-- The tables this view reads, DECLARED rather than parsed out of the SQL.
	-- Fabric infers them; inferring here would mean parsing dialect-specific
	-- SQL and silently mis-reporting staleness whenever the parse was wrong,
	-- which is a worse failure than asking for the list. JSON array.
	depends_on TEXT NOT NULL DEFAULT '[]',
	-- The source versions observed at the last successful refresh. Staleness is
	-- "a dependency has moved since", which is answerable only against what was
	-- actually read. JSON object of table name -> delta version.
	source_versions TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	last_refreshed_at INTEGER NOT NULL DEFAULT 0,
	last_refresh_status TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	UNIQUE (lakehouse_id, name)
);
CREATE TABLE IF NOT EXISTS pipeline_runs (
	job_id TEXT PRIMARY KEY REFERENCES job_instances(id) ON DELETE CASCADE,
	status TEXT NOT NULL,
	activity_runs TEXT NOT NULL   -- JSON array of activity-run records
);
CREATE TABLE IF NOT EXISTS notebook_runs (
	job_id TEXT PRIMARY KEY REFERENCES job_instances(id) ON DELETE CASCADE,
	status TEXT NOT NULL,
	run TEXT NOT NULL             -- JSON: {status, exitValue, cells:[...]}
);
CREATE TABLE IF NOT EXISTS lineage_edges (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	-- Nullable: a warehouse write has no Fabric job (see relaxLineageJobFK).
	job_id TEXT REFERENCES job_instances(id) ON DELETE CASCADE,
	activity_name TEXT NOT NULL,
	source_workspace_id TEXT NOT NULL,
	source_item_id TEXT NOT NULL,
	source_path TEXT NOT NULL,
	target_workspace_id TEXT NOT NULL,
	target_item_id TEXT NOT NULL,
	target_path TEXT NOT NULL,
	producer TEXT NOT NULL DEFAULT 'Copy',
	-- 'item' (a Fabric workspace/item/path) or 'connection' (source_item_id is
	-- a connection id: a source system outside Fabric). See the migration note.
	source_kind TEXT NOT NULL DEFAULT 'item',
	created_at INTEGER NOT NULL,
	UNIQUE(job_id, activity_name, source_item_id, source_path, target_item_id, target_path)
);
-- The UNIQUE above cannot dedupe job-less edges: SQL treats NULLs as distinct,
-- so every dbt rebuild would append a second copy of the same movement.
CREATE UNIQUE INDEX IF NOT EXISTS ux_lineage_edges_nojob
  ON lineage_edges (activity_name, source_item_id, source_path, target_item_id, target_path)
  WHERE job_id IS NULL;
CREATE TABLE IF NOT EXISTS notebook_accesses (
	job_id TEXT NOT NULL REFERENCES job_instances(id) ON DELETE CASCADE,
	cell_index INTEGER NOT NULL,
	item_id TEXT NOT NULL,
	path TEXT NOT NULL,
	direction TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE(job_id, cell_index, item_id, path, direction)
);
CREATE TABLE IF NOT EXISTS capacities (
	id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	sku TEXT NOT NULL,
	region TEXT NOT NULL,
	state TEXT NOT NULL
);
-- Typed item properties the Fabric REST reference exposes under an item's
-- "properties" object but that are not part of the generic item record — e.g.
-- a KQLDatabase's parentEventhouseItemId / databaseType, taken from the
-- creationPayload. Free-form key/value so new typed surfaces need no schema
-- change; the API decides which keys a given item type recognises.
CREATE TABLE IF NOT EXISTS item_properties (
	item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	value TEXT NOT NULL,
	PRIMARY KEY (item_id, name)
);
-- Tenant settings, shaped as the REST reference's TenantSetting object.
CREATE TABLE IF NOT EXISTS tenant_settings (
	setting_name TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 0,
	can_specify_security_groups INTEGER NOT NULL DEFAULT 0,
	tenant_setting_group TEXT NOT NULL DEFAULT '',
	delegate_to_capacity INTEGER NOT NULL DEFAULT 0,
	delegate_to_domain INTEGER NOT NULL DEFAULT 0,
	delegate_to_workspace INTEGER NOT NULL DEFAULT 0,
	enabled_groups_json TEXT NOT NULL DEFAULT 'null',
	excluded_groups_json TEXT NOT NULL DEFAULT 'null',
	properties_json TEXT NOT NULL DEFAULT 'null'
);
-- Capacity-level overrides of tenant settings. The documented payload is
-- stored whole: it is a read-mostly projection, and keeping it as one JSON
-- object avoids a column per documented field.
CREATE TABLE IF NOT EXISTS capacity_setting_overrides (
	capacity_id TEXT NOT NULL,
	setting_name TEXT NOT NULL,
	setting_json TEXT NOT NULL,
	PRIMARY KEY (capacity_id, setting_name)
);
-- Sensitivity labels. The taxonomy is emulator-provided (real Fabric gets it
-- from Purview, which is not attachable offline); the label-change event
-- model in governance/sensitivity-label-audit-schema.md is what is faithful.
CREATE TABLE IF NOT EXISTS sensitivity_labels (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	sort_order INTEGER NOT NULL   -- higher = more restrictive
);
CREATE TABLE IF NOT EXISTS item_labels (
	item_id TEXT PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
	label_id TEXT NOT NULL REFERENCES sensitivity_labels(id)
);
-- Audit log behind the admin activityevents API. Operation names come from
-- the documented audit vocabulary (admin/operation-list.md and
-- governance/domains-audit-schema.md); properties_json holds the
-- per-operation operationProperties.
CREATE TABLE IF NOT EXISTS activity_events (
	id TEXT PRIMARY KEY,
	created_at INTEGER NOT NULL,
	operation TEXT NOT NULL,
	user_id TEXT NOT NULL,
	user_type TEXT NOT NULL,
	workspace_id TEXT NOT NULL DEFAULT '',
	artifact_id TEXT NOT NULL DEFAULT '',
	artifact_name TEXT NOT NULL DEFAULT '',
	properties_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS ix_activity_events_time ON activity_events(created_at);
-- Tenant-level governance domains (fabric-docs governance/domains.md). A
-- subdomain points at its parent; deleting a domain takes its subdomains,
-- workspace assignments and role assignments with it.
CREATE TABLE IF NOT EXISTS domains (
	id TEXT PRIMARY KEY,
	-- Unique tenant-wide, case-insensitively, like workspace names.
	display_name TEXT NOT NULL COLLATE NOCASE UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	-- NULL for a root domain; a real id for a subdomain. Not '' — an empty
	-- string is not a domain id, and the foreign key would reject it.
	parent_domain_id TEXT REFERENCES domains(id) ON DELETE CASCADE,
	contributors_scope TEXT NOT NULL
);
-- A workspace belongs to at most one domain, so workspace_id is the key.
CREATE TABLE IF NOT EXISTS domain_workspaces (
	workspace_id TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
	domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS domain_role_assignments (
	domain_id TEXT NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
	principal_id TEXT NOT NULL,
	principal_type TEXT NOT NULL,
	role TEXT NOT NULL,
	PRIMARY KEY (domain_id, principal_id, role)
);
CREATE TABLE IF NOT EXISTS purview_typedefs (
	guid TEXT NOT NULL,
	name TEXT NOT NULL PRIMARY KEY,   -- Atlas resolves a type by bare name, so
	category TEXT NOT NULL,           -- the name is unique across ALL categories
	body TEXT NOT NULL,               -- the definition as sent; Atlas types are open
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS purview_entities (
	guid TEXT NOT NULL PRIMARY KEY,
	type_name TEXT NOT NULL,
	qualified_name TEXT NOT NULL,     -- Atlas's unique attribute, per type
	status TEXT NOT NULL DEFAULT 'ACTIVE',  -- soft delete: DELETED rows remain
	body TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE (type_name, qualified_name)
);
CREATE TABLE IF NOT EXISTS shortcuts (
	item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	path TEXT NOT NULL,            -- managed folder the shortcut lives in, e.g. Files
	name TEXT NOT NULL,            -- the symlink name
	target_workspace TEXT NOT NULL,
	target_item TEXT NOT NULL,
	target_path TEXT NOT NULL,
	target_type TEXT NOT NULL DEFAULT 'OneLake',
	target_location TEXT NOT NULL DEFAULT '',
	target_table TEXT NOT NULL DEFAULT '',
	connection_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	PRIMARY KEY (item_id, path, name)
);
CREATE TABLE IF NOT EXISTS deployment_pipelines (
	id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS deployment_pipeline_stages (
	id TEXT PRIMARY KEY,
	pipeline_id TEXT NOT NULL REFERENCES deployment_pipelines(id) ON DELETE CASCADE,
	stage_order INTEGER NOT NULL,   -- dense 0..n-1; deploys are adjacent-only
	display_name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	is_public INTEGER NOT NULL DEFAULT 0,
	-- Nullable so the workspace FK can SET NULL: deleting a workspace
	-- unassigns the stage instead of leaving it pointing at nothing.
	workspace_id TEXT REFERENCES workspaces(id) ON DELETE SET NULL,
	UNIQUE (pipeline_id, stage_order)
);
CREATE TABLE IF NOT EXISTS deployment_pipeline_pairs (
	pipeline_id TEXT NOT NULL REFERENCES deployment_pipelines(id) ON DELETE CASCADE,
	-- Pairs only ever span ADJACENT stages, named by deploy direction:
	-- "earlier" is the source of a deploy, "later" the target.
	earlier_stage_id TEXT NOT NULL REFERENCES deployment_pipeline_stages(id) ON DELETE CASCADE,
	earlier_item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	later_stage_id TEXT NOT NULL REFERENCES deployment_pipeline_stages(id) ON DELETE CASCADE,
	later_item_id TEXT NOT NULL REFERENCES items(id) ON DELETE CASCADE,
	-- An item pairs with at most one item on each side, so both directions
	-- are unique. Deleting an item drops its pairs.
	PRIMARY KEY (pipeline_id, earlier_stage_id, earlier_item_id),
	UNIQUE (pipeline_id, later_stage_id, later_item_id)
);
CREATE TABLE IF NOT EXISTS deployment_pipeline_operations (
	id TEXT PRIMARY KEY,           -- the LRO operation id, so /result can find it
	pipeline_id TEXT NOT NULL REFERENCES deployment_pipelines(id) ON DELETE CASCADE,
	source_stage_id TEXT NOT NULL,
	target_stage_id TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	performed_by TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	detail TEXT NOT NULL           -- JSON: [{sourceItemId,targetItemId,outcome,…}]
);
CREATE TABLE IF NOT EXISTS deployment_pipeline_roles (
	pipeline_id TEXT NOT NULL REFERENCES deployment_pipelines(id) ON DELETE CASCADE,
	principal_id TEXT NOT NULL,
	principal_type TEXT NOT NULL,
	role TEXT NOT NULL,
	PRIMARY KEY (pipeline_id, principal_id)
);
PRAGMA foreign_keys = ON;
`)
	if err != nil {
		return err
	}
	// Additive migrations for databases created before these columns; a
	// duplicate-column error means the column already exists.
	for _, alter := range []string{
		`ALTER TABLE connections ADD COLUMN credential_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE connections ADD COLUMN sso_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE connections ADD COLUMN encryption TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE connections ADD COLUMN credentials_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE lineage_edges ADD COLUMN producer TEXT NOT NULL DEFAULT 'Copy'`,
		// What KIND of thing the source ref names. Until now every lineage
		// endpoint was a Fabric (workspace, item, path) triple, which cannot
		// express "these bytes came from outside Fabric" — a vendor's REST API,
		// a database, a Kafka topic. `connection` says source_item_id holds a
		// CONNECTION id instead of an item id.
		//
		// Typing the existing ref rather than adding a parallel
		// source_connection_id column, because the UNIQUE key includes
		// source_item_id and SQLite cannot alter a constraint without a table
		// rebuild. A parallel column would leave two different sources landing
		// in one target indistinguishable to that key, and the second edge
		// would be silently dropped by ON CONFLICT DO NOTHING.
		`ALTER TABLE lineage_edges ADD COLUMN source_kind TEXT NOT NULL DEFAULT 'item'`,
		`ALTER TABLE onelake_paths ADD COLUMN etag TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE onelake_paths ADD COLUMN modified_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE shortcuts ADD COLUMN target_type TEXT NOT NULL DEFAULT 'OneLake'`,
		`ALTER TABLE shortcuts ADD COLUMN target_location TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE shortcuts ADD COLUMN connection_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE shortcuts ADD COLUMN target_table TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE items ADD COLUMN folder_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := s.db.Exec(alter); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	// lineage_edges.job_id becomes OPTIONAL.
	//
	// Every edge used to come from an executing activity, so job_id was NOT NULL
	// with a foreign key. A warehouse write has no Fabric job at all — dbt opens
	// a TDS connection and builds gold — so that constraint made the last hop of
	// a medallion unrecordable (internal/server/warehouselineage.go).
	//
	// NULL rather than '': a NULL foreign key is satisfied by definition, so
	// job-produced edges keep their referential integrity and their ON DELETE
	// CASCADE, while a job-less edge is simply not claiming a job it never had.
	// SQLite cannot relax a column constraint in place, hence the rebuild.
	if err := s.relaxLineageJobFK(); err != nil {
		return err
	}
	// Display-name uniqueness enforced by the DATABASE, not just the
	// pre-insert check in the API: check-then-insert races (two concurrent
	// creates of the same name both pass the check) would otherwise violate
	// the invariant. Case-insensitive, matching the checks in names.go.
	// A pre-existing dev database may already hold duplicates, so a failure
	// here is logged-and-skipped rather than fatal — new databases always
	// get the constraint.
	for _, idx := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_workspaces_display_name
		   ON workspaces (display_name COLLATE NOCASE)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ux_items_ws_name_type
		   ON items (workspace_id, display_name COLLATE NOCASE, type COLLATE NOCASE)`,
	} {
		if _, err := s.db.Exec(idx); err != nil {
			log.Printf("store: display-name uniqueness index not applied (pre-existing duplicates?): %v", err)
		}
	}
	if err := s.seedCapacity(); err != nil {
		return err
	}
	if err := s.seedLabels(); err != nil {
		return err
	}
	return s.seedTenantSettings()
}

// relaxLineageJobFK rebuilds lineage_edges so job_id may be NULL, for the
// warehouse writes that have no Fabric job. It is a no-op once applied, so
// startup stays idempotent.
//
// The rebuild runs with foreign_keys OFF (SQLite's documented procedure for
// altering a table other tables may reference) and inside a transaction, so a
// failure leaves the original table untouched rather than a half-migrated one.
func (s *Store) relaxLineageJobFK() error {
	var ddl string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='lineage_edges'`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(ddl, "job_id TEXT NOT NULL REFERENCES") {
		return nil // already relaxed
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() { _, _ = s.db.Exec(`PRAGMA foreign_keys = ON`) }()
	_, err := s.db.Exec(`
BEGIN;
CREATE TABLE lineage_edges_new (
	id TEXT PRIMARY KEY,
	workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	job_id TEXT REFERENCES job_instances(id) ON DELETE CASCADE,
	activity_name TEXT NOT NULL,
	source_workspace_id TEXT NOT NULL,
	source_item_id TEXT NOT NULL,
	source_path TEXT NOT NULL,
	target_workspace_id TEXT NOT NULL,
	target_item_id TEXT NOT NULL,
	target_path TEXT NOT NULL,
	producer TEXT NOT NULL DEFAULT 'Copy',
	-- 'item' (a Fabric workspace/item/path) or 'connection' (source_item_id is
	-- a connection id: a source system outside Fabric). See the migration note.
	source_kind TEXT NOT NULL DEFAULT 'item',
	created_at INTEGER NOT NULL,
	UNIQUE(job_id, activity_name, source_item_id, source_path, target_item_id, target_path)
);
-- The target columns are NAMED. Positionally, this INSERT supplied 12 values
-- into a 13-column table -- source_kind was added to the new shape after this
-- migration was written -- so the rebuild failed with "has 13 columns but 12
-- values were supplied" and took startup down with it. Naming them lets
-- source_kind take its DEFAULT, and makes the statement survive the next column
-- added above it.
INSERT INTO lineage_edges_new
	(id, workspace_id, job_id, activity_name,
	 source_workspace_id, source_item_id, source_path,
	 target_workspace_id, target_item_id, target_path, producer, created_at)
SELECT id, workspace_id, job_id, activity_name,
	source_workspace_id, source_item_id, source_path,
	target_workspace_id, target_item_id, target_path, producer, created_at
	FROM lineage_edges;
DROP TABLE lineage_edges;
ALTER TABLE lineage_edges_new RENAME TO lineage_edges;
-- The table's UNIQUE cannot dedupe job-less edges: SQL treats NULLs as
-- distinct, so every dbt rebuild would append a second copy of the same
-- movement. This partial index is that constraint for the job-less rows.
CREATE UNIQUE INDEX IF NOT EXISTS ux_lineage_edges_nojob
  ON lineage_edges (activity_name, source_item_id, source_path, target_item_id, target_path)
  WHERE job_id IS NULL;
COMMIT;`)
	return err
}

// NewID returns a random lowercase UUIDv4 — the id format Fabric uses for
// workspaces, items, and operations.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
