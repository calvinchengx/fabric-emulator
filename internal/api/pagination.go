package api

import (
	"encoding/base64"
	"net/http"
	"strconv"
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
func writePage[T any](w http.ResponseWriter, r *http.Request, items []T) {
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
	resp := map[string]any{"value": page}
	if end < len(items) {
		tok := encodePageToken(end)
		resp["continuationToken"] = tok
		q := r.URL.Query()
		q.Set("continuationToken", tok)
		resp["continuationUri"] = r.URL.Path + "?" + q.Encode()
	}
	writeJSON(w, http.StatusOK, resp)
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
