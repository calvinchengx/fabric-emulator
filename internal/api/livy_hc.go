package api

// High Concurrency (HC) Livy sessions — the Fabric-specific layer on top of the
// classic Livy contract. Fabric multiplexes up to five Spark REPLs onto one
// underlying Livy session and packs REPLs that share a `sessionTag`, so
// automation clients get parallel, isolated execution without managing sessions
// by hand. This is Fabric's own management layer (a vanilla Livy server has no
// REPL/HC concept), so the emulator implements it directly rather than proxying:
//
//   POST   …/livyapi/versions/{ver}/highConcurrencySessions            (acquire)
//   GET    …/highConcurrencySessions/{hcId}                            (retrieve)
//   DELETE …/highConcurrencySessions/{hcId}                            (release)
//   POST   …/highConcurrencySessions/{sid}/repls/{replId}/statements   (execute)
//   GET    …/highConcurrencySessions/{sid}/repls/{replId}/statements/{stId}
//
// Acquire/retrieve/release are pure control-plane state (no Spark needed). A
// REPL's statements execute on a real backend Livy session, created lazily on
// first statement; without a backend that step is an honest 501, consistent
// with the classic proxy.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// maxReplsPerLivySession is Fabric's documented cap: up to five REPLs share one
// underlying Livy session before a new one is opened.
const maxReplsPerLivySession = 5

// hcRepl is one acquired HC session: a REPL slot inside a logical Livy session.
type hcRepl struct {
	hcID        string // the HC session id the client holds (response "id")
	replID      string // the REPL id inside the group
	groupID     string // the underlying (logical) Livy session id ("sessionId")
	tag         string
	workspaceID string
	lakehouseID string
	creatorID   string
	createdAt   int64
	createBody  []byte // acquire payload, forwarded when the backend session is opened
	backendID   string // real backend Livy session id (lazy; empty until first statement)
	statements  []*livyStatement // native-agent path: this REPL's executed statements
	deleted     bool
}

// hcGroup is a logical Livy session hosting up to maxReplsPerLivySession REPLs.
type hcGroup struct {
	id    string
	seq   int64  // creation order; lets acquire deterministically prefer the oldest free slot
	scope string // workspace/lakehouse
	tag   string
	repls []string // active hcIDs (len ≤ maxReplsPerLivySession)
}

// hcManager holds HC packing state. Pure in-memory: HC sessions are ephemeral
// and, like classic Livy sessions, are not persisted across restarts.
type hcManager struct {
	mu      sync.Mutex
	byID    map[string]*hcRepl
	groups  map[string]*hcGroup
	nextSeq int64 // monotonic group-creation counter (groups map has no stable order)
}

func newHCManager() *hcManager {
	return &hcManager{byID: map[string]*hcRepl{}, groups: map[string]*hcGroup{}}
}

func (a *API) hcMgr() *hcManager {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.hc == nil {
		a.hc = newHCManager()
	}
	return a.hc
}

// acquire packs a new REPL: reuse a same-tag group with a free slot, else open a
// new logical Livy session. Non-idempotent — every call yields a fresh HC id.
func (m *hcManager) acquire(scope, tag, wid, lid, creator string, now int64, body []byte) *hcRepl {
	m.mu.Lock()
	defer m.mu.Unlock()
	var g *hcGroup
	if tag != "" {
		// Pick the oldest same-scope/tag group with a free slot. Iterating the
		// map alone is order-unstable, so compare by creation seq: with several
		// free groups (e.g. after a release opened a slot in an earlier group
		// while a later spill group also has room) packing stays deterministic.
		for _, cand := range m.groups {
			if cand.scope == scope && cand.tag == tag && len(cand.repls) < maxReplsPerLivySession {
				if g == nil || cand.seq < g.seq {
					g = cand
				}
			}
		}
	}
	if g == nil {
		m.nextSeq++
		g = &hcGroup{id: store.NewID(), seq: m.nextSeq, scope: scope, tag: tag}
		m.groups[g.id] = g
	}
	r := &hcRepl{
		hcID: store.NewID(), replID: store.NewID(), groupID: g.id, tag: tag,
		workspaceID: wid, lakehouseID: lid, creatorID: creator, createdAt: now, createBody: body,
	}
	g.repls = append(g.repls, r.hcID)
	m.byID[r.hcID] = r
	return r
}

func (m *hcManager) get(hcID string) (*hcRepl, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[hcID]
	if !ok || r.deleted {
		return nil, false
	}
	return r, true
}

