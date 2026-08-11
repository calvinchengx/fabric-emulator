package server_test

// getDefinition has TWO documented outcomes and real Fabric uses both: 200 with
// the body, and 202 + an operation whose /result carries it. The reference says
// so ("This API supports long running operations", with a 202 sample carrying
// Location, x-ms-operation-id and Retry-After), and a real tenant answered 202
// for a VariableLibrary getDefinition on 2026-08-11.
//
// The emulator only ever produced the 200, which is the dangerous half to be
// missing: a client that reads a 202's body gets `null` and reports an EMPTY
// DEFINITION rather than an error. That is a silent wrong answer, and it is
// exactly what happened when this repo first called the real API.
//
// So both paths are exercised here, and the 202 one is asserted to return the
// same parts as the 200 one — a divergence between them would be worse than
// either alone.

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const lroDefBody = `{"properties":{"activities":[]}}`

func seedPipelineWithDefinition(t *testing.T, f *fixture, wsName string) (string, string) {
	t.Helper()
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": wsName}, &ws)
	var pl struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "defn", "type": "DataPipeline"}, &pl),
		http.StatusCreated, "create pipeline")
	if err := f.srv.API.Store.SetDefinition(pl.ID, []store.DefinitionPart{
		{Path: "pipeline-content.json", PayloadType: "InlineBase64",
			Payload: base64.StdEncoding.EncodeToString([]byte(lroDefBody))},
	}); err != nil {
		t.Fatal(err)
	}
	return ws.ID, pl.ID
}

type defEnvelope struct {
	Definition struct {
		Parts []struct {
			Path        string
			Payload     string
			PayloadType string
		}
	}
}

