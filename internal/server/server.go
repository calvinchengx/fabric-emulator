// Package server assembles the emulator: the /v1 control plane, the
// /_emulator control surface (clock + faults — local testing plumbing, not
// part of the Fabric contract), and /health.
package server

import (
	"crypto/tls"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	airflowclient "github.com/calvinchengx/fabric-emulator/internal/airflow"
	"github.com/calvinchengx/fabric-emulator/internal/akv"
	"github.com/calvinchengx/fabric-emulator/internal/api"
	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/entra"
	"github.com/calvinchengx/fabric-emulator/internal/onelake"
	"github.com/calvinchengx/fabric-emulator/internal/purview"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
)

// SQLAudience is the Entra resource a Fabric SQL/Warehouse token carries
// (Azure SQL's audience), which the TDS endpoint validates FedAuth tokens
// against — distinct from the control-plane and Storage audiences.
var SQLAudience = []string{"https://database.windows.net", "https://database.windows.net/"}

// Server owns the emulator's components.
type Server struct {
	Cfg     *config.Config
	Store   *store.Store
	Clock   *clock.Clock
	API     *api.API
	OneLake *onelake.Service
	Purview *purview.Service
	// TDS is the warehouse SQL endpoint (nil when SQLTDSAddr is unset). main
	// starts its TCP listener; it authenticates FedAuth logins against entra.
	TDS *tds.Server
	// rec appends documented-surface responses when FABRIC_RECORD_RESPONSES
	// names a file, for OpenAPI conformance. Nil on every ordinary run; see
	// record.go for what is recorded and what is deliberately not.
	rec *recorder
	// EventKeepalive overrides how often the flow stream emits a keepalive
	// comment; 0 uses DefaultEventKeepalive. Tests lower it so teardown does
	// not wait out a full interval (see events.go for why it must wait at all).
	EventKeepalive time.Duration
	mux            *http.ServeMux
	armStop        chan struct{}
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
	a.SetTenantAdmins(cfg.TenantAdmins)
	a.ListPageSize = cfg.ListPageSize
	// Workspace-identity provisioning drives entra's admin API at the
	// issuer's origin, over the same HTTP client trust as JWKS.
	if origin, err := entra.OriginFromIssuer(cfg.EntraIssuer); err == nil {
		a.Entra = entra.New(origin, cfg.EntraTLSInsecure, jwksClient)
	}
	// AKV GETS ITS OWN TRANSPORT, and is the one client here that must.
	// jwksClient is built for entra-emulator's self-signed cert, so handing
	// it over made the vault client inherit a trust decision taken about a
	// different host — and akv's allowlist deliberately admits a REAL
	// *.vault.azure.net, where a secret and the token fetching it would then
	// cross an unverified connection. akv.New scopes the skip to
	// cfg.AKVVaultHost instead (see hostScopedTLS there); nil keeps the
	// injected-client parameter for in-process tests, which set API.AKV
	// directly.
	a.AKV = akv.New(cfg.EntraTLSInsecure, nil, cfg.AKVVaultHost)
	if err := a.SetLivyAgent(cfg.SparkAgentURL); err != nil {
		return nil, err
	}
	if err := a.SetLivyBackend(cfg.SparkLivyURL); err != nil {
		return nil, err
	}
	if cfg.AirflowURL != "" {
		client, err := airflowclient.New(cfg.AirflowURL, cfg.AirflowDAGDir, cfg.AirflowUsername, cfg.AirflowPassword)
		if err != nil {
			return nil, err
		}
		a.Airflow = client
	}
	a.ForceLRO = cfg.ForceLRO
	a.NameReservation = cfg.NameReservation
	a.WebActivityStub = cfg.WebActivityStub
	a.CustomActivityShell = cfg.CustomActivityShell
	a.DatabricksURL = cfg.DatabricksURL
	a.DatabricksToken = cfg.DatabricksToken
	if cfg.DatabricksTLSInsecure {
		a.DatabricksHTTP = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
	}
	if err := a.SetMLflowBackend(cfg.MLflowURL); err != nil {
		return nil, err
	}
	// The Eventhouse/KQL Database surface speaks the Kusto REST protocol on
	// its own audience, and relays the KQL to a real engine when one is
	// attached (docs/25-rti-kusto.md).
	kqlv := auth.New(cfg.EntraIssuer, cfg.EntraJWKSURL, cfg.EntraTLSInsecure, ck.Now, jwksClient)
	kqlv.Audiences = api.KustoAudience
	a.KQLAuth = kqlv
	if err := a.SetKQLBackend(cfg.KQLURL); err != nil {
		return nil, err
	}
	if err := a.SetDAXBackend(cfg.DAXURL); err != nil {
		return nil, err
	}
	if err := a.SetKafkaBootstrap(cfg.KafkaBootstrap); err != nil {
		return nil, err
	}

	// OneLake accepts only Storage-audience tokens, over the same JWKS.
	olv := auth.New(cfg.EntraIssuer, cfg.EntraJWKSURL, cfg.EntraTLSInsecure, ck.Now, jwksClient)
	olv.Audiences = onelake.StorageAudience
	ol := onelake.New(st, olv)
	a.ExternalDelta = ol

	// The Purview Data Map is Apache Atlas v2 on its own audience — the spec's
	// routes are literally /atlas/v2/… — so it validates a purview.azure.net
	// token, not a Fabric control-plane one (internal/purview).
	pvv := auth.New(cfg.EntraIssuer, cfg.EntraJWKSURL, cfg.EntraTLSInsecure, ck.Now, jwksClient)
	pvv.Audiences = purview.DataMapAudience
	pv := purview.New(st, pvv)
	if err := pv.Seed(); err != nil {
		return nil, err
	}

	// The Power BI executeQueries endpoint accepts only Power BI-audience tokens.
	pbiv := auth.New(cfg.EntraIssuer, cfg.EntraJWKSURL, cfg.EntraTLSInsecure, ck.Now, jwksClient)
	pbiv.Audiences = api.PowerBIAudience
	a.PBIAuth = pbiv

	// Every committed OneLake write emits an event; event triggers subscribe.
	// No broker: the emulator owns the storage layer, so a file event is
	// observable at the source whoever wrote it (internal/api/triggers.go).
	st.FileEvents = func(ev store.FileEvent) { a.DispatchFileEvent(ev) }

	s := &Server{Cfg: cfg, Store: st, Clock: ck, API: a, OneLake: ol, Purview: pv, mux: http.NewServeMux()}

	// The warehouse SQL endpoint terminates FedAuth by validating the client's
	// TDS-presented token against entra with the Azure SQL audience.
	if cfg.SQLTDSAddr != "" {
		// Tell the control plane which port to advertise on a Warehouse, so a
		// client can discover the SQL endpoint the way it does on Fabric rather
		// than being handed a hostname out of band.
		a.SQLEndpointPort = api.SQLPortOf(cfg.SQLTDSAddr)
		sqlv := auth.New(cfg.EntraIssuer, cfg.EntraJWKSURL, cfg.EntraTLSInsecure, ck.Now, jwksClient)
		sqlv.Audiences = SQLAudience
		s.TDS = &tds.Server{Auth: func(token string) error {
			_, err := sqlv.Validate(token)
			return err
		}, Strict: cfg.TSQLStrict}
		// FABRIC_TDS_TRACE logs every client→server TDS message to stderr: which
		// message type carries a statement decides what a SQL rewriter has to
		// parse (docs/29-tsql-parity.md, T6a). Off unless set — an unset trace hook
		// costs one nil check per message.
		if os.Getenv("FABRIC_TDS_TRACE") != "" {
			tds.SetTraceFunc(func(line string) { log.Println("tds:", line) })
		}
		// With a backend configured, authenticated queries relay to a real SQL
		// Server; without one, the endpoint answers the T1 stub.
		if cfg.WarehouseSQLURL != "" {
			be, err := tds.NewSQLServerBackend(cfg.WarehouseSQLURL)
			if err != nil {
				return nil, err
			}
			s.TDS.Backend = be
			// A Warehouse's own collationType decides how its database is created.
			// The backend cannot read the store (import direction), so it asks.
			be.CollationOf = func(database string) string {
				props, err := st.ItemProperties(database)
				if err != nil {
					return ""
				}
				return props[store.PropCollationType]
			}
			// Route each connection by item type (Lakehouse = read-only analytics
			// endpoint, Warehouse = read-write), each into its own database, with
			// workspace RBAC enforced for the token's principal.
			principalOf := func(token string) (string, error) {
				p, err := sqlv.Validate(token)
				if err != nil {
					return "", err
				}
				return p.ID, nil
			}
			route := warehouseRoute(st, be, ol)
			// Gold is built over this wire, so the flow graph only reaches it if
			// the TDS front records what its statements moved.
			s.TDS.Observe = newWarehouseLineage(st).observe
			// And a Warehouse table gets a version history from the same
			// statements (docs/35 Phase 4), so FOR TIMESTAMP AS OF has a past.
			if cfg.WarehouseVersioning {
				route = versionedRoute(route, st, cfg.WarehouseRetentionDays)
				s.TDS.Observe = chainObservers(s.TDS.Observe,
					newWarehouseVersioner(st, be, cfg.WarehouseRetentionDays).observe)
			}
			s.TDS.OnConnect = tokenRoute(principalOf, route)
			// Fabric's Data Warehouse MCP server runs T-SQL as its caller through
			// the same route, refusals, dialect and observers as this wire.
			a.SQLExecAs = sqlExecAsFor(be, route, s.TDS)
			// A Fabric SQL Database mirrors its SQL tables to OneLake Delta; wire the
			// control-plane refresh hook to the same per-item backend.
			a.MirrorItem = mirrorItem(be, st)
			// The lever a client uses to force the analytics endpoint to catch up
			// with Delta (refreshMetadata); without it the endpoint 501s honestly.
			a.LakehouseDB = lakehouseDBFor(be, st)
			// The pipeline Script/StoredProcedure activities run real T-SQL against a
			// Warehouse/SQLDatabase item's own database, on the same backend.
			a.SQLDB = sqlDBFor(be, st)
			// Direct Lake on SQL reads as the caller, never as the service account.
			a.SQLDBAs = sqlDBAsFor(be, st, ol)
			// Switching a SQL analytics endpoint's data access mode closes the
			// workspace's sessions and applies the mode's SQL side effects.
			a.SwitchDataAccessMode = dataAccessModeSwitch(be, st, s.TDS)
		}
	}

	a.Register(s.mux)
	pv.Register(s.mux)
	s.registerControl()
	s.registerEvents()
	s.registerPortal()
	s.registerTerminal()
	if cfg.ARMURL != "" {
		src := api.NewARMCapacities(st, cfg.ARMURL, cfg.EntraTLSInsecure, jwksClient,
			time.Duration(cfg.ARMPollSeconds)*time.Second)
		if err := src.Refresh(); err != nil {
			log.Printf("arm-capacities: initial refresh: %v", err)
		}
		s.armStop = make(chan struct{})
		go src.Run(s.armStop)
	}
	// Off unless FABRIC_RECORD_RESPONSES names a file; see record.go.
	s.rec = newRecorder()
	return s, nil
}

