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
	if err := c.SyncDAGs(context.Background(), "item", map[string][]byte{"nested/dag.py": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(c.DAGDir, "item", "nested", "dag.py")
	if raw, err := os.ReadFile(p); err != nil || string(raw) != "v1" {
		t.Fatalf("read=%q err=%v", raw, err)
	}
	if err := c.SyncDAGs(context.Background(), "item", map[string][]byte{"new.py": []byte("v2")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("stale DAG remains: %v", err)
	}
	for _, name := range []string{"../escape.py", "/absolute.py", "."} {
		if err := c.SyncDAGs(context.Background(), "item", map[string][]byte{name: []byte("x")}); err == nil {
			t.Errorf("path %q accepted", name)
		}
		if raw, err := os.ReadFile(filepath.Join(c.DAGDir, "item", "new.py")); err != nil || string(raw) != "v2" {
			t.Errorf("invalid replacement removed existing DAG: raw=%q err=%v", raw, err)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.SyncDAGs(cancelled, "item", map[string][]byte{"cancelled.py": []byte("x")}); !errors.Is(err, context.Canceled) {
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
	if err := c.TriggerAndWait(context.Background(), "item-1", "hello dag", "run-1", map[string]any{"x": 1}); err != nil {
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
			if err := c.TriggerAndWait(ctx, "item-1", "dag", "run", nil); err == nil {
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
// a changed file is the previous version -- a green run whose task instances
// belong to replaced code. This asserts the trigger waits for Airflow to
// report a parse NEWER than the files on disk.
func TestTriggerWaitsUntilAirflowHasParsedTheFilesJustSynced(t *testing.T) {
	dagDir := t.TempDir()
	item := "item-1"
	if err := os.MkdirAll(filepath.Join(dagDir, item, "dags"), 0o755); err != nil {
		t.Fatal(err)
	}
	dagFile := filepath.Join(dagDir, item, "dags", "d.py")
	if err := os.WriteFile(dagFile, []byte("# dag"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dagFile)
	if err != nil {
		t.Fatal(err)
	}
	written := info.ModTime()

	var parseChecks atomic.Int32
	var triggered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PATCH":
			w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/dags/d"):
			// STALE TWICE, THEN CURRENT. The first answers predate the file
			// on disk, which is exactly the window the old code triggered in.
			n := parseChecks.Add(1)
			stamp := written.Add(-time.Minute)
			if n > 2 {
				stamp = written.Add(time.Second)
			}
			w.Write([]byte(`{"last_parsed_time":"` +
				stamp.UTC().Format(time.RFC3339Nano) + `"}`))
		case r.Method == "POST":
			if parseChecks.Load() <= 2 {
				t.Errorf("triggered after %d parse checks -- the run was created "+
					"while Airflow still held the previous serialisation",
					parseChecks.Load())
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

	c, err := New(srv.URL, dagDir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	c.PollInterval = time.Millisecond
	if err := c.TriggerAndWait(context.Background(), item, "d", "run-1", nil); err != nil {
		t.Fatal(err)
	}
	if !triggered.Load() {
		t.Fatal("never triggered")
	}
	if parseChecks.Load() < 3 {
		t.Fatalf("parse checks=%d -- the wait did not actually poll", parseChecks.Load())
	}
}
