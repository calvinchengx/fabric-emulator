package store

import (
	"errors"
	"reflect"
	"testing"
)

// The store half of item permissions. What carries weight is the UNION: access a
// role implies must survive a revoke of the direct grant, and a direct grant must
// reach someone with no role at all. A store that returned only one half would
// pass any test that exercised only one half.

func sharedItem(t *testing.T, s *Store, typ string) (*Workspace, *Item) {
	t.Helper()
	ws := &Workspace{DisplayName: "share-ws-" + typ}
	if err := s.CreateWorkspace(ws, Principal{ID: "owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, DisplayName: "it", Type: typ}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return ws, it
}

func TestInheritedAccessFollowsTheRolesTable(t *testing.T) {
	for _, tc := range []struct {
		role, typ         string
		perms, additional []string
	}{
		{RoleViewer, "Lakehouse", []string{"Read"}, []string{"ReadData"}},
		{RoleContributor, "Lakehouse", []string{"Execute", "Read", "Write"}, []string{"ReadAll", "ReadData"}},
		{RoleMember, "Warehouse", []string{"Execute", "Read", "Reshare", "Write"}, []string{"ReadAll", "ReadData"}},
		{RoleAdmin, "Lakehouse", []string{"Execute", "Read", "Reshare", "Write"}, []string{"ReadAll", "ReadData"}},
		// Semantic models follow Put Dataset User's inheritance statement.
		{RoleViewer, "SemanticModel", []string{"Read"}, []string{}},
		{RoleContributor, "SemanticModel", []string{"Explore", "Read", "Write"}, []string{}},
		{RoleMember, "SemanticModel", []string{"Explore", "Read", "Reshare", "Write"}, []string{}},
		// No role implies nothing at all.
		{"", "Lakehouse", nil, nil},
	} {
		perms, additional := InheritedAccess(tc.role, tc.typ)
		if !reflect.DeepEqual(perms, tc.perms) || (tc.additional != nil && !reflect.DeepEqual(nonNil(additional), tc.additional)) ||
			(tc.additional == nil && additional != nil) {
			t.Errorf("%s on %s = %v %v, want %v %v", tc.role, tc.typ, perms, additional, tc.perms, tc.additional)
		}
	}
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func TestADirectGrantReachesAPrincipalWithNoRole(t *testing.T) {
	s := newTestStore(t)
	_, it := sharedItem(t, s, "Lakehouse")

	before, err := s.EffectiveItemAccess(it, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	if before.Has(PermRead) || before.Direct {
		t.Fatalf("a stranger had access before any grant: %+v", before)
	}

	if err := s.PutItemAccess(ItemAccess{ItemID: it.ID, PrincipalID: "stranger", PrincipalType: "User",
		Permissions: []string{PermRead}, Additional: []string{PermReadAll}}); err != nil {
		t.Fatal(err)
	}
	after, err := s.EffectiveItemAccess(it, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	if !after.Has(PermRead) || !after.Has(PermReadAll) || !after.Direct || after.Role != "" {
		t.Fatalf("the grant did not reach the stranger: %+v", after)
	}
}

// "If Veronica decides to remove Marta's item permissions from the report, Marta
// will still be able to view the report in the workspace."
func TestRevokingAGrantLeavesWhatTheRoleImplies(t *testing.T) {
	s := newTestStore(t)
	ws, it := sharedItem(t, s, "Lakehouse")
	if err := s.CreateRoleAssignment(&RoleAssignment{WorkspaceID: ws.ID,
		Principal: Principal{ID: "marta", Type: "User"}, Role: RoleViewer}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutItemAccess(ItemAccess{ItemID: it.ID, PrincipalID: "marta", PrincipalType: "User",
		Permissions: []string{PermRead}, Additional: []string{PermReadAll}}); err != nil {
		t.Fatal(err)
	}
	granted, _ := s.EffectiveItemAccess(it, "marta")
	if !granted.Has(PermReadAll) || !granted.Has(PermReadData) {
		t.Fatalf("union missing a half: %+v", granted)
	}

	if err := s.DeleteItemAccess(it.ID, "marta"); err != nil {
		t.Fatal(err)
	}
	revoked, _ := s.EffectiveItemAccess(it, "marta")
	if revoked.Has(PermReadAll) || revoked.Direct {
		t.Fatalf("the revoked grant survived: %+v", revoked)
	}
	if !revoked.Has(PermRead) || !revoked.Has(PermReadData) {
		t.Fatalf("revoking the grant took away what the Viewer role gives: %+v", revoked)
	}
}

func TestPutReplacesAGrantRatherThanMerging(t *testing.T) {
	s := newTestStore(t)
	_, it := sharedItem(t, s, "Lakehouse")
	for _, g := range []ItemAccess{
		{ItemID: it.ID, PrincipalID: "p", PrincipalType: "User", Permissions: []string{PermRead, PermReshare}, Additional: []string{PermReadAll}},
		{ItemID: it.ID, PrincipalID: "p", PrincipalType: "User", Permissions: []string{PermRead}},
	} {
		if err := s.PutItemAccess(g); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetItemAccess(it.ID, "p")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Permissions, []string{PermRead}) || len(got.Additional) != 0 {
		t.Fatalf("grant = %+v, want the second PUT only", got)
	}
}

func TestListingIsOrderedAndDeduplicated(t *testing.T) {
	s := newTestStore(t)
	_, it := sharedItem(t, s, "Warehouse")
	for _, id := range []string{"zed", "amy"} {
		if err := s.PutItemAccess(ItemAccess{ItemID: it.ID, PrincipalID: id, PrincipalType: "User",
			Permissions: []string{PermRead, PermRead}, Additional: []string{PermReadData, PermReadData}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListItemAccess(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PrincipalID != "amy" || got[1].PrincipalID != "zed" {
		t.Fatalf("order = %+v", got)
	}
	if !reflect.DeepEqual(got[0].Permissions, []string{PermRead}) || !reflect.DeepEqual(got[0].Additional, []string{PermReadData}) {
		t.Fatalf("not deduplicated: %+v", got[0])
	}
}

func TestRevokingNothingIsNotFound(t *testing.T) {
	s := newTestStore(t)
	_, it := sharedItem(t, s, "Lakehouse")
	if err := s.DeleteItemAccess(it.ID, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if g, err := s.GetItemAccess(it.ID, "nobody"); err != nil || g != nil {
		t.Fatalf("get = %+v, %v; want nil, nil", g, err)
	}
}

// Grants belong to an item, so they go with it rather than attaching to an id a
// later item could be assigned — orphaned access nobody authored.
func TestGrantsCascadeWithTheItem(t *testing.T) {
	s := newTestStore(t)
	ws, it := sharedItem(t, s, "Lakehouse")
	if err := s.PutItemAccess(ItemAccess{ItemID: it.ID, PrincipalID: "p", PrincipalType: "User",
		Permissions: []string{PermRead}}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListItemAccess(it.ID); err != nil || len(got) != 0 {
		t.Fatalf("grants outlived their item: %+v %v", got, err)
	}
	if err := s.PutItemAccess(ItemAccess{ItemID: "no-such-item", PrincipalID: "p", PrincipalType: "User",
		Permissions: []string{PermRead}}); err == nil {
		t.Fatal("a grant was stored against an item that does not exist")
	}
}

// A store failure is an error from every entry point, never an empty grant —
// "no access" is a statement about the data, not about the database.
func TestItemAccessReportsStoreFailures(t *testing.T) {
	s := newTestStore(t)
	_, it := sharedItem(t, s, "Lakehouse")
	_ = s.Close()
	if err := s.PutItemAccess(ItemAccess{ItemID: it.ID, PrincipalID: "p", PrincipalType: "User"}); err == nil {
		t.Error("PutItemAccess on a closed store returned nil")
	}
	if err := s.DeleteItemAccess(it.ID, "p"); err == nil {
		t.Error("DeleteItemAccess on a closed store returned nil")
	}
	if _, err := s.ListItemAccess(it.ID); err == nil {
		t.Error("ListItemAccess on a closed store returned nil")
	}
	if _, err := s.GetItemAccess(it.ID, "p"); err == nil {
		t.Error("GetItemAccess on a closed store returned nil")
	}
	if _, err := s.EffectiveItemAccess(it, "p"); err == nil {
		t.Error("EffectiveItemAccess on a closed store returned nil")
	}
}

// A corrupt permission list fails the read rather than reading as no access.
func TestAnUndecodableGrantFailsTheRead(t *testing.T) {
	for _, col := range []string{"permissions", "additional_permissions"} {
		t.Run(col, func(t *testing.T) {
			s := newTestStore(t)
			ws, it := sharedItem(t, s, "Lakehouse")
			if err := s.PutItemAccess(ItemAccess{ItemID: it.ID, PrincipalID: "p", PrincipalType: "User",
				Permissions: []string{PermRead}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE item_access SET ` + col + ` = 'not json'`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ListItemAccess(it.ID); err == nil {
				t.Error("List read a corrupt grant as a grant")
			}
			if _, err := s.GetItemAccess(it.ID, "p"); err == nil {
				t.Error("Get read a corrupt grant as a grant")
			}
			if _, err := s.EffectiveItemAccess(it, "p"); err == nil {
				t.Error("Effective read a corrupt grant as a grant")
			}
			// The role half on its own still reads, so the error really came from
			// the grant and not from the workspace.
			if _, err := s.RoleOf(ws.ID, "p"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A missing roles table fails EffectiveItemAccess before it reads any grant.
func TestEffectiveAccessFailsWhenTheRoleCannotBeRead(t *testing.T) {
	s := newTestStore(t)
	_, it := sharedItem(t, s, "Lakehouse")
	if _, err := s.db.Exec(`DROP TABLE role_assignments`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EffectiveItemAccess(it, "p"); err == nil {
		t.Fatal("an unreadable role was read as no role")
	}
}

func TestHasLooksAtBothHalves(t *testing.T) {
	a := Access{Permissions: []string{PermRead}, Additional: []string{PermReadAll}}
	if !a.Has(PermRead) || !a.Has(PermReadAll) || a.Has(PermReshare) {
		t.Fatalf("Has is wrong on %+v", a)
	}
}
