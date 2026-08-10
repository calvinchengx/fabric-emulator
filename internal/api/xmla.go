package api

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"fmt"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/xmla"
)

// The XMLA surface a real ADOMD.NET / TOM client demands before it will send a
// single query. Every route and every shape here was MEASURED against
// Microsoft's own clients in e2e/xmla and e2e/sempy, never inferred: each one
// failed first in a way that named something other than its cause, and those
// causes are recorded beside the code that satisfies them.
//
// The client's own sequence, which is the order below:
//
//	GET  /powerbi/databases/v201606/workspaces      routing
//	POST /metadata/v201606/generateastoken          the MWC token
//	POST /powerbi/.../workspaces/{ws}/getDatabaseName   dataset name -> database
//	POST /webapi/clusterResolve                     which host serves it
//	POST /webapi/xmla                               session, then Discover/Execute
//
// `getDatabaseName` exists here only because a REAL client was driven: a
// hand-written probe connects with a database name it already knows, so that
// call is invisible to one.

const (
	// nsEngine is ASSL's base namespace.
	nsEngine = "http://schemas.microsoft.com/analysisservices/2003/engine"
	// nsCompat carries CompatibilityLevel. NOT the 2003 engine namespace and
	// NOT .../200/200 — read off Microsoft.AnalysisServices.Tabular.dll, where
	// the property's XmlElementAttribute names exactly this.
	nsCompat = "http://schemas.microsoft.com/analysisservices/2010/engine/200"
	// nsMultipleResults wraps a <Batch> response. A DIFFERENT SCHEME from the
	// urn:schemas-microsoft-com:xml-analysis:* family, not a suffix on it; a
	// wrong container namespace is refused at the envelope before anything
	// inside is read.
	nsMultipleResults = "http://schemas.microsoft.com/analysisservices/2003/xmla-multipleresults"

	// xmlaSessionID is echoed in the Session header. Nothing validates it; the
	// client only requires that one comes back.
	xmlaSessionID = "fabric-emulator-session"
)

var reRequestType = regexp.MustCompile(`<RequestType>([^<]+)</RequestType>`)

// registerXMLA mounts the XMLA endpoint and the calls that precede it.
func (a *API) registerXMLA(mux *http.ServeMux) {
	mux.HandleFunc("GET /powerbi/databases/v201606/workspaces", a.withPBIAuth(a.xmlaRouting))
	mux.HandleFunc("POST /metadata/v201606/generateastoken", a.withPBIAuth(a.xmlaToken))
	// PAST THE TOKEN EXCHANGE THE CLIENT PRESENTS *OUR* TOKEN, not the AAD one.
	// That is the whole point of generateastoken: the service hands back a
	// short-lived MWC token and the client uses it from then on. Requiring an
	// AAD bearer on these routes rejects the client's own correct behaviour and
	// reports it as `missing bearer token`, which reads as a client fault.
	mux.HandleFunc("POST /powerbi/databases/v201606/workspaces/{ws}/getDatabaseName",
		a.withXMLAAuth(a.xmlaDatabaseName))
	// UNAUTHENTICATED, and that is the client's behaviour, not an oversight.
	// MEASURED: ADOMD.NET sends clusterResolve with NO Authorization header at
	// all — it is asking which host to talk to, before it has decided what
	// credential that host wants. Requiring a bearer here rejects a correct
	// client and reports it as `missing bearer token`. Nothing is disclosed: the
	// reply is the caller's own host and a fixed server name, both of which the
	// caller already knows.
	mux.HandleFunc("POST /webapi/clusterResolve", a.xmlaClusterResolveOpen)
	mux.HandleFunc("POST /webapi/xmla", a.withXMLAAuth(a.xmlaEndpoint))
	// The Power BI workspace list. Not part of XMLA, but the first call a real
	// SemPy client makes — `list_workspaces` — and without it the client gets a
	// 404 with an empty body and reports `JSONDecodeError`, which names the
	// parser rather than the missing route.
	mux.HandleFunc("GET /v1.0/myorg/groups", a.withPBIAuth(a.listPBIGroups))
}

