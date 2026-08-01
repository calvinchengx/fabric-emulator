package api

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// TestListPagination drives the continuation-token contract end to end through
// the real list handler: without maxPageSize the full set comes back with no
// token (unchanged); with maxPageSize the caller pages through, and the pages
// partition the set exactly — every item once, none dropped or duplicated.
func TestListPagination(t *testing.T) {
	a, st := newAPI(t)
	const total = 5
	for i := 0; i < total; i++ {
		ws := &store.Workspace{DisplayName: "w" + strconv.Itoa(i)}
		if err := st.CreateWorkspace(ws, store.Principal{ID: admin.ID, Type: admin.Type}); err != nil {
			t.Fatal(err)
		}
	}

	get := func(query string) map[string]any {
		r := httptest.NewRequest("GET", "/x?"+query, nil)
		w := httptest.NewRecorder()
		a.listWorkspaces(w, r, admin)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	// Default: full set, no continuation token.
	full := get("")
	if n := len(full["value"].([]any)); n != total {
		t.Fatalf("default list = %d, want %d", n, total)
	}
	if _, ok := full["continuationToken"]; ok {
		t.Fatal("default (unpaginated) list must not carry a continuationToken")
	}

	// Page through in size-2 pages; the pages must partition the set exactly.
	seen := map[string]bool{}
	query := "maxPageSize=2"
	pages := 0
	for {
		body := get(query)
		page := body["value"].([]any)
		if len(page) > 2 {
			t.Fatalf("page size = %d, want ≤ 2", len(page))
		}
		for _, it := range page {
			id := it.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("duplicate id %s across pages", id)
			}
			seen[id] = true
		}
		pages++
		if pages > total+2 {
			t.Fatal("pagination did not terminate")
		}
		tok, ok := body["continuationToken"].(string)
		if !ok {
			// Last page carries no token; a continuationUri only appears with one.
			if _, hasURI := body["continuationUri"]; hasURI {
				t.Fatal("last page should carry no continuationUri")
			}
			break
		}
		if body["continuationUri"] == nil {
			t.Fatal("a page with a continuationToken must include a continuationUri")
		}
		query = "maxPageSize=2&continuationToken=" + tok
	}
	if len(seen) != total {
		t.Fatalf("paged total = %d, want %d", len(seen), total)
	}
	if pages != 3 { // 2 + 2 + 1
		t.Fatalf("pages = %d, want 3", pages)
	}
}

// TestPageTokenDecodeGarbage: a malformed continuation token is treated as
// offset 0 (a fresh page), not a crash.
func TestPageTokenDecodeGarbage(t *testing.T) {
	if got := decodePageToken("!!!not-base64!!!"); got != 0 {
		t.Errorf("garbage token decoded to %d, want 0", got)
	}
	if got := decodePageToken(encodePageToken(7)); got != 7 {
		t.Errorf("round-trip token = %d, want 7", got)
	}
}

// Real Fabric returns continuationUri as a fully-qualified URL — its reference
// shows `https://api.fabric.microsoft.com/v1/workspaces/…/items?continuationToken=…`
// — not a bare path. A client that treats the field as a URL (which is what the
// name says) must be able to request it directly, so the shape has to match.
func TestContinuationUriIsAbsolute(t *testing.T) {
	items := []map[string]string{{"id": "1"}, {"id": "2"}, {"id": "3"}}

	for _, tc := range []struct {
		name, host, wantPrefix string
		tls                    bool
		fwdProto               string
	}{
		{name: "plain http", host: "localhost:9443", wantPrefix: "http://localhost:9443/v1/items?"},
		{name: "tls", host: "api.fabric.microsoft.com", tls: true,
			wantPrefix: "https://api.fabric.microsoft.com/v1/items?"},
		{name: "behind a TLS-terminating proxy", host: "api.fabric.microsoft.com",
			fwdProto: "https", wantPrefix: "https://api.fabric.microsoft.com/v1/items?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/items?maxPageSize=2", nil)
			r.Host = tc.host
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if tc.fwdProto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.fwdProto)
			}
			w := httptest.NewRecorder()
			writePage(&API{}, w, r, items)

			var body struct {
				ContinuationToken string `json:"continuationToken"`
				ContinuationUri   string `json:"continuationUri"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.ContinuationToken == "" {
				t.Fatal("no continuationToken emitted")
			}
			if !strings.HasPrefix(body.ContinuationUri, tc.wantPrefix) {
				t.Fatalf("continuationUri = %q, want prefix %q", body.ContinuationUri, tc.wantPrefix)
			}
			// It must parse as an absolute URL carrying the token back.
			u, err := url.Parse(body.ContinuationUri)
			if err != nil || !u.IsAbs() {
				t.Fatalf("not an absolute URL: %q (%v)", body.ContinuationUri, err)
			}
			if got := u.Query().Get("continuationToken"); got != body.ContinuationToken {
				t.Fatalf("uri token %q != body token %q", got, body.ContinuationToken)
			}
		})
	}
}

// A request with no Host cannot be qualified; emitting a wrong host would be
// worse than emitting the path.
func TestContinuationUriFallsBackWithoutHost(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/items?maxPageSize=1", nil)
	r.Host = ""
	if got := absoluteURI(r, "/v1/items?x=1"); got != "/v1/items?x=1" {
		t.Fatalf("got %q", got)
	}
}

// Pagination is on by default, as it is in real Fabric: a list longer than the
// server's page size comes back with a token, with no client opt-in.
func TestPaginationIsOnByDefault(t *testing.T) {
	items := make([]map[string]string, 5)
	for i := range items {
		items[i] = map[string]string{"id": strconv.Itoa(i)}
	}
	a := &API{ListPageSize: 2} // the testing lever: force small pages

	seen, pages, query := map[string]bool{}, 0, ""
	for {
		r := httptest.NewRequest("GET", "/v1/items"+query, nil)
		r.Host = "api.fabric.microsoft.com"
		w := httptest.NewRecorder()
		writePage(a, w, r, items)
		pages++

		var body struct {
			Value             []map[string]string `json:"value"`
			ContinuationToken string              `json:"continuationToken"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Value) > 2 {
			t.Fatalf("page %d returned %d items, over the page size", pages, len(body.Value))
		}
		for _, it := range body.Value {
			if seen[it["id"]] {
				t.Fatalf("item %s returned on more than one page", it["id"])
			}
			seen[it["id"]] = true
		}
		if body.ContinuationToken == "" {
			break
		}
		query = "?continuationToken=" + body.ContinuationToken
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 || len(seen) != 5 {
		t.Fatalf("got %d pages / %d items, want 3 / 5", pages, len(seen))
	}
}

