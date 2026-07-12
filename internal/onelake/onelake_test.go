package onelake

// Direct handler tests for the DFS surface: a real auth.Validator wired to a
// local JWKS server (Storage audience) and an in-memory store, so every
// managed-folder rule, addressing form, and error shape from
// onelake-api-parity.md is exercised without the full server stack.

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const testIssuer = "https://login.test/tenant-1/v2.0"

func b64(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// mint signs an RS256 token with the fixture key (kid "test-key").
func mint(t *testing.T, key *rsa.PrivateKey, oid string, aud any, exp int64) string {
	t.Helper()
	head := map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test-key"}
	claims := map[string]any{"iss": testIssuer, "aud": aud, "exp": exp, "oid": oid}
	signing := b64(t, head) + "." + b64(t, claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

type fixture struct {
	t     *testing.T
	svc   *Service
	st    *store.Store
	key   *rsa.PrivateKey
	token string // admin-1 with the Storage audience
	ws    *store.Workspace
	it    *store.Item
}

// newFixture builds the service over an in-memory store and a validator that
// trusts a local JWKS server, carrying the Storage audience as server.New does.
// admin-1 owns a workspace "datalake-ws" containing a Lakehouse "lake".
func newFixture(t *testing.T) *fixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "test-key",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)
	v := auth.New(testIssuer, srv.URL, false, func() int64 { return 1000 }, srv.Client())
	v.Audiences = StorageAudience

	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ws := &store.Workspace{DisplayName: "datalake-ws"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "admin-1", Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	it := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, svc: New(st, v), st: st, key: key, ws: ws, it: it}
	f.token = f.storageToken("admin-1")
	return f
}

// storageToken mints an unexpired Storage-audience token for oid.
func (f *fixture) storageToken(oid string) string {
	return mint(f.t, f.key, oid, StorageAudience[0], 2000)
}

// do drives ServeHTTP directly and returns the recorder.
func (f *fixture) do(method, target, token string, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r := httptest.NewRequest(method, target, rd)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	return w
}

// errCode returns the DFS error code, checking header and body agree — the
// writeDFSErr shape (x-ms-error-code + {"error":{code,message}} JSON).
func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	code := w.Header().Get("x-ms-error-code")
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("error Content-Type = %q", ct)
	}
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("not a DFS error body: %s", w.Body.Bytes())
	}
	if body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("body code %q / message %q vs header %q", body.Error.Code, body.Error.Message, code)
	}
	return code
}

func TestNew(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v := &auth.Validator{}
	s := New(st, v)
	if s.Store != st || s.Auth != v {
		t.Fatalf("New wired %+v", s)
	}
}

func TestWriteDFSErr(t *testing.T) {
	w := httptest.NewRecorder()
	writeDFSErr(w, dfsError{"PathNotFound", http.StatusNotFound, "The path does not exist."})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	if got := errCode(t, w); got != "PathNotFound" {
		t.Fatalf("code = %q", got)
	}
}

func TestPermHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	permHeaders(w)
	if w.Header().Get("x-ms-owner") != "$superuser" ||
		w.Header().Get("x-ms-group") != "$superuser" ||
		w.Header().Get("x-ms-permissions") != "---------" {
		t.Fatalf("canned permission headers = %v", w.Header())
	}
}

func TestAuthBoundary(t *testing.T) {
	f := newFixture(t)

	cases := map[string]string{
		"no token":       "",
		"garbage":        "not-a-jwt",
		"wrong audience": mint(t, f.key, "admin-1", "https://api.fabric.microsoft.com", 2000),
		"expired":        mint(t, f.key, "admin-1", StorageAudience[0], 500), // now 1000 > 500+60
	}
	for name, tok := range cases {
		w := f.do("HEAD", "/", tok, nil)
		if w.Code != http.StatusUnauthorized || errCode(t, w) != "AuthenticationFailed" {
			t.Errorf("%s: %d %s; want 401 AuthenticationFailed", name, w.Code, errCode(t, w))
		}
		if !strings.Contains(w.Header().Get("WWW-Authenticate"), testIssuer) {
			t.Errorf("%s: WWW-Authenticate = %q", name, w.Header().Get("WWW-Authenticate"))
		}
	}

	// The trailing-slash audience form is also accepted.
	w := f.do("HEAD", "/", mint(t, f.key, "admin-1", StorageAudience[1], 2000), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("aud with trailing slash = %d", w.Code)
	}
}

