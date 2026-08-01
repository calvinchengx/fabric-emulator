package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
)

// writePage writes a Fabric-shaped list response — `{"value":[...]}` — with
// optional continuation-token pagination (the shape real Fabric list APIs use).
//
// Pagination is **on by default**, as it is in real Fabric: a list longer than
// the server's page size comes back with a `continuationToken` (an opaque
// offset cursor) and a `continuationUri`, and the client passes the token back
// via `?continuationToken` for the next page. Fabric's List Items reference
// documents the token as a *response* field with no client-supplied page-size
// parameter, so a correct client must always be prepared to follow one.
//
// `?maxPageSize=N` narrows the page further when a caller wants smaller pages;
// it can only reduce the page, never raise it past the server's size.
//
// # The page size is a testing lever
//
// DefaultListPageSize mirrors a realistic Fabric page: large enough that
// ordinary use never sees a token. That makes the *contract* faithful but the
// *path* rare — and a pagination bug in a client is exactly the kind that hides
// until production data grows. So the size is configurable
// (`-list-page-size` / `FABRIC_LIST_PAGE_SIZE`): set it to 1 or 2 and every
// list forces the client through the token loop on the spot. Same idea as the
// controllable clock for LROs and the fault injector for failures — make the
// rare condition reproducible on demand rather than hoping to meet it.
//
// A size of 0 or less disables paging entirely (the pre-default behaviour), for
// a caller that needs whole lists.
// DefaultListPageSize is the server's page size when none is configured —
// large enough that everyday lists return whole, as Fabric's do.
const DefaultListPageSize = 100

func writePage[T any](a *API, w http.ResponseWriter, r *http.Request, items []T) {
	writePageKeyed(a, w, r, "value", items)
}

// writePageKeyed is writePage with a caller-chosen envelope key. The admin
// list APIs name their array after the resource (`workspaces`) rather than
// using `value`, per the REST reference.
func writePageKeyed[T any](a *API, w http.ResponseWriter, r *http.Request, key string, items []T) {
	offset := 0
	if tok := r.URL.Query().Get("continuationToken"); tok != "" {
		if n := decodePageToken(tok); n > 0 {
			offset = n
		}
	}
	if offset > len(items) {
		offset = len(items)
	}
	// The server's page size applies unless disabled; a client may ask for a
	// smaller page but never a larger one, since the limit is the server's.
	end := len(items)
	if size := a.pageSize(); size > 0 && offset+size < end {
		end = offset + size
	}
	if ms := r.URL.Query().Get("maxPageSize"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n > 0 && offset+n < end {
			end = offset + n
		}
	}
	page := items[offset:end]
	if page == nil {
		page = []T{}
	}
	resp := map[string]any{key: page}
	if end < len(items) {
		tok := encodePageToken(end)
		resp["continuationToken"] = tok
		q := r.URL.Query()
		q.Set("continuationToken", tok)
		resp["continuationUri"] = absoluteURI(r, r.URL.Path+"?"+q.Encode())
	}
	writeJSON(w, http.StatusOK, resp)
}

// absoluteURI renders a path+query as the fully-qualified URL real Fabric
// returns — its reference shows
// `https://api.fabric.microsoft.com/v1/workspaces/…/items?continuationToken=…`,
// not a bare path. A client that treats continuationUri as a URL (the obvious
// reading, and what the field name says) must be able to request it directly.
//
// The scheme follows the connection the request arrived on, so the emulator's
// TLS and -disable-tls modes each advertise a URI that actually works, and the
// host is echoed from the request rather than assumed — clients reach this
// server under several names (api.fabric.microsoft.com, localhost:9443, a
// compose service).
func absoluteURI(r *http.Request, pathAndQuery string) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	if r.Host == "" {
		return pathAndQuery // nothing to qualify it with; better than a wrong host
	}
	return scheme + "://" + r.Host + pathAndQuery
}

// encodePageToken/decodePageToken carry the next-item offset as an opaque token
// (base64url of the decimal offset) — clients treat it as opaque, as with Fabric.
func encodePageToken(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodePageToken(tok string) int {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// pageSize is the configured server page size, defaulting when unset. A
// negative value means "never page".
func (a *API) pageSize() int {
	if a.ListPageSize < 0 {
		return 0
	}
	if a.ListPageSize == 0 {
		return DefaultListPageSize
	}
	return a.ListPageSize
}
