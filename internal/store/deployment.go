package store

// Deployment pipelines (docs/23): the stage model and its access control.
// A pipeline is 2–10 ordered stages; each stage may have at most one
// workspace assigned. Deployment itself (D2) and pairing (D1) build on this.

import (
	"database/sql"
	"errors"
	"fmt"
)

// Stage-count bounds, from intro-to-deployment-pipelines.md ("anywhere from
// two to 10 stages ... The default is 3").
const (
	MinStages     = 2
	MaxStages     = 10
	DefaultStages = 3
)

// ErrStageCount is returned when a create would leave a pipeline outside the
// documented 2–10 stage bounds.
var ErrStageCount = errors.New("store: stage count out of range")

// DefaultStageNames are the three stages Fabric seeds a new pipeline with.
var DefaultStageNames = [DefaultStages]string{"Development", "Test", "Production"}

// DeploymentPipeline is the pipeline object itself. Stages hang off it and
// are always read as an ordered set.
type DeploymentPipeline struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
}

// DeploymentStage is one ordered stage. WorkspaceID is empty until a
// workspace is assigned (D1); deleting that workspace unassigns the stage
// rather than leaving a dangling reference.
type DeploymentStage struct {
	ID          string `json:"id"`
	PipelineID  string `json:"-"`
	Order       int    `json:"order"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	IsPublic    bool   `json:"isPublic"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	// WorkspaceName is resolved at read time from the workspace itself, so a
	// rename shows through without a write here.
	WorkspaceName string `json:"workspaceName,omitempty"`
}