// mwcToken is what generateastoken issues and what the client presents
// afterwards. It is not a credential: nothing outside this process mints or
// validates it, and it carries no claims. It exists because the client's flow
// requires a token to come back and be usable.
const mwcToken = "fabric-emulator-mwc-token"

// mwcHolder is the principal that obtained the MWC token. The token must carry
// an identity or the calls it authorises can see nothing: a fabricated
// principal owns no workspace, so `list_measures` would return an EMPTY frame
// rather than an error — well formed, never failing, and wrong. Recording who
// asked keeps the answer attributable.
var (
	mwcMu     sync.RWMutex
	mwcHolder *auth.Principal
)

// withXMLAAuth accepts EITHER an AAD Power BI token or the MWC token this
// server issued. The principal for an MWC-token call is the one that obtained
// it — modelled here as the caller of record, since a single-tenant emulator
// has no separate service identity to attribute it to.
func (a *API) withXMLAAuth(h handler) http.HandlerFunc {
	pbi := a.withPBIAuth(h)
	return func(w http.ResponseWriter, r *http.Request) {
		// The scheme is `MwcToken`, NOT `Bearer` — measured off the wire:
		//     Authorization: MwcToken fabric-emulator-mwc-token
		// Stripping only "Bearer " leaves the whole header unmatched, the
		// request falls through to AAD validation, and a correct client is
		// refused with `missing bearer token`.
		raw := strings.TrimSpace(r.Header.Get("Authorization"))
		tok := raw
		for _, scheme := range []string{"MwcToken ", "Bearer "} {
			if len(raw) >= len(scheme) && strings.EqualFold(raw[:len(scheme)], scheme) {
				tok = strings.TrimSpace(raw[len(scheme):])
				break
			}
		}
		if tok == mwcToken && a.xmlaMWCPrincipal() != nil {
			// Attribute to the workspace owner rather than inventing an
			// anonymous principal: an unattributed write would be worse than a
			// slightly generous read.
			h(w, r, a.xmlaMWCPrincipal())
			return
		}
		pbi(w, r)
	}
}

// xmlaMWCPrincipal is the identity an MWC-token call runs as: whoever exchanged
// an AAD token for it. Nil until that exchange happens, and a nil principal is
// refused rather than defaulted — a token nobody asked for should not authorise
// anything.
func (a *API) xmlaMWCPrincipal() *auth.Principal {
	mwcMu.RLock()
	defer mwcMu.RUnlock()
	return mwcHolder
}

