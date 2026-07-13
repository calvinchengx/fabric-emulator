package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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