func (d defEnvelope) decoded(t *testing.T) string {
	t.Helper()
	if len(d.Definition.Parts) != 1 {
		t.Fatalf("parts = %+v, want exactly one", d.Definition.Parts)
	}
	raw, err := base64.StdEncoding.DecodeString(d.Definition.Parts[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Default: synchronous 200, which is legal and is what most calls see.
func TestGetDefinitionSynchronousByDefault(t *testing.T) {
	f := newFixture(t)
	ws, pl := seedPipelineWithDefinition(t, f, "defn-sync-ws")

	var env defEnvelope
	resp := f.call("POST", "/v1/workspaces/"+ws+"/items/"+pl+"/getDefinition", f.token, nil, &env)
	f.mustStatus(resp, http.StatusOK, "getDefinition")
	if got := env.decoded(t); got != lroDefBody {
		t.Errorf("definition = %q", got)
	}
}

// Opted in: the documented async outcome, end to end.
func TestGetDefinitionAsLongRunningOperation(t *testing.T) {
	f := newFixture(t)
	// Seed with the flag OFF, then enable it. With ForceLRO on, item creation
	// is itself a 202 — correct, and covered by its own test below — so
	// seeding under the flag would put the create path in the way of the
	// getDefinition assertion this test is about.
	ws, pl := seedPipelineWithDefinition(t, f, "defn-lro-ws")
	f.srv.API.ForceLRO = true

	resp := f.call("POST", "/v1/workspaces/"+ws+"/items/"+pl+"/getDefinition", f.token, nil, nil)
	f.mustStatus(resp, http.StatusAccepted, "getDefinition (LRO)")

	// The three headers the reference's own 202 sample carries. A client that
	// reads only one of them is a client this path is meant to catch.
	loc := resp.Header.Get("Location")
	if loc == "" {
		t.Fatal("no Location on the 202")
	}
	opID := resp.Header.Get("x-ms-operation-id")
	if opID == "" {
		t.Error("no x-ms-operation-id on the 202")
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After on the 202")
	}
	if !strings.HasSuffix(loc, "/v1/operations/"+opID) {
		t.Errorf("Location %q does not name operation %q", loc, opID)
	}

	// The 202 has NO body worth reading — that is the trap being modelled.
	var body defEnvelope
	_ = body

	opPath := "/v1/operations/" + opID
	var op struct{ Status string }
	deadline := time.Now().Add(30 * time.Second)
	for {
		f.mustStatus(f.call("GET", opPath, f.token, nil, &op), http.StatusOK, "poll operation")
		if op.Status != "NotStarted" && op.Status != "Running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation never reached a terminal state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if op.Status != "Succeeded" {
		t.Fatalf("operation status = %s", op.Status)
	}

	var env defEnvelope
	f.mustStatus(f.call("GET", opPath+"/result", f.token, nil, &env), http.StatusOK, "operation result")
	if got := env.decoded(t); got != lroDefBody {
		t.Errorf("definition via LRO = %q, want the same bytes the 200 path returns", got)
	}
}

// The result is not fetchable before the operation succeeds — a client that
// races straight to /result must be refused rather than handed a partial.
func TestGetDefinitionResultRefusedBeforeSuccess(t *testing.T) {
	f := newFixture(t)
	ws, pl := seedPipelineWithDefinition(t, f, "defn-lro-early-ws")
	f.srv.API.ForceLRO = true
	// A delay long enough that the operation is still running when asked.
	f.srv.API.SetFaults(-1, -1, 60)

	resp := f.call("POST", "/v1/workspaces/"+ws+"/items/"+pl+"/getDefinition", f.token, nil, nil)
	f.mustStatus(resp, http.StatusAccepted, "getDefinition (LRO)")
	opID := resp.Header.Get("x-ms-operation-id")

	f.mustStatus(f.call("GET", "/v1/operations/"+opID+"/result", f.token, nil, nil),
		http.StatusBadRequest, "result before success")
}

// createItem has TWO documented outcomes too, and this is the sharper case.
// Create Warehouse documents 201 and 202, says "This API supports long running
// operations", AND says "This API does not support create a warehouse with
// definition" — while the emulator went async only for a definition-bearing
// create. So the one item type measured asynchronous on a real tenant was the
// one type guaranteed synchronous here, and a client indexing the 201 body got
// `None["id"]` against a tenant. Measured 2026-08-11 by local_87220308 running
// examples/medallion-pyspark/provision.py against the real trial capacity.
func TestCreateItemAsLongRunningOperation(t *testing.T) {
	f := newFixture(t)
	f.srv.API.ForceLRO = true

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "create-lro-ws"}, &ws)

	// No definition — a Warehouse cannot have one, which is the whole point.
	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "dw", "type": "Warehouse"}, nil)
	f.mustStatus(resp, http.StatusAccepted, "create Warehouse (LRO)")

	opID := resp.Header.Get("x-ms-operation-id")
	if opID == "" || resp.Header.Get("Location") == "" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("202 is missing documented headers: op=%q loc=%q retry=%q",
			opID, resp.Header.Get("Location"), resp.Header.Get("Retry-After"))
	}

	opPath := "/v1/operations/" + opID
	var op struct{ Status string }
	deadline := time.Now().Add(30 * time.Second)
	for {
		f.mustStatus(f.call("GET", opPath, f.token, nil, &op), http.StatusOK, "poll operation")
		if op.Status != "NotStarted" && op.Status != "Running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation never reached a terminal state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if op.Status != "Succeeded" {
		t.Fatalf("operation status = %s", op.Status)
	}

	// The result is the created item — the thing the client wanted from the
	// body it could not read.
	var it struct {
		ID, DisplayName, Type string
	}
	f.mustStatus(f.call("GET", opPath+"/result", f.token, nil, &it), http.StatusOK, "operation result")
	if it.ID == "" || it.DisplayName != "dw" || it.Type != "Warehouse" {
		t.Fatalf("result = %+v", it)
	}
	// And it really exists, not just as an operation payload.
	var got struct{ ID string }
	f.mustStatus(f.call("GET", "/v1/workspaces/"+ws.ID+"/items/"+it.ID, f.token, nil, &got),
		http.StatusOK, "get the created item")
	if got.ID != it.ID {
		t.Errorf("item id = %q, want %q", got.ID, it.ID)
	}
}

