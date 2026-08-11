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
	f.srv.API.DefinitionLRO = true
	ws, pl := seedPipelineWithDefinition(t, f, "defn-lro-ws")

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
	f.srv.API.DefinitionLRO = true
	// A delay long enough that the operation is still running when asked.
	f.srv.API.SetFaults(-1, -1, 60)
	ws, pl := seedPipelineWithDefinition(t, f, "defn-lro-early-ws")

	resp := f.call("POST", "/v1/workspaces/"+ws+"/items/"+pl+"/getDefinition", f.token, nil, nil)
	f.mustStatus(resp, http.StatusAccepted, "getDefinition (LRO)")
	opID := resp.Header.Get("x-ms-operation-id")

	f.mustStatus(f.call("GET", "/v1/operations/"+opID+"/result", f.token, nil, nil),
		http.StatusBadRequest, "result before success")
}
