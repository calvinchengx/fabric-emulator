package api

// Real-Time Intelligence: the Eventhouse / KQL Database execution surface.
//
// Fabric does not put KQL on the control plane. An Eventhouse item exposes a
// `properties.queryServiceUri` (fabric-docs
// real-time-intelligence/eventhouse-deploy-with-fabric-api.md), and clients
// then speak the *Kusto* REST protocol against it:
//
//	POST {queryServiceUri}/v1/rest/mgmt    {"db": "<database displayName>", "csl": ".create-merge table T(...)"}
//	POST {queryServiceUri}/v1/rest/query   {"db": "<database displayName>", "csl": "T | count"}
//	POST {queryServiceUri}/v2/rest/query   (the frame-stream dialect azure-kusto-data uses)
//
// So we terminate that protocol here — bearer validation on the Kusto
// audience, workspace RBAC, and per-item database isolation — and relay the
// KQL itself to a real Kusto engine (Microsoft's own kustainer container).
// Same shape as internal/tds + SQL Server: the contract is ours, the compute
// is a real engine's. With no engine attached every route answers an honest
// 501 rather than faking a result set.
//
// The emulator serves the Kusto endpoint on its own origin under /kusto/
// (real Fabric gives each eventhouse a `<guid>.z<n>.kusto.fabric.microsoft.com`
// host; a local emulator has no wildcard DNS, and every Kusto client builds
// its endpoints by appending "/v1/rest/…" to the cluster URI, so a
// path-prefixed cluster URI is transparent to them).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"net/http"
	"net/url"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// KustoAudience is the Entra resource a Kusto/Eventhouse token carries —
// Fabric's own Kusto resource, plus the Azure Data Explorer one its SDKs
// default to.
var KustoAudience = []string{
	"https://kusto.fabric.microsoft.com",
	"https://kusto.fabric.microsoft.com/",
	"https://api.kusto.windows.net",
	"https://api.kusto.windows.net/",
}

// Item property names persisted from a KQL Database's creationPayload and
// echoed back under the item's "properties" object.
const (
	propParentEventhouse = "parentEventhouseItemId"
	propDatabaseType     = "databaseType"
	propStoragePeriod    = "oneLakeStandardStoragePeriod"
)

// SetKQLBackend attaches a real Kusto engine (empty detaches it).
func (a *API) SetKQLBackend(raw string) error {
	if raw == "" {
		a.KQLURL = nil
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid Kusto engine URL %q", raw)
	}
	a.KQLURL = u
	if a.KQLHTTP == nil {
		a.KQLHTTP = &http.Client{}
	}
	a.kqlDatabases = map[string]bool{}
	return nil
}

// registerKQL mounts the Kusto protocol surface. One pattern covers
// v1/v2 × query/mgmt; the handler rejects anything else, since real Kusto has
// no /v2/rest/mgmt.
func (a *API) registerKQL(mux *http.ServeMux) {
	mux.HandleFunc("/kusto/{wid}/{ehid}/{ver}/rest/{kind}", a.withKustoAuth(a.kustoRelay))
}

