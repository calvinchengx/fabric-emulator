package api

// The admin activity (audit) log:
//
//	GET /v1.0/myorg/admin/activityevents?startDateTime='…'&endDateTime='…'
//	GET /v1.0/myorg/admin/activityevents?continuationToken='…'
//
// Shape and rules are the documented ones (fabric-docs
// enterprise/powerbi/service-admin-auditing.md): the DateTime values are
// single-quoted UTC, both must fall on the same day, and a large result set
// is paged with a continuationToken until `continuationUri` comes back null.
//
// The events are real: the emulator records them as operations happen, using
// the documented audit vocabulary. Nothing here is synthesised at read time.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// activityPageSize is how many entries one page carries. Real Fabric returns
// "around 5,000 to 10,000"; the emulator uses a small page so tests (and
// humans) actually exercise the continuation loop rather than never seeing it.
const activityPageSize = 200

func (a *API) registerActivityEvents(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1.0/myorg/admin/activityevents", a.withAuth(a.listActivityEvents))
}

// unquote strips the single quotes the API's DateTime and token parameters
// are documented to carry. Callers that omit them still work.
func unquote(s string) string { return strings.Trim(s, "'") }

// parseActivityTime accepts the documented UTC forms, with or without the
// trailing Z.
func parseActivityTime(raw string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02T15:04:05Z", "2006-01-02T15:04:05", time.RFC3339,
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// activityToken carries the query across pages. Real Fabric's token is
// opaque; so is this one (base64url of the window plus the next offset), and
// clients are documented to echo it back untouched.
type activityToken struct {
	From   int64 `json:"f"`
	To     int64 `json:"t"`
	Offset int   `json:"o"`
}

func encodeActivityToken(tok activityToken) string {
	raw, _ := json.Marshal(tok)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeActivityToken(s string) (activityToken, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return activityToken{}, false
	}
	var tok activityToken
	if json.Unmarshal(raw, &tok) != nil {
		return activityToken{}, false
	}
	return tok, true
}

// listActivityEvents serves one page of the audit log.
func (a *API) listActivityEvents(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	q := r.URL.Query()
	var tok activityToken

	if raw := unquote(q.Get("continuationToken")); raw != "" {
		var ok bool
		if tok, ok = decodeActivityToken(raw); !ok {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"continuationToken is not a token this API issued.")
			return
		}
	} else {
		start, startOK := parseActivityTime(unquote(q.Get("startDateTime")))
		end, endOK := parseActivityTime(unquote(q.Get("endDateTime")))
		if !startOK || !endOK {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"startDateTime and endDateTime are required, single-quoted UTC DateTime values.")
			return
		}
		if end.Before(start) {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"endDateTime must not precede startDateTime.")
			return
		}
		// Documented limit: one day of data per request, so both bounds must
		// name the same UTC day.
		if start.Format("2006-01-02") != end.Format("2006-01-02") {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"startDateTime and endDateTime must specify the same day.")
			return
		}
		tok = activityToken{From: start.Unix(), To: end.Unix()}
	}

	// Fetch one extra row: its presence is what says another page exists,
	// without a second COUNT query that could race an in-flight write.
	evs, err := a.Store.ActivityEvents(tok.From, tok.To, tok.Offset, activityPageSize+1)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	more := len(evs) > activityPageSize
	if more {
		evs = evs[:activityPageSize]
	}

	entities := make([]map[string]any, 0, len(evs))
	for _, ev := range evs {
		entities = append(entities, activityEntity(ev))
	}
	body := map[string]any{
		"activityEventEntities": entities,
		"continuationUri":       nil,
		"continuationToken":     nil,
		// Mirrors the documented sample, which reports false on both the
		// page that has a token and the empty final page.
		"lastResultSet": false,
	}
	if more {
		next := encodeActivityToken(activityToken{From: tok.From, To: tok.To, Offset: tok.Offset + len(evs)})
		body["continuationToken"] = next
		body["continuationUri"] = activityContinuationURI(r, next)
	}
	writeJSON(w, http.StatusOK, body)
}

// activityContinuationURI rebuilds this endpoint's URL with the token, the
// way the documented response does — derived from the caller's own request so
// it is reachable by whatever host reached us.
func activityContinuationURI(r *http.Request, token string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host + r.URL.Path +
		"?continuationToken=" + url.QueryEscape("'"+token+"'")
}

// activityEntity renders one audit record. The documented per-operation
// operationProperties are merged in at the top level, which is where the
// audit schema puts them.
func activityEntity(ev *store.ActivityEvent) map[string]any {
	e := map[string]any{
		"Id":           ev.ID,
		"CreationTime": ev.UTCTime(),
		"Operation":    ev.Operation,
		"UserId":       ev.UserID,
		"UserType":     ev.UserType,
	}
	if ev.WorkspaceID != "" {
		e["WorkspaceId"] = ev.WorkspaceID
	}
	if ev.ArtifactID != "" {
		e["ArtifactId"] = ev.ArtifactID
	}
	if ev.ArtifactName != "" {
		e["ArtifactName"] = ev.ArtifactName
	}
	for k, v := range ev.Properties {
		e[k] = v
	}
	return e
}

// audit records one event, best-effort: a failure to write the audit log must
// never fail the operation the caller actually asked for.
func (a *API) audit(p *auth.Principal, ev *store.ActivityEvent) {
	if p != nil {
		ev.UserID = p.ID
		ev.UserType = p.Type
	}
	_ = a.Store.RecordActivity(ev)
}
