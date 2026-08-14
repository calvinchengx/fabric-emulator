package store

import (
	"database/sql"
	"errors"
)

// Folder organizes items within a workspace (the /folders REST resource —
// fabric-cicd lists these on every publish).
type Folder struct {
	ID             string `json:"id"`
	WorkspaceID    string `json:"workspaceId"`
	DisplayName    string `json:"displayName"`
	ParentFolderID string `json:"parentFolderId,omitempty"`
	CreatedAt      int64  `json:"-"`
}

// ErrFolderNotEmpty is returned when a folder still holds items or child folders.
var ErrFolderNotEmpty = errors.New("folder is not empty")

// ErrFolderCycle is returned when a move would place a folder under itself.
var ErrFolderCycle = errors.New("folder move would create a cycle")

const folderCols = `id, workspace_id, display_name, parent_id, created_at`

func scanFolder(row interface{ Scan(dest ...any) error }) (*Folder, error) {
	f := &Folder{}
	err := row.Scan(&f.ID, &f.WorkspaceID, &f.DisplayName, &f.ParentFolderID, &f.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
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
	return nameConflict(err)
}

// ListFolders returns a workspace's folders.
func (s *Store) ListFolders(workspaceID string) ([]*Folder, error) {
	rows, err := s.db.Query(
		`SELECT `+folderCols+` FROM folders WHERE workspace_id = ? ORDER BY rowid`,
		workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Folder
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFolder fetches one folder scoped to a workspace.
func (s *Store) GetFolder(workspaceID, id string) (*Folder, error) {
	return scanFolder(s.db.QueryRow(
		`SELECT `+folderCols+` FROM folders WHERE workspace_id = ? AND id = ?`,
		workspaceID, id))
}

// UpdateFolder applies a display-name change. Parenting is move, not update.
func (s *Store) UpdateFolder(f *Folder) error {
	res, err := s.db.Exec(
		`UPDATE folders SET display_name = ? WHERE workspace_id = ? AND id = ?`,
		f.DisplayName, f.WorkspaceID, f.ID)
	if err != nil {
		return nameConflict(err)
	}
	return oneRow(res)
}

// DeleteFolder removes an empty folder. Items or child folders are FolderNotEmpty.
func (s *Store) DeleteFolder(workspaceID, id string) error {
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(1) FROM folders WHERE workspace_id = ? AND parent_id = ?`,
		workspaceID, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrFolderNotEmpty
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(1) FROM items WHERE workspace_id = ? AND folder_id = ?`,
		workspaceID, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrFolderNotEmpty
	}
	res, err := s.db.Exec(`DELETE FROM folders WHERE workspace_id = ? AND id = ?`, workspaceID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// MoveFolder reparents a folder ("" = workspace root). A move into the folder
// itself or any of its descendants is a cycle.
func (s *Store) MoveFolder(workspaceID, id, targetFolderID string) error {
	if _, err := s.GetFolder(workspaceID, id); err != nil {
		return err
	}
	if targetFolderID != "" {
		if targetFolderID == id {
			return ErrFolderCycle
		}
		if _, err := s.GetFolder(workspaceID, targetFolderID); err != nil {
			return err
		}
		under, err := s.folderIsUnder(workspaceID, id, targetFolderID)
		if err != nil {
			return err
		}
		if under {
			return ErrFolderCycle
		}
	}
	res, err := s.db.Exec(
		`UPDATE folders SET parent_id = ? WHERE workspace_id = ? AND id = ?`,
		targetFolderID, workspaceID, id)
	if err != nil {
		return nameConflict(err)
	}
	return oneRow(res)
}

// folderIsUnder reports whether nodeID is id or a descendant of id.
func (s *Store) folderIsUnder(workspaceID, ancestorID, nodeID string) (bool, error) {
	seen := map[string]bool{}
	cur := nodeID
	for cur != "" {
		if cur == ancestorID {
			return true, nil
		}
		if seen[cur] {
			return true, nil
		}
		seen[cur] = true
		f, err := s.GetFolder(workspaceID, cur)
		if err != nil {
			return false, err
		}
		cur = f.ParentFolderID
	}
	return false, nil
}
