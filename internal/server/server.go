// Package server assembles the emulator: the /v1 control plane, the
// /_emulator control surface (clock + faults — local testing plumbing, not
// part of the Fabric contract), and /health.
package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/akv"
	"github.com/calvinchengx/fabric-emulator/internal/api"
	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/entra"
	"github.com/calvinchengx/fabric-emulator/internal/onelake"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Server owns the emulator's components.
type Server struct {
	Cfg     *config.Config
	Store   *store.Store
	Clock   *clock.Clock
	API     *api.API
	OneLake *onelake.Service
	mux     *http.ServeMux
}

// New wires the emulator. jwksClient overrides the JWKS-fetching HTTP client
// when non-nil (in-process tests against entra-emulator's test listener).
func New(cfg *config.Config, jwksClient *http.Client) (*Server, error) {
	ck := clock.New()
	st, err := store.Open(cfg.DataDir, ck)
	if err != nil {
		return nil, err
	}
	v := auth.New(cfg.EntraIssuer, cfg.EntraJWKSURL, cfg.EntraTLSInsecure, ck.Now, jwksClient)
	a := api.New(st, v, cfg.RetryAfterSeconds, cfg.LRODelaySeconds)
	// Workspace-identity provisioning drives entra's admin API at the
	// issuer's origin, over the same HTTP client trust as JWKS.
	if origin, err := entra.OriginFromIssuer(cfg.EntraIssuer); err == nil {
		a.Entra = entra.New(origin, cfg.EntraTLSInsecure, jwksClient)
	}
	a.AKV = akv.New(cfg.EntraTLSInsecure, jwksClient)

	// OneLake accepts only Storage-audience tokens, over the same JWKS.
	olv := auth.New(cfg.EntraIssuer, cfg.EntraJWKSURL, cfg.EntraTLSInsecure, ck.Now, jwksClient)
	olv.Audiences = onelake.StorageAudience
	ol := onelake.New(st, olv)

	s := &Server{Cfg: cfg, Store: st, Clock: ck, API: a, OneLake: ol, mux: http.NewServeMux()}
	a.Register(s.mux)
	s.registerControl()
	return s, nil
}

// Handler returns the root handler: Host-routed like real Fabric —
// onelake.dfs.fabric.microsoft.com serves the data plane, everything else
// the control plane.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "onelake.") {
			s.OneLake.ServeHTTP(w, r)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// Close releases resources.
func (s *Server) Close() error { return s.Store.Close() }

// registerControl mounts /health and the /_emulator control surface.
func (s *Server) registerControl() {
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "now": s.Clock.Now()})
	})

	// Clock control — the LRO lever: advance past completeAt and a Running
	// operation succeeds on the next poll.
	s.mux.HandleFunc("GET /_emulator/clock", func(w http.ResponseWriter, r *http.Request) {
		offset, frozen, now := s.Clock.State()
		writeJSON(w, http.StatusOK, map[string]any{"offset": offset, "frozen": frozen, "now": now})
	})
	s.mux.HandleFunc("POST /_emulator/clock", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Advance *int64 `json:"advance"`
			Offset  *int64 `json:"offset"`
			Freeze  *bool  `json:"freeze"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed JSON"})
			return
		}
		if body.Offset != nil {
			s.Clock.SetOffset(*body.Offset)
		}
		if body.Advance != nil {
			s.Clock.Advance(*body.Advance)
		}
		if body.Freeze != nil {
			if *body.Freeze {
				s.Clock.Freeze()
			} else {
				s.Clock.Unfreeze()
			}
		}
		offset, frozen, now := s.Clock.State()
		writeJSON(w, http.StatusOK, map[string]any{"offset": offset, "frozen": frozen, "now": now})
	})

	// Fault injection: fail the next N operations, reject the next N
	// requests outright, or slow every new LRO.
	s.mux.HandleFunc("POST /_emulator/faults", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			FailNextOperations *int   `json:"failNextOperations"`
			RejectNextRequests *int   `json:"rejectNextRequests"`
			LRODelaySeconds    *int64 `json:"lroDelaySeconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "malformed JSON"})
			return
		}
		fail, reject, delay := -1, -1, int64(-1)
		if body.FailNextOperations != nil {
			fail = *body.FailNextOperations
		}
		if body.RejectNextRequests != nil {
			reject = *body.RejectNextRequests
		}
		if body.LRODelaySeconds != nil {
			delay = *body.LRODelaySeconds
		}
		s.API.SetFaults(fail, reject, delay)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