// withKustoAuth validates a Kusto-audience bearer token. A missing validator
// or a missing engine both fail loudly — never with a synthesized result.
func (a *API) withKustoAuth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.KQLAuth == nil {
			writeKustoErr(w, http.StatusNotImplemented, "KQLNotConfigured",
				"The Kusto endpoint is not configured.")
			return
		}
		p, err := a.KQLAuth.ValidateRequest(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer authorization_uri="`+a.KQLAuth.Issuer+`"`)
			writeKustoErr(w, http.StatusUnauthorized, "General_Forbidden", err.Error())
			return
		}
		h(w, r, p)
	}
}

// kustoRequest is the documented Kusto REST body. `properties` is passed
// through untouched — the docs show it both as a nested object and as a
// JSON-escaped string, and json.RawMessage carries either.
type kustoRequest struct {
	DB         string          `json:"db"`
	CSL        string          `json:"csl"`
	Properties json.RawMessage `json:"properties,omitempty"`
}

// kustoRelay authenticates, authorizes, maps the Fabric database name onto the
// engine's isolated database, and relays the command upstream verbatim.
func (a *API) kustoRelay(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	if a.KQLURL == nil {
		writeKustoErr(w, http.StatusNotImplemented, "KQLEngineNotConfigured",
			"No Kusto engine is attached: start the emulator with --kql-url (FABRIC_KQL_URL) pointing at one.")
		return
	}
	ver, kind := r.PathValue("ver"), r.PathValue("kind")
	if !(ver == "v1" && (kind == "query" || kind == "mgmt")) && !(ver == "v2" && kind == "query") {
		writeKustoErr(w, http.StatusNotFound, "General_BadRequest",
			"Only /v1/rest/query, /v1/rest/mgmt and /v2/rest/query exist.")
		return
	}
	if r.Method != http.MethodPost {
		writeKustoErr(w, http.StatusMethodNotAllowed, "General_BadRequest", "Use POST with a JSON body.")
		return
	}
	// A management command mutates (create table, ingest, alter policy); a
	// query reads. Same split the rest of the emulator enforces.
	role := store.RoleViewer
	if kind == "mgmt" {
		role = store.RoleContributor
	}
	wid := r.PathValue("wid")
	if _, _, ok := a.requireKustoRole(w, wid, p, role); !ok {
		return
	}
	eh, err := a.Store.GetItem(wid, r.PathValue("ehid"))
	if err != nil || eh.Type != "Eventhouse" {
		writeKustoErr(w, http.StatusNotFound, "General_BadRequest", "The eventhouse was not found.")
		return
	}
	raw, ok := httpx.ReadBounded(r.Body, httpx.MaxProxyBody)
	if !ok {
		writeKustoErr(w, http.StatusRequestEntityTooLarge, "General_BadRequest",
			"The request body is too large.")
		return
	}
	var body kustoRequest
	if err := json.Unmarshal(raw, &body); err != nil || strings.TrimSpace(body.CSL) == "" {
		writeKustoErr(w, http.StatusBadRequest, "General_BadRequest", "csl is required.")
		return
	}
	db, err := a.resolveKQLDatabase(wid, eh.ID, body.DB)
	if err != nil {
		writeKustoErr(w, http.StatusNotFound, "General_DatabaseNotFound", err.Error())
		return
	}
	engineDB := engineDatabaseName(db.ID)
	if err := a.ensureKustoDatabase(r.Context(), engineDB); err != nil {
		writeKustoErr(w, http.StatusBadGateway, "KQLEngineError", err.Error())
		return
	}
	status, payload, err := a.callKusto(r.Context(), ver, kind, kustoRequest{
		DB: engineDB, CSL: body.CSL, Properties: body.Properties,
	})
	if err != nil {
		writeKustoErr(w, http.StatusBadGateway, "KQLEngineError", err.Error())
		return
	}
	// The engine echoes its own (isolated) database name in results such as
	// `.show database`; map it back to the Fabric display name so a client
	// never sees the emulator's internal naming.
	payload = bytes.ReplaceAll(payload, []byte(engineDB), []byte(db.DisplayName))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// requireKustoRole is requireRole with the Kusto error envelope.
func (a *API) requireKustoRole(w http.ResponseWriter, wid string, p *auth.Principal, need string) (*store.Workspace, string, bool) {
	ws, err := a.Store.GetWorkspace(wid)
	if err != nil {
		writeKustoErr(w, http.StatusNotFound, "General_BadRequest", "The workspace was not found.")
		return nil, "", false
	}
	role, err := a.Store.RoleOf(wid, p.ID)
	if err != nil {
		writeKustoErr(w, http.StatusInternalServerError, "General_InternalServerError", err.Error())
		return nil, "", false
	}
	if role == "" || store.RoleRank(role) < store.RoleRank(need) {
		writeKustoErr(w, http.StatusForbidden, "General_Forbidden",
			"The principal does not have "+need+" access to the workspace.")
		return nil, "", false
	}
	return ws, role, true
}

// resolveKQLDatabase maps the caller's `db` (a KQL Database item's
// displayName, exactly what real clients send) to the item under this
// eventhouse. An empty `db` picks the eventhouse's default database — the
// child Fabric creates with the eventhouse's own name.
func (a *API) resolveKQLDatabase(wid, eventhouseID, name string) (*store.Item, error) {
	dbs, err := a.Store.ListItems(wid, "KQLDatabase")
	if err != nil {
		return nil, err
	}
	var fallback *store.Item
	for _, db := range dbs {
		props, err := a.Store.ItemProperties(db.ID)
		if err != nil || props[propParentEventhouse] != eventhouseID {
			continue
		}
		if db.DisplayName == name {
			return db, nil
		}
		if fallback == nil {
			fallback = db
		}
	}
	if name == "" && fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("database %q was not found in this eventhouse", name)
}

// engineDatabaseName isolates each Fabric KQL Database in its own engine
// database, named from the item id — the same isolation the warehouse gets on
// SQL Server. Kusto names are identifiers, so the GUID's dashes go.
func engineDatabaseName(itemID string) string {
	return "fabric" + strings.ReplaceAll(itemID, "-", "")
}

// ensureKustoDatabase creates the engine-side database on first use. The
// `persist` form is the one Microsoft documents for the emulator; the folders
// must not already exist, which is why creation happens exactly once per
// database and is remembered.
func (a *API) ensureKustoDatabase(ctx context.Context, engineDB string) error {
	a.kqlMu.Lock()
	defer a.kqlMu.Unlock()
	if a.kqlDatabases == nil {
		a.kqlDatabases = map[string]bool{}
	}
	if a.kqlDatabases[engineDB] {
		return nil
	}
	present, err := a.kustoDatabaseExists(ctx, engineDB)
	if err != nil {
		return err
	}
	if !present {
		csl := fmt.Sprintf(`.create database %s persist (@"/kustodata/dbs/%s/md", @"/kustodata/dbs/%s/data")`,
			engineDB, engineDB, engineDB)
		status, payload, err := a.callKusto(ctx, "v1", "mgmt", kustoRequest{CSL: csl})
		if err != nil {
			return err
		}
		if status >= 300 {
			return fmt.Errorf("creating database %s: engine returned %d: %s", engineDB, status, truncate(payload))
		}
	}
	a.kqlDatabases[engineDB] = true
	return nil
}

// kustoDatabaseExists asks the engine, so a restarted emulator against a
// still-running engine does not try to re-create a database it already has.
func (a *API) kustoDatabaseExists(ctx context.Context, engineDB string) (bool, error) {
	status, payload, err := a.callKusto(ctx, "v1", "mgmt", kustoRequest{CSL: ".show databases"})
	if err != nil {
		return false, err
	}
	if status >= 300 {
		return false, fmt.Errorf("listing databases: engine returned %d: %s", status, truncate(payload))
	}
	return kustoV1HasValue(payload, "DatabaseName", engineDB), nil
}

// kustoV1HasValue reports whether a v1 response has a row whose `column`
// equals value, in any of its tables.
func kustoV1HasValue(payload []byte, column, value string) bool {
	var wire struct {
		Tables []struct {
			Columns []struct {
				ColumnName string `json:"ColumnName"`
			} `json:"Columns"`
			Rows [][]any `json:"Rows"`
		} `json:"Tables"`
	}
	if json.Unmarshal(payload, &wire) != nil {
		return false
	}
	for _, table := range wire.Tables {
		idx := -1
		for i, col := range table.Columns {
			if col.ColumnName == column {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		for _, row := range table.Rows {
			if idx < len(row) {
				if s, ok := row[idx].(string); ok && s == value {
					return true
				}
			}
		}
	}
	return false
}

// callKusto POSTs one request to the attached engine and returns its status
// and body. Nothing of the caller's transport is forwarded: the engine is a
// trusted sidecar reached over the compose network, and its own responses are
// what the client sees.
func (a *API) callKusto(ctx context.Context, ver, kind string, body kustoRequest) (int, []byte, error) {
	if a.KQLURL == nil {
		return 0, nil, fmt.Errorf("no Kusto engine attached")
	}
	target := *a.KQLURL
	target.Path = strings.TrimSuffix(target.Path, "/") + "/" + ver + "/rest/" + kind
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	client := a.KQLHTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// The engine's answer is relayed to the caller verbatim, so a truncated
	// read would serve a corrupt result set as a complete one — the read-side
	// twin of the OneLake append bug.
	payload, ok := httpx.ReadBounded(resp.Body, httpx.MaxProxyBody)
	if !ok {
		return 0, nil, fmt.Errorf("the Kusto engine returned more than %d bytes, or the read failed; refusing to relay a partial result", int64(httpx.MaxProxyBody))
	}
	return resp.StatusCode, payload, nil
}

func truncate(b []byte) string {
	if len(b) > 512 {
		return string(b[:512])
	}
	return string(b)
}

// writeKustoErr emits Kusto's own error envelope (OneApiError), not Fabric's —
// a real Kusto client parses this shape.
func writeKustoErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code":       code,
		"message":    "Request is invalid and cannot be processed.",
		"@type":      "Kusto.Data.Exceptions.KustoServiceException",
		"@message":   msg,
		"@permanent": true,
	}})
}

// ---- item properties: the Fabric-side half of the surface ----

// itemWithProperties is an item plus the fields the REST reference documents
// on the Item object beyond the generic record: the typed "properties" object
// (RTI items), and "sensitivityLabel", which the reference defines as a
// SensitivityLabel object carrying just an id. The embedded pointer inlines
// the item's own fields.
type itemWithProperties struct {
	*store.Item
	Properties       map[string]any    `json:"properties,omitempty"`
	SensitivityLabel *sensitivityLabel `json:"sensitivityLabel,omitempty"`
}

// sensitivityLabel is the documented shape — an id and nothing else.
type sensitivityLabel struct {
	ID string `json:"id"`
}

// kustoBaseURI is the eventhouse's cluster URI — what real Fabric surfaces as
// queryServiceUri. Derived from the caller's own request so it is reachable by
// whatever host/scheme reached us (container network name, localhost, TLS or
// not), exactly like OneLake's endpoints.
func kustoBaseURI(r *http.Request, wid, eventhouseID string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host + "/kusto/" + wid + "/" + eventhouseID
}

// itemView returns the item, wrapped with its typed properties when its type
// has any (Eventhouse / KQLDatabase today).
func (a *API) itemView(r *http.Request, it *store.Item) any {
	props := a.typedItemProperties(r, it)
	var label *sensitivityLabel
	if l, err := a.Store.ItemLabel(it.ID); err == nil {
		label = &sensitivityLabel{ID: l.ID}
	}
	if props == nil && label == nil {
		return it
	}
	return itemWithProperties{Item: it, Properties: props, SensitivityLabel: label}
}

func (a *API) typedItemProperties(r *http.Request, it *store.Item) map[string]any {
	switch it.Type {
	case "VariableLibrary":
		// `activeValueSetName` is the one property the REST reference defines
		// for this type, and it is the whole environment switch: the same
		// definition resolves differently per workspace according to it.
		// Absent until set, rather than reported as "", because a blank name
		// would read as "a value set called nothing is active".
		props, err := a.Store.ItemProperties(it.ID)
		if err != nil || props[propActiveValueSet] == "" {
			return nil
		}
		return map[string]any{propActiveValueSet: props[propActiveValueSet]}
	case "Warehouse":
		// See warehouse_endpoint.go. Nil rather than an empty string when no SQL
		// endpoint is running: a property that is present but blank reads as "the
		// warehouse has no address", which is a different claim from "this build
		// serves no SQL".
		if cs := a.warehouseConnectionString(r); cs != "" {
			return map[string]any{"connectionString": cs}
		}
		return nil
	case "Lakehouse":
		// The SQL analytics endpoint: the read-only T-SQL surface over the
		// lakehouse's Delta tables. It is the same TDS listener a Warehouse
		// uses — the emulator routes by item and makes a Lakehouse read-only
		// (internal/server/warehouse.go) — but it is a DIFFERENT property, and
		// real Fabric names it differently, so it is reported under the name
		// real Fabric reports it under.
		//
		// `id` IS DELIBERATELY ABSENT. On real Fabric the analytics endpoint is
		// its own SQLEndpoint item with its own GUID; the emulator has no such
		// item and routes the endpoint by the lakehouse id. Reporting the
		// lakehouse id under that name would invite a consumer to use it as a
		// database name, which works here and fails on real Fabric. Leaving it
		// out fails the other way round: locally, loudly, before it ships.
		if cs := a.warehouseConnectionString(r); cs != "" {
			return map[string]any{"sqlEndpointProperties": map[string]any{
				"connectionString": cs,
				// The emulator provisions the endpoint on first connect, so it
				// is never pending from the caller's point of view. Real Fabric
				// can answer InProgress, which a client must handle: hence a
				// status field at all rather than an implied one.
				"provisioningStatus": "Success",
			}}
		}
		return nil
	case "Eventhouse":
		base := kustoBaseURI(r, it.WorkspaceID, it.ID)
		ids := []string{}
		if dbs, err := a.Store.ListItems(it.WorkspaceID, "KQLDatabase"); err == nil {
			for _, db := range dbs {
				if props, err := a.Store.ItemProperties(db.ID); err == nil && props[propParentEventhouse] == it.ID {
					ids = append(ids, db.ID)
				}
			}
		}
		return map[string]any{
			"queryServiceUri": base,
			// The attached engine has no separate data-management service
			// (kustainer documents queued ingestion as unsupported), so the
			// ingestion URI is the engine itself — direct ingestion commands
			// (.ingest inline, .set-or-append) are what work.
			"ingestionServiceUri": base,
			"databasesItemIds":    ids,
		}
	case "KQLDatabase":
		stored, err := a.Store.ItemProperties(it.ID)
		if err != nil {
			return nil
		}
		parent := stored[propParentEventhouse]
		dbType := stored[propDatabaseType]
		if dbType == "" {
			dbType = "ReadWrite"
		}
		props := map[string]any{
			propParentEventhouse: parent,
			propDatabaseType:     dbType,
		}
		if parent != "" {
			base := kustoBaseURI(r, it.WorkspaceID, parent)
			props["queryServiceUri"] = base
			props["ingestionServiceUri"] = base
		}
		if period := stored[propStoragePeriod]; period != "" {
			props[propStoragePeriod] = period
		}
		return props
	}
	return nil
}

// applyCreationPayload persists the typed creationPayload of an RTI item and
// reproduces Fabric's documented eventhouse behaviour: creating an eventhouse
// also creates a default child KQL database with the same name
// (fabric-docs real-time-intelligence/create-eventhouse.md).
func (a *API) applyCreationPayload(it *store.Item, payload map[string]any) {
	str := func(key string) string {
		v, _ := payload[key].(string)
		return v
	}
	switch it.Type {
	case "KQLDatabase":
		props := map[string]string{propDatabaseType: "ReadWrite"}
		for _, key := range []string{propParentEventhouse, propDatabaseType, propStoragePeriod} {
			if v := str(key); v != "" {
				props[key] = v
			}
		}
		_ = a.Store.SetItemProperties(it.ID, props)
	case "Eventhouse":
		child := &store.Item{WorkspaceID: it.WorkspaceID, Type: "KQLDatabase", DisplayName: it.DisplayName}
		if err := a.Store.CreateItem(child, nil); err != nil {
			return
		}
		_ = a.Store.SetItemProperties(child.ID, map[string]string{
			propParentEventhouse: it.ID,
			propDatabaseType:     "ReadWrite",
		})
	}
}
