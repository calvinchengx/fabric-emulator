package store

// Deploy Stage Content (docs/23, D2): promotion between adjacent stages.
//
// Three rules the implementation exists to honour, all easy to get wrong:
//
//   1. METADATA ONLY. The item and its definition are copied; OneLake bytes,
//      role assignments and item IDs are not. A deployed Lakehouse arrives
//      empty, which is correct.
//   2. NOT A MIRROR. Items present in the target but absent from the source
//      are left alone. updateFromGit deletes stale items; deploy must not,
//      and reusing that path here would be the natural wrong shortcut.
//   3. PAIRS DECIDE, NOT NAMES. A paired target item is overwritten in place
//      whatever it is now called; an unpaired source item is clean-deployed
//      and the new copy is paired.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Deployment outcomes recorded per item.
const (
	DeployCreated = "Created" // clean deploy: no pair existed
	DeployUpdated = "Updated" // paired item overwritten in place
)

var (
	// ErrStagesNotAdjacent rejects a deploy between non-neighbouring stages:
	// pairing only ever spans adjacent stages, so anything else has no
	// defined meaning.
	ErrStagesNotAdjacent = errors.New("store: stages are not adjacent")
	// ErrStageUnassigned rejects a deploy involving a stage with no workspace.
	ErrStageUnassigned = errors.New("store: stage has no workspace assigned")
)

// DeployedItem is one line of a deployment's result.
type DeployedItem struct {
	SourceItemID string `json:"sourceItemId"`
	TargetItemID string `json:"targetItemId"`
	ItemType     string `json:"itemType"`
	DisplayName  string `json:"displayName"`
	Outcome      string `json:"outcome"`
}

// DeploymentOperation is the record behind Get/List Deployment Pipeline
// Operations, and the body served from the LRO's /result.
type DeploymentOperation struct {
	ID            string         `json:"id"`
	PipelineID    string         `json:"-"`
	SourceStageID string         `json:"sourceStageId"`
	TargetStageID string         `json:"targetStageId"`
	Note          string         `json:"note,omitempty"`
	PerformedBy   string         `json:"performedBy,omitempty"`
	CreatedAt     int64          `json:"-"`
	Items         []DeployedItem `json:"items"`
}

// ItemSelector names one item to deploy. An empty selection deploys
// everything in the source stage ("If no specific items are selected, all
// items are deployed").
type ItemSelector struct {
	SourceItemID string
	ItemType     string
}