// listPBIGroups answers the Power BI `groups` shape: an OData-style envelope,
// unlike the ADOMD routing call above which is a bare array. The two differ
// deliberately and the client refuses each in the other's shape.
func (a *API) listPBIGroups(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wss, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(wss))
	for _, ws := range wss {
		out = append(out, map[string]any{
			"id": ws.ID, "name": ws.DisplayName, "type": "Workspace",
			"isReadOnly": false, "isOnDedicatedCapacity": true,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

// xmlaRouting answers the client's first call.
//
// A BARE ARRAY, not a {"value": [...]} envelope — an envelope fails as
// `ConnectionException: The specified Power BI workspace (...) is not found`,
// which reads like a lookup miss and is really a shape error. `capacitySku` is
// an ENUM VALUE ("Premium"), not an SKU name: "P1" fails as
// `ArgumentException: Requested value 'P1' was not found`. `capacityUri` needs
// at least ONE PATH SEGMENT; a bare origin gives IndexOutOfRangeException.
func (a *API) xmlaRouting(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	// A LIST, and every workspace the caller can see. The client matches the
	// reply's `name` against the workspace in its own Data Source and refuses a
	// mismatch as `The specified Power BI workspace ('<x>') is not found` —
	// which reads as a lookup miss and is really the server answering about a
	// different workspace. Returning them all means the client finds its own
	// rather than this handler guessing which was meant, and sempy names a
	// workspace by GUID while a human names it by title.
	base := externalBase(r)
	wss, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(wss)*2)
	add := func(id, name string) {
		out = append(out, map[string]any{
			"id": id, "name": name, "type": "Group",
			// An ENUM VALUE, not an SKU name: "P1" fails as
			// `Requested value 'P1' was not found`.
			"capacitySku": "Premium",
			// >= 1 PATH SEGMENT; a bare origin gives IndexOutOfRangeException.
			"capacityUri": base + "/" + id,
			"tenantId":    workspaceRoutingID,
		})
	}
	for _, ws := range wss {
		// BOTH the id and the display name: the client compares by whichever
		// string its Data Source carries, and we cannot tell which that is.
		add(ws.ID, ws.ID)
		if ws.DisplayName != "" && ws.DisplayName != ws.ID {
			add(ws.ID, ws.DisplayName)
		}
	}
	if len(out) == 0 {
		add(workspaceRoutingID, "ws")
	}
	writeJSON(w, http.StatusOK, out)
}

// workspaceRoutingID is the stable id the routing reply and clusterResolve
// agree on. The client carries it forward as `capacityObjectId`.
const workspaceRoutingID = "00000000-0000-0000-0000-000000000001"

// xmlaToken answers the MWC token exchange. The contract is a single `Token`
// member, read off ADOMD.NET's own DataContract rather than guessed.
func (a *API) xmlaToken(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	mwcMu.Lock()
	mwcHolder = p
	mwcMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"Token": mwcToken})
}

// xmlaDatabaseName resolves a dataset name to the database the XMLA connection
// then opens. Only a real client issues this.
func (a *API) xmlaDatabaseName(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var req struct {
		DatasetName string `json:"datasetName"`
	}
	raw, ok := httpx.ReadBounded(r.Body, 1<<20)
	if !ok {
		writeErr(w, http.StatusRequestEntityTooLarge, "RequestTooLarge",
			"The request body is too large.")
		return
	}
	_ = json.Unmarshal(raw, &req)
	// RESOLVE, never echo. Returning the caller's own string reflects
	// user-controlled input into the response — CodeQL flags it as
	// `go/reflected-xss`, and it is also simply the wrong answer: this endpoint
	// exists to MAP a dataset name onto the database that backs it, so parroting
	// the request would "succeed" for a dataset that does not exist and the
	// client would then open a connection to nothing.
	name := ""
	for _, ws := range a.xmlaVisibleWorkspaces(p) {
		items, err := a.Store.ListItems(ws.ID, "SemanticModel")
		if err != nil {
			continue
		}
		for _, it := range items {
			if strings.EqualFold(it.DisplayName, req.DatasetName) {
				name = it.DisplayName // the STORED name, not the supplied one
			}
		}
	}
	if name == "" {
		writeErr(w, http.StatusNotFound, "DatasetNotFound",
			"No semantic model with that name is visible to this principal.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"databaseName": name})
}

// xmlaClusterResolve tells the client which host serves the workspace.
//
// The reply is deserialised as `ASAzureUtility+NameResolutionResult` —
// `clusterFQDN`, `coreServerName`, `tenantId` — and consumed as
// `new UriBuilder(dataSourceUri){ Host = r.ClusterFqdn }`. So clusterFQDN is a
// BARE FQDN: no scheme, no port. The port is inherited from the Data Source and
// was never ours to send. Answering with the sibling contract
// (`PowerBIClusterResolutionResult`, FixedClusterUri/DynamicClusterUri) leaves
// Host null and throws `UriFormatException: The hostname could not be parsed`.
// xmlaClusterResolveOpen is the unauthenticated entry point; the handler proper
// takes no principal because it reads nothing owned by one.
func (a *API) xmlaClusterResolveOpen(w http.ResponseWriter, r *http.Request) {
	a.xmlaClusterResolve(w, r, nil)
}

func (a *API) xmlaClusterResolve(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"clusterFQDN":    host,
		"coreServerName": "ws",
		"tenantId":       workspaceRoutingID,
	})
}