func TestRBACRequiresContributor(t *testing.T) {
	f := newFixture(t)
	grant := func(id, role string) {
		t.Helper()
		err := f.st.CreateRoleAssignment(&store.RoleAssignment{
			WorkspaceID: f.ws.ID, Principal: store.Principal{ID: id, Type: "User"}, Role: role,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	grant("viewer-1", store.RoleViewer)
	grant("contrib-1", store.RoleContributor)

	// No grant and Viewer are both denied (OneLake API access is ReadAll).
	for _, id := range []string{"nobody-1", "viewer-1"} {
		w := f.do("HEAD", "/"+f.ws.ID, f.storageToken(id), nil)
		if w.Code != http.StatusForbidden || errCode(t, w) != "AuthorizationFailure" {
			t.Fatalf("%s: %d; want 403 AuthorizationFailure", id, w.Code)
		}
	}
	// Contributor and above pass.
	for _, id := range []string{"contrib-1", "admin-1"} {
		if w := f.do("HEAD", "/"+f.ws.ID, f.storageToken(id), nil); w.Code != http.StatusOK {
			t.Fatalf("%s: %d; want 200", id, w.Code)
		}
	}
}

func TestAccountLevelHeadOnly(t *testing.T) {
	f := newFixture(t)

	w := f.do("HEAD", "/", f.token, nil)
	if w.Code != http.StatusOK || w.Header().Get("x-ms-owner") != "$superuser" {
		t.Fatalf("account HEAD = %d owner %q", w.Code, w.Header().Get("x-ms-owner"))
	}
	for _, method := range []string{"GET", "PUT", "DELETE", "PATCH"} {
		w := f.do(method, "/", f.token, nil)
		if w.Code != http.StatusBadRequest || errCode(t, w) != "OperationNotAllowedOnAccount" {
			t.Fatalf("account %s = %d %s", method, w.Code, errCode(t, w))
		}
	}
}

func TestWorkspaceLevel(t *testing.T) {
	f := newFixture(t)

	// HEAD works by GUID and by display name (same filesystem).
	for _, seg := range []string{f.ws.ID, "datalake-ws"} {
		w := f.do("HEAD", "/"+seg, f.token, nil)
		if w.Code != http.StatusOK || w.Header().Get("x-ms-permissions") != "---------" {
			t.Fatalf("HEAD /%s = %d perms %q", seg, w.Code, w.Header().Get("x-ms-permissions"))
		}
	}
	// Everything else is a Fabric-experience operation.
	for _, method := range []string{"PUT", "DELETE", "PATCH"} {
		w := f.do(method, "/"+f.ws.ID, f.token, nil)
		if w.Code != http.StatusConflict || errCode(t, w) != "OperationNotAllowedOnFilesystem" {
			t.Fatalf("workspace %s = %d %s", method, w.Code, errCode(t, w))
		}
	}
	// GET without resource=filesystem is not a listing.
	if w := f.do("GET", "/"+f.ws.ID, f.token, nil); w.Code != http.StatusConflict {
		t.Fatalf("bare GET workspace = %d", w.Code)
	}
	if w := f.do("GET", "/"+f.ws.ID+"?resource=filesystem", f.token, nil); w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestManagedFolderEnforcement(t *testing.T) {
	f := newFixture(t)

	// Item root and first level, by GUID and by name addressing.
	paths := []string{
		"/" + f.ws.ID + "/" + f.it.ID,
		"/" + f.ws.ID + "/" + f.it.ID + "/Files",
		"/" + f.ws.ID + "/" + f.it.ID + "/Tables",
		"/datalake-ws/lake.Lakehouse/Files",
	}
	for _, p := range paths {
		w := f.do("HEAD", p, f.token, nil)
		if w.Code != http.StatusOK || w.Header().Get("x-ms-resource-type") != "directory" ||
			w.Header().Get("x-ms-owner") != "$superuser" {
			t.Fatalf("HEAD %s = %d type %q", p, w.Code, w.Header().Get("x-ms-resource-type"))
		}
		w = f.do("GET", p, f.token, nil)
		if w.Code != http.StatusBadRequest || errCode(t, w) != "PathIsDirectory" {
			t.Fatalf("GET %s = %d %s", p, w.Code, errCode(t, w))
		}
		for _, method := range []string{"PUT", "DELETE", "PATCH"} {
			w := f.do(method, p+"?resource=directory", f.token, nil)
			if w.Code != http.StatusConflict || errCode(t, w) != "OperationNotAllowedOnManagedFolder" {
				t.Fatalf("%s %s = %d %s", method, p, w.Code, errCode(t, w))
			}
		}
	}
}

func TestRejectedQueryParams(t *testing.T) {
	f := newFixture(t)
	base := "/" + f.ws.ID + "/" + f.it.ID + "/Files/x"

	for _, action := range []string{"setAccessControl", "setaccesscontrolrecursive", "SETACCESSCONTROL"} {
		w := f.do("PATCH", base+"?action="+action, f.token, nil)
		if w.Code != http.StatusBadRequest || errCode(t, w) != "UnsupportedQueryParameter" {
			t.Fatalf("action=%s = %d %s", action, w.Code, errCode(t, w))
		}
	}
	// The rejection happens before auth — no token needed to observe it.
	if w := f.do("PATCH", base+"?action=setAccessControl", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("pre-auth rejection = %d", w.Code)
	}
}

func TestIgnoredHeadersEchoed(t *testing.T) {
	f := newFixture(t)

	r := httptest.NewRequest("HEAD", "/", nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("x-ms-owner", "someone")
	r.Header.Set("x-ms-acls", "user::rwx")
	w := httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("request with banned headers failed: %d", w.Code)
	}
	echoed := w.Header().Get("x-ms-rejected-headers")
	if !strings.Contains(echoed, "x-ms-owner") || !strings.Contains(echoed, "x-ms-acls") {
		t.Fatalf("x-ms-rejected-headers = %q", echoed)
	}
	// The canned owner overwrites the echo target: response still $superuser.
	if w.Header().Get("x-ms-owner") != "$superuser" {
		t.Fatalf("x-ms-owner = %q", w.Header().Get("x-ms-owner"))
	}
	// No banned headers → no echo header at all.
	if w := f.do("HEAD", "/", f.token, nil); w.Header().Get("x-ms-rejected-headers") != "" {
		t.Fatalf("unexpected x-ms-rejected-headers = %q", w.Header().Get("x-ms-rejected-headers"))
	}
}

func TestFileCRUD(t *testing.T) {
	f := newFixture(t)
	base := "/" + f.ws.ID + "/" + f.it.ID + "/Files/raw/a.txt"
	dir := "/" + f.ws.ID + "/" + f.it.ID + "/Files/raw/deep"

	// Create a file with content, and a directory.
	if w := f.do("PUT", base, f.token, []byte("hello")); w.Code != http.StatusCreated {
		t.Fatalf("create file = %d %s", w.Code, w.Body.Bytes())
	}
	if w := f.do("PUT", dir+"?resource=directory", f.token, nil); w.Code != http.StatusCreated {
		t.Fatalf("create dir = %d", w.Code)
	}

	// GET reads the file back; directories refuse GET.
	w := f.do("GET", base, f.token, nil)
	if w.Code != http.StatusOK || w.Body.String() != "hello" ||
		w.Header().Get("Content-Type") != "application/octet-stream" ||
		w.Header().Get("Content-Length") != "5" {
		t.Fatalf("read = %d %q ct %q len %q", w.Code, w.Body.String(),
			w.Header().Get("Content-Type"), w.Header().Get("Content-Length"))
	}
	w = f.do("GET", dir, f.token, nil)
	if w.Code != http.StatusBadRequest || errCode(t, w) != "PathIsDirectory" {
		t.Fatalf("GET dir = %d %s", w.Code, errCode(t, w))
	}

	// HEAD: canned permissions plus length and resource type.
	w = f.do("HEAD", base, f.token, nil)
	if w.Code != http.StatusOK || w.Header().Get("x-ms-resource-type") != "file" ||
		w.Header().Get("Content-Length") != "5" || w.Header().Get("x-ms-owner") != "$superuser" {
		t.Fatalf("HEAD file headers = %v", w.Header())
	}
	if w := f.do("HEAD", dir, f.token, nil); w.Header().Get("x-ms-resource-type") != "directory" {
		t.Fatalf("HEAD dir type = %q", w.Header().Get("x-ms-resource-type"))
	}

	// PUT over an existing path replaces its content.
	f.do("PUT", base, f.token, []byte("bye"))
	if w := f.do("GET", base, f.token, nil); w.Body.String() != "bye" {
		t.Fatalf("overwrite = %q", w.Body.String())
	}

	// Missing paths are DFS 404s across methods.
	missing := "/" + f.ws.ID + "/" + f.it.ID + "/Files/raw/none"
	for _, method := range []string{"HEAD", "GET", "DELETE"} {
		w := f.do(method, missing, f.token, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s missing = %d", method, w.Code)
		}
		if method != "HEAD" && errCode(t, w) != "PathNotFound" {
			t.Fatalf("%s missing code = %s", method, errCode(t, w))
		}
	}

	// Deleting a directory removes its subtree.
	sub := dir + "/b.txt"
	f.do("PUT", sub, f.token, []byte("x"))
	if w := f.do("DELETE", dir, f.token, nil); w.Code != http.StatusOK {
		t.Fatalf("delete dir = %d", w.Code)
	}
	if w := f.do("GET", sub, f.token, nil); w.Code != http.StatusNotFound {
		t.Fatalf("file survived dir delete = %d", w.Code)
	}

	// Unsupported verb.
	w = f.do("POST", base, f.token, nil)
	if w.Code != http.StatusMethodNotAllowed || errCode(t, w) != "UnsupportedHttpVerb" {
		t.Fatalf("POST = %d %s", w.Code, errCode(t, w))
	}
}

func TestPatchAppendFlush(t *testing.T) {
	f := newFixture(t)
	base := "/" + f.ws.ID + "/" + f.it.ID + "/Files/raw/a.txt"
	dir := "/" + f.ws.ID + "/" + f.it.ID + "/Files/raw/d"
	f.do("PUT", base, f.token, nil)
	f.do("PUT", dir+"?resource=directory", f.token, nil)

	// Appends at the correct positions, then flush at the final length.
	if w := f.do("PATCH", base+"?action=append&position=0", f.token, []byte("hello ")); w.Code != http.StatusAccepted {
		t.Fatalf("append 1 = %d %s", w.Code, w.Body.Bytes())
	}
	if w := f.do("PATCH", base+"?action=append&position=6", f.token, []byte("world")); w.Code != http.StatusAccepted {
		t.Fatalf("append 2 = %d", w.Code)
	}
	w := f.do("PATCH", base+"?action=append&position=3", f.token, []byte("x"))
	if w.Code != http.StatusBadRequest || errCode(t, w) != "InvalidFlushPosition" {
		t.Fatalf("bad append position = %d %s", w.Code, errCode(t, w))
	}
	w = f.do("PATCH", base+"?action=flush&position=11", f.token, nil)
	if w.Code != http.StatusOK || w.Header().Get("Content-Length") != "0" {
		t.Fatalf("flush = %d len %q", w.Code, w.Header().Get("Content-Length"))
	}
	if w := f.do("PATCH", base+"?action=flush&position=5", f.token, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad flush position = %d", w.Code)
	}
	if w := f.do("GET", base, f.token, nil); w.Body.String() != "hello world" {
		t.Fatalf("content after appends = %q", w.Body.String())
	}

	// Error shapes: missing paths, directories, unknown/missing actions.
	missing := "/" + f.ws.ID + "/" + f.it.ID + "/Files/raw/none"
	w = f.do("PATCH", missing+"?action=flush&position=0", f.token, nil)
	if w.Code != http.StatusNotFound || errCode(t, w) != "PathNotFound" {
		t.Fatalf("flush missing = %d %s", w.Code, errCode(t, w))
	}
	if w := f.do("PATCH", missing+"?action=append&position=0", f.token, []byte("x")); w.Code != http.StatusBadRequest {
		t.Fatalf("append missing = %d", w.Code)
	}
	if w := f.do("PATCH", dir+"?action=append&position=0", f.token, []byte("x")); w.Code != http.StatusBadRequest {
		t.Fatalf("append to dir = %d", w.Code)
	}
	for _, q := range []string{"?action=bogus", ""} {
		w := f.do("PATCH", base+q, f.token, nil)
		if w.Code != http.StatusBadRequest || errCode(t, w) != "UnsupportedQueryParameter" {
			t.Fatalf("PATCH %q = %d %s", q, w.Code, errCode(t, w))
		}
	}
}

type listEntry struct {
	Name          string `json:"name"`
	IsDirectory   string `json:"isDirectory"`
	ContentLength string `json:"contentLength"`
}

func (f *fixture) list(t *testing.T, query string) []listEntry {
	t.Helper()
	w := f.do("GET", "/"+f.ws.ID+"?resource=filesystem"+query, f.token, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list %q = %d %s", query, w.Code, w.Body.Bytes())
	}
	var out struct {
		Paths []listEntry `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("list %q: bad JSON %s", query, w.Body.Bytes())
	}
	return out.Paths
}

func TestList(t *testing.T) {
	f := newFixture(t)
	prefix := "/" + f.ws.ID + "/" + f.it.ID + "/"
	f.do("PUT", prefix+"Files/raw/a.txt", f.token, []byte("hello"))
	f.do("PUT", prefix+"Files/raw/deep/b.txt", f.token, []byte("xy"))
	f.do("PUT", prefix+"Tables/dim?resource=directory", f.token, nil)

	names := func(entries []listEntry) map[string]listEntry {
		m := map[string]listEntry{}
		for _, e := range entries {
			m[e.Name] = e
		}
		return m
	}

	// Top level, non-recursive: items appear as name.Type directories only.
	got := f.list(t, "")
	if len(got) != 1 || got[0].Name != "lake.Lakehouse" || got[0].IsDirectory != "true" {
		t.Fatalf("top-level listing = %+v", got)
	}

	// Recursive spans the whole filesystem with sizes and dir flags.
	m := names(f.list(t, "&recursive=true"))
	if m["lake.Lakehouse"].IsDirectory != "true" ||
		m["lake.Lakehouse/Files/raw/a.txt"].ContentLength != "5" ||
		m["lake.Lakehouse/Files/raw/deep/b.txt"].ContentLength != "2" ||
		m["lake.Lakehouse/Tables/dim"].IsDirectory != "true" {
		t.Fatalf("recursive listing = %+v", m)
	}

	// directory= scopes; non-recursive collapses to first-level directories.
	got = f.list(t, "&directory=lake.Lakehouse/Files&recursive=false")
	if len(got) != 1 || got[0].Name != "lake.Lakehouse/Files/raw" || got[0].IsDirectory != "true" {
		t.Fatalf("collapsed listing = %+v", got)
	}
	m = names(f.list(t, "&directory=lake.Lakehouse/Files/raw"))
	if len(m) != 2 || m["lake.Lakehouse/Files/raw/a.txt"].ContentLength != "5" ||
		m["lake.Lakehouse/Files/raw/deep"].IsDirectory != "true" {
		t.Fatalf("scoped listing = %+v", m)
	}
	m = names(f.list(t, "&directory=lake.Lakehouse/Files&recursive=TRUE")) // EqualFold
	if m["lake.Lakehouse/Files/raw/deep/b.txt"].ContentLength != "2" {
		t.Fatalf("scoped recursive listing = %+v", m)
	}

	// The directory item segment also resolves by GUID.
	m = names(f.list(t, "&directory="+f.it.ID+"/Files&recursive=true"))
	if m[f.it.ID+"/Files/raw/a.txt"].ContentLength != "5" {
		t.Fatalf("GUID-scoped listing = %+v", m)
	}

	// Unknown item in directory= is a DFS 404.
	w := f.do("GET", "/"+f.ws.ID+"?resource=filesystem&directory=nope.Lakehouse", f.token, nil)
	if w.Code != http.StatusNotFound || errCode(t, w) != "PathNotFound" {
		t.Fatalf("unknown directory = %d %s", w.Code, errCode(t, w))
	}

	// An empty filesystem serializes as paths:[] — never null.
	ws2 := &store.Workspace{DisplayName: "empty-ws"}
	if err := f.st.CreateWorkspace(ws2, store.Principal{ID: "admin-1", Type: "ServicePrincipal"}); err != nil {
		t.Fatal(err)
	}
	w = f.do("GET", "/"+ws2.ID+"?resource=filesystem", f.token, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"paths":[]`) {
		t.Fatalf("empty listing = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestResolveWorkspaceAndItem(t *testing.T) {
	f := newFixture(t)

	// GUID and name addressing resolve to the same workspace.
	byID, derr := f.svc.resolveWorkspace(f.ws.ID)
	if derr != nil {
		t.Fatal(derr.msg)
	}
	byName, derr := f.svc.resolveWorkspace("datalake-ws")
	if derr != nil {
		t.Fatal(derr.msg)
	}
	if byID.ID != f.ws.ID || byName.ID != f.ws.ID {
		t.Fatalf("workspace resolution: id %q name %q; want %q", byID.ID, byName.ID, f.ws.ID)
	}
	if _, derr := f.svc.resolveWorkspace("nope"); derr == nil ||
		derr.code != "FilesystemNotFound" || derr.status != http.StatusNotFound {
		t.Fatalf("unknown workspace = %+v", derr)
	}

	// Same for items: GUID and name.Type (case-insensitive type).
	for _, seg := range []string{f.it.ID, "lake.Lakehouse", "lake.lakehouse"} {
		it, derr := f.svc.resolveItem(f.ws.ID, seg)
		if derr != nil || it.ID != f.it.ID {
			t.Fatalf("resolveItem(%q) = %+v, %+v", seg, it, derr)
		}
	}
	for _, seg := range []string{"nope.Lakehouse", "lake.Warehouse", "nodot", ".Lakehouse"} {
		if _, derr := f.svc.resolveItem(f.ws.ID, seg); derr == nil ||
			derr.code != "PathNotFound" || derr.status != http.StatusNotFound {
			t.Fatalf("resolveItem(%q) = %+v; want PathNotFound 404", seg, derr)
		}
	}

	// Through the handler: write by GUID, read back by name — one object.
	f.do("PUT", "/"+f.ws.ID+"/"+f.it.ID+"/Files/raw/a.txt", f.token, []byte("same"))
	w := f.do("GET", "/datalake-ws/lake.Lakehouse/Files/raw/a.txt", f.token, nil)
	if w.Code != http.StatusOK || w.Body.String() != "same" {
		t.Fatalf("name-addressed read = %d %q", w.Code, w.Body.String())
	}
	// Unknown segments surface as DFS-shaped 404s.
	w = f.do("GET", "/nope/x.Lakehouse/Files/a", f.token, nil)
	if w.Code != http.StatusNotFound || errCode(t, w) != "FilesystemNotFound" {
		t.Fatalf("unknown ws via handler = %d %s", w.Code, errCode(t, w))
	}
	w = f.do("GET", "/"+f.ws.ID+"/nope.Lakehouse/Files/a", f.token, nil)
	if w.Code != http.StatusNotFound || errCode(t, w) != "PathNotFound" {
		t.Fatalf("unknown item via handler = %d %s", w.Code, errCode(t, w))
	}
}