// CreateDeploymentPipeline writes the pipeline, its stages (renumbered 0..n-1
// in the order given), and the creator's Admin role, atomically.
func (s *Store) CreateDeploymentPipeline(p *DeploymentPipeline, stages []*DeploymentStage, creator Principal) error {
	if len(stages) < MinStages || len(stages) > MaxStages {
		return fmt.Errorf("%w: %d (want %d–%d)", ErrStageCount, len(stages), MinStages, MaxStages)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if p.ID == "" {
		p.ID = NewID()
	}
	if _, err := tx.Exec(
		`INSERT INTO deployment_pipelines (id, display_name, description) VALUES (?, ?, ?)`,
		p.ID, p.DisplayName, p.Description); err != nil {
		return err
	}
	for i, st := range stages {
		if st.ID == "" {
			st.ID = NewID()
		}
		st.PipelineID, st.Order = p.ID, i
		if _, err := tx.Exec(`
INSERT INTO deployment_pipeline_stages (id, pipeline_id, stage_order, display_name, description, is_public)
VALUES (?, ?, ?, ?, ?, ?)`,
			st.ID, p.ID, i, st.DisplayName, st.Description, st.IsPublic); err != nil {
			return err
		}
	}
	// The creator becomes Admin, mirroring workspace creation — otherwise the
	// pipeline would be invisible to its own author on List.
	if _, err := tx.Exec(`
INSERT INTO deployment_pipeline_roles (pipeline_id, principal_id, principal_type, role)
VALUES (?, ?, ?, ?)`, p.ID, creator.ID, creator.Type, RoleAdmin); err != nil {
		return err
	}
	return tx.Commit()
}

// GetDeploymentPipeline fetches one pipeline regardless of access; callers
// gate on DeploymentPipelineRole.
func (s *Store) GetDeploymentPipeline(id string) (*DeploymentPipeline, error) {
	p := &DeploymentPipeline{}
	err := s.db.QueryRow(
		`SELECT id, display_name, description FROM deployment_pipelines WHERE id = ?`, id).
		Scan(&p.ID, &p.DisplayName, &p.Description)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// ListDeploymentPipelinesFor returns the pipelines the principal holds a role
// on — "deployment pipelines that the user has access to".
func (s *Store) ListDeploymentPipelinesFor(principalID string) ([]*DeploymentPipeline, error) {
	rows, err := s.db.Query(`
SELECT p.id, p.display_name, p.description
FROM deployment_pipelines p
JOIN deployment_pipeline_roles r ON r.pipeline_id = p.id
WHERE r.principal_id = ?
ORDER BY p.rowid`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*DeploymentPipeline{}
	for rows.Next() {
		p := &DeploymentPipeline{}
		if err := rows.Scan(&p.ID, &p.DisplayName, &p.Description); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdateDeploymentPipeline writes displayName/description.
func (s *Store) UpdateDeploymentPipeline(p *DeploymentPipeline) error {
	_, err := s.db.Exec(
		`UPDATE deployment_pipelines SET display_name = ?, description = ? WHERE id = ?`,
		p.DisplayName, p.Description, p.ID)
	return err
}

// DeleteDeploymentPipeline removes the pipeline; stages and roles cascade.
func (s *Store) DeleteDeploymentPipeline(id string) error {
	_, err := s.db.Exec(`DELETE FROM deployment_pipelines WHERE id = ?`, id)
	return err
}

// scanStages reads stage rows and resolves each assigned workspace's current
// display name.
func (s *Store) scanStages(rows *sql.Rows) ([]*DeploymentStage, error) {
	defer rows.Close()
	out := []*DeploymentStage{}
	for rows.Next() {
		st := &DeploymentStage{}
		var wsID sql.NullString
		if err := rows.Scan(&st.ID, &st.PipelineID, &st.Order, &st.DisplayName,
			&st.Description, &st.IsPublic, &wsID); err != nil {
			return nil, err
		}
		st.WorkspaceID = wsID.String
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, st := range out {
		if st.WorkspaceID == "" {
			continue
		}
		ws, err := s.GetWorkspace(st.WorkspaceID)
		if errors.Is(err, ErrNotFound) {
			// The ON DELETE SET NULL below should prevent this; if it ever
			// happens, report the stage unassigned rather than inventing a name.
			st.WorkspaceID = ""
			continue
		}
		if err != nil {
			return nil, err
		}
		st.WorkspaceName = ws.DisplayName
	}
	return out, nil
}

const stageCols = `id, pipeline_id, stage_order, display_name, description, is_public, workspace_id`

// ListDeploymentStages returns a pipeline's stages in order.
func (s *Store) ListDeploymentStages(pipelineID string) ([]*DeploymentStage, error) {
	rows, err := s.db.Query(
		`SELECT `+stageCols+` FROM deployment_pipeline_stages WHERE pipeline_id = ? ORDER BY stage_order`,
		pipelineID)
	if err != nil {
		return nil, err
	}
	return s.scanStages(rows)
}

// GetDeploymentStage fetches one stage of one pipeline.
func (s *Store) GetDeploymentStage(pipelineID, stageID string) (*DeploymentStage, error) {
	rows, err := s.db.Query(
		`SELECT `+stageCols+` FROM deployment_pipeline_stages WHERE pipeline_id = ? AND id = ?`,
		pipelineID, stageID)
	if err != nil {
		return nil, err
	}
	sts, err := s.scanStages(rows)
	if err != nil {
		return nil, err
	}
	if len(sts) == 0 {
		return nil, ErrNotFound
	}
	return sts[0], nil
}

// UpdateDeploymentStage writes the mutable stage fields (name, description,
// visibility). Order and workspace assignment are not editable here.
func (s *Store) UpdateDeploymentStage(st *DeploymentStage) error {
	_, err := s.db.Exec(`
UPDATE deployment_pipeline_stages SET display_name = ?, description = ?, is_public = ?
WHERE pipeline_id = ? AND id = ?`,
		st.DisplayName, st.Description, st.IsPublic, st.PipelineID, st.ID)
	return err
}

// SetStageWorkspace assigns (or, with an empty id, unassigns) a stage's
// workspace. The REST endpoints that drive this land in D1 together with
// item pairing; the store half is here because stage reads depend on it.
func (s *Store) SetStageWorkspace(pipelineID, stageID, workspaceID string) error {
	var arg any
	if workspaceID != "" {
		arg = workspaceID
	}
	_, err := s.db.Exec(
		`UPDATE deployment_pipeline_stages SET workspace_id = ? WHERE pipeline_id = ? AND id = ?`,
		arg, pipelineID, stageID)
	return err
}

// DeploymentPipelineRole returns the principal's role on the pipeline, or
// ErrNotFound when they hold none.
func (s *Store) DeploymentPipelineRole(pipelineID, principalID string) (string, error) {
	var role string
	err := s.db.QueryRow(
		`SELECT role FROM deployment_pipeline_roles WHERE pipeline_id = ? AND principal_id = ?`,
		pipelineID, principalID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return role, err
}
