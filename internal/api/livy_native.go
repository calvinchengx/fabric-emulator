package api

// Native Livy termination. Instead of reverse-proxying to an (retired) Apache
// Livy server, the emulator implements the Livy REST session/statement contract
// itself and drives a Spark statement-executor agent (e2e/livy/agent.py) — the
// same stance as the HC packing layer and the TDS warehouse: speak the wire
// protocol, back it with a real engine. A real Livy client (pylivy, sparkmagic,
// plain requests) creates a session, submits statements, and gets results
// computed by real Spark, unmodified.
//
// Interactive path only (sessions + statements); batches still proxy. Statements
// execute synchronously against the agent and are recorded "available", which
// polling Livy clients handle transparently.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// SetLivyAgent points the native Livy layer at a Spark statement-executor agent
// (empty disables it, falling back to the proxy/501).
func (a *API) SetLivyAgent(rawURL string) error {
	if rawURL == "" {
		a.livyAgent = nil
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	a.livyAgent = u
	return nil
}

type livyStatement struct {
	ID     int            `json:"id"`
	State  string         `json:"state"`
	Output map[string]any `json:"output,omitempty"`
}

type livySession struct {
	ID         int
	Kind       string
	statements []*livyStatement
}

// livyManager holds native Livy session state (in-memory; sessions are
// ephemeral, like a real Livy server's).
type livyManager struct {
	mu       sync.Mutex
	sessions map[int]*livySession
	nextID   int
}

func (a *API) livyMgr() *livyManager {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.livyNativeState == nil {
		a.livyNativeState = &livyManager{sessions: map[int]*livySession{}}
	}
	return a.livyNativeState
}

// agentPost calls the Spark agent's JSON endpoint.
func (a *API) agentPost(path string, body any) (map[string]any, error) {
	u := *a.livyAgent
	u.Path = strings.TrimRight(u.Path, "/") + path
	b, _ := json.Marshal(body)
	resp, err := a.hcClient().Post(u.String(), "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("agent returned %d", resp.StatusCode)
	}
	return out, nil
}

// livyNative dispatches the Livy-native suffix. RBAC + lakehouse existence are
// already checked by livyProxy.
func (a *API) livyNative(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.PathValue("livypath"), "/"), "/")
	m := a.livyMgr()
	switch {
	case len(parts) == 1 && parts[0] == "sessions" && r.Method == http.MethodPost:
		a.createLivySession(w, r, m)
	case len(parts) == 1 && parts[0] == "sessions" && r.Method == http.MethodGet:
		m.mu.Lock()
		ids := make([]map[string]any, 0, len(m.sessions))
		for _, s := range m.sessions {
			ids = append(ids, map[string]any{"id": s.ID, "state": "idle", "kind": s.Kind})
		}
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"sessions": ids})
	case len(parts) == 2 && parts[0] == "sessions" && r.Method == http.MethodGet:
		a.getLivySession(w, parts[1], m)
	case len(parts) == 2 && parts[0] == "sessions" && r.Method == http.MethodDelete:
		a.deleteLivySession(w, parts[1], m)
	case len(parts) == 3 && parts[0] == "sessions" && parts[2] == "statements" && r.Method == http.MethodPost:
		a.submitLivyStatement(w, r, parts[1], m)
	case len(parts) == 4 && parts[0] == "sessions" && parts[2] == "statements" && r.Method == http.MethodGet:
		a.getLivyStatement(w, parts[1], parts[3], m)
	case len(parts) >= 1 && parts[0] == "batches":
		// Batches are the spark-submit path; not part of the interactive spike.
		writeErr(w, http.StatusNotImplemented, "BatchesNotImplemented",
			"The native Livy layer implements interactive sessions; batches are a follow-up.")
	default:
		writeErr(w, http.StatusNotFound, "LivyPathNotFound", "Unknown Livy path.")
	}
}

func sessionBody(s *livySession) map[string]any {
	return map[string]any{"id": s.ID, "state": "idle", "kind": s.Kind, "appId": fmt.Sprintf("livy-agent-%d", s.ID)}
}

func (a *API) createLivySession(w http.ResponseWriter, r *http.Request, m *livyManager) {
	var body struct {
		Kind string `json:"kind"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Kind == "" {
		body.Kind = "pyspark"
	}
	// Confirm the agent is reachable/ready before claiming a session.
	if _, err := a.agentGet("/health"); err != nil {
		writeErr(w, http.StatusBadGateway, "SparkAgentUnreachable", "The Spark agent is not reachable: "+err.Error())
		return
	}
	m.mu.Lock()
	s := &livySession{ID: m.nextID, Kind: body.Kind}
	m.sessions[s.ID] = s
	m.nextID++
	m.mu.Unlock()
	writeJSON(w, http.StatusCreated, sessionBody(s))
}

func (a *API) agentGet(path string) (map[string]any, error) {
	u := *a.livyAgent
	u.Path = strings.TrimRight(u.Path, "/") + path
	resp, err := a.hcClient().Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("agent %s returned %d", path, resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, nil
}

func (a *API) lookupSession(w http.ResponseWriter, id string, m *livyManager) (*livySession, bool) {
	n, err := strconv.Atoi(id)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidSessionId", "Session id must be an integer.")
		return nil, false
	}
	m.mu.Lock()
	s, ok := m.sessions[n]
	m.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "SessionNotFound", "No such Livy session.")
		return nil, false
	}
	return s, true
}

func (a *API) getLivySession(w http.ResponseWriter, id string, m *livyManager) {
	if s, ok := a.lookupSession(w, id, m); ok {
		writeJSON(w, http.StatusOK, sessionBody(s))
	}
}

func (a *API) deleteLivySession(w http.ResponseWriter, id string, m *livyManager) {
	s, ok := a.lookupSession(w, id, m)
	if !ok {
		return
	}
	_, _ = a.agentPost("/close", map[string]any{"session": strconv.Itoa(s.ID)})
	m.mu.Lock()
	delete(m.sessions, s.ID)
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"msg": "deleted"})
}

func (a *API) submitLivyStatement(w http.ResponseWriter, r *http.Request, id string, m *livyManager) {
	s, ok := a.lookupSession(w, id, m)
	if !ok {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// Drive the agent's REPL for this session's namespace — real Spark runs it.
	out, err := a.agentPost("/statements", map[string]any{"session": strconv.Itoa(s.ID), "code": body.Code})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "SparkAgentError", err.Error())
		return
	}
	m.mu.Lock()
	st := &livyStatement{ID: len(s.statements), State: "available", Output: out}
	s.statements = append(s.statements, st)
	m.mu.Unlock()
	writeJSON(w, http.StatusCreated, st)
}

func (a *API) getLivyStatement(w http.ResponseWriter, id, stid string, m *livyManager) {
	s, ok := a.lookupSession(w, id, m)
	if !ok {
		return
	}
	n, err := strconv.Atoi(stid)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil || n < 0 || n >= len(s.statements) {
		writeErr(w, http.StatusNotFound, "StatementNotFound", "No such statement.")
		return
	}
	writeJSON(w, http.StatusOK, s.statements[n])
}
