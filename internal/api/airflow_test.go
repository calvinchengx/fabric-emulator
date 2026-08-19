package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

type airflowWitness struct {
	mu              sync.Mutex
	files           map[string][]byte
	dagID, runID    string
	conf            map[string]any
	syncErr, runErr error
}

func (w *airflowWitness) SyncDAGs(_ context.Context, _ string, files map[string][]byte) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.files = files
	// `true`, so the witness exercises the path a real changed sync takes.
	return true, w.syncErr
}
func (w *airflowWitness) DAGFingerprint(_ context.Context, _ string) string { return "" }

func (w *airflowWitness) TriggerAndWait(_ context.Context, dagID, runID, _ string, conf map[string]any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dagID, w.runID, w.conf = dagID, runID, conf
	return w.runErr
}

func TestAirflowFileManagement(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "ApacheAirflowJob", DisplayName: "air"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{"wid": ws.ID, "iid": it.ID, "path": "dags/hello.py"}
	w := do(a.putAirflowFile, admin, "PUT", "print('hello')", vals)
	if w.Code != 200 {
		t.Fatalf("put = %d %s", w.Code, w.Body.String())
	}
	w = do(a.getAirflowFile, admin, "GET", "", vals)
	if w.Code != 200 || w.Body.String() != "print('hello')" {
		t.Fatalf("get = %d %q", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/x?rootPath=dags", nil)
	r.SetPathValue("wid", ws.ID)
	r.SetPathValue("iid", it.ID)
	w = httptest.NewRecorder()
	a.listAirflowFiles(w, r, admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"filePath":"dags/hello.py"`) || !strings.Contains(w.Body.String(), `"sizeInBytes":14`) {
		t.Fatalf("list = %d %s", w.Code, w.Body.String())
	}
	w = do(a.deleteAirflowFile, admin, "DELETE", "", vals)
	if w.Code != 200 {
		t.Fatalf("delete = %d", w.Code)
	}
	if w = do(a.getAirflowFile, admin, "GET", "", vals); w.Code != 404 {
		t.Fatalf("deleted get = %d", w.Code)
	}

	for _, raw := range []string{"../secret", "/../../secret"} {
		if w = do(a.putAirflowFile, admin, "PUT", "x", map[string]string{"wid": ws.ID, "iid": it.ID, "path": raw}); w.Code != 400 {
			t.Errorf("unsafe path %q = %d", raw, w.Code)
		}
	}
	if w = do(a.putAirflowFile, admin, "PUT", string([]byte{0xff}), vals); w.Code != 400 {
		t.Errorf("non-UTF8 = %d", w.Code)
	}
	if w = do(a.putAirflowFile, viewer, "PUT", "x", vals); w.Code != 403 {
		t.Errorf("viewer put = %d", w.Code)
	}
	other := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "n"}
	_ = st.CreateItem(other, nil)
	if w = do(a.getAirflowFile, admin, "GET", "", map[string]string{"wid": ws.ID, "iid": other.ID, "path": "x"}); w.Code != 404 {
		t.Errorf("wrong type = %d", w.Code)
	}
	for name, h := range map[string]handler{
		"list":   a.listAirflowFiles,
		"get":    a.getAirflowFile,
		"delete": a.deleteAirflowFile,
	} {
		t.Run(name+" rejects traversal", func(t *testing.T) {
			r := httptest.NewRequest("GET", "/x?rootPath=../escape", nil)
			r.SetPathValue("wid", ws.ID)
			r.SetPathValue("iid", it.ID)
			r.SetPathValue("path", "../escape")
			w := httptest.NewRecorder()
			h(w, r, admin)
			if w.Code != 400 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	if w = do(a.deleteAirflowFile, admin, "DELETE", "", vals); w.Code != 404 {
		t.Errorf("delete missing = %d", w.Code)
	}
}

func TestAirflowRunSuccessFailureAndConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		runtime  *airflowWitness
		dagID    string
		withFile bool
		want     string
	}{
		{name: "success", runtime: &airflowWitness{}, dagID: "hello", withFile: true, want: store.JobCompleted},
		{name: "engine failure", runtime: &airflowWitness{runErr: errors.New("task failed")}, dagID: "hello", withFile: true, want: store.JobFailed},
		{name: "sync failure", runtime: &airflowWitness{syncErr: errors.New("disk full")}, dagID: "hello", withFile: true, want: store.JobFailed},
		{name: "missing file", runtime: &airflowWitness{}, dagID: "hello", want: store.JobFailed},
		{name: "missing engine", dagID: "hello", withFile: true, want: store.JobFailed},
		{name: "missing dag id", runtime: &airflowWitness{}, withFile: true, want: store.JobFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			if tc.runtime != nil {
				a.Airflow = tc.runtime
			}
			ws := seedWorkspace(t, st)
			it := &store.Item{WorkspaceID: ws.ID, Type: "ApacheAirflowJob", DisplayName: "air"}
			if err := st.CreateItem(it, nil); err != nil {
				t.Fatal(err)
			}
			if tc.withFile {
				_ = st.CreateOneLakePath(&store.OneLakePath{WorkspaceID: ws.ID, ItemID: it.ID, RelPath: "Files/dags/hello.py", Content: []byte("# dag")}, false)
			}
			body := `{"executionData":{"dagId":"` + tc.dagID + `","conf":{"answer":42}}}`
			w := do(a.createJobInstance, admin, "POST", body, map[string]string{"wid": ws.ID, "iid": it.ID})
			// do() uses /x, so provide the required query explicitly.
			if w.Code == 400 {
				r := httptest.NewRequest("POST", "/x?jobType=Run", strings.NewReader(body))
				r.SetPathValue("wid", ws.ID)
				r.SetPathValue("iid", it.ID)
				w = httptest.NewRecorder()
				a.createJobInstance(w, r, admin)
			}
			if w.Code != 202 {
				t.Fatalf("run = %d %s", w.Code, w.Body.String())
			}
			jid := strings.TrimPrefix(w.Header().Get("Location"), "https://example.com/v1/workspaces/"+ws.ID+"/items/"+it.ID+"/jobs/instances/")
			deadline := time.Now().Add(time.Second)
			for {
				job, err := st.GetJobInstance(it.ID, jid)
				if err != nil {
					t.Fatal(err)
				}
				if got := job.StatusAt(st.Now()); got == store.JobCompleted || got == store.JobFailed {
					if got != tc.want {
						t.Fatalf("status=%s code=%s want %s", got, job.FailWith, tc.want)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("Airflow job did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			if tc.name == "success" {
				tc.runtime.mu.Lock()
				defer tc.runtime.mu.Unlock()
				if tc.runtime.dagID != "hello" || len(tc.runtime.files) != 1 || tc.runtime.conf["answer"] != float64(42) {
					t.Fatalf("witness=%+v", tc.runtime)
				}
			}
		})
	}
}

func TestDataflowManagementAndHonestExecutionBoundary(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	it := &store.Item{WorkspaceID: ws.ID, Type: "Dataflow", DisplayName: "mashup"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	for _, jobType := range []string{"Refresh", "Publish"} {
		r := httptest.NewRequest("POST", "/x?jobType="+jobType, strings.NewReader(`{"executionData":{}}`))
		r.SetPathValue("wid", ws.ID)
		r.SetPathValue("iid", it.ID)
		w := httptest.NewRecorder()
		a.createJobInstance(w, r, admin)
		if w.Code != 202 {
			t.Fatalf("%s = %d", jobType, w.Code)
		}
		jid := pathTail(w.Header().Get("Location"))
		job, _ := st.GetJobInstance(it.ID, jid)
		if job.StatusAt(st.Now()) != store.JobFailed || job.FailWith != "DataflowEngineNotImplemented" {
			t.Fatalf("%s job=%+v", jobType, job)
		}
	}
}

func TestAirflowError(t *testing.T) {
	if got := (&airflowError{code: "AirflowDAGFileRequired"}).Error(); got != "AirflowDAGFileRequired" {
		t.Fatalf("Error()=%q", got)
	}
}

func pathTail(s string) string {
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return s
	}
	return s[i+1:]
}

// A DAG-SYNC FAILURE MUST NOT LOOK LIKE A DAG THAT FAILED. Both used to
// finalize as `AirflowRunFailed`, whose message is the bare "The job failed."
// -- so an operator whose emulator could not WRITE the DAG files saw a failed
// run beside an empty dags folder and no reason at all. That is not
// hypothetical: it is how a consumer platform's first end-to-end run failed,
// and the cause (a shared volume the emulator's non-root uid could not write)
// took a permissions audit to find rather than a read of the error.
//
// The distinction has to survive to the CODE, because the message is derived
// from it.
func TestAirflowDAGSyncFailureIsDistinctFromARunFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime *airflowWitness
		want    string
	}{
		{"sync", &airflowWitness{syncErr: errors.New("permission denied")}, "AirflowDAGSyncFailed"},
		{"run", &airflowWitness{runErr: errors.New("task failed")}, "AirflowRunFailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, st := newAPI(t)
			a.Airflow = tc.runtime
			ws := seedWorkspace(t, st)
			it := &store.Item{WorkspaceID: ws.ID, Type: "ApacheAirflowJob", DisplayName: "air"}
			if err := st.CreateItem(it, nil); err != nil {
				t.Fatal(err)
			}
			_ = st.CreateOneLakePath(&store.OneLakePath{
				WorkspaceID: ws.ID, ItemID: it.ID,
				RelPath: "Files/dags/hello.py", Content: []byte("# dag"),
			}, false)
			body := `{"executionData":{"dagId":"hello"}}`
			r := httptest.NewRequest("POST", "/x?jobType=Run", strings.NewReader(body))
			r.SetPathValue("wid", ws.ID)
			r.SetPathValue("iid", it.ID)
			w := httptest.NewRecorder()
			a.createJobInstance(w, r, admin)
			if w.Code != 202 {
				t.Fatalf("run = %d %s", w.Code, w.Body.String())
			}
			jid := strings.TrimPrefix(
				w.Header().Get("Location"),
				"https://example.com/v1/workspaces/"+ws.ID+"/items/"+it.ID+"/jobs/instances/")
			deadline := time.Now().Add(2 * time.Second)
			for {
				job, err := st.GetJobInstance(it.ID, jid)
				if err != nil {
					t.Fatal(err)
				}
				if job.FailWith != "" {
					if job.FailWith != tc.want {
						t.Fatalf("failure code = %q, want %q", job.FailWith, tc.want)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("job never failed: %+v", job)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}

	// And the code has to carry a message that names what to check. A distinct
	// code whose message is still "The job failed." would have moved the
	// problem rather than fixed it.
	msg := jobFailureMessage("AirflowDAGSyncFailed")
	if msg == "The job failed." {
		t.Fatal("AirflowDAGSyncFailed has no message of its own")
	}
	for _, want := range []string{"DAG", "writable", "uid"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message does not mention %q: %s", want, msg)
		}
	}
}
