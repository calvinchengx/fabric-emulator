package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestShortcutsCRUD(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "src"}
	tgt := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "tgt"}
	for _, it := range []*store.Item{src, tgt} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	pvIt := map[string]string{"wid": ws.ID, "iid": src.ID}
	body := func(path, name, twid, tiid string) string {
		return `{"path":"` + path + `","name":"` + name + `","target":{"oneLake":{"workspaceId":"` + twid + `","itemId":"` + tiid + `","path":"Files/data"}}}`
	}

	// Create.
	w := do(a.createShortcut, admin, "POST", body("Files", "linked", ws.ID, tgt.ID), pvIt)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.Bytes())
	}
	// Duplicate → 409.
	if w := do(a.createShortcut, admin, "POST", body("Files", "linked", ws.ID, tgt.ID), pvIt); w.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d", w.Code)
	}
	// Get + list.
	if w := do(a.getShortcut, admin, "GET", "", map[string]string{"wid": ws.ID, "iid": src.ID, "path": "Files", "name": "linked"}); w.Code != http.StatusOK {
		t.Fatalf("get = %d", w.Code)
	}
	var list struct{ Value []struct{ Path, Name string } }
	w = do(a.listShortcuts, admin, "GET", "", pvIt)
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Value) != 1 || list.Value[0].Name != "linked" {
		t.Fatalf("list = %+v", list.Value)
	}
	// Delete + gone.
	dpv := map[string]string{"wid": ws.ID, "iid": src.ID, "path": "Files", "name": "linked"}
	if w := do(a.deleteShortcut, admin, "DELETE", "", dpv); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := do(a.getShortcut, admin, "GET", "", dpv); w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d", w.Code)
	}
	if w := do(a.deleteShortcut, admin, "DELETE", "", dpv); w.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d", w.Code)
	}
}

func TestShortcutValidation(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "src"}
	if err := st.CreateItem(src, nil); err != nil {
		t.Fatal(err)
	}
	pvIt := map[string]string{"wid": ws.ID, "iid": src.ID}

	// External targets validate URL and connection.
	ext := `{"path":"Files","name":"s3link","target":{"amazonS3":{"location":"s3://b/k","connectionId":"x"}}}`
	if w := do(a.createShortcut, admin, "POST", ext, pvIt); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid external target = %d; want 400", w.Code)
	}
	// Malformed / incomplete.
	for _, b := range []string{`{`, `{"name":"x"}`, `{"path":"Files","name":"x"}`, `{"path":"Files","name":"x","target":{"oneLake":{"workspaceId":"w"}}}`} {
		if w := do(a.createShortcut, admin, "POST", b, pvIt); w.Code != http.StatusBadRequest {
			t.Fatalf("bad body %q = %d", b, w.Code)
		}
	}
	// Non-existent target item → 400.
	nt := `{"path":"Files","name":"x","target":{"oneLake":{"workspaceId":"` + ws.ID + `","itemId":"nope"}}}`
	if w := do(a.createShortcut, admin, "POST", nt, pvIt); w.Code != http.StatusBadRequest {
		t.Fatalf("missing target = %d", w.Code)
	}
	// Self-target cycle → 400.
	self := `{"path":"Files","name":"x","target":{"oneLake":{"workspaceId":"` + ws.ID + `","itemId":"` + src.ID + `"}}}`
	if w := do(a.createShortcut, admin, "POST", self, pvIt); w.Code != http.StatusBadRequest {
		t.Fatalf("self target = %d", w.Code)
	}
	// Viewer cannot create; unknown source item 404.
	if w := do(a.createShortcut, viewer, "POST", `{"path":"Files","name":"x","target":{"oneLake":{"workspaceId":"w","itemId":"i"}}}`, pvIt); w.Code != http.StatusForbidden {
		t.Fatalf("viewer create = %d", w.Code)
	}
	if w := do(a.listShortcuts, admin, "GET", "", map[string]string{"wid": ws.ID, "iid": "nope"}); w.Code != http.StatusNotFound {
		t.Fatalf("unknown item list = %d", w.Code)
	}
}

func TestExternalShortcutsCRUD(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "src"}
	if err := st.CreateItem(src, nil); err != nil {
		t.Fatal(err)
	}
	conn := &store.Connection{DisplayName: "object-store", CredentialDetails: &store.CredentialDetails{CredentialType: "Anonymous"}, CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	pv := map[string]string{"wid": ws.ID, "iid": src.ID}
	for _, tc := range []struct{ name, kind string }{{"adls", "adlsGen2"}, {"s3", "amazonS3"}} {
		body := `{"path":"Files","name":"` + tc.name + `","target":{"` + tc.kind + `":{"location":"http://storage.test/root","subpath":"/folder","connectionId":"` + conn.ID + `"}}}`
		w := do(a.createShortcut, admin, "POST", body, pv)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s create = %d %s", tc.kind, w.Code, w.Body.Bytes())
		}
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		target := got["target"].(map[string]any)
		if target["type"] == "" || target[tc.kind] == nil {
			t.Fatalf("%s response = %#v", tc.kind, got)
		}
	}
	missing := `{"path":"Files","name":"bad","target":{"amazonS3":{"location":"https://storage.test","connectionId":"missing"}}}`
	if w := do(a.createShortcut, admin, "POST", missing, pv); w.Code != http.StatusBadRequest || errorCode(t, w) != "ConnectionNotFound" {
		t.Fatalf("missing connection = %d %s", w.Code, w.Body.Bytes())
	}
}
