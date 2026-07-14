package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// refreshMirror snapshots a Fabric SQL Database's SQL tables to OneLake as Delta
// — the emulator's explicit hook for the mirroring real Fabric performs
// continuously in the background. Contributor+ on the workspace; the item must
// be a SQLDatabase; 501 when no warehouse SQL backend is wired.
func (a *API) refreshMirror(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil || it.Type != "SQLDatabase" {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The SQL database is not available.")
		return
	}
	if a.MirrorItem == nil {
		writeErr(w, http.StatusNotImplemented, "MirroringUnavailable",
			"OneLake mirroring requires a warehouse SQL backend (--warehouse-sql-url).")
		return
	}
	if err := a.MirrorItem(r.Context(), it.ID); err != nil {
		writeErr(w, http.StatusBadGateway, "MirrorFailed", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// sqlConnDetails is the connectionDetails shape a MirroredDatabase's source
// Connection carries for a SQL Server source — the emulator's own scoped
// mapping (real Fabric's connectionDetails wire shape isn't otherwise
// validated against a schema; this file defines the one MirroredDatabase
// mirroring understands).
type sqlConnDetails struct {
	Server   string `json:"server"`
	Database string `json:"database"`
}

// refreshMirroredDatabase snapshots an *external* SQL source — reached via a
// Connection, not the emulator's own per-item warehouse backend — to OneLake as
// Delta, reusing the exact same mirror writer the Fabric SQL Database uses
// (warehouse.Mirror): same code, external source. Body: {"connectionId": "..."}.
// Contributor+; the item must be a MirroredDatabase; the connection must carry
// Basic (username/password) credentials and a {server, database} connectionDetails
// — the scoped subset this emulator supports (real Mirroring also covers
// Snowflake/CosmosDB/on-prem SQL Server via gateway; those are out of scope).
// This is a snapshot on demand, not continuous/CDC replication.
func (a *API) refreshMirroredDatabase(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	if err != nil || it.Type != "MirroredDatabase" {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The mirrored database is not available.")
		return
	}
	var body struct {
		ConnectionID string `json:"connectionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ConnectionID == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "connectionId is required.")
		return
	}
	conn, err := a.Store.GetConnection(body.ConnectionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusBadRequest, "ConnectionNotFound", "No such connection.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	var details sqlConnDetails
	if err := json.Unmarshal(conn.Details, &details); err != nil || details.Server == "" || details.Database == "" {
		writeErr(w, http.StatusBadRequest, "InvalidConnection",
			"The connection's connectionDetails must include server and database.")
		return
	}
	var creds connectionCredentials
	if conn.CredentialsJSON == "" || json.Unmarshal([]byte(conn.CredentialsJSON), &creds) != nil || creds.CredentialType != "Basic" {
		writeErr(w, http.StatusBadRequest, "UnsupportedCredentials",
			"MirroredDatabase sources require Basic (username/password) credentials.")
		return
	}

	dsn := fmt.Sprintf("sqlserver://%s:%s@%s?database=%s&encrypt=disable",
		url.QueryEscape(creds.Username), url.QueryEscape(creds.Password), details.Server, url.QueryEscape(details.Database))
	be, err := tds.NewSQLServerBackend(dsn)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "MirrorFailed", "connecting to the source: "+err.Error())
		return
	}
	db := be.DB("")
	defer db.Close()
	if err := warehouse.Mirror(r.Context(), db, a.Store, it.ID); err != nil {
		writeErr(w, http.StatusBadGateway, "MirrorFailed", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}
