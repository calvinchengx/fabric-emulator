package api

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type dbxStandIn struct {
	t          *testing.T
	token      string
	imported   string
	importedB  []byte
	created    map[string]any
	runNow     map[string]any
	polls      atomic.Int32
	failRun    bool
	failCreate bool
	delayPolls int32
}

func (s *dbxStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
		http.Error(w, `{"error_code":"UNAUTHENTICATED"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/2.0/workspace/import":
		var body struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.imported = body.Path
		b, err := base64.StdEncoding.DecodeString(body.Content)
		if err != nil {
			http.Error(w, "content must be base64", http.StatusBadRequest)
			return
		}
		s.importedB = b
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	case r.Method == "POST" && r.URL.Path == "/api/2.2/jobs/create":
		if s.failCreate {
			http.Error(w, `{"error_code":"BAD_REQUEST","message":"create refused"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&s.created)
		_, _ = w.Write([]byte(`{"job_id":11}`))
	case r.Method == "POST" && r.URL.Path == "/api/2.2/jobs/run-now":
		_ = json.NewDecoder(r.Body).Decode(&s.runNow)
		_, _ = w.Write([]byte(`{"run_id":22}`))
	case r.Method == "GET" && r.URL.Path == "/api/2.2/jobs/runs/get":
		n := s.polls.Add(1)
		life := "TERMINATED"
		result := "SUCCESS"
		if s.delayPolls > 0 && n <= s.delayPolls {
			life = "RUNNING"
			result = ""
		}
		if s.failRun && life == "TERMINATED" {
			result = "FAILED"
		}
		_, _ = w.Write([]byte(`{"run_id":22,"job_id":11,"state":{"life_cycle_state":"` +
			life + `","result_state":"` + result + `","state_message":"engine said no"},` +
			`"executedBy":"the emulator's Spark engine, not a Databricks cluster"}`))
	case r.Method == "GET" && r.URL.Path == "/api/2.2/jobs/runs/get-output":
		_, _ = w.Write([]byte(`{"error":"engine said no"}`))
	default:
		s.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func attachDatabricks(t *testing.T, a *API, stub *dbxStandIn) {
	t.Helper()
	ts := httptest.NewServer(stub)
	t.Cleanup(ts.Close)
	a.DatabricksURL = ts.URL
	a.DatabricksToken = stub.token
}

// TestDatabricksRemoteAcceptsDBFS: with FABRIC_DATABRICKS_URL set, a dbfs:
// path is submitted as written — the mapping the local path used to refuse.
func TestDatabricksRemoteAcceptsDBFS(t *testing.T) {
	a, st := newAPI(t)
	stub := &dbxStandIn{t: t, token: "dapi-test"}
	attachDatabricks(t, a, stub)
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksSparkPython",
		`"pythonFile":"dbfs:/jobs/etl.py","parameters":["--full"]`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	if stub.imported != "" {
		t.Fatalf("a native dbfs: path was imported; it should already live on the remote: %q", stub.imported)
	}
	task, _ := stub.created["tasks"].([]any)
	if len(task) != 1 {
		t.Fatalf("create tasks = %+v", stub.created)
	}
	body, _ := json.Marshal(task[0])
	if !strings.Contains(string(body), `"python_file":"dbfs:/jobs/etl.py"`) {
		t.Fatalf("remote job did not keep the dbfs: path: %s", body)
	}
	if !strings.Contains(string(body), "--full") {
		t.Fatalf("python parameters did not reach jobs/create: %s", body)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	out := outputOf(runs, "Dbx")
	if s, _ := out["executedBy"].(string); !strings.Contains(s, "not a Databricks cluster") {
		t.Fatalf("output does not carry the remote engine: %+v", out)
	}
	if out["run_id"] == nil {
		t.Fatalf("output missing run_id: %+v", out)
	}
}

// TestDatabricksRemoteImportsLakehouseFile: a lakehouse path is still legal
// when the URL is set — the bytes are imported, then the job names the
// imported workspace path.
func TestDatabricksRemoteImportsLakehouseFile(t *testing.T) {
	a, st := newAPI(t)
	stub := &dbxStandIn{t: t, token: "dapi-test", delayPolls: 1}
	attachDatabricks(t, a, stub)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")
	seedFile(t, st, ws.ID, lh.ID, "Files/nb/transform.py", []byte("print(run_date)\n"))

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"`+lh.ID+`/Files/nb/transform.py",
         "baseParameters":{"run_date":"2026-08-08"}`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Completed" {
		_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
		t.Fatalf("job = %s; runs=%+v", s, runs)
	}
	if string(stub.importedB) != "print(run_date)\n" {
		t.Fatalf("imported bytes = %q", stub.importedB)
	}
	if !strings.Contains(stub.imported, "/Shared/fabric-emulator/") {
		t.Fatalf("import path %q is not under the fabric workspace prefix", stub.imported)
	}
	task, _ := stub.created["tasks"].([]any)
	body, _ := json.Marshal(task[0])
	if !strings.Contains(string(body), `"notebook_path":"`+stub.imported+`"`) {
		t.Fatalf("job did not name the imported path: %s", body)
	}
	if !strings.Contains(string(body), "run_date") {
		t.Fatalf("base_parameters did not reach jobs/create: %s", body)
	}
}

// TestDatabricksRemoteFailureFailsTheActivity: a FAILED remote run is the
// activity's error, not a fabricated Succeeded.
func TestDatabricksRemoteFailureFailsTheActivity(t *testing.T) {
	a, st := newAPI(t)
	stub := &dbxStandIn{t: t, token: "dapi-test", failRun: true}
	attachDatabricks(t, a, stub)
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"/Workspace/etl"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "engine said no") {
		t.Fatalf("error %q does not carry the remote run's message", e)
	}
}

func TestDatabricksRemoteCreateErrorIsHonest(t *testing.T) {
	a, st := newAPI(t)
	stub := &dbxStandIn{t: t, token: "dapi-test", failCreate: true}
	attachDatabricks(t, a, stub)
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"/Shared/etl"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "create refused") {
		t.Fatalf("error %q does not carry the remote create body", e)
	}
}

func TestDatabricksRemoteRejectsWrongToken(t *testing.T) {
	a, st := newAPI(t)
	stub := &dbxStandIn{t: t, token: "dapi-real"}
	ts := httptest.NewServer(stub)
	t.Cleanup(ts.Close)
	a.DatabricksURL = ts.URL
	a.DatabricksToken = "dapi-wrong"
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"/Repos/etl"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "UNAUTHENTICATED") {
		t.Fatalf("error %q does not name the 401", e)
	}
}

func TestDatabricksRemoteMissingLakehouseFile(t *testing.T) {
	a, st := newAPI(t)
	stub := &dbxStandIn{t: t, token: "dapi-test"}
	attachDatabricks(t, a, stub)
	ws := seedWorkspace(t, st)
	lh := seedLakehouse(t, st, ws.ID, "lake")

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"`+lh.ID+`/Files/missing.py"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "no file at") {
		t.Fatalf("error %q does not name the missing lakehouse file", e)
	}
}

// TestDatabricksRemoteStillRefusesJAR: pointing at a workspace does not
// invent a main-class submission path. The JAR refusal is about the task
// type, not about where the file lives.
func TestDatabricksRemoteStillRefusesJAR(t *testing.T) {
	a, st := newAPI(t)
	stub := &dbxStandIn{t: t, token: "dapi-test"}
	attachDatabricks(t, a, stub)
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksSparkJar",
		`"mainClassName":"com.acme.Job"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	if stub.created != nil {
		t.Fatalf("JAR task reached jobs/create: %+v", stub.created)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); !strings.Contains(e, "no submission path for one on either engine") {
		t.Fatalf("error %q lost the JAR refusal", e)
	}
}

func TestDatabricksRemoteUnreachableHost(t *testing.T) {
	a, st := newAPI(t)
	a.DatabricksURL = "http://127.0.0.1:1"
	a.DatabricksToken = "dapi-test"
	ws := seedWorkspace(t, st)

	pl := createPipeline(t, st, ws.ID, dbxPipeline("DatabricksNotebook",
		`"notebookPath":"/Users/someone/etl"`))
	_, jid := runJob(t, a, ws.ID, pl.ID, "jobType=Pipeline", "{}")
	if s := awaitJob(t, a, ws.ID, pl.ID, jid); s != "Failed" {
		t.Fatalf("job = %s, want Failed", s)
	}
	_, runs := activityRuns(t, a, ws.ID, pl.ID, jid)
	if e, _ := runs[0]["error"].(string); e == "" {
		t.Fatal("unreachable host produced an empty error")
	}
}

func TestIsDatabricksNativePath(t *testing.T) {
	for _, p := range []string{"dbfs:/x", "/Workspace/x", "/Shared/x", "/Repos/x", "/Users/x"} {
		if !isDatabricksNativePath(p) {
			t.Errorf("%q should be native", p)
		}
	}
	if isDatabricksNativePath("abc/Files/x.py") {
		t.Error("a lakehouse path must not look native")
	}
}

func TestDatabricksJSONEmptyDest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "")
	}))
	t.Cleanup(ts.Close)
	e := &pipelineExecutor{a: &API{DatabricksURL: ts.URL}}
	if err := e.databricksJSON("GET", "/", nil, nil); err != nil {
		t.Fatal(err)
	}
}