// xmlaEndpoint serves the XMLA conversation itself.
func (a *API) xmlaEndpoint(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	// ReadBounded, not a bare LimitReader: a LimitReader DISCARDS the excess and
	// reports success, so an oversized envelope would be parsed as a fragment —
	// a truncated Batch would silently answer fewer rowsets than were asked for,
	// which is exactly the count mismatch that breaks table naming.
	body, ok := httpx.ReadBounded(r.Body, 8<<20)
	if !ok {
		writeErr(w, http.StatusRequestEntityTooLarge, "RequestTooLarge",
			"The XMLA request body is too large.")
		return
	}
	text := string(body)

	// ORDER MATTERS: a batched Execute CONTAINS <Discover> children, so testing
	// for "<Discover" first shadows the batch entirely.
	types := reRequestType.FindAllStringSubmatch(text, -1)
	switch {
	case len(types) > 1 && strings.Contains(text, "<Batch"):
		a.xmlaBatch(w, r, p, types)
	case len(types) == 1:
		a.xmlaDiscover(w, r, p, types[0][1], text)
	case strings.Contains(text, "<Batch") &&
		(strings.Contains(text, "<Create") || strings.Contains(text, "<Alter")):
		a.xmlaWrite(w, r, p, text)
	case strings.Contains(text, "<Statement>"):
		a.xmlaExecute(w, r, p, text)
	default:
		// The session handshake: an Execute with an EMPTY <Statement/>.
		writeXMLA(w, sessionEnvelope())
	}
}

// xmlaBatch answers TOM's catalogue request: one Execute whose Command is a
// <Batch> of ~35 <Discover> elements, one <root> back per request IN ORDER.
//
// EVERY request type must yield a rowset with columns, including the ones this
// model has none of. `AmoDataAdapter.AdjustTableNames` renames the DataSet's
// tables from the <root name="..."> attributes and BAILS OUT ENTIRELY when the
// name count does not match the table count — so a single column-less rowset,
// which Fill skips, breaks naming for all the others and
// `DdlUtil.ObtainModelTable`'s `Tables["Model"]` comes back null.
func (a *API) xmlaBatch(w http.ResponseWriter, r *http.Request, p *auth.Principal,
	types [][]string) {
	model, data, err := a.xmlaModel(r, p)
	if err != nil {
		writeXMLA(w, soapFault(err.Error()))
		return
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">` +
		`<soap:Body><ExecuteResponse xmlns="urn:schemas-microsoft-com:xml-analysis">` +
		`<return><results xmlns="` + nsMultipleResults + `">`)
	for _, m := range types {
		rs, err := xmla.DiscoverRowset(model, data, m[1])
		if err != nil {
			// An unrecognised type is not "this model has none of those"; say so
			// rather than inventing an empty rowset for it.
			writeXMLA(w, soapFault(err.Error()))
			return
		}
		b.Write(rs.RootFragment())
	}
	b.WriteString(`</results></return></ExecuteResponse></soap:Body></soap:Envelope>`)
	writeXMLA(w, append([]byte(b.String()), 0x00))
}

