package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Materialized lake views.
//
// A view is a named query against a lakehouse that a refresh RE-COMPUTES into a
// real Delta table under `Tables/`. The rows are produced by the engine the
// emulator already runs and land through the storage layer, so everything that
// watches OneLake — the flow stream, lineage, a Delta reader, the SQL endpoint
// — sees a refresh exactly as it sees any other write.
//
// THE DEFINITION SURFACE IS EMULATOR-NATIVE, on the precedent this repo already
// set for Reflex trigger bindings: Fabric creates these with Spark SQL DDL that
// no capture here has observed, and inventing that syntax would be the
// fabrication the oracle rule exists to prevent. So the shape below is the
// emulator's own, labelled as such in docs/parity.md, and what is faithful is
// everything downstream of the definition.
//
// DEPENDENCIES ARE DECLARED, NOT PARSED. Fabric works out which tables a view
// reads from its SQL. Doing that here means parsing dialect-specific SQL, and a
// parse that is wrong does not fail — it silently reports a stale view as
// fresh, which is worse than asking for the list. `DependsOn` is therefore an
// input, and `SourceVersions` records what those tables were at the last
// successful refresh so staleness is measured against something real.

// MaterializedLakeView is one view definition and the state of its last refresh.
type MaterializedLakeView struct {
	ID                string         `json:"id"`
	WorkspaceID       string         `json:"workspaceId"`
	LakehouseID       string         `json:"lakehouseId"`
	Name              string         `json:"name"`
	Query             string         `json:"query"`
	DependsOn         []string       `json:"dependsOn"`
	SourceVersions    map[string]int `json:"sourceVersions"`
	CreatedAt         int64          `json:"createdAt"`
	LastRefreshedAt   int64          `json:"lastRefreshedAt,omitempty"`
	LastRefreshStatus string         `json:"lastRefreshStatus,omitempty"`
	LastError         string         `json:"lastError,omitempty"`
}

const mlvCols = `id, workspace_id, lakehouse_id, name, query, depends_on, source_versions,
created_at, last_refreshed_at, last_refresh_status, last_error`

// ErrDuplicateMLV is returned when a lakehouse already holds a view by that
// name — a name collision is a caller error, not a silent overwrite of someone
// else's definition.
var ErrDuplicateMLV = errors.New("a materialized lake view with that name already exists in this lakehouse")

func scanMLV(row interface{ Scan(...any) error }) (*MaterializedLakeView, error) {
	v := &MaterializedLakeView{}
	var deps, versions string
	err := row.Scan(&v.ID, &v.WorkspaceID, &v.LakehouseID, &v.Name, &v.Query, &deps, &versions,
		&v.CreatedAt, &v.LastRefreshedAt, &v.LastRefreshStatus, &v.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.DependsOn = []string{}
	v.SourceVersions = map[string]int{}
	_ = json.Unmarshal([]byte(deps), &v.DependsOn)
	_ = json.Unmarshal([]byte(versions), &v.SourceVersions)
	return v, nil
}

// CreateMaterializedLakeView stores a definition. Nothing is materialised here:
// a view that has never been refreshed HAS no table, and reporting otherwise
// would claim rows nobody computed.
func (s *Store) CreateMaterializedLakeView(v *MaterializedLakeView) error {
	if v.ID == "" {
		v.ID = NewID()
	}
	v.CreatedAt = s.Now()
	if v.DependsOn == nil {
		v.DependsOn = []string{}
	}
	deps, _ := json.Marshal(v.DependsOn)
	_, err := s.db.Exec(`
INSERT INTO materialized_lake_views (`+mlvCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.WorkspaceID, v.LakehouseID, v.Name, v.Query, string(deps), "{}",
		v.CreatedAt, 0, "", "")
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrDuplicateMLV
	}
	return err
}

// GetMaterializedLakeView fetches one view by name, scoped to its lakehouse.
func (s *Store) GetMaterializedLakeView(lakehouseID, name string) (*MaterializedLakeView, error) {
	return scanMLV(s.db.QueryRow(`SELECT `+mlvCols+`
FROM materialized_lake_views WHERE lakehouse_id = ? AND name = ?`, lakehouseID, name))
}

// ListMaterializedLakeViews returns a lakehouse's views, oldest first.
func (s *Store) ListMaterializedLakeViews(lakehouseID string) ([]*MaterializedLakeView, error) {
	rows, err := s.db.Query(`SELECT `+mlvCols+`
FROM materialized_lake_views WHERE lakehouse_id = ? ORDER BY created_at, name`, lakehouseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MaterializedLakeView{}
	for rows.Next() {
		v, err := scanMLV(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteMaterializedLakeView removes a definition. The materialised table is
// left alone deliberately: the rows are real data in OneLake, and dropping a
// definition is not a licence to delete what a reader may still be using.
func (s *Store) DeleteMaterializedLakeView(lakehouseID, name string) error {
	res, err := s.db.Exec(`
DELETE FROM materialized_lake_views WHERE lakehouse_id = ? AND name = ?`, lakehouseID, name)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// RecordMaterializedLakeViewRefresh writes the outcome of a refresh attempt.
// The source versions are stored ONLY on success: recording them for a failed
// refresh would mark the view fresh against sources it never actually read.
func (s *Store) RecordMaterializedLakeViewRefresh(lakehouseID, name, status, errMsg string, versions map[string]int) error {
	if status == "Succeeded" {
		if versions == nil {
			versions = map[string]int{}
		}
		blob, _ := json.Marshal(versions)
		_, err := s.db.Exec(`
UPDATE materialized_lake_views
SET last_refreshed_at = ?, last_refresh_status = ?, last_error = ?, source_versions = ?
WHERE lakehouse_id = ? AND name = ?`, s.Now(), status, "", string(blob), lakehouseID, name)
		return err
	}
	_, err := s.db.Exec(`
UPDATE materialized_lake_views SET last_refresh_status = ?, last_error = ?
WHERE lakehouse_id = ? AND name = ?`, status, errMsg, lakehouseID, name)
	return err
}

// DeltaTableVersion reports the latest committed version of a lakehouse table,
// read from its `_delta_log`. `ok` is false for a path that holds no committed
// Delta table — which is a different answer from version 0, and conflating the
// two would report a table that has never been written as freshly at version 0.
func (s *Store) DeltaTableVersion(itemID, table string) (version int, ok bool) {
	root := table
	if !strings.HasPrefix(root, "Tables/") {
		root = path.Join("Tables", table)
	}
	entries, err := s.ListOneLakePaths(itemID, path.Join(root, "_delta_log"), false)
	if err != nil {
		return 0, false
	}
	found := false
	for _, e := range entries {
		base := path.Base(e.RelPath)
		if !strings.HasSuffix(base, ".json") {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(strings.TrimSuffix(base, ".json"), "%020d", &v); err != nil {
			continue
		}
		if !found || v > version {
			version, found = v, true
		}
	}
	return version, found
}
