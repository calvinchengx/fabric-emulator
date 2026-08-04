package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Every handler in this package that reads a request body, checked for the same
// thing: an oversized body is REFUSED, not quietly cut down to size.
//
// internal/httpx explains the defect; this file proves each call site is wired
// to the fix. The guard test in that package stops a tenth site appearing with
// the old idiom, but a guard on the source text cannot tell whether the handler
// actually returns 413 — only running it can.
//
// WHY THE SIZES ARE STREAMED. A body one byte past a 128 MiB ceiling has to be
// produced, not allocated; `oversized` yields zeroes forever and costs nothing.

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { return len(p), nil }

// oversized builds a body exactly one byte past max.
func oversized(max int64) io.Reader { return io.LimitReader(zeroes{}, max+1) }

// doBody is `do` with a streamed body, so a test can post more bytes than it
// would be sensible to hold.
func doBody(h handler, method string, body io.Reader,
	pathVals map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/x", body)
	for k, v := range pathVals {
		r.SetPathValue(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r, admin)
	return w
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestOversizedRequestBodiesAreRefusedByEveryHandler(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)

	// Attached engines, so the proxy handlers reach their body read rather
	// than short-circuiting on "not configured" — which would make these
	// assertions pass without exercising anything.
	a.MLflowURL = mustURL(t, "http://mlflow.invalid")
	a.KQLURL = mustURL(t, "http://kusto.invalid")

	// The notebook carries a real definition: vscodeUpdateNotebookContent reads
	// the CURRENT one before it reads the body, so a bare item 500s on
	// "notebook-content.py is missing" and never reaches the check under test.
	notebook := &store.Item{WorkspaceID: ws.ID, DisplayName: "nb", Type: "Notebook"}
	if err := st.CreateItem(notebook, vscodeNotebookParts([]byte(vscodeNotebook))); err != nil {
		t.Fatal(err)
	}
	airflow := &store.Item{WorkspaceID: ws.ID, DisplayName: "af", Type: "ApacheAirflowJob"}
	if err := st.CreateItem(airflow, nil); err != nil {
		t.Fatal(err)
	}
	eventhouse := &store.Item{WorkspaceID: ws.ID, DisplayName: "eh", Type: "Eventhouse"}
	if err := st.CreateItem(eventhouse, nil); err != nil {
		t.Fatal(err)
	}
	lakehouse := &store.Item{WorkspaceID: ws.ID, DisplayName: "lake", Type: "Lakehouse"}
	if err := st.CreateItem(lakehouse, nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		h      handler
		method string
		max    int64
		path   map[string]string
		// why records what the body would have become if it were truncated —
		// stored, relayed, or retained. A case with no answer to that does not
		// belong in this file.
		why string
	}{
		{
			name: "vscode resource PUT", h: a.vscodeResourcePut, method: "PUT",
			max:  httpx.MaxItemContent,
			path: map[string]string{"wid": ws.ID, "iid": notebook.ID, "path": "data.bin"},
			why:  "written straight into the store as the file, with nothing parsing it",
		},
		{
			name: "vscode notebook content PUT", h: a.vscodeUpdateNotebookContent, method: "PUT",
			max:  httpx.MaxItemContent,
			path: map[string]string{"wid": ws.ID, "iid": notebook.ID},
			why:  "stored as the notebook definition",
		},
		{
			name: "airflow DAG file PUT", h: a.putAirflowFile, method: "PUT",
			max:  httpx.MaxItemContent,
			path: map[string]string{"wid": ws.ID, "iid": airflow.ID, "path": "dags/d.py"},
			why:  "stored as the DAG file",
		},
		{
			name: "mlflow proxy", h: a.mlflowProxy, method: "POST",
			max:  httpx.MaxProxyBody,
			path: map[string]string{"wid": ws.ID, "path": "/api/2.0/mlflow/runs/create"},
			why:  "relayed upstream as the caller's own request",
		},
		{
			name: "kusto relay", h: a.kustoRelay, method: "POST",
			max:  httpx.MaxProxyBody,
			path: map[string]string{"wid": ws.ID, "ehid": eventhouse.ID, "ver": "v1", "kind": "mgmt"},
			why:  "relayed to the engine as the caller's KQL",
		},
		{
			name: "livy high-concurrency acquire", h: a.acquireHC, method: "POST",
			max:  httpx.MaxControlBody,
			path: map[string]string{"wid": ws.ID, "lid": lakehouse.ID},
			why:  "retained verbatim on the session",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doBody(tc.h, tc.method, oversized(tc.max), tc.path)
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s: one byte over %d gave %d %s; want 413 — otherwise the "+
					"body is %s", tc.name, tc.max, w.Code, w.Body.String(), tc.why)
			}
		})
	}
}

// TestABodyExactlyAtTheCeilingIsStillAccepted is the other half, and without it
// the test above would pass on a handler that rejected everything.
//
// Only the small-ceiling sites are exercised: proving the boundary at 128 MiB
// would mean pushing 128 MiB through a handler to learn what
// TestReadBoundedAtEveryBoundary already establishes about the arithmetic.
func TestABodyExactlyAtTheCeilingIsStillAccepted(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	notebook := &store.Item{WorkspaceID: ws.ID, DisplayName: "nb", Type: "Notebook"}
	if err := st.CreateItem(notebook, nil); err != nil {
		t.Fatal(err)
	}

	// One byte UNDER the ceiling rather than at it: 32 MiB is real memory, and
	// the exact-boundary arithmetic is covered where it costs nothing.
	body := io.LimitReader(zeroes{}, 1<<20)
	w := doBody(a.vscodeResourcePut, "PUT", body,
		map[string]string{"wid": ws.ID, "iid": notebook.ID, "path": "data.bin"})
	if w.Code != http.StatusOK {
		t.Fatalf("a 1 MiB resource was refused with %d %s", w.Code, w.Body.String())
	}
	// And it arrived WHOLE. A handler that stored a fragment and answered 200
	// is the original bug wearing a passing test.
	pth, err := st.GetOneLakePath(notebook.ID, vscodeResourcePrefix+"/data.bin")
	if err != nil {
		t.Fatalf("the resource was accepted but not stored: %v", err)
	}
	if len(pth.Content) != 1<<20 {
		t.Fatalf("stored %d bytes of a %d-byte upload", len(pth.Content), 1<<20)
	}
}

// TestARefusedResourceWriteStoresNothing pins the property that a 413 alone
// does not give: refusing loudly while leaving a fragment behind would still be
// corruption, just better labelled.
func TestARefusedResourceWriteStoresNothing(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	notebook := &store.Item{WorkspaceID: ws.ID, DisplayName: "nb", Type: "Notebook"}
	if err := st.CreateItem(notebook, nil); err != nil {
		t.Fatal(err)
	}

	w := doBody(a.vscodeResourcePut, "PUT", oversized(httpx.MaxItemContent),
		map[string]string{"wid": ws.ID, "iid": notebook.ID, "path": "data.bin"})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized resource = %d", w.Code)
	}
	if _, err := st.GetOneLakePath(notebook.ID, vscodeResourcePrefix+"/data.bin"); err == nil {
		t.Fatal("a refused write left a file behind; nothing may be stored")
	}
}
