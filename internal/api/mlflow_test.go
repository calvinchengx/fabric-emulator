package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

type mlflowCall struct {
	method, path, query, body, authorization string
}

type mlflowBackend struct {
	mu     sync.Mutex
	calls  []mlflowCall
	prefix string
}

func (b *mlflowBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	b.calls = append(b.calls, mlflowCall{r.Method, r.URL.Path, r.URL.RawQuery, string(raw), r.Header.Get("Authorization")})
	if r.URL.Path == "/api/2.0/mlflow/experiments/create" {
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if name, _ := body["name"].(string); strings.HasSuffix(name, "sales") {
			b.prefix = strings.TrimSuffix(name, "sales")
		}
	}
	prefix := b.prefix
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/2.0/mlflow/experiments/create":
		_, _ = w.Write([]byte(`{"experiment_id":"17"}`))
	case "/api/2.0/mlflow/runs/create":
		_, _ = w.Write([]byte(`{"run":{"info":{"run_id":"run-1","experiment_id":"17"}}}`))
	case "/api/2.0/mlflow/registered-models/create":
		_, _ = w.Write([]byte(`{"registered_model":{"name":"` + prefix + `forecast"}}`))
	case "/api/2.0/mlflow/experiments/search":
		_, _ = w.Write([]byte(`{"experiments":[{"experiment_id":"17","name":"` + prefix + `sales"},{"experiment_id":"99","name":"fabric__other__secret"}]}`))
	case "/fail":
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"failed"}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

