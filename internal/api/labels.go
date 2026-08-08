package api

// Sensitivity labels: the admin bulk label APIs cited by fabric-docs
// governance/sensitivity-label-audit-schema.md —
//
//	POST /v1/admin/items/bulkSetLabels
//	POST /v1/admin/items/bulkRemoveLabels
//
// Applying or clearing a label writes a real audit event with the documented
// SensitivityLabelEventData fields (SensitivityLabelId,
// OldSensitivityLabelId, ActionSource, ActionSourceDetail, LabelEventType),
// so the activityevents API surfaces label history like the real service.
//
// The label taxonomy is the emulator's own — real Fabric gets labels and
// their sensitivity order from Purview, which cannot be attached offline, and
// LabelEventType (upgraded / downgraded / same order) is only meaningful with
// an order to compare against. GET /v1/admin/labels exposes it so a client can
// discover the ids; that read is an emulator affordance, not a Fabric API.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Documented activity keys for label changes.
const (
	opLabelApplied = "SensitivityLabelApplied"
	opLabelChanged = "SensitivityLabelChanged"
	opLabelRemoved = "SensitivityLabelRemoved"
)

func (a *API) registerLabels(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/labels", a.withTenantRead(a.listLabels))
	mux.HandleFunc("POST /v1/admin/items/bulkSetLabels", a.withTenantAdmin(a.bulkSetLabels))
	mux.HandleFunc("POST /v1/admin/items/bulkRemoveLabels", a.withTenantAdmin(a.bulkRemoveLabels))
}

func (a *API) listLabels(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	ls, err := a.Store.ListLabels()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"labels": ls})
}

// bulkLabelBody is the shape both bulk endpoints take: the items to act on,
// plus (for set) the label to apply.
type bulkLabelBody struct {
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
	LabelID string `json:"labelId"`
}

// labelOutcome is one entry of the per-item result. Bulk APIs report
// successes and failures separately rather than failing the whole call.
type labelOutcome struct {
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
}

func decodeBulkLabels(w http.ResponseWriter, r *http.Request) (*bulkLabelBody, bool) {
	var body bulkLabelBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Items) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "items is required.")
		return nil, false
	}
	return &body, true
}

// findItem locates an item by id without knowing its workspace: the bulk
// label APIs are tenant-scoped and address items by id alone.
func (a *API) findItem(id string) (*store.Item, error) {
	return a.Store.GetItemByID(id)
}

func (a *API) bulkSetLabels(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	body, ok := decodeBulkLabels(w, r)
	if !ok {
		return
	}
	if body.LabelID == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "labelId is required.")
		return
	}
	label, err := a.Store.GetLabel(body.LabelID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "LabelNotFound",
				"No label matches labelId. GET /v1/admin/labels lists them.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	succeeded, failed := []labelOutcome{}, []labelOutcome{}
	for _, ref := range body.Items {
		it, err := a.findItem(ref.ID)
		if err != nil {
			failed = append(failed, labelOutcome{ID: ref.ID, Error: "ItemNotFound"})
			continue
		}
		old, _ := a.Store.ItemLabel(it.ID)
		if err := a.Store.SetItemLabel(it.ID, label.ID); err != nil {
			failed = append(failed, labelOutcome{ID: ref.ID, Error: "InternalError"})
			continue
		}
		a.auditLabelChange(p, it, old, label)
		succeeded = append(succeeded, labelOutcome{ID: ref.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"successfulItems": succeeded, "failedItems": failed,
	})
}

func (a *API) bulkRemoveLabels(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	body, ok := decodeBulkLabels(w, r)
	if !ok {
		return
	}
	succeeded, failed := []labelOutcome{}, []labelOutcome{}
	for _, ref := range body.Items {
		it, err := a.findItem(ref.ID)
		if err != nil {
			failed = append(failed, labelOutcome{ID: ref.ID, Error: "ItemNotFound"})
			continue
		}
		old, _ := a.Store.ItemLabel(it.ID)
		had, err := a.Store.RemoveItemLabel(it.ID)
		if err != nil {
			failed = append(failed, labelOutcome{ID: ref.ID, Error: "InternalError"})
			continue
		}
		// Removing a label an item never had is not an error, but it is not
		// a label *change* either — no event is written for it.
		if had {
			a.auditLabelChange(p, it, old, nil)
		}
		succeeded = append(succeeded, labelOutcome{ID: ref.ID})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"successfulItems": succeeded, "failedItems": failed,
	})
}

// auditLabelChange writes the documented SensitivityLabelEventData. Which
// activity key applies, and which of the two label-id fields appear, is
// exactly what the schema prescribes:
//
//	SensitivityLabelApplied — no previous label; carries SensitivityLabelId
//	SensitivityLabelChanged — replaced;  carries both ids
//	SensitivityLabelRemoved — cleared;   carries OldSensitivityLabelId
func (a *API) auditLabelChange(p *auth.Principal, it *store.Item, old, next *store.SensitivityLabel) {
	props := map[string]any{
		"ActionSource":       store.ActionSourceManual,
		"ActionSourceDetail": store.ActionSourceDetailAPI,
		"LabelEventType":     store.LabelEventType(old, next),
		"ArtifactType":       store.ArtifactTypeFabricItem,
	}
	op := opLabelApplied
	switch {
	case next == nil:
		op = opLabelRemoved
	case old != nil:
		op = opLabelChanged
	}
	if next != nil {
		props["SensitivityLabelId"] = next.ID
	}
	if old != nil {
		props["OldSensitivityLabelId"] = old.ID
	}
	a.audit(p, &store.ActivityEvent{
		Operation: op, WorkspaceID: it.WorkspaceID,
		ArtifactID: it.ID, ArtifactName: it.DisplayName, Properties: props,
	})
}