// xmlaWrite answers TOM's SaveChanges: an Execute whose Command is a
// <Batch Transaction="true"> of <Create>/<Alter> elements in the 2014/engine
// namespace.
//
// MEASURED 2026-08-10, and it is NOT what docs/32 planned for. TOM does not
// send TMSL JSON. It sends a ROW-BASED DELTA in exactly the rowset shape this
// server EMITS for Discover: per object type an inline xs:schema followed by
// <row> elements carrying only the changed fields, keyed by the object ids we
// handed out.
func (a *API) xmlaWrite(w http.ResponseWriter, r *http.Request, p *auth.Principal,
	text string) {
	id, err := a.xmlaItemID(r, p)
	if err != nil {
		writeXMLA(w, soapFault(err.Error()))
		return
	}
	bim, err := a.definitionPartExact(id, "model.bim")
	if err != nil {
		// TMDL-serialised models are read fine and written not at all. Saying so
		// beats accepting the batch and dropping it: TOM reports SaveChanges as
		// successful whenever the server answers.
		writeXMLA(w, soapFault("this semantic model has no model.bim part, so "+
			"XMLA writes are not supported for it"))
		return
	}
	cmds, err := xmla.ParseWriteBatch(text)
	if err != nil {
		writeXMLA(w, soapFault(err.Error()))
		return
	}
	updated, err := xmla.ApplyWrite(bim, cmds)
	if err != nil {
		writeXMLA(w, soapFault(err.Error()))
		return
	}
	if err := a.replaceDefinitionPart(id, "model.bim", updated); err != nil {
		writeXMLA(w, soapFault(err.Error()))
		return
	}
	writeXMLA(w, writeAck(len(cmds)))
}

// writeAck is the response TOM accepts for a write batch.
//
// MEASURED: the empty root `sessionEnvelope` sends for a bare command is
// REFUSED here — `ResponseFormatException: The result set returned by the
// server is not a rowset` — so a write is answered with the multipleresults
// container and one rowset per command, exactly as the Discover batch is.
func writeAck(n int) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">` +
		`<soap:Body><ExecuteResponse xmlns="urn:schemas-microsoft-com:xml-analysis">` +
		`<return><results xmlns="` + nsMultipleResults + `">`)
	for i := 0; i < n; i++ {
		rs := xmla.Rowset{Columns: []string{"ID"}}
		b.Write(rs.RootFragment())
	}
	b.WriteString(`</results></return></ExecuteResponse></soap:Body></soap:Envelope>`)
	return append([]byte(b.String()), 0x00)
}

// xmlaDiscover answers a standalone Discover. DISCOVER_XML_METADATA carries an
// ASSL document rather than a rowset of its own.
func (a *API) xmlaDiscover(w http.ResponseWriter, r *http.Request, p *auth.Principal,
	requestType, text string) {
	if requestType == "DISCOVER_XML_METADATA" {
		dbScoped := strings.Contains(text, "<DatabaseID>")
		name, id := a.xmlaDatabaseIdentity(r, p)
		rs := xmla.Rowset{Columns: []string{"METADATA"}, RawCells: true,
			Rows: [][]string{{asslDocument(dbScoped, name, id)}}}
		writeXMLA(w, rs.DiscoverResponse())
		return
	}
	model, data, err := a.xmlaModel(r, p)
	if err != nil {
		writeXMLA(w, soapFault(err.Error()))
		return
	}
	rs, err := xmla.DiscoverRowset(model, data, requestType)
	if err != nil {
		writeXMLA(w, soapFault(err.Error()))
		return
	}
	writeXMLA(w, rs.DiscoverResponse())
}

// xmlaExecute runs the statement in the envelope. A DMV `SELECT ... FROM
// $SYSTEM.TMSCHEMA_*` and a DAX `EVALUATE` arrive by the SAME route and differ
// only in their grammar, which is why both are dispatched here.
func (a *API) xmlaExecute(w http.ResponseWriter, r *http.Request, p *auth.Principal,
	text string) {
	stmt := betweenTags(text, "<Statement>", "</Statement>")
	if strings.TrimSpace(stmt) == "" {
		writeXMLA(w, sessionEnvelope())
		return
	}
	model, data, err := a.xmlaModel(r, p)
	if err != nil {
		writeXMLA(w, soapFault(err.Error()))
		return
	}
	var rs xmla.Rowset
	if xmla.IsDMV(stmt) {
		rs, err = xmla.DMV(model, data, stmt)
		if err != nil {
			writeXMLA(w, soapFault(err.Error()))
			return
		}
	} else {
		res, evalErr := semanticmodel.Evaluate(model, data, stmt)
		if evalErr != nil {
			writeXMLA(w, soapFault(evalErr.Error()))
			return
		}
		rs = xmla.FromDAX(res)
	}
	writeXMLA(w, rs.ExecuteResponse())
}