// Handler returns the root handler: Host-routed like real Fabric —
// onelake.dfs.* serves the DFS data plane and onelake.blob.* the Blob
// dialect. For clients that override the endpoint instead of the Host
// (delta-rs/object_store pointing at localhost), the azurite-style
// account-prefixed path /onelake/{workspace}/… reaches the Blob surface on
// any host — the account name is always the literal "onelake", as
// documented.
func (s *Server) Handler() http.Handler {
	// RECORDING WRAPS ONLY THE CONTROL PLANE, not the OneLake data plane.
	//
	// recordable() already refuses /onelake, so wrapping the host-routed
	// branches below achieved nothing except putting a second ResponseWriter
	// in front of every byte of a Delta file. Scoping it to the mux keeps this
	// code off the data plane's response path entirely, which is both cheaper
	// and a smaller blast radius for a wrapper whose only job is diagnostics.
	recorded := s.record(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mux.ServeHTTP(w, r)
	}))
	routed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.Host, "onelake.blob."):
			s.OneLake.ServeBlob(w, r)
		case strings.HasPrefix(r.Host, "onelake."):
			s.OneLake.ServeHTTP(w, r)
		case r.URL.Path == "/onelake" || strings.HasPrefix(r.URL.Path, "/onelake/"):
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/onelake")
			s.OneLake.ServeBlob(w, r2)
		default:
			recorded.ServeHTTP(w, r)
		}
	})
	return s.boundBodies(routed)
}

