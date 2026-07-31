package store

import (
	"encoding/json"
	"time"
)

// The activity (audit) log behind the admin activityevents API.
//
// Operation names come from the documented audit vocabulary, not invented:
// fabric-docs admin/operation-list.md for the core surface (CreateWorkspace,
// CreateArtifact, UpdateArtifact, DeleteArtifact, CreateFolder, …) and
// governance/domains-audit-schema.md for the domain operations, which also
// documents the operationProperties each one carries
// (DataDomainObjectId, DataDomainDisplayName, ParentObjectId,
// FoldersToSetCounter, FoldersToUnsetCount).
//
// Items are "artifacts" in this vocabulary — CreateArtifact, not CreateItem.
const (
	OpCreateWorkspace = "CreateWorkspace"
	OpUpdateWorkspace = "UpdateWorkspace"
	OpDeleteWorkspace = "DeleteWorkspace"
	OpCreateArtifact  = "CreateArtifact"
	OpUpdateArtifact  = "UpdateArtifact"
	OpDeleteArtifact  = "DeleteArtifact"
	OpCreateFolder    = "CreateFolder"
	OpDeleteFolder    = "DeleteFolder"

	// Domain operations, verbatim from domains-audit-schema.md.
	OpInsertDomain            = "InsertDataDomainAsAdmin"
	OpUpdateDomain            = "UpdateDataDomainAsAdmin"
	OpDeleteDomain            = "DeleteDataDomainAsAdmin"
	OpUpdateDomainWorkspaces  = "UpdateDataDomainFoldersRelationsAsAdmin"
	OpDeleteAllDomainWorkspae = "DeleteAllDataDomainFoldersRelationsAsAdmin"
)

// ActivityEvent is one audit record. Properties carries the per-operation
// operationProperties documented for that operation; it is merged into the
// entity at the API boundary, which is how the real payload is shaped.
type ActivityEvent struct {
	ID           string
	CreatedAt    int64
	Operation    string
	UserID       string
	UserType     string
	WorkspaceID  string
	ArtifactID   string
	ArtifactName string
	Properties   map[string]any
}

// RecordActivity appends one audit event. Auditing must never break the
// operation being audited, so callers ignore the error and this returns it
// only for tests.
func (s *Store) RecordActivity(ev *ActivityEvent) error {
	if ev.ID == "" {
		ev.ID = NewID()
	}
	if ev.CreatedAt == 0 {
		ev.CreatedAt = s.Now()
	}
	if ev.UserType == "" {
		ev.UserType = "User"
	}
	props := "{}"
	if len(ev.Properties) > 0 {
		if raw, err := json.Marshal(ev.Properties); err == nil {
			props = string(raw)
		}
	}
	_, err := s.db.Exec(
		`INSERT INTO activity_events
		   (id, created_at, operation, user_id, user_type, workspace_id, artifact_id, artifact_name, properties_json)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		ev.ID, ev.CreatedAt, ev.Operation, ev.UserID, ev.UserType,
		ev.WorkspaceID, ev.ArtifactID, ev.ArtifactName, props)
	return err
}

// ActivityEvents returns events in [from, to] (inclusive, epoch seconds),
// oldest first, starting at offset and returning at most limit. Ordering is
// stable across pages — the continuation token is an offset into this order.
func (s *Store) ActivityEvents(from, to int64, offset, limit int) ([]*ActivityEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, created_at, operation, user_id, user_type,
		        workspace_id, artifact_id, artifact_name, properties_json
		 FROM activity_events
		 WHERE created_at >= ? AND created_at <= ?
		 ORDER BY created_at, rowid
		 LIMIT ? OFFSET ?`, from, to, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*ActivityEvent{}
	for rows.Next() {
		ev := &ActivityEvent{}
		var props string
		if err := rows.Scan(&ev.ID, &ev.CreatedAt, &ev.Operation, &ev.UserID, &ev.UserType,
			&ev.WorkspaceID, &ev.ArtifactID, &ev.ArtifactName, &props); err != nil {
			return nil, err
		}
		if props != "" && props != "{}" {
			_ = json.Unmarshal([]byte(props), &ev.Properties)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// UTCTime renders an event's timestamp the way the audit payload does.
func (ev *ActivityEvent) UTCTime() string {
	return time.Unix(ev.CreatedAt, 0).UTC().Format("2006-01-02T15:04:05Z")
}
