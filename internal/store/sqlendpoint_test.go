package store

import (
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func TestDataAccessMode(t *testing.T) {
	st, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ws := &Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, Principal{ID: "u", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lake := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	other := &Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "other"}
	for _, it := range []*Item{lake, other} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	// No endpoint item yet: nothing has switched it.
	if m, err := st.DataAccessMode(lake); err != nil || m != AccessModeDelegated {
		t.Fatalf("no endpoint: %q, %v", m, err)
	}
	ep := &Item{WorkspaceID: ws.ID, Type: "SQLEndpoint", DisplayName: "lake"}
	otherEp := &Item{WorkspaceID: ws.ID, Type: "SQLEndpoint", DisplayName: "other"}
	for it, parent := range map[*Item]string{ep: lake.ID, otherEp: other.ID} {
		if err := st.CreateItem(it, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.SetItemProperties(it.ID, map[string]string{PropParentLakehouse: parent}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := st.SQLEndpointOf(lake); err != nil || got.ID != ep.ID {
		t.Fatalf("endpoint of lake = %v, %v", got, err)
	}
	for value, want := range map[string]string{"": AccessModeDelegated, "userIDENTITY": AccessModeUserIdentity, "sometimes": AccessModeDelegated} {
		if err := st.SetItemProperties(ep.ID, map[string]string{PropDataAccessMode: value}); err != nil {
			t.Fatal(err)
		}
		if m, err := st.DataAccessMode(lake); err != nil || m != want {
			t.Errorf("stored %q: %q, %v; want %q", value, m, err, want)
		}
	}
	// One endpoint's mode is not its neighbour's.
	if m, _ := st.DataAccessMode(other); m != AccessModeDelegated {
		t.Errorf("the other lakehouse = %q", m)
	}
	if NormalizeAccessMode("delegatedidentity") != AccessModeDelegated || NormalizeAccessMode("user") != "" {
		t.Error("NormalizeAccessMode")
	}
	// Reads fail closed when the store cannot answer.
	if _, err := st.db.Exec(`DROP TABLE item_properties`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DataAccessMode(lake); err == nil {
		t.Error("an unreadable property table read as delegated")
	}
	if _, err := st.db.Exec(`DROP TABLE items`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SQLEndpointOf(lake); err == nil {
		t.Error("an unreadable item table found no endpoint")
	}
}
