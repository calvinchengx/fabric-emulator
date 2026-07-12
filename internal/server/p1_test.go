package server_test

// P1 e2e: the CI/CD surface — definitions round-trip, typed aliases, git
// connect → initialize → commit → clone-into-second-workspace → status,
// and clock-driven job instances. Same real-token fixture as P0.

import (
	"net/http"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
)

const platformPart = `{"path":".platform","payload":"e30=","payloadType":"InlineBase64"}`

func TestDefinitionRoundTripAndTypedAliases(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "cicd"}, &ws)

	// Typed alias create: no "type" in the body — the collection implies it.
	var nb struct{ ID, Type string }
	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token,
		map[string]any{"displayName": "nb"}, &nb)
	f.mustStatus(resp, http.StatusCreated, "typed create")
	if nb.Type != "Notebook" {
		t.Fatalf("typed create type = %q", nb.Type)
	}
	// Typed get resolves; the same id under /lakehouses 404s.
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/notebooks/"+nb.ID, f.token, nil, nil),
		http.StatusOK, "typed get")
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/lakehouses/"+nb.ID, f.token, nil, nil),
		http.StatusNotFound, "cross-type get")
	// Typed list filters.
	f.call("POST", "/v1/workspaces/"+ws.ID+"/lakehouses", f.token, map[string]any{"displayName": "lh"}, nil)
	var listed struct{ Value []struct{ Type string } }
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/notebooks", f.token, nil, &listed), http.StatusOK, "typed list")
	if len(listed.Value) != 1 || listed.Value[0].Type != "Notebook" {
		t.Fatalf("typed list = %+v", listed)
	}

	// updateDefinition (202 LRO) → getDefinition returns the parts verbatim.
	update := map[string]any{"definition": map[string]any{"parts": []map[string]string{
		{"path": "notebook-content.py", "payload": "cHJpbnQoMSk=", "payloadType": "InlineBase64"},
		{"path": ".platform", "payload": "e30=", "payloadType": "InlineBase64"},
	}}}
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+nb.ID+"/updateDefinition", f.token, update, nil)
	f.mustStatus(resp, http.StatusAccepted, "updateDefinition")
	opID := resp.Header.Get("x-ms-operation-id")
	var op struct{ Status string }
	f.call("GET", "/v1/operations/"+opID, f.token, nil, &op)
	if op.Status != "Succeeded" {
		t.Fatalf("updateDefinition op = %q", op.Status)
	}
	var def struct {
		Definition struct {
			Parts []struct{ Path, Payload, PayloadType string }
		}
	}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+nb.ID+"/getDefinition", f.token, nil, &def),
		http.StatusOK, "getDefinition")
	if len(def.Definition.Parts) != 2 || def.Definition.Parts[0].Payload != "cHJpbnQoMSk=" {
		t.Fatalf("definition parts = %+v", def.Definition)
	}
}

