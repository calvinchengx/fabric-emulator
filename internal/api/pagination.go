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
// Pagination is **opt-in**: without a `?maxPageSize` the full set is returned and
// no token is emitted, so existing callers and an empty list still serialize as
// `{"value":[]}`. With `?maxPageSize=N`, at most N items are returned and, when
// more remain, a `continuationToken` (an opaque offset cursor) and a
// `continuationUri` are included; the client passes the token back via
// `?continuationToken` to fetch the next page.
//
// **Known divergence, deliberately kept.** Real Fabric paginates on its own
// schedule — its List Items reference documents `continuationToken` as a
// *response* field with no client-supplied page-size parameter, so a real client
// must always be prepared to follow a token. Here the emulator returns
// everything in one response unless asked to page, which means a client that
// ignores `continuationToken` passes locally and could still break against
// Fabric. Paginating by default would be more faithful, but it would change the
// response of every list endpoint for every existing caller and test, so the
// choice is recorded rather than made silently (docs/parity.md, list
// pagination).
func writePage[T any](w http.ResponseWriter, r *http.Request, items []T) {
	writePageKeyed(w, r, "value", items)
}

// writePageKeyed is writePage with a caller-chosen envelope key. The admin
// list APIs name their array after the resource (`workspaces`) rather than
// using `value`, per the REST reference.
func writePageKeyed[T any](w http.ResponseWriter, r *http.Request, key string, items []T) {
	offset := 0
	if tok := r.URL.Query().Get("continuationToken"); tok != "" {
		if n := decodePageToken(tok); n > 0 {
			offset = n
		}
	}
	if offset > len(items) {
		offset = len(items)
	}
	end := len(items)
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
