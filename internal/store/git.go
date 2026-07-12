package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// CredentialDetails is the read shape of a connection's credential
// configuration: metadata only — secret material is write-only and never
// serialized back, as in real Fabric.
type CredentialDetails struct {
	CredentialType       string `json:"credentialType"`
	SingleSignOnType     string `json:"singleSignOnType,omitempty"`
	ConnectionEncryption string `json:"connectionEncryption,omitempty"`
}

// Connection is a stored credentials connection (git providers, sources).
// Details are stored verbatim; credential secret material lives only in the
// write-only CredentialsJSON column.
type Connection struct {
	ID                string             `json:"id"`
	DisplayName       string             `json:"displayName"`
	ConnectivityType  string             `json:"connectivityType,omitempty"`
	Details           json.RawMessage    `json:"connectionDetails,omitempty"`
	CredentialDetails *CredentialDetails `json:"credentialDetails,omitempty"`
	// CredentialsJSON holds the secret material (write-only; json:"-").
	CredentialsJSON string `json:"-"`
	CreatedAt       int64  `json:"-"`
}

// CreateConnection stores a connection.
func (s *Store) CreateConnection(c *Connection) error {
	if c.ID == "" {
		c.ID = NewID()
	}
	c.CreatedAt = s.Now()
	details := "{}"
	if len(c.Details) > 0 {
		details = string(c.Details)
	}
	credType, sso, enc := "", "", ""
	if c.CredentialDetails != nil {
		credType = c.CredentialDetails.CredentialType
		sso = c.CredentialDetails.SingleSignOnType
		enc = c.CredentialDetails.ConnectionEncryption
	}
	_, err := s.db.Exec(
		`INSERT INTO connections (id, display_name, connectivity_type, details_json, credential_type, sso_type, encryption, credentials_json, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?)`,
		c.ID, c.DisplayName, c.ConnectivityType, details, credType, sso, enc, c.CredentialsJSON, c.CreatedAt)
	return err
}

