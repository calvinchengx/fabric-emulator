package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
)

// Writing through an ADLS Gen2 shortcut must reach the storage account, and
// deleting through one must remove it there.
//
// This drives the whole DFS create → append → flush sequence over HTTP, which
// is what makes it worth having beyond the unit tests: the hooks that divert a
// flush and a delete upstream live in the request path, and before they
// existed a write here landed in the local store and returned 200 while the
// target stayed untouched. `e2e/azurite-shortcut` proves the same thing
// against Microsoft's emulator; this proves it in-process, on every run.
func TestWriteThroughADLSShortcutReachesTarget(t *testing.T) {
	var mu sync.Mutex
	upstream := map[string][]byte{}
	var methods []string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			upstream[r.URL.Path] = body
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			delete(upstream, r.URL.Path)
			w.WriteHeader(http.StatusAccepted)
		case http.MethodGet:
			body, ok := upstream[r.URL.Path]
			if !ok {
				http.Error(w, "gone", http.StatusNotFound)
				return
			}
			_, _ = w.Write(body)
		}
	}))
	defer target.Close()

	f := newFixture(t)
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})

	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "extwrite"}, &ws)
	var lake struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/lakehouses", f.token,
		map[string]any{"displayName": "lh"}, &lake)
	var conn struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/connections", f.token, map[string]any{
		"displayName":      "ext",
		"connectivityType": "ShareableCloud",
		"credentialDetails": map[string]any{
			"credentials": map[string]any{"credentialType": "Anonymous"},
		},
	}, &conn), http.StatusCreated, "connection")
	f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+lake.ID+"/shortcuts", f.token,
		map[string]any{"path": "Files", "name": "ext", "target": map[string]any{
			"adlsGen2": map[string]any{
				"location": target.URL + "/container", "subpath": "", "connectionId": conn.ID,
			}}}, nil)

	base := "/" + ws.ID + "/" + lake.ID + "/Files/ext/out.csv"
	payload := []byte("a,b\n1,2\n")

	olStatus(t, f.ol(t, "PUT", base+"?resource=file", storage, nil), http.StatusCreated, "create")
	olStatus(t, f.ol(t, "PATCH", base+"?action=append&position=0", storage, payload),
		http.StatusAccepted, "append")
	olStatus(t, f.ol(t, "PATCH", base+"?action=flush&position=8", storage, nil),
		http.StatusOK, "flush")

	mu.Lock()
	got := upstream["/container/out.csv"]
	mu.Unlock()
	if string(got) != string(payload) {
		t.Fatalf("the target holds %q, not what was written — the flush never reached it", got)
	}

	// Reading it back must come from the target, not from a local copy: the
	// local buffer is dropped at flush precisely so it cannot shadow the
	// target. Changing the bytes upstream and re-reading proves which one the
	// read path is actually using.
	mu.Lock()
	upstream["/container/out.csv"] = []byte("changed,at,source\n")
	mu.Unlock()
	resp := f.ol(t, "GET", base, storage, nil)
	olStatus(t, resp, http.StatusOK, "read back")
	back, _ := io.ReadAll(resp.Body)
	if string(back) != "changed,at,source\n" {
		t.Fatalf("read returned %q — a stale local copy is shadowing the target", back)
	}

	// Deleting through the shortcut removes it at the target.
	olStatus(t, f.ol(t, "DELETE", base, storage, nil), http.StatusOK, "delete")
	mu.Lock()
	_, still := upstream["/container/out.csv"]
	mu.Unlock()
	if still {
		t.Fatal("the file survives at the target — the delete never reached it")
	}

	mu.Lock()
	seen := append([]string(nil), methods...)
	mu.Unlock()
	if len(seen) == 0 || seen[0] != "PUT /container/out.csv" {
		t.Fatalf("upstream calls = %v; the first must be the flush's PUT", seen)
	}
	if seen[len(seen)-1] != "DELETE /container/out.csv" {
		t.Fatalf("upstream calls = %v; the last must be the DELETE", seen)
	}
}

// An S3 shortcut must refuse a write outright: Fabric documents S3 shortcuts
// as read-only "regardless of the user's permissions", so the target must
// never see the request at all.
func TestWriteThroughS3ShortcutIsRefused(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			reached = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	f := newFixture(t)
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "s3ro"}, &ws)
	var lake struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/lakehouses", f.token,
		map[string]any{"displayName": "lh"}, &lake)
	var conn struct{ ID string }
	f.call("POST", "/v1/connections", f.token, map[string]any{
		"displayName":      "s3conn",
		"connectivityType": "ShareableCloud",
		"credentialDetails": map[string]any{
			"credentials": map[string]any{
				"credentialType": "Basic", "username": "AK", "password": "SK",
			}},
	}, &conn)
	f.call("POST", "/v1/workspaces/"+ws.ID+"/items/"+lake.ID+"/shortcuts", f.token,
		map[string]any{"path": "Files", "name": "s3", "target": map[string]any{
			"amazonS3": map[string]any{
				"location": target.URL + "/bucket", "subpath": "", "connectionId": conn.ID,
			}}}, nil)

	base := "/" + ws.ID + "/" + lake.ID + "/Files/s3/out.csv"
	f.ol(t, "PUT", base+"?resource=file", storage, nil)
	f.ol(t, "PATCH", base+"?action=append&position=0", storage, []byte("x"))
	resp := f.ol(t, "PATCH", base+"?action=flush&position=1", storage, nil)
	if resp.StatusCode < 400 {
		t.Fatalf("flush through an S3 shortcut returned %d; it must be refused", resp.StatusCode)
	}
	if reached {
		t.Fatal("the S3 target received a write — it must be refused before the network")
	}
}