func TestMLflowProxyContract(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	b := &mlflowBackend{}
	backend := httptest.NewServer(b)
	defer backend.Close()
	if err := a.SetMLflowBackend(backend.URL); err != nil {
		t.Fatal(err)
	}

	call := func(p *auth.Principal, method, endpoint, rawQuery, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/x?"+rawQuery, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fabric-token")
		r.SetPathValue("wid", ws.ID)
		r.SetPathValue("path", strings.TrimPrefix(endpoint, "/"))
		w := httptest.NewRecorder()
		a.mlflowProxy(w, r, p)
		return w
	}

	w := call(admin, http.MethodPost, "/api/2.0/mlflow/experiments/create", "", `{"name":"sales"}`)
	if w.Code != http.StatusOK || w.Body.String() != `{"experiment_id":"17"}` {
		t.Fatalf("create experiment = %d %s", w.Code, w.Body.String())
	}
	experiment, err := st.GetItemByName(ws.ID, "sales", "MLExperiment")
	if err != nil {
		t.Fatalf("MLExperiment was not synchronized: %v", err)
	}
	metadata, err := a.mlflowItemMetadata(experiment.ID)
	if err != nil || metadata["experimentId"] != "17" {
		t.Fatalf("experiment metadata = %#v, %v", metadata, err)
	}

	w = call(admin, http.MethodPost, "/api/2.0/mlflow/runs/create", "", `{"experiment_id":"17","run_name":"fit"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create run = %d %s", w.Code, w.Body.String())
	}
	w = call(admin, http.MethodPost, "/api/2.0/mlflow/runs/log-metric", "", `{"run_id":"run-1","key":"score","value":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("owned run metric = %d %s", w.Code, w.Body.String())
	}
	w = call(admin, http.MethodPost, "/api/2.0/mlflow/runs/log-metric", "", `{"run_id":"foreign","key":"score","value":1}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("foreign run metric = %d %s", w.Code, w.Body.String())
	}
	w = call(admin, http.MethodPost, "/api/2.0/mlflow/runs/create", "", `{"experiment_id":"99"}`)
	if w.Code != http.StatusForbidden || errorCode(t, w) != "MLflowWorkspaceMismatch" {
		t.Fatalf("foreign experiment = %d %s", w.Code, w.Body.String())
	}

	w = call(admin, http.MethodPost, "/api/2.0/mlflow/registered-models/create", "", `{"name":"forecast"}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "fabric__") {
		t.Fatalf("create model = %d %s", w.Code, w.Body.String())
	}
	if _, err := st.GetItemByName(ws.ID, "forecast", "MLModel"); err != nil {
		t.Fatalf("MLModel was not synchronized: %v", err)
	}

	w = call(viewer, http.MethodGet, "/api/2.0/mlflow/experiments/get-by-name", "experiment_name=sales", "")
	if w.Code != http.StatusOK {
		t.Fatalf("viewer read = %d %s", w.Code, w.Body.String())
	}
	w = call(viewer, http.MethodPost, "/api/2.0/mlflow/experiments/create", "", `{"name":"denied"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer write = %d", w.Code)
	}

	w = call(viewer, http.MethodPost, "/api/2.0/mlflow/experiments/search", "", `{}`)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), `"name":"sales"`) {
		t.Fatalf("filtered search = %d %s", w.Code, w.Body.String())
	}

	w = call(admin, http.MethodPut, "/api/2.0/mlflow-artifacts/artifacts/17/run-1/artifacts/report.txt", "", "artifact")
	if w.Code != http.StatusOK {
		t.Fatalf("artifact upload = %d %s", w.Code, w.Body.String())
	}
	got, err := st.GetOneLakePath(experiment.ID, "Files/mlflow-artifacts/run-1/artifacts/report.txt")
	if err != nil {
		t.Fatalf("mirrored artifact: %v", err)
	}
	if string(got.Content) != "artifact" {
		t.Fatalf("mirrored artifact = %q", got.Content)
	}
	w = call(admin, http.MethodPut, "/api/2.0/mlflow-artifacts/artifacts/17/run-1/../../escape.txt", "", "escape")
	if w.Code != http.StatusForbidden || errorCode(t, w) != "MLflowWorkspaceMismatch" {
		t.Fatalf("artifact traversal = %d %s", w.Code, w.Body.String())
	}
	if _, err := st.GetOneLakePath(experiment.ID, "Files/escape.txt"); err == nil {
		t.Fatal("artifact traversal escaped the mirror prefix")
	}

	b.mu.Lock()
	calls := append([]mlflowCall(nil), b.calls...)
	b.mu.Unlock()
	if !strings.Contains(calls[0].body, mlflowPrefix(ws.ID)+"sales") || calls[0].authorization != "" {
		t.Fatalf("experiment upstream call = %+v", calls[0])
	}
	foundQuery := false
	for _, got := range calls {
		if got.path == "/api/2.0/mlflow/experiments/get-by-name" {
			values, _ := url.ParseQuery(got.query)
			foundQuery = values.Get("experiment_name") == mlflowPrefix(ws.ID)+"sales"
		}
	}
	if !foundQuery {
		t.Fatalf("GET experiment name was not namespaced: %+v", calls)
	}
}

func TestMLflowProxyErrors(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	call := func(method, endpoint, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/x", strings.NewReader(body))
		r.SetPathValue("wid", ws.ID)
		r.SetPathValue("path", strings.TrimPrefix(endpoint, "/"))
		w := httptest.NewRecorder()
		a.mlflowProxy(w, r, admin)
		return w
	}
	if w := call("GET", "/version", ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured = %d", w.Code)
	}
	for _, raw := range []string{"://bad", "relative"} {
		if err := a.SetMLflowBackend(raw); err == nil {
			t.Errorf("SetMLflowBackend(%q) succeeded", raw)
		}
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream"}`))
	}))
	if err := a.SetMLflowBackend(backend.URL); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/private", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unsupported = %d", w.Code)
	}
	if w := call("POST", "/api/2.0/mlflow/experiments/create", `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON = %d", w.Code)
	}
	if w := call("POST", "/api/2.0/mlflow/experiments/create", `{"name":"failed"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("upstream error = %d", w.Code)
	}
	if _, err := st.GetItemByName(ws.ID, "failed", "MLExperiment"); err == nil {
		t.Fatal("failed upstream request synchronized an item")
	}
	backend.Close()
	if w := call("GET", "/version", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("transport error = %d", w.Code)
	}
}

func TestMLflowResponseFilterLeavesNonJSON(t *testing.T) {
	if got := string(filterAndStripMLflowResponse("w", []byte("not-json"))); got != "not-json" {
		t.Fatalf("non-JSON response = %q", got)
	}
	filtered := filterAndStripMLflowResponse("w", []byte(`{"registered_models":[{"name":"fabric__w__mine"},{"name":"foreign"}],"next_page_token":"x"}`))
	var got map[string]any
	if err := json.Unmarshal(filtered, &got); err != nil {
		t.Fatal(err)
	}
	if models := got["registered_models"].([]any); len(models) != 1 {
		t.Fatalf("filtered models = %#v", models)
	}
}
