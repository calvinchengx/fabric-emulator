package server_test

// P3 e2e: the OneLake data plane. Host-routed (onelake.dfs.fabric.microsoft.com),
// Storage-audience tokens only, ADLS-Gen2 wire subset, and the managed-folder
// rules from onelake-api-parity.md.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
)

const oneLakeHost = "onelake.dfs.fabric.microsoft.com"

// forgeToken mints an arbitrary token via entra's forge.
func (f *fixture) forgeToken(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := f.emu.HTTPClient().Post(f.emu.Origin+"/admin/api/tokens", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Token       string `json:"token"`
	}
	rawResp, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(rawResp, &tok)
	if tok.AccessToken != "" {
		return tok.AccessToken
	}
	if tok.Token == "" {
		t.Fatalf("forge returned no token: %s", rawResp)
	}
	return tok.Token
}

// ol performs a OneLake request (Host-routed to the data plane).
func (f *fixture) ol(t *testing.T, method, path, token string, body []byte) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, f.fabric.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = oneLakeHost
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := f.fabric.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp
}

func olStatus(t *testing.T, resp *http.Response, want int, ctx string) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s: status %d; want %d — %s", ctx, resp.StatusCode, want, body)
	}
}

func TestOneLakeDataPlane(t *testing.T) {
	f := newFixture(t)

	// Control plane: a workspace and a lakehouse.
	var ws struct{ ID string }
	f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "datalake-ws"}, &ws)
	var lake struct{ ID string }
	f.call("POST", "/v1/workspaces/"+ws.ID+"/lakehouses", f.token, map[string]any{"displayName": "lake"}, &lake)

	// Storage-audience token for the same daemon SP (app-only, via the forge —
	// the platform mints it; there is no storage app registration).
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})

	// Audience walls, both directions.
	olStatus(t, f.ol(t, "HEAD", "/", f.token, nil), http.StatusUnauthorized, "fabric token on onelake")
	f.mustStatus(f.call("GET", "/v1/workspaces", storage, nil, nil), http.StatusUnauthorized, "storage token on control plane")

	// Account level: HEAD only.
	olStatus(t, f.ol(t, "HEAD", "/", storage, nil), http.StatusOK, "account HEAD")
	olStatus(t, f.ol(t, "PUT", "/", storage, nil), http.StatusBadRequest, "account PUT")

	// Managed-folder walls: workspace, item root, first level.
	olStatus(t, f.ol(t, "PUT", "/"+ws.ID, storage, nil), http.StatusConflict, "create filesystem")
	olStatus(t, f.ol(t, "DELETE", "/"+ws.ID+"/"+lake.ID, storage, nil), http.StatusConflict, "delete item root")
	olStatus(t, f.ol(t, "PUT", "/"+ws.ID+"/"+lake.ID+"/Files?resource=directory", storage, nil),
		http.StatusConflict, "create first-level dir")
	olStatus(t, f.ol(t, "HEAD", "/"+ws.ID+"/"+lake.ID+"/Files", storage, nil), http.StatusOK, "HEAD managed folder")

	// Rejected query param + ignored-but-echoed header.
	olStatus(t, f.ol(t, "PATCH", "/"+ws.ID+"/"+lake.ID+"/Files/x?action=setAccessControl", storage, nil),
		http.StatusBadRequest, "setAccessControl")
	req, _ := http.NewRequest("HEAD", f.fabric.URL+"/", nil)
	req.Host = oneLakeHost
	req.Header.Set("Authorization", "Bearer "+storage)
	req.Header.Set("x-ms-owner", "someone")
	resp, err := f.fabric.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("x-ms-rejected-headers"), "x-ms-owner") {
		t.Fatalf("x-ms-rejected-headers = %q", resp.Header.Get("x-ms-rejected-headers"))
	}

	// Write flow (GUID addressing): create → append → append → flush.
	base := "/" + ws.ID + "/" + lake.ID + "/Files/raw/a.txt"
	olStatus(t, f.ol(t, "PUT", base+"?resource=file", storage, nil), http.StatusCreated, "create file")
	olStatus(t, f.ol(t, "PATCH", base+"?action=append&position=0", storage, []byte("hello ")), http.StatusAccepted, "append 1")
	olStatus(t, f.ol(t, "PATCH", base+"?action=append&position=6", storage, []byte("world")), http.StatusAccepted, "append 2")
	olStatus(t, f.ol(t, "PATCH", base+"?action=append&position=3", storage, []byte("x")), http.StatusBadRequest, "bad position")
	olStatus(t, f.ol(t, "PATCH", base+"?action=flush&position=11", storage, nil), http.StatusOK, "flush")
	olStatus(t, f.ol(t, "PATCH", base+"?action=flush&position=5", storage, nil), http.StatusBadRequest, "bad flush")

	// Read back via NAME addressing — both address forms hit the same item.
	named := "/datalake-ws/lake.Lakehouse/Files/raw/a.txt"
	resp = f.ol(t, "GET", named, storage, nil)
	olStatus(t, resp, http.StatusOK, "read by name")
	content, _ := io.ReadAll(resp.Body)
	if string(content) != "hello world" {
		t.Fatalf("content = %q", content)
	}
	// HEAD: canned permission headers + length.
	resp = f.ol(t, "HEAD", base, storage, nil)
	olStatus(t, resp, http.StatusOK, "HEAD file")
	if resp.Header.Get("x-ms-owner") != "$superuser" || resp.Header.Get("x-ms-permissions") != "---------" ||
		resp.Header.Get("Content-Length") != "11" || resp.Header.Get("x-ms-resource-type") != "file" {
		t.Fatalf("HEAD headers = owner:%q perms:%q len:%q type:%q", resp.Header.Get("x-ms-owner"),
			resp.Header.Get("x-ms-permissions"), resp.Header.Get("Content-Length"), resp.Header.Get("x-ms-resource-type"))
	}

	// Listing: recursive across the filesystem, then scoped + collapsed.
	var listing struct {
		Paths []struct {
			Name          string `json:"name"`
			IsDirectory   string `json:"isDirectory"`
			ContentLength string `json:"contentLength"`
		} `json:"paths"`
	}
	resp = f.ol(t, "GET", "/"+ws.ID+"?resource=filesystem&recursive=true", storage, nil)
	olStatus(t, resp, http.StatusOK, "list recursive")
	_ = json.NewDecoder(resp.Body).Decode(&listing)
	var haveItem, haveFile bool
	for _, p := range listing.Paths {
		if p.Name == "lake.Lakehouse" && p.IsDirectory == "true" {
			haveItem = true
		}
		if p.Name == "lake.Lakehouse/Files/raw/a.txt" && p.ContentLength == "11" {
			haveFile = true
		}
	}
	if !haveItem || !haveFile {
		t.Fatalf("recursive listing = %+v", listing.Paths)
	}
	resp = f.ol(t, "GET", "/"+ws.ID+"?resource=filesystem&directory=lake.Lakehouse/Files&recursive=false", storage, nil)
	olStatus(t, resp, http.StatusOK, "list dir")
	listing.Paths = nil
	_ = json.NewDecoder(resp.Body).Decode(&listing)
	if len(listing.Paths) != 1 || listing.Paths[0].Name != "lake.Lakehouse/Files/raw" || listing.Paths[0].IsDirectory != "true" {
		t.Fatalf("collapsed listing = %+v", listing.Paths)
	}

	// RBAC on the data plane: ungranted principal 403; Viewer reads, no writes.
	aliceStorage := f.forgeToken(t, map[string]any{
		"userId": entra.AliceOID, "audience": "https://storage.azure.com",
	})
	olStatus(t, f.ol(t, "GET", named, aliceStorage, nil), http.StatusForbidden, "ungranted read")
	f.call("POST", "/v1/workspaces/"+ws.ID+"/roleAssignments", f.token,
		map[string]any{"principal": map[string]string{"id": entra.AliceOID, "type": "User"}, "role": "Viewer"}, nil)
	olStatus(t, f.ol(t, "GET", named, aliceStorage, nil), http.StatusOK, "viewer read")
	olStatus(t, f.ol(t, "PUT", "/"+ws.ID+"/"+lake.ID+"/Files/raw/b.txt?resource=file", aliceStorage, nil),
		http.StatusForbidden, "viewer write")

	// Delete a directory removes its subtree.
	olStatus(t, f.ol(t, "DELETE", "/"+ws.ID+"/"+lake.ID+"/Files/raw", storage, nil), http.StatusOK, "delete dir")
	olStatus(t, f.ol(t, "GET", base, storage, nil), http.StatusNotFound, "file gone with dir")

	// Unknown workspace/item 404.
	olStatus(t, f.ol(t, "GET", "/nope/x.Lakehouse/Files/a", storage, nil), http.StatusNotFound, "unknown ws")
	olStatus(t, f.ol(t, "GET", "/"+ws.ID+"/nope.Lakehouse/Files/a", storage, nil), http.StatusNotFound, "unknown item")
}