// Default stays synchronous: 201 with the body, which is equally legal and is
// what most calls see. Flipping the default would be a breaking change dressed
// as fidelity.
func TestCreateItemSynchronousByDefault(t *testing.T) {
	f := newFixture(t)
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "create-sync-ws"}, &ws)
	var it struct{ ID, Type string }
	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "dw2", "type": "Warehouse"}, &it)
	f.mustStatus(resp, http.StatusCreated, "create Warehouse")
	if it.ID == "" {
		t.Fatal("201 body carried no id")
	}
}

// The THIRD documented dual-outcome surface: Git Initialize Connection lists
// 200 and 202 and says "This API supports long running operations". The
// emulator answered 200 unconditionally.
//
// Its LRO has a real result body (InitializeGitConnectionResponse), unlike
// commitToGit/updateFromGit which have none — so this also covers the case
// where the async answer must carry back the same object the sync one did.
func TestGitInitializeConnectionAsLongRunningOperation(t *testing.T) {
	f := newFixture(t)

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "git-lro-ws"}, &ws)

	var conn struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/connections", f.token, map[string]any{"displayName": "github-pat-lro",
		"connectivityType":  "ShareableCloud",
		"connectionDetails": map[string]any{"type": "GitHubSourceControl", "creationMethod": "GitHubSourceControl.Contents", "parameters": []map[string]any{{"dataType": "Text", "name": "url", "value": "https://github.com/calvin/demo"}}},
	}, &conn), http.StatusCreated, "create connection")

	provider := map[string]string{
		"gitProviderType": "GitHub", "ownerName": "calvin", "repositoryName": "demo",
		"branchName": "main", "directoryName": "/",
	}
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/git/connect", f.token, map[string]any{
		"gitProviderDetails": provider,
		"myGitCredentials":   map[string]string{"source": "ConfiguredConnection", "connectionId": conn.ID},
	}, nil), http.StatusOK, "git connect")

	// Content in the workspace, virgin remote → CommitToGit is the answer the
	// sync path gives, so it is what the async result must give too.
	f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token, map[string]any{
		"displayName": "nb", "type": "Notebook",
		"definition": map[string]any{"parts": []map[string]string{
			{"path": ".platform", "payload": "e30=", "payloadType": "InlineBase64"},
		}},
	}, nil)

	f.srv.API.ForceLRO = true

	resp := f.call("POST", "/v1/workspaces/"+ws.ID+"/git/initializeConnection", f.token, nil, nil)
	f.mustStatus(resp, http.StatusAccepted, "initializeConnection (LRO)")
	opID := resp.Header.Get("x-ms-operation-id")
	if opID == "" || resp.Header.Get("Location") == "" || resp.Header.Get("Retry-After") == "" {
		t.Fatal("202 is missing documented headers")
	}

	opPath := "/v1/operations/" + opID
	var op struct{ Status string }
	deadline := time.Now().Add(30 * time.Second)
	for {
		f.mustStatus(f.call("GET", opPath, f.token, nil, &op), http.StatusOK, "poll operation")
		if op.Status != "NotStarted" && op.Status != "Running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation never reached a terminal state")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if op.Status != "Succeeded" {
		t.Fatalf("operation status = %s", op.Status)
	}

	var init struct {
		RequiredAction   string
		RemoteCommitHash string
	}
	f.mustStatus(f.call("GET", opPath+"/result", f.token, nil, &init), http.StatusOK, "operation result")
	if init.RequiredAction != "CommitToGit" || init.RemoteCommitHash != "" {
		t.Fatalf("async result = %+v, want the same object the 200 path returns", init)
	}
}
