package api

// Cross-workspace authorization on the MLflow proxy.
//
// The proxy relays to a shared MLflow server whose own ids carry no workspace,
// so `authorizeMLflowReference` is the ONLY thing stopping a caller with rights
// in workspace A from reading or mutating workspace B's experiments and runs by
// naming B's ids. Each way an id can arrive is a separate branch, and five of
// them had never executed: the `experiment_ids` batch list, the `experiment_id`
// query parameter, the artifact path's experiment, the `run_id`/`run_uuid`
// query parameters, and the `runs:/…` source used when registering a model.
//
// A branch that never runs is a branch that cannot be shown to refuse, and the
// consequence of it not refusing is reading another tenant's data.

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// seedMLExperiment creates an MLExperiment in wid that owns the given MLflow
// experiment id and runs, in the metadata shape mlflowItemMetadata reads.
func seedMLExperiment(t *testing.T, st *store.Store, wid, name, experimentID string, runIDs ...string) *store.Item {
	t.Helper()
	metadata := map[string]string{"experimentId": experimentID}
	for _, r := range runIDs {
		metadata["runId:"+r] = "true"
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	it := &store.Item{WorkspaceID: wid, Type: "MLExperiment", DisplayName: name}
	if err := st.CreateItem(it, []store.DefinitionPart{{
		Path: mlflowMetadataPart, PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(raw),
	}}); err != nil {
		t.Fatal(err)
	}
	return it
}

// twoWorkspaces returns (mine, theirs) where each owns one experiment and run.
// "Theirs" is what every refusal below is trying to reach.
func twoWorkspaces(t *testing.T) (*API, string, string) {
	t.Helper()
	a, st := newAPI(t)
	mine := seedWorkspace(t, st)
	theirs := &store.Workspace{DisplayName: "other-tenant"}
	if err := st.CreateWorkspace(theirs, store.Principal{
		ID: "someone-else", Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	seedMLExperiment(t, st, mine.ID, "mine", "17", "run-mine")
	seedMLExperiment(t, st, theirs.ID, "theirs", "99", "run-theirs")
	return a, mine.ID, theirs.ID
}

func TestMLflowAuthorizationAcceptsOwnReferences(t *testing.T) {
	a, mine, _ := twoWorkspaces(t)

	cases := []struct {
		name, endpoint, body, query string
	}{
		{"experiment_id in body", "/api/2.0/mlflow/runs/create", `{"experiment_id":"17"}`, ""},
		{"experiment_ids batch", "/api/2.0/mlflow/runs/search", `{"experiment_ids":["17"]}`, ""},
		{"experiment_id in query", "/api/2.0/mlflow/experiments/get", `{}`, "experiment_id=17"},
		{"artifact path", "/api/2.0/mlflow-artifacts/artifacts/17/model.pkl", `{}`, ""},
		{"run_id in body", "/api/2.0/mlflow/runs/log-metric", `{"run_id":"run-mine"}`, ""},
		{"run_uuid in query", "/api/2.0/mlflow/runs/get", `{}`, "run_uuid=run-mine"},
		{"runs:/ source", "/api/2.0/mlflow/model-versions/create",
			`{"source":"runs:/run-mine/model"}`, ""},
	}
	for _, tc := range cases {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.authorizeMLflowReference(mine, tc.endpoint, []byte(tc.body), q); err != nil {
			t.Errorf("%s: own reference refused: %v", tc.name, err)
		}
	}
}

// TestMLflowAuthorizationRefusesAnotherWorkspacesReferences is the one that
// matters. Every row names an id that exists — it belongs to the OTHER
// workspace — so a branch that fails to check does not error, it succeeds and
// leaks.
func TestMLflowAuthorizationRefusesAnotherWorkspacesReferences(t *testing.T) {
	a, mine, _ := twoWorkspaces(t)

	cases := []struct {
		name, endpoint, body, query, wantIn string
	}{
		{"experiment_id in body", "/api/2.0/mlflow/runs/create",
			`{"experiment_id":"99"}`, "", `experiment "99"`},
		{"experiment_ids batch", "/api/2.0/mlflow/runs/search",
			`{"experiment_ids":["17","99"]}`, "", `experiment "99"`},
		{"experiment_id in query", "/api/2.0/mlflow/experiments/get",
			`{}`, "experiment_id=99", `experiment "99"`},
		{"artifact path", "/api/2.0/mlflow-artifacts/artifacts/99/model.pkl",
			`{}`, "", "artifact experiment"},
		{"run_id in body", "/api/2.0/mlflow/runs/log-metric",
			`{"run_id":"run-theirs"}`, "", `run "run-theirs"`},
		{"run_uuid in body", "/api/2.0/mlflow/runs/log-metric",
			`{"run_uuid":"run-theirs"}`, "", `run "run-theirs"`},
		{"run_id in query", "/api/2.0/mlflow/runs/get",
			`{}`, "run_id=run-theirs", `run "run-theirs"`},
		{"run_uuid in query", "/api/2.0/mlflow/runs/get",
			`{}`, "run_uuid=run-theirs", `run "run-theirs"`},
		{"runs:/ source", "/api/2.0/mlflow/model-versions/create",
			`{"source":"runs:/run-theirs/model"}`, "", `run "run-theirs"`},
	}
	for _, tc := range cases {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		err = a.authorizeMLflowReference(mine, tc.endpoint, []byte(tc.body), q)
		if err == nil {
			t.Errorf("%s: another workspace's reference was ALLOWED", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s: error %q does not name the offending reference (want %q)",
				tc.name, err, tc.wantIn)
		}
	}
}

// TestMLflowAuthorizationRefusesAnUnparseableArtifactPath: the artifact branch
// resolves the experiment out of the URL, so a path it cannot parse must be
// refused rather than skipped — skipping would let `../` style references past
// the only check that reads the experiment at all.
func TestMLflowAuthorizationRefusesAnUnparseableArtifactPath(t *testing.T) {
	a, mine, _ := twoWorkspaces(t)

	for _, endpoint := range []string{
		"/api/2.0/mlflow-artifacts/artifacts/17",             // no artifact path
		"/api/2.0/mlflow-artifacts/artifacts/",               // neither part
		"/api/2.0/mlflow-artifacts/artifacts/17/../99/x.pkl", // traversal out of 17
	} {
		if err := a.authorizeMLflowReference(mine, endpoint, []byte(`{}`), url.Values{}); err == nil {
			t.Errorf("%s: was allowed; want refusal", endpoint)
		}
	}
}

// TestMLflowAuthorizationIgnoresAnUnknownReferenceShape: a body that names no
// experiment or run is not this function's business — endpoint-level RBAC has
// already run — so it must not invent a refusal.
func TestMLflowAuthorizationIgnoresAnUnknownReferenceShape(t *testing.T) {
	a, mine, _ := twoWorkspaces(t)

	for _, body := range []string{
		`{}`,
		`{"name":"an experiment name, not an id"}`,
		`{"experiment_ids":[17, null]}`, // non-strings are not ids
		`not json at all`,
		`{"source":"s3://bucket/model"}`, // a source that is not runs:/
	} {
		if err := a.authorizeMLflowReference(mine, "/api/2.0/mlflow/experiments/search",
			[]byte(body), url.Values{}); err != nil {
			t.Errorf("body %q was refused: %v", body, err)
		}
	}
}