// asslDocument is the DISCOVER_XML_METADATA payload, EMBEDDED as XML rather
// than escaped — an escaped string deserialises to an empty root, which the
// client reports as `Unexpected root ” (namespace ”)`.
//
// The root depends on the restriction list: <DatabaseID> present means the
// client is reading a Tabular.Database, absent a Tabular.Server.
// CompatibilityLevel must be >= 1200 (below that the database is not "tabular")
// and lives in nsCompat. `Model` is XmlIgnore on the type and must NOT appear.
func asslDocument(databaseScoped bool, dbName, dbID string) string {
	db := `<Database xmlns="` + nsEngine + `">` +
		`<Name>` + xmlEscape(dbName) + `</Name><ID>` + xmlEscape(dbID) + `</ID>` +
		`<ddl200:CompatibilityLevel xmlns:ddl200="` + nsCompat + `">1567` +
		`</ddl200:CompatibilityLevel>` +
		`<LastUpdate>2020-01-01T00:00:00</LastUpdate>` +
		`</Database>`
	if databaseScoped {
		return db
	}
	return `<Server xmlns="` + nsEngine + `"><Name>ws</Name><ID>ws</ID><Databases>` +
		`<Database><Name>` + xmlEscape(dbName) + `</Name><ID>` + xmlEscape(dbID) + `</ID>` +
		`<LastUpdate>2020-01-01T00:00:00</LastUpdate></Database>` +
		`</Databases></Server>`
}

// sessionEnvelope answers the handshake: an ExecuteResponse with an empty root,
// a Session header, and the trailing protocol byte.
func sessionEnvelope() []byte {
	return append([]byte(`<?xml version="1.0" encoding="utf-8"?>`+
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">`+
		`<soap:Header><Session xmlns="urn:schemas-microsoft-com:xml-analysis" `+
		`SessionId="`+xmlaSessionID+`"/></soap:Header><soap:Body>`+
		`<ExecuteResponse xmlns="urn:schemas-microsoft-com:xml-analysis">`+
		`<return><root xmlns="urn:schemas-microsoft-com:xml-analysis:empty"/>`+
		`</return></ExecuteResponse></soap:Body></soap:Envelope>`), 0x00)
}

func soapFault(msg string) []byte {
	return append([]byte(`<?xml version="1.0" encoding="utf-8"?>`+
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">`+
		`<soap:Body><soap:Fault><faultcode>XMLAnalysisError</faultcode>`+
		`<faultstring>`+xmlEscape(msg)+`</faultstring>`+
		`</soap:Fault></soap:Body></soap:Envelope>`), 0x00)
}