func scanConnection(scan func(dest ...any) error) (*Connection, error) {
	c := &Connection{}
	var details, credType, sso, enc string
	err := scan(&c.ID, &c.DisplayName, &c.ConnectivityType, &details, &credType, &sso, &enc, &c.CredentialsJSON, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	c.Details = json.RawMessage(details)
	if credType != "" {
		c.CredentialDetails = &CredentialDetails{CredentialType: credType, SingleSignOnType: sso, ConnectionEncryption: enc}
	}
	return c, nil
}

const connectionCols = `id, display_name, connectivity_type, details_json, credential_type, sso_type, encryption, credentials_json, created_at`

// GetConnection fetches one connection.
func (s *Store) GetConnection(id string) (*Connection, error) {
	row := s.db.QueryRow(`SELECT `+connectionCols+` FROM connections WHERE id = ?`, id)
	c, err := scanConnection(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ListConnections returns all connections.
func (s *Store) ListConnections() ([]*Connection, error) {
	rows, err := s.db.Query(`SELECT ` + connectionCols + ` FROM connections ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Connection
	for rows.Next() {
		c, err := scanConnection(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GitConnection binds a workspace to a git provider branch+directory.
type GitConnection struct {
	WorkspaceID  string
	ProviderJSON string // gitProviderDetails verbatim
	RemoteKey    string // canonical provider|org|project|repo|directory
	Branch       string
	CredSource   string // "Automatic" | "ConfiguredConnection"
	ConnectionID string
	Initialized  bool
	SyncedCommit string // remote commit hash at last sync ("" = never)
}

// SetGitConnection creates or replaces the workspace's git binding.
func (s *Store) SetGitConnection(g *GitConnection) error {
	_, err := s.db.Exec(`
INSERT INTO git_connections (workspace_id, provider_json, remote_key, branch, cred_source, connection_id, initialized, synced_commit)
VALUES (?,?,?,?,?,?,?,?)
ON CONFLICT(workspace_id) DO UPDATE SET
	provider_json = excluded.provider_json, remote_key = excluded.remote_key,
	branch = excluded.branch, cred_source = excluded.cred_source,
	connection_id = excluded.connection_id, initialized = excluded.initialized,
	synced_commit = excluded.synced_commit`,
		g.WorkspaceID, g.ProviderJSON, g.RemoteKey, g.Branch, g.CredSource, g.ConnectionID, g.Initialized, g.SyncedCommit)
	return err
}

// GetGitConnection fetches the workspace's git binding.
func (s *Store) GetGitConnection(workspaceID string) (*GitConnection, error) {
	g := &GitConnection{}
	err := s.db.QueryRow(`
SELECT workspace_id, provider_json, remote_key, branch, cred_source, connection_id, initialized, synced_commit
FROM git_connections WHERE workspace_id = ?`, workspaceID).
		Scan(&g.WorkspaceID, &g.ProviderJSON, &g.RemoteKey, &g.Branch, &g.CredSource, &g.ConnectionID, &g.Initialized, &g.SyncedCommit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return g, err
}

// DeleteGitConnection removes the binding (git disconnect).
func (s *Store) DeleteGitConnection(workspaceID string) error {
	res, err := s.db.Exec(`DELETE FROM git_connections WHERE workspace_id = ?`, workspaceID)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// RemoteItem is one item in the emulated git remote: the committed source
// (definition parts) keyed by type+displayName with a stable logicalId.
type RemoteItem struct {
	LogicalID   string
	Type        string
	DisplayName string
	Parts       []DefinitionPart
}

// GetRemoteHead returns the branch's commit hash ("" when the branch has
// never been committed to).
func (s *Store) GetRemoteHead(remoteKey, branch string) (string, error) {
	var hash string
	err := s.db.QueryRow(
		`SELECT commit_hash FROM git_remote_heads WHERE remote_key = ? AND branch = ?`, remoteKey, branch).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

// ListRemoteItems returns the branch's committed items.
func (s *Store) ListRemoteItems(remoteKey, branch string) ([]*RemoteItem, error) {
	rows, err := s.db.Query(`
SELECT logical_id, item_type, display_name, parts_json
FROM git_remote_items WHERE remote_key = ? AND branch = ? ORDER BY item_type, display_name`, remoteKey, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RemoteItem
	for rows.Next() {
		ri := &RemoteItem{}
		var parts string
		if err := rows.Scan(&ri.LogicalID, &ri.Type, &ri.DisplayName, &parts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(parts), &ri.Parts); err != nil {
			return nil, err
		}
		out = append(out, ri)
	}
	return out, rows.Err()
}

// CommitRemoteItems replaces the branch content with items and stamps a new
// commit hash, preserving logical ids of items that already existed.
func (s *Store) CommitRemoteItems(remoteKey, branch string, items []*RemoteItem) (string, error) {
	existing, err := s.ListRemoteItems(remoteKey, branch)
	if err != nil {
		return "", err
	}
	logical := map[string]string{}
	for _, ri := range existing {
		logical[ri.Type+"\x00"+ri.DisplayName] = ri.LogicalID
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE FROM git_remote_items WHERE remote_key = ? AND branch = ?`, remoteKey, branch); err != nil {
		return "", err
	}
	for _, ri := range items {
		if id := logical[ri.Type+"\x00"+ri.DisplayName]; id != "" {
			ri.LogicalID = id
		} else if ri.LogicalID == "" {
			ri.LogicalID = NewID()
		}
		parts, err := json.Marshal(ri.Parts)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(`
INSERT INTO git_remote_items (remote_key, branch, logical_id, item_type, display_name, parts_json)
VALUES (?,?,?,?,?,?)`, remoteKey, branch, ri.LogicalID, ri.Type, ri.DisplayName, string(parts)); err != nil {
			return "", err
		}
	}
	hash := NewID()
	if _, err := tx.Exec(`
INSERT INTO git_remote_heads (remote_key, branch, commit_hash) VALUES (?,?,?)
ON CONFLICT(remote_key, branch) DO UPDATE SET commit_hash = excluded.commit_hash`,
		remoteKey, branch, hash); err != nil {
		return "", err
	}
	return hash, tx.Commit()
}
