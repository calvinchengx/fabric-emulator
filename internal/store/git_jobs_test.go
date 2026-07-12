package store

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestConnections(t *testing.T) {
	s := newTestStore(t)
	c := &Connection{DisplayName: "gh", ConnectivityType: "ShareableCloud",
		Details: json.RawMessage(`{"type":"GitHubSourceControl"}`)}
	if err := s.CreateConnection(c); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetConnection(c.ID)
	if err != nil || got.DisplayName != "gh" || string(got.Details) != `{"type":"GitHubSourceControl"}` {
		t.Fatalf("connection = %+v, %v", got, err)
	}
	if _, err := s.GetConnection("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing connection err = %v", err)
	}
	list, err := s.ListConnections()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	// Empty details default to {}.
	c2 := &Connection{DisplayName: "empty"}
	if err := s.CreateConnection(c2); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetConnection(c2.ID)
	if string(got2.Details) != "{}" {
		t.Fatalf("empty details = %q", got2.Details)
	}
}

func TestGitConnectionLifecycle(t *testing.T) {
	s := newTestStore(t)
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGitConnection(ws.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbound workspace err = %v", err)
	}
	g := &GitConnection{WorkspaceID: ws.ID, ProviderJSON: "{}", RemoteKey: "gh|o||r|d", Branch: "main", CredSource: "Automatic"}
	if err := s.SetGitConnection(g); err != nil {
		t.Fatal(err)
	}
	// Upsert replaces.
	g.Branch = "dev"
	g.SyncedCommit = "abc"
	if err := s.SetGitConnection(g); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetGitConnection(ws.ID)
	if err != nil || got.Branch != "dev" || got.SyncedCommit != "abc" {
		t.Fatalf("git connection = %+v, %v", got, err)
	}
	if err := s.DeleteGitConnection(ws.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGitConnection(ws.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double disconnect err = %v", err)
	}
}

func TestRemoteCommitPreservesLogicalIDs(t *testing.T) {
	s := newTestStore(t)
	const rk, br = "gh|o||r|d", "main"

	if head, err := s.GetRemoteHead(rk, br); err != nil || head != "" {
		t.Fatalf("virgin head = %q, %v", head, err)
	}
	parts := []DefinitionPart{{Path: ".platform", Payload: "e30=", PayloadType: "InlineBase64"}}
	h1, err := s.CommitRemoteItems(rk, br, []*RemoteItem{
		{Type: "Notebook", DisplayName: "nb", Parts: parts},
	})
	if err != nil || h1 == "" {
		t.Fatal(err)
	}
	first, _ := s.ListRemoteItems(rk, br)
	if len(first) != 1 || first[0].LogicalID == "" {
		t.Fatalf("committed items = %+v", first)
	}
	lid := first[0].LogicalID

	// Second commit of the same item keeps its logical id; head moves.
	h2, err := s.CommitRemoteItems(rk, br, []*RemoteItem{
		{Type: "Notebook", DisplayName: "nb", Parts: parts},
		{Type: "Lakehouse", DisplayName: "lh", Parts: nil},
	})
	if err != nil || h2 == h1 {
		t.Fatalf("second commit: %v (h1=%s h2=%s)", err, h1, h2)
	}
	second, _ := s.ListRemoteItems(rk, br)
	if len(second) != 2 {
		t.Fatalf("items after second commit = %d", len(second))
	}
	for _, ri := range second {
		if ri.DisplayName == "nb" && ri.LogicalID != lid {
			t.Fatalf("logical id not preserved: %s != %s", ri.LogicalID, lid)
		}
	}
	if head, _ := s.GetRemoteHead(rk, br); head != h2 {
		t.Fatalf("head = %s; want %s", head, h2)
	}
}

func TestJobInstanceStates(t *testing.T) {
	s := newTestStore(t)
	s.Clock.Freeze()
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	now := s.Now()
	j := &JobInstance{ItemID: it.ID, JobType: "RunNotebook", CompleteAt: now + 60}
	if err := s.CreateJobInstance(j); err != nil {
		t.Fatal(err)
	}
	if j.InvokeType != "Manual" {
		t.Fatalf("invokeType default = %q", j.InvokeType)
	}
	if st := j.StatusAt(now); st != JobNotStarted {
		t.Fatalf("t0 = %q", st)
	}
	if st := j.StatusAt(now + 30); st != JobInProgress {
		t.Fatalf("t+30 = %q", st)
	}
	if st := j.StatusAt(now + 60); st != JobCompleted {
		t.Fatalf("t+60 = %q", st)
	}
	fail := JobInstance{FailWith: "Boom", CreatedAt: now, CompleteAt: now}
	if st := fail.StatusAt(now + 1); st != JobFailed {
		t.Fatalf("failed = %q", st)
	}
	// Cancel wins over everything.
	if err := s.CancelJobInstance(it.ID, j.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJobInstance(it.ID, j.ID)
	if err != nil || got.StatusAt(now+30) != JobCancelled {
		t.Fatalf("cancelled job = %+v, %v", got, err)
	}
	if _, err := s.GetJobInstance(it.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job err = %v", err)
	}
	if err := s.CancelJobInstance(it.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel missing err = %v", err)
	}
	// Item delete cascades jobs.
	if err := s.DeleteItem(ws.ID, it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetJobInstance(it.ID, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("job survived item delete")
	}
}

func TestWorkspaceIdentityStore(t *testing.T) {
	s := newTestStore(t)
	ws := &Workspace{DisplayName: "w"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorkspaceIdentity(ws.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no identity err = %v", err)
	}
	wi := &WorkspaceIdentity{WorkspaceID: ws.ID, IdentityID: "sp-1", AppID: "app-1"}
	if err := s.SetWorkspaceIdentity(wi); err != nil {
		t.Fatal(err)
	}
	// Upsert replaces.
	wi2 := &WorkspaceIdentity{WorkspaceID: ws.ID, IdentityID: "sp-2", AppID: "app-2"}
	if err := s.SetWorkspaceIdentity(wi2); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkspaceIdentity(ws.ID)
	if err != nil || got.IdentityID != "sp-2" || got.AppID != "app-2" {
		t.Fatalf("identity = %+v, %v", got, err)
	}
	// Workspace delete cascades the link.
	if err := s.DeleteWorkspace(ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorkspaceIdentity(ws.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("identity link survived workspace delete")
	}
	if err := s.DeleteWorkspaceIdentity(ws.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing identity err = %v", err)
	}
}

func TestCapacitySeed(t *testing.T) {
	s := newTestStore(t)
	c, err := s.GetCapacity(DefaultCapacityID)
	if err != nil || c.DisplayName != "Emulator Capacity" || c.State != "Active" {
		t.Fatalf("seeded capacity = %+v, %v", c, err)
	}
	if _, err := s.GetCapacity("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing capacity err = %v", err)
	}
	list, err := s.ListCapacities()
	if err != nil || len(list) != 1 {
		t.Fatalf("capacities = %d, %v", len(list), err)
	}
}
