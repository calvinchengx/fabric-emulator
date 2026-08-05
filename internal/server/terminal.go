// Terminal pane: a ttyd session proxied through the portal's origin so the Flow
// view can drive a pipeline beside the graph that shows it running.
//
// WHY THIS FILE CARRIES ITS OWN AUTH. Every other portal route is a GET over
// local state, and portal.go says plainly that the surface is unauthenticated
// *because* it is read-only. A terminal is not another read — it is arbitrary
// execution, and the compose stack publishes 9443 on all interfaces. Inheriting
// the portal's premise would turn "anyone who can reach the port can read
// emulator state" into "anyone who can reach the port gets a shell", which is a
// different product. So the proxy demands a bearer the portal never serves.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// terminalProbeTimeout bounds the availability check. It is short because the
// answer is rendered in a UI toggle: a slow "is it there?" is worse than a
// fast "no".
const terminalProbeTimeout = 2 * time.Second

// registerTerminal mounts the proxy, but ONLY when a terminal is configured.
// An unset TerminalURL leaves the routes absent rather than 404 — the
// difference matters for a surface whose whole risk is existing at all.
func (s *Server) registerTerminal() {
	if s.Cfg.TerminalURL == "" {
		return
	}
	s.mux.HandleFunc("GET /_emulator/portal/terminal/status", s.terminalStatus)
	s.mux.HandleFunc("/_emulator/portal/terminal/ws", s.terminalProxy)
}

// terminalStatus answers whether a terminal can actually be reached, by
// DIALLING it rather than by reporting what was configured.
//
// This is the shape of a bug this repo has already paid for once: the medallion
// compose set FABRIC_SPARK_AGENT_URL unconditionally while the agent service sat
// behind `profiles: ["livy"]`, so the emulator believed it had an agent that was
// deliberately never started, and every PySpark leg failed with a notebook error
// that named nothing. Configuration is not availability. A terminal profile that
// is off must read as "no terminal", not as a pane that fails when clicked.
func (s *Server) terminalStatus(w http.ResponseWriter, r *http.Request) {
	u, err := url.Parse(s.Cfg.TerminalURL)
	if err != nil || u.Host == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"reason":    fmt.Sprintf("terminal URL %q is not a URL", s.Cfg.TerminalURL),
		})
		return
	}
	conn, err := net.DialTimeout("tcp", hostPort(u), terminalProbeTimeout)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			// Named, because the likeliest cause is a profile that is off and
			// the operator needs to know which knob to turn.
			"reason": fmt.Sprintf("no terminal at %s — is the `terminal` compose profile enabled?", u.Host),
		})
		return
	}
	_ = conn.Close()
	writeJSON(w, http.StatusOK, map[string]any{"available": true})
}

// hostPort fills in the scheme's default port, which url.Host omits.
func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" || u.Scheme == "wss" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}

// terminalProxy relays a websocket to ttyd after checking the bearer.
//
// The token may arrive as `Authorization: Bearer` or as a `token` query
// parameter: a browser's WebSocket constructor cannot set headers, so the query
// form is the only one the pane can use. It is not weaker here — both travel
// the same TLS connection to the same origin — but it does end up in the
// emulator's request log, so the token is single-purpose and per-run.
func (s *Server) terminalProxy(w http.ResponseWriter, r *http.Request) {
	if !s.terminalAuthorised(r) {
		// 401 without a WWW-Authenticate challenge: a browser prompt would be
		// useless here (the pane holds the token, the user is not typing a
		// password into a dialog) and would train the wrong habit.
		writeJSONError(w, http.StatusUnauthorized, "Unauthorized",
			"The terminal requires the token printed at emulator startup.")
		return
	}
	target, err := url.Parse(s.Cfg.TerminalURL)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "TerminalMisconfigured", err.Error())
		return
	}

	// Hijack rather than use httputil.ReverseProxy: this is a websocket
	// upgrade, and the two halves must be spliced byte-for-byte once ttyd has
	// agreed to it.
	upstream, err := net.DialTimeout("tcp", hostPort(target), terminalProbeTimeout)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "TerminalUnreachable",
			fmt.Sprintf("no terminal at %s: %v", target.Host, err))
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "TerminalMisconfigured",
			"this server cannot hijack the connection")
		return
	}

	// Forward the upgrade verbatim, minus our own auth: ttyd must not see the
	// emulator's bearer, and a `token` query parameter is ours, not its.
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host = "http", target.Host
	out.URL.Path = strings.TrimPrefix(target.Path, "/") + "/ws"
	if !strings.HasPrefix(out.URL.Path, "/") {
		out.URL.Path = "/" + out.URL.Path
	}
	q := out.URL.Query()
	q.Del("token")
	out.URL.RawQuery = q.Encode()
	out.Header = r.Header.Clone()
	out.Header.Del("Authorization")
	out.Host = target.Host

	if err := out.Write(upstream); err != nil {
		writeJSONError(w, http.StatusBadGateway, "TerminalUnreachable", err.Error())
		return
	}

	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	// Anything the client already sent past the header goes upstream first, or
	// the first keystroke of a fast client is lost.
	if n := buf.Reader.Buffered(); n > 0 {
		if pending, rerr := buf.Reader.Peek(n); rerr == nil {
			_, _ = upstream.Write(pending)
		}
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// terminalAuthorised compares the presented token against the configured one.
//
// Length-independent equality is deliberate: the token is 32 random bytes, so a
// timing oracle on a local socket is not the threat — an EMPTY configured token
// matching an empty request is. Both are refused explicitly.
func (s *Server) terminalAuthorised(r *http.Request) bool {
	want := s.Cfg.TerminalToken
	if want == "" {
		return false
	}
	if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got == want {
		return true
	}
	return r.URL.Query().Get("token") == want
}

// terminalJSON is here so the status shape has one definition; the portal reads
// it and the tests assert on it.
type terminalJSON struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

func decodeTerminalStatus(r io.Reader) (terminalJSON, error) {
	var out terminalJSON
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}
