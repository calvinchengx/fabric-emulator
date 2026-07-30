package api

// Deployment pipelines — D0: the model and its read surface (docs/23).
// Pairing (D1), deployment (D2) and the role-assignment CRUD (D3) build on
// this. Wire shapes are REST-reference-only
// (/rest/api/fabric/core/deployment-pipelines); fabric-docs carries the
// conceptual model, not the schema.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// requirePipeline resolves a pipeline and gates on the caller holding a role
// on it. A principal with no role gets 404, not 403 — the pipeline is not
// theirs to know about, matching how workspaces behave here.
func (a *API) requirePipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) (*store.DeploymentPipeline, bool) {
	pl, err := a.Store.GetDeploymentPipeline(r.PathValue("pid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "DeploymentPipelineNotFound",
				"No deployment pipeline matches the requested id.")
			return nil, false
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, false
	}
	if _, err := a.Store.DeploymentPipelineRole(pl.ID, p.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "DeploymentPipelineNotFound",
				"No deployment pipeline matches the requested id.")
			return nil, false
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, false
	}
	return pl, true
}

// listDeploymentPipelines returns the pipelines the caller holds a role on.
func (a *API) listDeploymentPipelines(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ps, err := a.Store.ListDeploymentPipelinesFor(p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writePage(w, r, ps)
}

// createDeploymentPipeline creates a pipeline; the caller becomes its Admin.
// Omitting stages seeds the documented default three.
func (a *API) createDeploymentPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body struct {
		DisplayName string `json:"displayName"`
		Description string `json:"description"`
		Stages      []struct {
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			IsPublic    bool   `json:"isPublic"`
		} `json:"stages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName is required.")
		return
	}
	var stages []*store.DeploymentStage
	if len(body.Stages) == 0 {
		for _, n := range store.DefaultStageNames {
			stages = append(stages, &store.DeploymentStage{DisplayName: n})
		}
	} else {
		for _, st := range body.Stages {
			if st.DisplayName == "" {
				writeErr(w, http.StatusBadRequest, "InvalidRequest", "Each stage requires a displayName.")
				return
			}
			stages = append(stages, &store.DeploymentStage{
				DisplayName: st.DisplayName, Description: st.Description, IsPublic: st.IsPublic,
			})
		}
	}
	if len(stages) < store.MinStages || len(stages) > store.MaxStages {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"A deployment pipeline must have between 2 and 10 stages.")
		return
	}
	pl := &store.DeploymentPipeline{DisplayName: body.DisplayName, Description: body.Description}
	if err := a.Store.CreateDeploymentPipeline(pl, stages, store.Principal{ID: p.ID, Type: p.Type}); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, pl)
}

func (a *API) getDeploymentPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, ok := a.requirePipeline(w, r, p)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, pl)
}

// updateDeploymentPipeline patches displayName/description. Absent fields are
// left alone, so a caller can rename without restating the description.
func (a *API) updateDeploymentPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, ok := a.requirePipeline(w, r, p)
	if !ok {
		return
	}
	var body struct {
		DisplayName *string `json:"displayName"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed request body.")
		return
	}
	if body.DisplayName != nil {
		if *body.DisplayName == "" {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName may not be empty.")
			return
		}
		pl.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		pl.Description = *body.Description
	}
	if err := a.Store.UpdateDeploymentPipeline(pl); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pl)
}

func (a *API) deleteDeploymentPipeline(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, ok := a.requirePipeline(w, r, p)
	if !ok {
		return
	}
	if err := a.Store.DeleteDeploymentPipeline(pl.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) listDeploymentStages(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	pl, ok := a.requirePipeline(w, r, p)
	if !ok {
		return
	}
	sts, err := a.Store.ListDeploymentStages(pl.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writePage(w, r, sts)
}

// requireStage resolves a stage within an already-gated pipeline.
func (a *API) requireStage(w http.ResponseWriter, r *http.Request, p *auth.Principal) (*store.DeploymentStage, bool) {
	pl, ok := a.requirePipeline(w, r, p)
	if !ok {
		return nil, false
	}
	st, err := a.Store.GetDeploymentStage(pl.ID, r.PathValue("sid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "DeploymentPipelineStageNotFound",
				"No stage matches the requested id.")
			return nil, false
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, false
	}
	return st, true
}

func (a *API) getDeploymentStage(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	st, ok := a.requireStage(w, r, p)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// updateDeploymentStage patches a stage's name/description/visibility. Order
// and workspace assignment are not editable here — assignment has its own
// endpoints (D1).
func (a *API) updateDeploymentStage(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	st, ok := a.requireStage(w, r, p)
	if !ok {
		return
	}
	var body struct {
		DisplayName *string `json:"displayName"`
		Description *string `json:"description"`
		IsPublic    *bool   `json:"isPublic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed request body.")
		return
	}
	if body.DisplayName != nil {
		if *body.DisplayName == "" {
			writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName may not be empty.")
			return
		}
		st.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		st.Description = *body.Description
	}
	if body.IsPublic != nil {
		st.IsPublic = *body.IsPublic
	}
	if err := a.Store.UpdateDeploymentStage(st); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// assignStageWorkspace attaches a workspace to a stage and pairs its items
// against the adjacent stages (D1). The caller must be able to administer the
// workspace they are attaching — otherwise pipeline access alone would let
// anyone pull someone else's workspace into a promotion path.
func (a *API) assignStageWorkspace(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	st, ok := a.requireStage(w, r, p)
	if !ok {
		return
	}
	var body struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkspaceID == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "workspaceId is required.")
		return
	}
	if _, _, ok := a.requireRole(w, body.WorkspaceID, p, store.RoleAdmin); !ok {
		return
	}
	if err := a.Store.AssignStageWorkspace(st.PipelineID, st.ID, body.WorkspaceID); err != nil {
		switch {
		case errors.Is(err, store.ErrPairingAmbiguous):
			// The assignment is refused rather than applied unpaired: an
			// unpaired promotion path duplicates silently on the next deploy.
			writeErr(w, http.StatusConflict, "DeploymentPipelineStagePairingFailed", err.Error())
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, "WorkspaceNotFound", "No workspace matches workspaceId.")
		default:
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		}
		return
	}
	a.writeStage(w, st.PipelineID, st.ID)
}

// unassignStageWorkspace detaches the workspace and drops the stage's pairs.
func (a *API) unassignStageWorkspace(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	st, ok := a.requireStage(w, r, p)
	if !ok {
		return
	}
	if err := a.Store.UnassignStageWorkspace(st.PipelineID, st.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.writeStage(w, st.PipelineID, st.ID)
}

// writeStage re-reads and returns a stage, so the response reflects what was
// actually persisted rather than the pre-write copy.
func (a *API) writeStage(w http.ResponseWriter, pipelineID, stageID string) {
	st, err := a.Store.GetDeploymentStage(pipelineID, stageID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// listDeploymentStageItems returns the supported items in the workspace
// assigned to the stage. An unassigned stage has no items — an empty page,
// not an error: a freshly created pipeline is in exactly that state.
func (a *API) listDeploymentStageItems(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	st, ok := a.requireStage(w, r, p)
	if !ok {
		return
	}
	if st.WorkspaceID == "" {
		writePage(w, r, []*store.Item{})
		return
	}
	items, err := a.Store.ListItems(st.WorkspaceID, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if items == nil {
		items = []*store.Item{}
	}
	writePage(w, r, items)
}