// replFor resolves the REPL addressed by a statement path — {sid} may be the HC
// id or the group (Livy session) id, and {replId} the REPL within it.
func (m *hcManager) replFor(sid, replID string) (*hcRepl, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[replID] // some clients pass the HC id as the repl id
	if !ok {
		for _, cand := range m.byID {
			if cand.replID == replID {
				r, ok = cand, true
				break
			}
		}
	}
	if !ok || r.deleted || (sid != r.hcID && sid != r.groupID) {
		return nil, false
	}
	return r, true
}

// release removes a REPL, freeing its slot (a later same-tag acquire can repack
// into the group) and dropping the group when it empties. Returns the backend
// session id to tear down, if any.
func (m *hcManager) release(hcID string) (backendID, replID string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[hcID]
	if !ok || r.deleted {
		return "", "", false
	}
	r.deleted = true
	replID = r.replID
	if g := m.groups[r.groupID]; g != nil {
		out := g.repls[:0]
		for _, id := range g.repls {
			if id != hcID {
				out = append(out, id)
			}
		}
		g.repls = out
		if len(g.repls) == 0 {
			delete(m.groups, g.id)
		}
	}
	return r.backendID, replID, true
}

// registerHCLivy mounts the HC routes. They carry a literal `highConcurrencySessions`
// segment, so ServeMux prefers them over the classic `{livypath...}` catch-all.
func (a *API) registerHCLivy(mux *http.ServeMux) {
	const p = "/v1/workspaces/{wid}/lakehouses/{lid}/livyapi/versions/{ver}/highConcurrencySessions"
	mux.HandleFunc("POST "+p, a.withAuth(a.acquireHC))
	mux.HandleFunc("GET "+p+"/{hcid}", a.withAuth(a.getHC))
	mux.HandleFunc("DELETE "+p+"/{hcid}", a.withAuth(a.deleteHC))
	mux.HandleFunc("POST "+p+"/{sid}/repls/{replid}/statements", a.withAuth(a.hcStatement))
	mux.HandleFunc("GET "+p+"/{sid}/repls/{replid}/statements/{stid}", a.withAuth(a.hcStatement))
}

// hcScope guards the shared preconditions: RBAC and that the lakehouse exists.
func (a *API) hcScope(w http.ResponseWriter, r *http.Request, p *auth.Principal, min string) bool {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, min); !ok {
		return false
	}
	if _, err := a.Store.GetItem(wid, r.PathValue("lid")); err != nil {
		writeErr(w, http.StatusNotFound, "LakehouseNotFound", "The lakehouse is not available.")
		return false
	}
	return true
}