func TestGitRoundTrip(t *testing.T) {
	f := newFixture(t)

	// SP + Automatic credentials is rejected (documented SP constraint).
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "src"}, &ws)
	provider := map[string]any{
		"gitProviderType": "GitHub", "ownerName": "calvin", "repositoryName": "demo",
		"branchName": "main", "directoryName": "/",
	}
	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/git/connect", f.token,
		map[string]any{"gitProviderDetails": provider}, nil)
	f.mustStatus(resp, http.StatusBadRequest, "SP with Automatic creds")

	// A connection unlocks connect; a bogus connectionId is rejected.
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/git/connect", f.token, map[string]any{
		"gitProviderDetails": provider,
		"myGitCredentials":   map[string]string{"source": "ConfiguredConnection", "connectionId": "nope"},
	}, nil)
	f.mustStatus(resp, http.StatusBadRequest, "bogus connectionId")
	var conn struct{ ID string }
	resp = f.call("POST", "/v1/connections", f.token, map[string]any{
		"displayName":       "github-pat",
		"connectivityType":  "ShareableCloud",
		"connectionDetails": map[string]string{"type": "GitHubSourceControl"},
	}, &conn)
	f.mustStatus(resp, http.StatusCreated, "create connection")
	var conns struct{ Value []struct{ ID string } }
	f.mustStatus(f.call("GET", "/v1/connections", f.token, nil, &conns), http.StatusOK, "list connections")
	if len(conns.Value) != 1 {
		t.Fatalf("connections = %+v", conns)
	}
	creds := map[string]string{"source": "ConfiguredConnection", "connectionId": conn.ID}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/git/connect", f.token,
		map[string]any{"gitProviderDetails": provider, "myGitCredentials": creds}, nil),
		http.StatusOK, "git connect")
	var myCreds struct{ Source, ConnectionID string }
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/git/myGitCredentials", f.token, nil, &myCreds),
		http.StatusOK, "myGitCredentials")
	if myCreds.Source != "ConfiguredConnection" || myCreds.ConnectionID != conn.ID {
		t.Fatalf("myGitCredentials = %+v", myCreds)
	}

	// Workspace has content, remote is virgin → CommitToGit required.
	f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token, map[string]any{
		"displayName": "nb", "type": "Notebook",
		"definition": map[string]any{"parts": []map[string]string{
			{"path": ".platform", "payload": "e30=", "payloadType": "InlineBase64"},
		}},
	}, nil)
	var init struct {
		RequiredAction   string
		RemoteCommitHash string
	}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/git/initializeConnection", f.token, nil, &init),
		http.StatusOK, "initialize")
	if init.RequiredAction != "CommitToGit" || init.RemoteCommitHash != "" {
		t.Fatalf("initialize = %+v", init)
	}

	// Status shows the workspace-added item; commit; status is clean.
	var status struct {
		RemoteCommitHash string
		Changes          []struct {
			WorkspaceChange string
			RemoteChange    string
		}
	}
	f.call("GET", "/v1/workspaces/"+ws.ID+"/git/status", f.token, nil, &status)
	if len(status.Changes) != 1 || status.Changes[0].WorkspaceChange != "Added" {
		t.Fatalf("pre-commit status = %+v", status)
	}
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/git/commitToGit", f.token,
		map[string]any{"mode": "All", "comment": "initial"}, nil)
	f.mustStatus(resp, http.StatusAccepted, "commitToGit")
	var op struct{ Status string }
	f.call("GET", "/v1/operations/"+resp.Header.Get("x-ms-operation-id"), f.token, nil, &op)
	if op.Status != "Succeeded" {
		t.Fatalf("commit op = %q", op.Status)
	}
	f.call("GET", "/v1/workspaces/"+ws.ID+"/git/status", f.token, nil, &status)
	if len(status.Changes) != 0 || status.RemoteCommitHash == "" {
		t.Fatalf("post-commit status = %+v", status)
	}

	// A second workspace connected to the same remote pulls the item —
	// the full CI/CD round-trip, definitions included.
	var ws2 struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "clone"}, &ws2)
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws2.ID+"/git/connect", f.token,
		map[string]any{"gitProviderDetails": provider, "myGitCredentials": creds}, nil),
		http.StatusOK, "connect clone")
	f.call("POST", "/v1/workspaces/"+ws2.ID+"/git/initializeConnection", f.token, nil, &init)
	if init.RequiredAction != "UpdateFromGit" {
		t.Fatalf("clone initialize = %+v", init)
	}
	resp = f.call("POST", "/v1/workspaces/"+ws2.ID+"/git/updateFromGit", f.token,
		map[string]any{"remoteCommitHash": init.RemoteCommitHash}, nil)
	f.mustStatus(resp, http.StatusAccepted, "updateFromGit")
	var items struct {
		Value []struct{ ID, Type, DisplayName string }
	}
	f.call("GET", "/v1/workspaces/"+ws2.ID+"/items", f.token, nil, &items)
	if len(items.Value) != 1 || items.Value[0].DisplayName != "nb" {
		t.Fatalf("cloned items = %+v", items)
	}
	var def struct {
		Definition struct{ Parts []struct{ Path string } }
	}
	f.call("POST", "/v1/workspaces/"+ws2.ID+"/items/"+items.Value[0].ID+"/getDefinition", f.token, nil, &def)
	if len(def.Definition.Parts) != 1 || def.Definition.Parts[0].Path != ".platform" {
		t.Fatalf("cloned definition = %+v", def.Definition)
	}

	// Disconnect; git routes then report not-connected.
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws2.ID+"/git/disconnect", f.token, nil, nil),
		http.StatusOK, "disconnect")
	resp = f.call("GET", "/v1/workspaces/"+ws2.ID+"/git/status", f.token, nil, nil)
	f.mustStatus(resp, http.StatusBadRequest, "status after disconnect")
}

