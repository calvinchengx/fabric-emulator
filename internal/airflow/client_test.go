package airflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSyncDAGsReplacesItemTreeAndRejectsTraversal(t *testing.T) {
	c := &Client{DAGDir: t.TempDir()}
	if _, err := c.SyncDAGs(context.Background(), "item", map[string][]byte{"nested/dag.py": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(c.DAGDir, "item", "nested", "dag.py")
	if raw, err := os.ReadFile(p); err != nil || string(raw) != "v1" {
		t.Fatalf("read=%q err=%v", raw, err)
	}
	if _, err := c.SyncDAGs(context.Background(), "item", map[string][]byte{"new.py": []byte("v2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("stale DAG remains: %v", err)
	}
	for _, name := range []string{"../escape.py", "/absolute.py", "."} {
		if _, err := c.SyncDAGs(context.Background(), "item", map[string][]byte{name: []byte("x")}); err == nil {
			t.Errorf("path %q accepted", name)
		}
		if raw, err := os.ReadFile(filepath.Join(c.DAGDir, "item", "new.py")); err != nil || string(raw) != "v2" {
			t.Errorf("invalid replacement removed existing DAG: raw=%q err=%v", raw, err)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SyncDAGs(cancelled, "item", map[string][]byte{"cancelled.py": []byte("x")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sync error=%v", err)
	}
}

func TestTriggerAndWaitSuccessWithBasicAuth(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "admin" || p != "secret" {
			t.Errorf("basic auth=%q %q %v", u, p, ok)
		}
		switch {
		case r.Method == "PATCH":
			w.Write([]byte(`{}`))
		case r.Method == "POST":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["dag_run_id"] != "run-1" {
				t.Errorf("trigger=%#v", body)
			}
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		case r.Method == "GET":
			state := "running"
			if polls.Add(1) > 1 {
				state = "success"
			}
			w.Write([]byte(`{"state":"` + state + `"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, t.TempDir(), "admin", "secret")
	if err != nil {
		t.Fatal(err)
	}
	c.PollInterval = time.Millisecond
	if err := c.TriggerAndWait(context.Background(), "hello dag", "run-1", "", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 2 {
		t.Fatalf("polls=%d", polls.Load())
	}
}

func TestTriggerAndWaitFailuresAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		cancel  bool
	}{
		{name: "load timeout", handler: func(w http.ResponseWriter, r *http.Request) { http.Error(w, "missing", 404) }, cancel: true},
		{name: "trigger", handler: func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PATCH" {
				w.Write([]byte(`{}`))
				return
			}
			http.Error(w, "bad", 500)
		}},
		{name: "poll", handler: func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PATCH" || r.Method == "POST" {
				w.Write([]byte(`{}`))
				return
			}
			http.Error(w, "bad", 500)
		}},
		{name: "dag failed", handler: func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PATCH" || r.Method == "POST" {
				w.Write([]byte(`{}`))
				return
			}
			w.Write([]byte(`{"state":"failed"}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c, _ := New(srv.URL, t.TempDir(), "", "")
			c.PollInterval = time.Millisecond
			ctx := context.Background()
			var cancel context.CancelFunc
			if tc.cancel {
				ctx, cancel = context.WithTimeout(ctx, 5*time.Millisecond)
				defer cancel()
			}
			if err := c.TriggerAndWait(ctx, "dag", "run", "", nil); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestNewAndCallValidation(t *testing.T) {
	for _, args := range [][2]string{{"bad", "/tmp"}, {"https://airflow", ""}} {
		if _, err := New(args[0], args[1], "", ""); err == nil {
			t.Errorf("New%v succeeded", args)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`not-json`)) }))
	defer srv.Close()
	c, _ := New(srv.URL, t.TempDir(), "", "")
	if _, err := c.call(context.Background(), "GET", "/", nil, &map[string]any{}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("decode err=%v", err)
	}
}

func TestCallTransportAndRequestErrors(t *testing.T) {
	c := &Client{BaseURL: "://bad", HTTP: http.DefaultClient}
	if _, err := c.call(context.Background(), "GET", "/", nil, nil); err == nil {
		t.Fatal("invalid request URL succeeded")
	}
	c = &Client{BaseURL: "http://127.0.0.1:1", HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	if _, err := c.call(context.Background(), "GET", "/", nil, nil); err == nil {
		t.Fatal("transport failure succeeded")
	}
}

// A DAG that EXISTS is not a DAG that is CURRENT. Triggering on existence
// alone creates the run from whatever is serialised at that instant, which for
// a changed file is the previous topology -- a green run whose task instances
// belong to replaced code.
func TestTriggerWaitsUntilTheSerialisedTaskSetChanges(t *testing.T) {
	var taskPolls atomic.Int32
	var triggered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PATCH":
			w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tasks"):
			// STALE THREE TIMES, THEN THE NEW TOPOLOGY. This is the window in
			// which the old code triggered, and in which `last_parsed_time`
			// and `dagSources` both already read as current.
			if taskPolls.Add(1) > 3 {
				w.Write([]byte(`{"tasks":[{"task_id":"a"},{"task_id":"b"}]}`))
				return
			}
			w.Write([]byte(`{"tasks":[{"task_id":"a"}]}`))
		case r.Method == "POST":
			if taskPolls.Load() <= 3 {
				t.Errorf("triggered after %d polls -- the run was created while "+
					"Airflow still held the previous serialisation", taskPolls.Load())
			}
			triggered.Store(true)
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		case r.Method == "GET":
			w.Write([]byte(`{"state":"success"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.PollInterval = time.Millisecond
	// "a" is what Airflow served BEFORE the sync.
	if err := c.TriggerAndWait(context.Background(), "d", "run-1", "a", nil); err != nil {
		t.Fatal(err)
	}
	if !triggered.Load() {
		t.Fatal("never triggered")
	}
}

// A DAG Airflow has never served cannot be stale, and waiting for it to
// "change" would block every first run until the timeout.
func TestTriggerDoesNotWaitWhenThereIsNoBaseline(t *testing.T) {
	var triggered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PATCH":
			w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/tasks"):
			t.Error("asked for the task set with no baseline to compare it to")
		case r.Method == "POST":
			triggered.Store(true)
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`{"state":"success"}`))
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, t.TempDir(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.PollInterval = time.Millisecond
	if err := c.TriggerAndWait(context.Background(), "d", "run-1", "", nil); err != nil {
		t.Fatal(err)
	}
	if !triggered.Load() {
		t.Fatal("never triggered")
	}
}

// An unchanged DAG must not be restamped, or every run pays a scheduler parse
// interval for nothing -- and a correct wait that always costs 30 seconds is a
// wait somebody deletes.
func TestSyncKeepsTheTimestampOfAnUnchangedFile(t *testing.T) {
	dagDir := t.TempDir()
	c, err := New("http://airflow.invalid", dagDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	files := map[string][]byte{"dags/d.py": []byte("# one")}
	if _, err := c.SyncDAGs(ctx, "item-1", files); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dagDir, "item-1", "dags", "d.py")
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Backdate, so a rewrite is unmistakable rather than lost in clock
	// resolution.
	old := first.ModTime().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := c.SyncDAGs(ctx, "item-1", files); err != nil {
		t.Fatal(err)
	}
	same, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !same.ModTime().Equal(old) {
		t.Fatalf("identical bytes were restamped: %v -> %v", old, same.ModTime())
	}

	// CHANGED bytes must take the new time, or the wait would be skipped
	// exactly when it is needed.
	if _, err := c.SyncDAGs(ctx, "item-1", map[string][]byte{"dags/d.py": []byte("# two")}); err != nil {
		t.Fatal(err)
	}
	changed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed.ModTime().After(old) {
		t.Fatalf("changed bytes kept the old timestamp: %v", changed.ModTime())
	}
}

// Re-running an UNMODIFIED DAG is the common case, and it must not wait.
//
// Only a changed file can leave Airflow's serialisation stale. If the bytes
// are identical, what Airflow holds is already what is on disk -- and waiting
// for its task set to "change" would wait for something that will never
// happen, spending the entire timeout on every ordinary re-run.
func TestSyncReportsWhetherAnythingActuallyChanged(t *testing.T) {
	dagDir := t.TempDir()
	c, err := New("http://airflow.invalid", dagDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	files := map[string][]byte{"dags/d.py": []byte("# one")}

	changed, err := c.SyncDAGs(ctx, "item-1", files)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a first sync creates the file and must report a change")
	}

	if changed, err = c.SyncDAGs(ctx, "item-1", files); err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("identical bytes reported as a change -- every re-run would pay the wait")
	}

	if changed, err = c.SyncDAGs(ctx, "item-1", map[string][]byte{"dags/d.py": []byte("# two")}); err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("different bytes reported as unchanged -- the wait would be skipped when needed")
	}

	// A REMOVED file changes the item as surely as an edited one.
	if changed, err = c.SyncDAGs(ctx, "item-1", map[string][]byte{
		"dags/d.py": []byte("# two"), "dags/e.py": []byte("# new")}); err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an added file reported as unchanged")
	}
	if changed, err = c.SyncDAGs(ctx, "item-1", map[string][]byte{"dags/d.py": []byte("# two")}); err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a removed file reported as unchanged")
	}
}