// writeXMLA sends an XMLA payload with the capability header the client
// REQUIRES on the response. Without it the client raises `InvalidDataException:
// The 'x-ms-xmlacaps-negotiation-flags' header is missing from the HTTP
// response!` in ProcessWebResponse — BEFORE reading the body, so a perfectly
// good envelope is discarded on a missing header. Selecting the empty subset
// rather than echoing the client's offer: echoing accepts every capability
// offered, including ones this server does not implement.
func writeXMLA(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("x-ms-xmlacaps-negotiation-flags", "0,0,0,0,0")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// xmlaModel resolves the semantic model this XMLA connection is bound to.
//
// The client names a DATABASE, which `getDatabaseName` mapped from a dataset
// name, so the lookup is by item name across the workspaces the principal can
// see. A miss is an error rather than an empty model: an XMLA client that
// successfully connects to nothing looks like a model with no tables, which is
// the fabrication-class failure — well formed, never errors, silently wrong.
func (a *API) xmlaModel(r *http.Request, p *auth.Principal) (*semanticmodel.Model, semanticmodel.Data, error) {
	id, err := a.xmlaItemID(r, p)
	if err != nil {
		return nil, nil, err
	}
	return a.loadSemanticModel(id, p)
}

// xmlaItemID finds the SemanticModel item backing this connection.
func (a *API) xmlaItemID(r *http.Request, p *auth.Principal) (string, error) {
	want := strings.TrimSpace(r.Header.Get("x-ms-xmlaserver"))
	wss, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		return "", err
	}
	var first string
	for _, ws := range wss {
		items, err := a.Store.ListItems(ws.ID, "SemanticModel")
		if err != nil {
			continue
		}
		for _, it := range items {
			if first == "" {
				first = it.ID
			}
			if want != "" && strings.EqualFold(it.DisplayName, want) {
				return it.ID, nil
			}
		}
	}
	if first == "" {
		return "", fmt.Errorf("no semantic model is visible to this principal")
	}
	// The header names a WORKSPACE more often than a dataset, so falling back to
	// the only model in view is right far more often than failing — but never
	// silently: a caller that wanted a specific one asked by name and got it.
	return first, nil
}

// xmlaDatabaseIdentity is the name and id the ASSL document reports, and TOM matches on it
// — `DatasetNotFoundException: Dataset 'RetailAnalysis' not found` is what an id
// here produces. The ID stays the item id so the two are still linked.
func (a *API) xmlaDatabaseIdentity(r *http.Request, p *auth.Principal) (name, id string) {
	id, err := a.xmlaItemID(r, p)
	if err != nil {
		return "model", "model"
	}
	if it := a.xmlaItem(r, p); it != nil && it.DisplayName != "" {
		return it.DisplayName, id
	}
	return id, id
}

// xmlaVisibleWorkspaces wraps the shared helper with a nil-principal guard: the
// unauthenticated clusterResolve path has no principal, and a nil deref there
// would be a crash on the one route that is deliberately open.
func (a *API) xmlaVisibleWorkspaces(p *auth.Principal) []*store.Workspace {
	if p == nil {
		return nil
	}
	return a.visibleWorkspaces(p)
}

// xmlaItem is the SemanticModel item this connection is bound to.
func (a *API) xmlaItem(r *http.Request, p *auth.Principal) *store.Item {
	want := strings.TrimSpace(r.Header.Get("x-ms-xmlaserver"))
	wss, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		return nil
	}
	var first *store.Item
	for _, ws := range wss {
		items, err := a.Store.ListItems(ws.ID, "SemanticModel")
		if err != nil {
			continue
		}
		for _, it := range items {
			if first == nil {
				first = it
			}
			if want != "" && strings.EqualFold(it.DisplayName, want) {
				return it
			}
		}
	}
	return first
}

// externalBase is the scheme://host the client should come back to. It is what
// the client saw, not what this process bound: behind the 443 forwarder every
// real client uses, those differ.
func externalBase(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "http"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	return scheme + "://" + r.Host
}

func betweenTags(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i+len(open):], close)
	if j < 0 {
		return ""
	}
	return s[i+len(open) : i+len(open)+j]
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// replaceDefinitionPart rewrites ONE part of an item's definition, leaving the
// others byte-identical.
//
// Not SetDefinition with a fresh slice: a semantic model's definition also
// carries data.json and .platform, and replacing the whole definition to change
// the model would drop the table data the DAX evaluator reads.
func (a *API) replaceDefinitionPart(itemID, path string, payload []byte) error {
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return err
	}
	encoded := base64.StdEncoding.EncodeToString(payload)
	for i := range parts {
		if parts[i].Path == path {
			parts[i].Payload = encoded
			return a.Store.SetDefinition(itemID, parts)
		}
	}
	return fmt.Errorf("no %s in definition", path)
}