// The default page size leaves everyday lists whole, so ordinary callers see no
// behaviour change — the contract is faithful without the path being noisy.
func TestDefaultPageSizeLeavesSmallListsWhole(t *testing.T) {
	items := make([]map[string]string, DefaultListPageSize)
	for i := range items {
		items[i] = map[string]string{"id": strconv.Itoa(i)}
	}
	r := httptest.NewRequest("GET", "/v1/items", nil)
	w := httptest.NewRecorder()
	writePage(&API{}, w, r, items) // ListPageSize 0 => DefaultListPageSize

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, paged := body["continuationToken"]; paged {
		t.Fatal("a list at exactly the page size should come back whole")
	}
	if n := len(body["value"].([]any)); n != DefaultListPageSize {
		t.Fatalf("got %d items, want %d", n, DefaultListPageSize)
	}
}

// A negative size opts out of paging entirely.
func TestNegativePageSizeDisablesPaging(t *testing.T) {
	items := make([]map[string]string, 50)
	r := httptest.NewRequest("GET", "/v1/items", nil)
	w := httptest.NewRecorder()
	writePage(&API{ListPageSize: -1}, w, r, items)

	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	if _, paged := body["continuationToken"]; paged {
		t.Fatal("paging was disabled but a token was emitted")
	}
}

// maxPageSize may narrow a page but never widen it past the server's limit.
func TestMaxPageSizeCannotExceedTheServerPageSize(t *testing.T) {
	items := make([]map[string]string, 20)
	for i := range items {
		items[i] = map[string]string{"id": strconv.Itoa(i)}
	}
	a := &API{ListPageSize: 3}

	r := httptest.NewRequest("GET", "/v1/items?maxPageSize=100", nil)
	w := httptest.NewRecorder()
	writePage(a, w, r, items)
	var body struct {
		Value []map[string]string `json:"value"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Value) != 3 {
		t.Fatalf("maxPageSize widened the page to %d, want the server's 3", len(body.Value))
	}
}

// A token pointing past the end (a stale cursor, or a list that shrank between
// pages) yields an empty final page rather than a panic or a wrapped offset.
func TestTokenBeyondTheEndYieldsAnEmptyPage(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/items?continuationToken="+encodePageToken(99), nil)
	w := httptest.NewRecorder()
	writePage(&API{ListPageSize: 2}, w, r, []map[string]string{{"id": "1"}})

	var body struct {
		Value             []map[string]string `json:"value"`
		ContinuationToken string              `json:"continuationToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Value) != 0 || body.ContinuationToken != "" {
		t.Fatalf("got %d items / token %q, want an empty terminal page", len(body.Value), body.ContinuationToken)
	}
}

// A malformed or negative token is treated as "start from the beginning"
// rather than trusted — it arrives from the client and cannot be assumed sane.
func TestMalformedTokenRestartsFromTheBeginning(t *testing.T) {
	for _, tok := range []string{"not-base64!!", encodePageToken(-5), ""} {
		if got := decodePageToken(tok); got != 0 {
			t.Fatalf("decodePageToken(%q) = %d, want 0", tok, got)
		}
	}
}
