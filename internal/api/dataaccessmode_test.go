package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// The emulator-native data access mode switch (docs/60).

type modeSwitch struct {
	endpoint, lakehouse, to string
}

func accessModeFixture(t *testing.T) (*API, *store.Store, *store.Workspace, *store.Item, *store.Item, *[]modeSwitch) {
	t.Helper()
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	assignRole(t, st, ws.ID, contributor, store.RoleContributor)
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	ep, err := st.GetItemByID(a.ensureSQLEndpointItem(lake))
	if err != nil {
		t.Fatal(err)
	}
	var calls []modeSwitch
	a.SwitchDataAccessMode = func(_ context.Context, endpoint, lakehouse *store.Item, to string) error {
		calls = append(calls, modeSwitch{endpoint.ID, lakehouse.ID, to})
		return st.SetItemProperties(endpoint.ID, map[string]string{store.PropDataAccessMode: to})
	}
	return a, st, ws, lake, ep, &calls
}

func modeCall(a *API, h handler, p *auth.Principal, method, body, wid, epid string) (int, string) {
	w := do(h, p, method, body, map[string]string{"wid": wid, "epid": epid})
	return w.Code, w.Body.String()
}

func TestDataAccessModeIsDelegatedUntilSwitched(t *testing.T) {
	a, _, ws, lake, ep, calls := accessModeFixture(t)
	if code, body := modeCall(a, a.getDataAccessMode, viewer, "GET", "", ws.ID, ep.ID); code != http.StatusOK || !strings.Contains(body, `"DelegatedIdentity"`) {
		t.Fatalf("default = %d %s", code, body)
	}
	// Admin or Member switches; the hook receives the endpoint, its lakehouse
	// and the target mode; every reader sees the new mode.
	if code, body := modeCall(a, a.putDataAccessMode, admin, "PUT", `{"dataAccessMode":"userIdentity"}`, ws.ID, ep.ID); code != http.StatusOK || !strings.Contains(body, `"UserIdentity"`) {
		t.Fatalf("switch = %d %s", code, body)
	}
	if len(*calls) != 1 || (*calls)[0] != (modeSwitch{ep.ID, lake.ID, store.AccessModeUserIdentity}) {
		t.Fatalf("hook calls = %+v", *calls)
	}
	if _, body := modeCall(a, a.getDataAccessMode, viewer, "GET", "", ws.ID, ep.ID); !strings.Contains(body, `"UserIdentity"`) {
		t.Errorf("after switch = %s", body)
	}
	// Setting the mode it already has is no switch: nothing is closed or dropped.
	if code, _ := modeCall(a, a.putDataAccessMode, admin, "PUT", `{"dataAccessMode":"UserIdentity"}`, ws.ID, ep.ID); code != http.StatusOK || len(*calls) != 1 {
		t.Errorf("a no-op switch = %d, hook calls %d", code, len(*calls))
	}
	if _, _ = modeCall(a, a.putDataAccessMode, admin, "PUT", `{"dataAccessMode":"DelegatedIdentity"}`, ws.ID, ep.ID); len(*calls) != 2 || (*calls)[1].to != store.AccessModeDelegated {
		t.Errorf("switching back: %+v", *calls)
	}
}