// DeployStageContent promotes items from one stage to the adjacent one.
//
// opID ties the record to the LRO the caller already started, so
// /operations/{id}/result can serve the detail.
func (s *Store) DeployStageContent(pipelineID, sourceStageID, targetStageID, opID, note, performedBy string,
	selected []ItemSelector) (*DeploymentOperation, error) {

	source, err := s.GetDeploymentStage(pipelineID, sourceStageID)
	if err != nil {
		return nil, err
	}
	target, err := s.GetDeploymentStage(pipelineID, targetStageID)
	if err != nil {
		return nil, err
	}
	if diff := source.Order - target.Order; diff != 1 && diff != -1 {
		return nil, fmt.Errorf("%w: stage %d -> %d", ErrStagesNotAdjacent, source.Order, target.Order)
	}
	if source.WorkspaceID == "" || target.WorkspaceID == "" {
		return nil, ErrStageUnassigned
	}

	items, err := s.ListItems(source.WorkspaceID, "")
	if err != nil {
		return nil, err
	}
	if len(selected) > 0 {
		want := make(map[string]bool, len(selected))
		for _, sel := range selected {
			want[sel.SourceItemID] = true
		}
		kept := items[:0]
		for _, it := range items {
			if want[it.ID] {
				kept = append(kept, it)
			}
		}
		items = kept
	}

	// Pairs are stored earlier-stage-first; a backward deploy reads them the
	// other way round.
	earlierStage, laterStage := source, target
	if target.Order < source.Order {
		earlierStage, laterStage = target, source
	}
	pairs, err := s.ListItemPairs(pipelineID, earlierStage.ID, laterStage.ID)
	if err != nil {
		return nil, err
	}
	partner := make(map[string]string, len(pairs)) // source item id -> target item id
	for _, pr := range pairs {
		if earlierStage.ID == source.ID {
			partner[pr.EarlierItemID] = pr.LaterItemID
		} else {
			partner[pr.LaterItemID] = pr.EarlierItemID
		}
	}

	op := &DeploymentOperation{
		ID: opID, PipelineID: pipelineID,
		SourceStageID: sourceStageID, TargetStageID: targetStageID,
		Note: note, PerformedBy: performedBy, CreatedAt: s.Now(),
		Items: []DeployedItem{},
	}

	for _, src := range items {
		parts, err := s.GetDefinition(src.ID)
		if err != nil {
			return nil, err
		}
		line := DeployedItem{SourceItemID: src.ID, ItemType: src.Type, DisplayName: src.DisplayName}

		if targetID, paired := partner[src.ID]; paired {
			// Overwrite in place. The target keeps its own id AND its own
			// display name: renaming does not unpair, so a paired item may
			// legitimately be called something else (docs/23 Q2).
			tgt, err := s.GetItemByID(targetID)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					// The pair outlived its item; treat as unpaired below.
					paired = false
				} else {
					return nil, err
				}
			}
			if paired {
				tgt.Description = src.Description
				if err := s.UpdateItem(tgt); err != nil {
					return nil, err
				}
				if err := s.SetDefinition(tgt.ID, parts); err != nil {
					return nil, err
				}
				line.TargetItemID, line.Outcome = tgt.ID, DeployUpdated
				op.Items = append(op.Items, line)
				continue
			}
		}

		// Clean deploy: create in the target and pair the copy.
		clone := &Item{
			WorkspaceID: target.WorkspaceID,
			Type:        src.Type,
			DisplayName: src.DisplayName,
			Description: src.Description,
		}
		if err := s.CreateItem(clone, parts); err != nil {
			// A same-named unpaired item already in the target trips the
			// uniqueness index. Fail loudly rather than silently skipping or
			// renaming — a silent divergence in a promotion path is the worst
			// outcome available (docs/23 Q1).
			return nil, err
		}
		earlierItem, laterItem := src.ID, clone.ID
		if earlierStage.ID != source.ID {
			earlierItem, laterItem = clone.ID, src.ID
		}
		if _, err := s.db.Exec(`
INSERT INTO deployment_pipeline_pairs
  (pipeline_id, earlier_stage_id, earlier_item_id, later_stage_id, later_item_id)
VALUES (?, ?, ?, ?, ?)`,
			pipelineID, earlierStage.ID, earlierItem, laterStage.ID, laterItem); err != nil {
			return nil, err
		}
		line.TargetItemID, line.Outcome = clone.ID, DeployCreated
		op.Items = append(op.Items, line)
	}

	if err := s.recordDeployment(op); err != nil {
		return nil, err
	}
	return op, nil
}

func (s *Store) recordDeployment(op *DeploymentOperation) error {
	detail, err := json.Marshal(op.Items)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
INSERT INTO deployment_pipeline_operations
  (id, pipeline_id, source_stage_id, target_stage_id, note, performed_by, created_at, detail)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.PipelineID, op.SourceStageID, op.TargetStageID,
		op.Note, op.PerformedBy, op.CreatedAt, string(detail))
	return err
}

const deployOpCols = `id, pipeline_id, source_stage_id, target_stage_id, note, performed_by, created_at, detail`

func scanDeployOp(scan func(...any) error) (*DeploymentOperation, error) {
	op := &DeploymentOperation{}
	var detail string
	if err := scan(&op.ID, &op.PipelineID, &op.SourceStageID, &op.TargetStageID,
		&op.Note, &op.PerformedBy, &op.CreatedAt, &detail); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(detail), &op.Items); err != nil {
		return nil, err
	}
	return op, nil
}

// GetDeploymentOperation fetches one recorded deployment.
func (s *Store) GetDeploymentOperation(pipelineID, opID string) (*DeploymentOperation, error) {
	row := s.db.QueryRow(
		`SELECT `+deployOpCols+` FROM deployment_pipeline_operations WHERE pipeline_id = ? AND id = ?`,
		pipelineID, opID)
	op, err := scanDeployOp(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return op, nil
}

// GetDeploymentOperationByID fetches a deployment by operation id alone —
// the LRO result path knows the operation, not the pipeline.
func (s *Store) GetDeploymentOperationByID(opID string) (*DeploymentOperation, error) {
	row := s.db.QueryRow(
		`SELECT `+deployOpCols+` FROM deployment_pipeline_operations WHERE id = ?`, opID)
	op, err := scanDeployOp(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return op, nil
}

// ListDeploymentOperations returns a pipeline's deployments, newest first.
func (s *Store) ListDeploymentOperations(pipelineID string) ([]*DeploymentOperation, error) {
	rows, err := s.db.Query(
		`SELECT `+deployOpCols+` FROM deployment_pipeline_operations
		 WHERE pipeline_id = ? ORDER BY created_at DESC, rowid DESC`, pipelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*DeploymentOperation{}
	for rows.Next() {
		op, err := scanDeployOp(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
