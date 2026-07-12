package store

import (
	"database/sql"
	"errors"
)

// WorkspaceIdentity links a workspace to its entra-managed identity. The
// identity itself (app registration + SP + state) lives in entra-emulator;
// fabric caches the ids it needs for RBAC and the wire shape.
type WorkspaceIdentity struct {
	WorkspaceID string `json:"-"`
	// IdentityID is entra's service principal object id.
	IdentityID string `json:"servicePrincipalId"`
	// AppID is the client id — the sub/appid claim in tokens the identity mints.
	AppID     string `json:"applicationId"`
	CreatedAt int64  `json:"-"`
}

// SetWorkspaceIdentity records a provisioned identity (one per workspace).
func (s *Store) SetWorkspaceIdentity(wi *WorkspaceIdentity) error {
	wi.CreatedAt = s.Now()
	_, err := s.db.Exec(`
INSERT INTO workspace_identities (workspace_id, identity_id, app_id, created_at) VALUES (?,?,?,?)
ON CONFLICT(workspace_id) DO UPDATE SET identity_id = excluded.identity_id, app_id = excluded.app_id`,
		wi.WorkspaceID, wi.IdentityID, wi.AppID, wi.CreatedAt)
	return err
}

// GetWorkspaceIdentity fetches the workspace's identity link.
func (s *Store) GetWorkspaceIdentity(workspaceID string) (*WorkspaceIdentity, error) {
	wi := &WorkspaceIdentity{}
	err := s.db.QueryRow(
		`SELECT workspace_id, identity_id, app_id, created_at FROM workspace_identities WHERE workspace_id = ?`,
		workspaceID).Scan(&wi.WorkspaceID, &wi.IdentityID, &wi.AppID, &wi.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return wi, err
}

// DeleteWorkspaceIdentity removes the link (deprovision).
func (s *Store) DeleteWorkspaceIdentity(workspaceID string) error {
	res, err := s.db.Exec(`DELETE FROM workspace_identities WHERE workspace_id = ?`, workspaceID)
	if err != nil {
		return err
	}
	return oneRow(res)
}
