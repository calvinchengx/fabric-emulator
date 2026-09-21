package store

import (
	"fmt"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

// "If the source table has any OneLake-level security rule applied — such as
// row-level security (RLS) or column-level security (CLS) — the SQL analytics
// endpoint blocks access to that shortcut", in delegated identity mode.
func TestDelegatedShortcutBlock(t *testing.T) {
	st, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ws := &Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	mk := func(typ, name string) *Item {
		it := &Item{WorkspaceID: ws.ID, Type: typ, DisplayName: name}
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
		return it
	}
	lake, src, ep := mk("Lakehouse", "lake"), mk("Lakehouse", "src"), mk("SQLEndpoint", "lake")
	if err := st.SetItemProperties(ep.ID, map[string]string{PropParentLakehouse: lake.ID}); err != nil {
		t.Fatal(err)
	}
	role := func(name, table, constraints string) OneLakeRole {
		if constraints != "" {
			constraints = `,"constraints":{` + constraints + `}`
		}
		return OneLakeRole{ItemID: src.ID, Name: name, Body: []byte(fmt.Sprintf(`{"name":%q,"decisionRules":[{"effect":"Permit","permission":[
		  {"attributeName":"Path","attributeValueIncludedIn":["Tables/%s"]},
		  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}]%s}],
		  "members":{"microsoftEntraMembers":[{"objectId":"someone"}]}}`, name, table, constraints))}
	}
	rows := `"rows":[{"tablePath":"/Tables/t","value":"SELECT * FROM t WHERE a = 1"}]`
	cols := `"columns":[{"tablePath":"/Tables/t","columnNames":["a"],"columnEffect":"Permit","columnAction":["Read"]}]`
	sc := &Shortcut{ItemID: lake.ID, Path: "Tables", Name: "t", TargetType: "OneLake", TargetWorkspace: ws.ID, TargetItem: src.ID, TargetPath: "Tables/t"}

	check := func(name string, roles []OneLakeRole, shortcut *Shortcut, want string) {
		t.Helper()
		if err := st.PutOneLakeRoles(src.ID, roles); err != nil {
			t.Fatal(err)
		}
		if got, err := st.DelegatedShortcutBlock(lake, shortcut); err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", name, got, err, want)
		}
	}
	check("no roles at the source", nil, sc, "")
	check("a role granting the table whole", []OneLakeRole{role("R", "t", "")}, sc, "")
	check("a row filter", []OneLakeRole{role("R", "t", rows)}, sc, "row-level security")
	check("a column allow-list", []OneLakeRole{role("R", "t", cols)}, sc, "column-level security")
	check("both", []OneLakeRole{role("R", "t", rows+","+cols)}, sc, "row-level and column-level security")
	check("any role counts, not only the whole one", []OneLakeRole{role("Whole", "t", ""), role("Filtered", "t", rows)}, sc, "row-level security")
	check("a rule on another table", []OneLakeRole{role("R", "other", rows)}, sc, "")
	check("an external shortcut has no OneLake source", []OneLakeRole{role("R", "t", rows)},
		&Shortcut{ItemID: lake.ID, Path: "Tables", Name: "t", TargetType: "AmazonS3", TargetLocation: "https://s3.example", ConnectionID: "c"}, "")
	check("a source that is gone", []OneLakeRole{role("R", "t", rows)},
		&Shortcut{ItemID: lake.ID, Path: "Tables", Name: "t", TargetType: "OneLake", TargetWorkspace: ws.ID, TargetItem: "missing", TargetPath: "Tables/t"}, "")

	// User identity mode reads as the caller, so nothing is blocked here.
	if err := st.SetItemProperties(ep.ID, map[string]string{PropDataAccessMode: AccessModeUserIdentity}); err != nil {
		t.Fatal(err)
	}
	check("user identity mode", []OneLakeRole{role("R", "t", rows)}, sc, "")

	// It fails closed when the store cannot answer.
	if _, err := st.db.Exec(`DROP TABLE item_properties`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DelegatedShortcutBlock(lake, sc); err == nil {
		t.Error("the store could not say which mode the endpoint is in, and the shortcut was judged anyway")
	}
}