func (a *API) acquireHC(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if !a.hcScope(w, r, p, store.RoleContributor) {
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req struct {
		SessionTag string `json:"sessionTag"`
	}
	_ = json.Unmarshal(body, &req)
	wid, lid := r.PathValue("wid"), r.PathValue("lid")
	repl := a.hcMgr().acquire(wid+"/"+lid, req.SessionTag, wid, lid, p.ID, a.Store.Now(), body)
	writeJSON(w, http.StatusOK, a.hcBody(repl))
}

func (a *API) getHC(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if !a.hcScope(w, r, p, store.RoleViewer) {
		return
	}
	repl, ok := a.hcMgr().get(r.PathValue("hcid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "HighConcurrencySessionNotFound", "No such high concurrency session.")
		return
	}
	writeJSON(w, http.StatusOK, a.hcBody(repl))
}

func (a *API) deleteHC(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if !a.hcScope(w, r, p, store.RoleContributor) {
		return
	}
	backendID, replID, ok := a.hcMgr().release(r.PathValue("hcid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "HighConcurrencySessionNotFound", "No such high concurrency session.")
		return
	}
	// Native path: drop the REPL's agent namespace. Proxy path: best-effort
	// teardown of the real backend session, if one was opened.
	if a.livyAgent != nil {
		_, _ = a.agentPost("/close", map[string]any{"session": replID})
	} else if backendID != "" && a.livyBackend != nil {
		u := *a.livyBackend
		u.Path = strings.TrimRight(u.Path, "/") + "/sessions/" + backendID
		if req, err := http.NewRequest(http.MethodDelete, u.String(), nil); err == nil {
			if resp, err := a.hcClient().Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// hcStatement executes (POST) or reads (GET) a statement in a REPL by proxying
// to the REPL's real backend Livy session — opened lazily on first execute.
func (a *API) hcStatement(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	min := store.RoleViewer
	if r.Method == http.MethodPost {
		min = store.RoleContributor
	}
	if !a.hcScope(w, r, p, min) {
		return
	}
	repl, ok := a.hcMgr().replFor(r.PathValue("sid"), r.PathValue("replid"))
	if !ok {
		writeErr(w, http.StatusNotFound, "ReplNotFound", "No such REPL in this high concurrency session.")
		return
	}
	// Native path: each REPL is its own namespace in the Spark agent — the HC
	// packing model (up to 5 REPLs sharing one Spark session, isolated) made
	// real. Statements are computed by real Spark; no external Livy server.
	if a.livyAgent != nil {
		a.hcStatementNative(w, r, repl)
		return
	}
	if a.livy == nil {
		writeErr(w, http.StatusNotImplemented, "SparkBackendNotConfigured",
			"No Spark/Livy backend is configured; set --spark-agent-url (native) or --spark-livy-url (proxy) to run Spark for real.")
		return
	}
	if r.Method == http.MethodPost {
		if err := a.ensureBackendSession(repl); err != nil {
			writeErr(w, http.StatusBadGateway, "SparkBackendError", err.Error())
			return
		}
	}
	if repl.backendID == "" { // GET before any statement was ever submitted
		writeErr(w, http.StatusNotFound, "StatementNotFound", "No statement has been submitted to this REPL.")
		return
	}
	// Rewrite to the classic Livy statements path on the REPL's backend session;
	// the proxy director prepends the backend base path.
	path := "/sessions/" + repl.backendID + "/statements"
	if stid := r.PathValue("stid"); stid != "" {
		path += "/" + stid
	}
	r.URL.Path = path
	r.URL.RawPath = ""
	a.livy.ServeHTTP(w, r)
}

// hcStatementNative runs (POST) or reads (GET) a statement in the REPL's agent
// namespace — real Spark, one namespace per REPL for isolation.
func (a *API) hcStatementNative(w http.ResponseWriter, r *http.Request, repl *hcRepl) {
	m := a.hcMgr()
	if r.Method == http.MethodPost {
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		out, err := a.agentPost("/statements", map[string]any{"session": repl.replID, "code": body.Code})
		if err != nil {
			writeErr(w, http.StatusBadGateway, "SparkAgentError", err.Error())
			return
		}
		m.mu.Lock()
		st := &livyStatement{ID: len(repl.statements), State: "available", Output: out}
		repl.statements = append(repl.statements, st)
		m.mu.Unlock()
		writeJSON(w, http.StatusCreated, st)
		return
	}
	n, err := strconv.Atoi(r.PathValue("stid"))
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil || n < 0 || n >= len(repl.statements) {
		writeErr(w, http.StatusNotFound, "StatementNotFound", "No such statement.")
		return
	}
	writeJSON(w, http.StatusOK, repl.statements[n])
}

// ensureBackendSession opens the REPL's real Livy session once, forwarding the
// acquire payload as the session-create body (Livy ignores the HC-only fields).
func (a *API) ensureBackendSession(repl *hcRepl) error {
	m := a.hcMgr()
	m.mu.Lock()
	defer m.mu.Unlock()
	if repl.backendID != "" {
		return nil
	}
	u := *a.livyBackend
	u.Path = strings.TrimRight(u.Path, "/") + "/sessions"
	resp, err := a.hcClient().Post(u.String(), "application/json", bytes.NewReader(repl.createBody))
	if err != nil {
		return fmt.Errorf("opening backend Livy session: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("backend Livy session create returned %d", resp.StatusCode)
	}
	var out struct {
		ID json.Number `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == "" {
		return fmt.Errorf("backend Livy session response had no id")
	}
	repl.backendID = out.ID.String()
	return nil
}

// hcBody is the HighConcurrencySessionResponse wire shape.
func (a *API) hcBody(r *hcRepl) map[string]any {
	var tag any
	if r.tag != "" {
		tag = r.tag
	}
	return map[string]any{
		"id":                     r.hcID,
		"state":                  "Idle",
		"fabricSessionStateInfo": map[string]any{"state": "Idle", "errorMessage": nil},
		"sessionId":              r.groupID,
		"workspaceId":            r.workspaceID,
		"artifactId":             r.lakehouseID,
		"creatorId":              r.creatorID,
		"createdAt":              time.Unix(r.createdAt, 0).UTC().Format(time.RFC3339),
		"replId":                 r.replID,
		"sessionTag":             tag,
	}
}
