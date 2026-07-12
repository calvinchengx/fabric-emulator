package store

// Folder organizes items within a workspace (the /folders REST resource —
// fabric-cicd lists these on every publish).
type Folder struct {
	ID             string `json:"id"`
	WorkspaceID    string `json:"workspaceId"`
	DisplayName    string `json:"displayName"`
	ParentFolderID string `json:"parentFolderId,omitempty"`
	CreatedAt      int64  `json:"-"`
}

// CreateFolder inserts a folder.
func (s *Store) CreateFolder(f *Folder) error {
	if f.ID == "" {
		f.ID = NewID()
	}
	f.CreatedAt = s.Now()
	_, err := s.db.Exec(
		`INSERT INTO folders (id, workspace_id, display_name, parent_id, created_at) VALUES (?,?,?,?,?)`,
		f.ID, f.WorkspaceID, f.DisplayName, f.ParentFolderID, f.CreatedAt)
	return err
}

// ListFolders returns a workspace's folders.
func (s *Store) ListFolders(workspaceID string) ([]*Folder, error) {
	rows, err := s.db.Query(
		`SELECT id, workspace_id, display_name, parent_id, created_at FROM folders WHERE workspace_id = ? ORDER BY rowid`,
		workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Folder
	for rows.Next() {
		f := &Folder{}
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.DisplayName, &f.ParentFolderID, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
