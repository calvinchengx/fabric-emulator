package server

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/config"
)

// terminalServer builds the smallest Server that can answer the terminal
// routes. The portal's other handlers need a store; these do not — the proxy
// reads config and a socket, nothing else.
func terminalServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	s := &Server{Cfg: cfg, mux: http.NewServeMux()}
	s.registerTerminal()
	return s
}

// fakeTTYD is a listener that accepts and records what the proxy forwarded.
func fakeTTYD(t *testing.T) (addr string, seen chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	seen = make(chan string, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		req, rerr := http.ReadRequest(bufio.NewReader(c))
		if rerr != nil {
			return
		}
		seen <- req.URL.String() + "\n" + req.Header.Get("Authorization")
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\n"))
	}()
	return ln.Addr().String(), seen
}

// TestTerminalIsAbsentUnlessConfigured.
//
// "Off" must mean the routes do not exist, not that they 404. Every other
// portal route is an unauthenticated GET over local state, and portal.go says
// the surface is unauthenticated BECAUSE it is read-only; a terminal breaks
// that premise, so an emulator nobody asked for one on should not carry the
// code path at all.
func TestTerminalIsAbsentUnlessConfigured(t *testing.T) {
	s := terminalServer(t, &config.Config{})
	for _, path := range []string{
		"/_emulator/portal/terminal/status",
		"/_emulator/portal/terminal/ws",
	} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d with no terminal configured; want 404 (route not mounted)",
				path, rec.Code)
		}
	}
}

// TestTerminalStatusDialsRatherThanTrustsConfig.
//
// The lesson from the spark-agent profile bug, applied before it can repeat:
// the medallion compose set FABRIC_SPARK_AGENT_URL unconditionally while the
// service sat behind `profiles: ["livy"]`, so the emulator believed it had an
// agent that was deliberately never started. Every PySpark leg then failed with
// an error that named nothing.
//
// A configured-but-absent terminal must therefore read as UNAVAILABLE, and say
// which knob to turn — not present a pane that dies when clicked.
func TestTerminalStatusDialsRatherThanTrustsConfig(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		addr, _ := fakeTTYD(t)
		s := terminalServer(t, &config.Config{TerminalURL: "http://" + addr})
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_emulator/portal/terminal/status", nil))
		got, err := decodeTerminalStatus(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Available {
			t.Errorf("status = %+v; want available with a listener up", got)
		}
	})

	t.Run("configured but nothing listening", func(t *testing.T) {
		// A port nobody is on: configuration says yes, reality says no.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		dead := ln.Addr().String()
		_ = ln.Close()

		s := terminalServer(t, &config.Config{TerminalURL: "http://" + dead})
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_emulator/portal/terminal/status", nil))
		got, err := decodeTerminalStatus(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got.Available {
			t.Fatal("a configured URL with nothing behind it reported available — " +
				"that is the spark-agent profile bug with a shell instead of a notebook")
		}
		if !strings.Contains(got.Reason, "profile") {
			t.Errorf("reason = %q; it must name the likeliest cause, or the "+
				"operator does not know which knob to turn", got.Reason)
		}
	})
}

// TestTerminalProxyRefusesWithoutTheToken covers the whole point of the file.
func TestTerminalProxyRefusesWithoutTheToken(t *testing.T) {
	addr, _ := fakeTTYD(t)
	cfg := &config.Config{TerminalURL: "http://" + addr, TerminalToken: "secret-token"}
	s := terminalServer(t, cfg)

	for _, tc := range []struct{ name, path, header string }{
		{"no token at all", "/_emulator/portal/terminal/ws", ""},
		{"wrong query token", "/_emulator/portal/terminal/ws?token=nope", ""},
		{"wrong bearer", "/_emulator/portal/terminal/ws", "Bearer nope"},
		{"empty bearer", "/_emulator/portal/terminal/ws", "Bearer "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			s.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s = %d; want 401 — this route is arbitrary execution",
					tc.name, rec.Code)
			}
		})
	}
}

// TestAnUnsetTokenRefusesEveryone.
//
// The trap: comparing a presented token against an EMPTY configured one makes
// every anonymous request match. An emulator that somehow reaches this state
// must refuse, not admit — "no token configured" is not "no token required".
func TestAnUnsetTokenRefusesEveryone(t *testing.T) {
	addr, _ := fakeTTYD(t)
	s := terminalServer(t, &config.Config{TerminalURL: "http://" + addr, TerminalToken: ""})

	for _, path := range []string{
		"/_emulator/portal/terminal/ws",
		"/_emulator/portal/terminal/ws?token=",
	} {
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no configured token = %d; want 401 — an empty "+
				"token must not match an empty request", path, rec.Code)
		}
	}
}

// TestTerminalTokenIsNotForwardedUpstream.
//
// ttyd has its own auth story and must never see the emulator's bearer: a token
// that leaks into the upstream's logs or handshake is a token that has escaped
// the boundary it was minted for. The `token` query parameter is ours too, and
// is stripped for the same reason.
func TestTerminalTokenIsNotForwardedUpstream(t *testing.T) {
	addr, seen := fakeTTYD(t)
	s := terminalServer(t, &config.Config{TerminalURL: "http://" + addr, TerminalToken: "secret-token"})

	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet,
		srv.URL+"/_emulator/portal/terminal/ws?token=secret-token&cols=80", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil {
		defer resp.Body.Close()
	}

	select {
	case got := <-seen:
		if strings.Contains(got, "secret-token") {
			t.Errorf("upstream saw the emulator's token in %q — it must not "+
				"cross the proxy boundary", got)
		}
		// The caller's own parameters still reach ttyd.
		if !strings.Contains(got, "cols=80") {
			t.Errorf("upstream request %q lost the caller's parameters", got)
		}
	default:
		t.Fatal("the proxy never reached the upstream with a valid token")
	}
}
