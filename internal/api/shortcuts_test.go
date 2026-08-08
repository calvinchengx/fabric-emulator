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

// A Dataverse target is addressed unlike every other one: no `location`, and
// four fields that together name the environment, the folder and the table.
// The response must echo those four back — reusing the storage-shaped
// location/subpath DTO would invent fields the reference does not have and
// drop two that it does.
func TestDataverseShortcutRoundTripsItsDocumentedFields(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "src"}
	if err := st.CreateItem(src, nil); err != nil {
		t.Fatal(err)
	}
	conn := &store.Connection{DisplayName: "dataverse", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	pv := map[string]string{"wid": ws.ID, "iid": src.ID}
	body := `{"path":"Tables","name":"account","target":{"dataverse":{` +
		`"environmentDomain":"https://contoso.crm11.dynamics.com",` +
		`"deltaLakeFolder":"deltalake","tableName":"account",` +
		`"connectionId":"` + conn.ID + `"}}}`

	w := do(a.createShortcut, admin, "POST", body, pv)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body.Bytes())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	target, _ := got["target"].(map[string]any)
	if target == nil || target["type"] != "Dataverse" {
		t.Fatalf("target = %#v, want type Dataverse", target)
	}
	if target["location"] != nil || target["subpath"] != nil {
		t.Fatalf("target carries storage fields it should not: %#v", target)
	}
	dv, _ := target["dataverse"].(map[string]any)
	for field, want := range map[string]string{
		"environmentDomain": "https://contoso.crm11.dynamics.com",
		"deltaLakeFolder":   "deltalake",
		"tableName":         "account",
		"connectionId":      conn.ID,
	} {
		if dv[field] != want {
			t.Errorf("dataverse.%s = %v, want %q", field, dv[field], want)
		}
	}

	// The GET must reconstruct the same four fields from storage. deltaLakeFolder
	// and tableName share one path in the store, so a bad split shows up here
	// and nowhere else.
	w = do(a.getShortcut, admin, "GET", "", map[string]string{
		"wid": ws.ID, "iid": src.ID, "path": "Tables", "name": "account"})
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d", w.Code)
	}
	var reread map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &reread)
	rdv := reread["target"].(map[string]any)["dataverse"].(map[string]any)
	if rdv["deltaLakeFolder"] != "deltalake" || rdv["tableName"] != "account" {
		t.Fatalf("re-read folder/table = %v/%v, want deltalake/account — the two "+
			"documented fields are not surviving the round trip", rdv["deltaLakeFolder"], rdv["tableName"])
	}
}

// None of the four fields has a defensible default: together they ARE the
// location. A target missing one cannot resolve, so it must be refused at
// create rather than 502 later at read.
func TestDataverseShortcutRequiresEveryDocumentedField(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "src"}
	if err := st.CreateItem(src, nil); err != nil {
		t.Fatal(err)
	}
	conn := &store.Connection{DisplayName: "dataverse", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	pv := map[string]string{"wid": ws.ID, "iid": src.ID}
	full := map[string]string{
		"environmentDomain": `"https://contoso.crm11.dynamics.com"`,
		"deltaLakeFolder":   `"deltalake"`,
		"tableName":         `"account"`,
		"connectionId":      `"` + conn.ID + `"`,
	}
	for omit := range full {
		fields := ""
		for k, v := range full {
			if k == omit {
				continue
			}
			if fields != "" {
				fields += ","
			}
			fields += `"` + k + `":` + v
		}
		body := `{"path":"Tables","name":"x","target":{"dataverse":{` + fields + `}}}`
		w := do(a.createShortcut, admin, "POST", body, pv)
		if w.Code != http.StatusBadRequest {
			t.Errorf("omitting %s = %d, want 400 — a Dataverse target with no %s "+
				"cannot address anything", omit, w.Code, omit)
		}
	}

	// A non-http(s) environmentDomain is refused for the same reason the other
	// external targets refuse a bad location: the read path builds a URL from it.
	bad := `{"path":"Tables","name":"y","target":{"dataverse":{` +
		`"environmentDomain":"contoso.crm11.dynamics.com","deltaLakeFolder":"d",` +
		`"tableName":"t","connectionId":"` + conn.ID + `"}}}`
	if w := do(a.createShortcut, admin, "POST", bad, pv); w.Code != http.StatusBadRequest {
		t.Errorf("schemeless environmentDomain = %d, want 400", w.Code)
	}

	// And the connection must resolve, or the shortcut has no credential to
	// present under Dataverse's delegated authorization model.
	orphan := `{"path":"Tables","name":"z","target":{"dataverse":{` +
		`"environmentDomain":"https://contoso.crm11.dynamics.com","deltaLakeFolder":"d",` +
		`"tableName":"t","connectionId":"11111111-2222-3333-4444-555555555555"}}}`
	if w := do(a.createShortcut, admin, "POST", orphan, pv); w.Code != http.StatusBadRequest ||
		errorCode(t, w) != "ConnectionNotFound" {
		t.Errorf("unresolvable connectionId = %d %s", w.Code, w.Body.Bytes())
	}
}

// The documented `Type` enum spells ADLS as **AdlsGen2**. The emulator stores
// "ADLSGen2" internally (and keeps doing so — renaming the column would be a
// migration for no gain), but the wire value is a documented enum member and
// was wrong. Caught while adding Dataverse next to it.
func TestExternalShortcutTypeUsesTheDocumentedEnumSpelling(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	src := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "src"}
	if err := st.CreateItem(src, nil); err != nil {
		t.Fatal(err)
	}
	conn := &store.Connection{DisplayName: "s", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	pv := map[string]string{"wid": ws.ID, "iid": src.ID}
	for kind, wantType := range map[string]string{"adlsGen2": "AdlsGen2", "amazonS3": "AmazonS3"} {
		body := `{"path":"Files","name":"` + kind + `","target":{"` + kind +
			`":{"location":"http://storage.test/root","subpath":"/f","connectionId":"` + conn.ID + `"}}}`
		w := do(a.createShortcut, admin, "POST", body, pv)
		if w.Code != http.StatusCreated {
			t.Fatalf("%s create = %d %s", kind, w.Code, w.Body.Bytes())
		}
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if gotType := got["target"].(map[string]any)["type"]; gotType != wantType {
			t.Errorf("%s target.type = %v, want %q (the documented Type enum member)", kind, gotType, wantType)
		}
	}
}