// boundBodies installs the ONE outer ceiling on every inbound request body.
//
// WHY AT THE ROOT AND NOT PER HANDLER. internal/httpx already bounds the sites
// that read a body as BYTES -- 23 ReadBounded calls, each with a ceiling chosen
// by the handler that knows what it is reading. What no ceiling reached was the
// 68 `json.NewDecoder(r.Body)` sites: a streaming decoder allocates as it goes,
// so before this the emulator would allocate whatever a client chose to send on
// any of them, and a 69th added tomorrow inherited the same exposure invisibly.
// One bound at the root closes the class instead of the instances, which is why
// it is not 68 edits. scripts/check_perf_regressions.py holds the coupling.
//
// IT WRAPS THE DATA PLANE TOO, unlike the recorder above. The recorder is
// diagnostics and has no business on a Delta file's response path; this is a
// memory bound, and the data plane is the surface most able to exhaust memory.
// Large uploads are unaffected because the ceiling sits ABOVE the Blob one --
// httpx.DefaultMaxRequestBody is 320 MiB against MaxBlobWrite's 256 MiB, so an
// oversized blob write still fails with that package's specific fit-vs-truncated
// message rather than this one's generic refusal. That ordering is the whole
// reason the default is not 256 MiB; see httpx.DefaultMaxRequestBody.
//
// TWO PATHS, because http.MaxBytesReader alone cannot answer 413. It makes reads
// FAIL, and the handler that was decoding then reports its own error -- so an
// oversized body would be refused as "malformed JSON", telling the caller
// something untrue about their request. So:
//
//   - A declared Content-Length over the bound is refused here, unread, with
//     413. Every client sending a body sets it, so this is the path real traffic
//     takes and the one the status code matters for.
//   - Anything else (chunked, or no declared length) is wrapped in
//     MaxBytesReader. The allocation is still capped -- which is the property
//     that matters -- and the handler reports the read failure in its own voice.
//
// A bound of 0 disables both, which is FABRIC_MAX_REQUEST_BYTES=0: the documented
// escape hatch for anyone already posting something larger than the default.
func (s *Server) boundBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		max := s.Cfg.MaxRequestBytes
		if max <= 0 {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > max {
			// Written in the control plane's error shape. The data plane's
			// dialects spell errors differently, but a request refused before
			// routing has not yet been attributed to a surface, and 413 with a
			// readable reason is more use to a caller than a guess at which
			// vocabulary they expected.
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error":           "request body too large",
				"maxRequestBytes": max,
				"contentLength":   r.ContentLength,
				"hint": "Raise or remove the bound with FABRIC_MAX_REQUEST_BYTES " +
					"(0 means unlimited); see docs/04-configuration.md.",
			})
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// Close releases resources.
func (s *Server) Close() error {
	if s.armStop != nil {
		close(s.armStop)
		s.armStop = nil
	}
	if err := s.rec.Close(); err != nil {
		log.Printf("record: closing the response recording: %v", err)
	}
	return s.Store.Close()
}

// registerControl mounts /health and the /_emulator control surface.
func (s *Server) registerControl() {
	s.mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// `build` rides on /health because the portal already fetches it on
		// load — the top bar costs no second request — and because anyone
		// filing a bug can read it with one curl.
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "ok", "now": s.Clock.Now(), "build": s.Cfg.Build(),
		})
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
		// Moving the clock is what makes a schedule due, so evaluate now:
		// the emulator runs no background scheduler on purpose (wall-clock
		// ticking would make job outcomes depend on how long a test took).
		// `{"advance":0}` is therefore also a plain "tick now".
		started := s.API.TickSchedules()
		admitted := s.API.DrainCapacityQueues()
		offset, frozen, now := s.Clock.State()
		writeJSON(w, http.StatusOK, map[string]any{
			"offset": offset, "frozen": frozen, "now": now,
			"scheduledJobsStarted": started, "queuedJobsAdmitted": admitted})
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
