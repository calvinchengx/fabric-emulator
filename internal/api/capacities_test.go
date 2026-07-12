package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestCapacities(t *testing.T) {
	a, st := newAPI(t)

	// The seeded capacity is listed.
	w := do(a.listCapacities, admin, "GET", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	}
	var list struct {
		Value []struct{ ID, SKU, State string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Value) != 1 || list.Value[0].ID != store.DefaultCapacityID ||
		list.Value[0].SKU != "F64" || list.Value[0].State != "Active" {
		t.Fatalf("capacities = %+v", list.Value)
	}

	// Workspace create with no capacityId auto-assigns the default.
	w = do(a.createWorkspace, admin, "POST", `{"displayName":"auto"}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	var ws struct{ ID, CapacityID string }
	_ = json.Unmarshal(w.Body.Bytes(), &ws)
	if ws.CapacityID != store.DefaultCapacityID {
		t.Fatalf("auto-assigned capacity = %q; want default", ws.CapacityID)
	}
	// An unknown explicit capacityId is refused.
	if w := do(a.createWorkspace, admin, "POST", `{"displayName":"x","capacityId":"nope"}`, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown capacity create = %d; want 404", w.Code)
	}

	wid := map[string]string{"wid": ws.ID}
	// Unassign clears; assign restores; both are Admin-only 202 LROs.
	if w := do(a.unassignFromCapacity, admin, "POST", "", wid); w.Code != http.StatusAccepted {
		t.Fatalf("unassign = %d", w.Code)
	}
	got, _ := st.GetWorkspace(ws.ID)
	if got.CapacityID != "" {
		t.Fatalf("capacity after unassign = %q", got.CapacityID)
	}
	if w := do(a.assignToCapacity, admin, "POST", `{"capacityId":"`+store.DefaultCapacityID+`"}`, wid); w.Code != http.StatusAccepted {
		t.Fatalf("assign = %d %s", w.Code, w.Body.Bytes())
	}
	got, _ = st.GetWorkspace(ws.ID)
	if got.CapacityID != store.DefaultCapacityID {
		t.Fatalf("capacity after assign = %q", got.CapacityID)
	}

	// Error branches: malformed body, unknown capacity, non-Admin.
	if w := do(a.assignToCapacity, admin, "POST", `{`, wid); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed assign = %d", w.Code)
	}
	if w := do(a.assignToCapacity, admin, "POST", `{"capacityId":"nope"}`, wid); w.Code != http.StatusNotFound {
		t.Fatalf("unknown capacity assign = %d", w.Code)
	}
	if w := do(a.assignToCapacity, nobody, "POST", `{"capacityId":"x"}`, wid); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted assign = %d", w.Code)
	}
	if w := do(a.unassignFromCapacity, nobody, "POST", "", wid); w.Code != http.StatusForbidden {
		t.Fatalf("ungranted unassign = %d", w.Code)
	}
}

func TestCapacityStorageFailure(t *testing.T) {
	a, st, dir := newDiskAPI(t)
	ws := seedWorkspace(t, st)
	dropTable(t, dir, "capacities")
	if w := do(a.listCapacities, admin, "GET", "", nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("list = %d; want 500", w.Code)
	}
	if w := do(a.assignToCapacity, admin, "POST", `{"capacityId":"x"}`, map[string]string{"wid": ws.ID}); w.Code != http.StatusInternalServerError {
		t.Fatalf("assign = %d; want 500", w.Code)
	}
}