func TestJobInstancesOnTheClock(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "jobs"}, &ws)
	var nb struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/notebooks", f.token, map[string]any{"displayName": "nb"}, &nb)

	// Missing jobType → 400.
	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+nb.ID+"/jobs/instances", f.token, nil, nil)
	f.mustStatus(resp, http.StatusBadRequest, "no jobType")

	// Freeze + slow: walk NotStarted → InProgress → Completed.
	f.call("POST", "/_emulator/clock", "", map[string]bool{"freeze": true}, nil)
	f.call("POST", "/_emulator/faults", "", map[string]int64{"lroDelaySeconds": 120}, nil)
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+nb.ID+"/jobs/instances?jobType=RunNotebook", f.token, nil, nil)
	f.mustStatus(resp, http.StatusAccepted, "schedule job")
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/jobs/instances/") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("job 202 headers: loc=%q", loc)
	}
	path := loc[strings.Index(loc, "/v1/"):]
	var job struct{ Status, JobType string }
	f.call("GET", path, f.token, nil, &job)
	if job.Status != "NotStarted" || job.JobType != "RunNotebook" {
		t.Fatalf("job at t0 = %+v", job)
	}
	f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 10}, nil)
	f.call("GET", path, f.token, nil, &job)
	if job.Status != "InProgress" {
		t.Fatalf("job at t+10 = %q", job.Status)
	}
	f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 120}, nil)
	var done struct {
		Status     string
		EndTimeUtc string
	}
	f.call("GET", path, f.token, nil, &done)
	if done.Status != "Completed" || done.EndTimeUtc == "" {
		t.Fatalf("job at t+130 = %+v", done)
	}

	// Cancel a fresh slow job; failure-injected job reports failureReason.
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+nb.ID+"/jobs/instances?jobType=RunNotebook", f.token, nil, nil)
	path = resp.Header.Get("Location")[strings.Index(resp.Header.Get("Location"), "/v1/"):]
	resp = f.call("POST", path+"/cancel", f.token, nil, nil)
	f.mustStatus(resp, http.StatusAccepted, "cancel")
	f.call("GET", path, f.token, nil, &job)
	if job.Status != "Cancelled" {
		t.Fatalf("cancelled job = %q", job.Status)
	}
	f.call("POST", "/_emulator/faults", "", map[string]any{"lroDelaySeconds": 0, "failNextOperations": 1}, nil)
	resp = f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+nb.ID+"/jobs/instances?jobType=RunNotebook", f.token, nil, nil)
	path = resp.Header.Get("Location")[strings.Index(resp.Header.Get("Location"), "/v1/"):]
	f.call("POST", "/_emulator/clock", "", map[string]int64{"advance": 1}, nil)
	var failed struct {
		Status        string
		FailureReason *struct{ ErrorCode string }
	}
	f.call("GET", path, f.token, nil, &failed)
	if failed.Status != "Failed" || failed.FailureReason == nil {
		t.Fatalf("failed job = %+v", failed)
	}
}

func TestGitRBAC(t *testing.T) {
	f := newFixture(t)
	alice := f.forgeUserToken(entra.AliceOID)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "gated"}, &ws)
	var conn struct{ ID string }
	f.call("POST", "/v1/connections", f.token, map[string]any{"displayName": "c"}, &conn)
	provider := map[string]any{"gitProviderType": "GitHub", "ownerName": "o", "repositoryName": "r",
		"branchName": "main", "directoryName": "/"}
	creds := map[string]string{"source": "ConfiguredConnection", "connectionId": conn.ID}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/git/connect", f.token,
		map[string]any{"gitProviderDetails": provider, "myGitCredentials": creds}, nil), http.StatusOK, "connect")

	// A Contributor may see status but not connect/disconnect (Admin-only).
	f.call("POST", "/v1/workspaces/"+ws.ID+"/roleAssignments", f.token,
		map[string]any{"principal": map[string]string{"id": entra.AliceOID, "type": "User"}, "role": "Contributor"}, nil)
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/git/status", alice, nil, nil), http.StatusOK, "contributor status")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/git/disconnect", alice, nil, nil),
		http.StatusForbidden, "contributor disconnect")
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/git/connect", alice,
		map[string]any{"gitProviderDetails": provider, "myGitCredentials": creds}, nil),
		http.StatusForbidden, "contributor connect")
	// Alice (a User) may use Automatic credentials on her own workspace.
	var aws struct{ ID string }
	f.call("POST", "/v1/workspaces", alice, map[string]string{"displayName": "alice-ws"}, &aws)
	f.mustStatus(f.call("POST", "/v1/workspaces/"+aws.ID+"/git/connect", alice,
		map[string]any{"gitProviderDetails": provider}, nil), http.StatusOK, "user Automatic connect")
}