// "An Admin or Member must switch it": a Contributor and a Viewer cannot, and
// their attempt changes nothing.
func TestOnlyAdminOrMemberSwitchesTheDataAccessMode(t *testing.T) {
	a, st, ws, lake, ep, calls := accessModeFixture(t)
	member := &auth.Principal{ID: "member-1", Type: "User"}
	assignRole(t, st, ws.ID, member, store.RoleMember)
	for _, p := range []*auth.Principal{viewer, contributor, stranger} {
		if code, _ := modeCall(a, a.putDataAccessMode, p, "PUT", `{"dataAccessMode":"UserIdentity"}`, ws.ID, ep.ID); code == http.StatusOK {
			t.Errorf("%s switched the mode", p.ID)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("a refused switch reached the engine: %+v", *calls)
	}
	if code, body := modeCall(a, a.putDataAccessMode, member, "PUT", `{"dataAccessMode":"UserIdentity"}`, ws.ID, ep.ID); code != http.StatusOK {
		t.Errorf("member = %d %s", code, body)
	}
	if m, _ := st.DataAccessMode(lake); m != store.AccessModeUserIdentity {
		t.Errorf("mode = %q", m)
	}
}

func TestDataAccessModeRefusals(t *testing.T) {
	a, st, ws, _, ep, calls := accessModeFixture(t)
	wh := &store.Item{WorkspaceID: ws.ID, Type: "Warehouse", DisplayName: "dw"}
	orphan := &store.Item{WorkspaceID: ws.ID, Type: "SQLEndpoint", DisplayName: "orphan"}
	for _, it := range []*store.Item{wh, orphan} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		epid, body string
		code       int
		want       string
	}{
		"a warehouse":              {wh.ID, `{"dataAccessMode":"UserIdentity"}`, http.StatusBadRequest, "secured by T-SQL"},
		"an unknown item":          {"nope", `{"dataAccessMode":"UserIdentity"}`, http.StatusNotFound, "ItemNotFound"},
		"an endpoint with no lake": {orphan.ID, `{"dataAccessMode":"UserIdentity"}`, http.StatusNotFound, "serves no lakehouse"},
		"an unknown mode":          {ep.ID, `{"dataAccessMode":"Hybrid"}`, http.StatusBadRequest, "must be DelegatedIdentity or UserIdentity"},
		"an unknown field":         {ep.ID, `{"dataAccessMode":"UserIdentity","force":true}`, http.StatusBadRequest, "unknown field"},
		"not JSON":                 {ep.ID, `UserIdentity`, http.StatusBadRequest, "InvalidRequest"},
	} {
		if code, body := modeCall(a, a.putDataAccessMode, admin, "PUT", tc.body, ws.ID, tc.epid); code != tc.code || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s, want %d naming %q", name, code, body, tc.code, tc.want)
		}
	}
	if code, _ := modeCall(a, a.getDataAccessMode, admin, "GET", "", "no-such-ws", ep.ID); code != http.StatusNotFound {
		t.Errorf("unknown workspace = %d", code)
	}
	if len(*calls) != 0 {
		t.Errorf("a refused request reached the engine: %+v", *calls)
	}
}

// A switch the engine cannot apply leaves the endpoint in the mode it was in;
// without an engine attached, the mode is recorded and there is nothing else.
func TestDataAccessModeSwitchFailuresAndNoEngine(t *testing.T) {
	a, st, ws, lake, ep, _ := accessModeFixture(t)
	a.SwitchDataAccessMode = func(context.Context, *store.Item, *store.Item, string) error { return errors.New("engine down") }
	if code, body := modeCall(a, a.putDataAccessMode, admin, "PUT", `{"dataAccessMode":"UserIdentity"}`, ws.ID, ep.ID); code != http.StatusInternalServerError || !strings.Contains(body, "engine down") {
		t.Fatalf("failed switch = %d %s", code, body)
	}
	if m, _ := st.DataAccessMode(lake); m != store.AccessModeDelegated {
		t.Fatalf("a failed switch changed the mode to %q", m)
	}
	a.SwitchDataAccessMode = nil
	if code, _ := modeCall(a, a.putDataAccessMode, admin, "PUT", `{"dataAccessMode":"UserIdentity"}`, ws.ID, ep.ID); code != http.StatusOK {
		t.Fatalf("no engine = %d", code)
	}
	if m, _ := st.DataAccessMode(lake); m != store.AccessModeUserIdentity {
		t.Fatalf("no engine: mode %q", m)
	}
}

func TestDataAccessModeFailsClosed(t *testing.T) {
	for _, h := range []string{"get", "put"} {
		t.Run(h, func(t *testing.T) {
			a, st, dir := newDiskAPI(t)
			ws := seedWorkspace(t, st)
			lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
			if err := st.CreateItem(lake, nil); err != nil {
				t.Fatal(err)
			}
			epID := a.ensureSQLEndpointItem(lake)
			dropTable(t, dir, "item_properties")
			handlerFor := map[string]handler{"get": a.getDataAccessMode, "put": a.putDataAccessMode}[h]
			if code, _ := modeCall(a, handlerFor, admin, strings.ToUpper(h), `{"dataAccessMode":"UserIdentity"}`, ws.ID, epID); code != http.StatusInternalServerError {
				t.Errorf("%s with unreadable properties = %d", h, code)
			}
		})
	}
}
