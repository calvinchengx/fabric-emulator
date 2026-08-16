package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// kustoEngine is a stand-in for the real engine (kustainer) on the Kusto REST
// contract: it records what the relay actually sent and answers in the
// documented v1 shape. It is deliberately dumb — every assertion here is about
// the emulator's half of the protocol (auth, RBAC, database isolation,
// relaying), never about KQL semantics, which only a real engine can settle.
//
// The one exception is kqlSchemaKeyword below: a fake that accepts KQL no real
// engine would accept lets a test read as a working example while the same
// command earns a 400 from kustainer. That is a narrow syntax gate, not a step
// toward parsing KQL here.
type kustoEngine struct {
	mu         sync.Mutex
	calls      []kustoCall
	databases  map[string]bool
	failCreate bool
	status     int
}

type kustoCall struct {
	path, db, csl, properties string
}

func newKustoEngine() *kustoEngine {
	return &kustoEngine{databases: map[string]bool{}}
}

func (e *kustoEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		DB         string          `json:"db"`
		CSL        string          `json:"csl"`
		Properties json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(raw, &body)
	e.mu.Lock()
	e.calls = append(e.calls, kustoCall{r.URL.Path, body.DB, body.CSL, string(body.Properties)})
	// Recorded first, then refused: a test asking what the relay sent still
	// gets its answer, exactly as it would from an engine that logged the
	// request before parsing it.
	if kw := kqlSchemaKeyword(body.CSL); kw != "" {
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		// The real engine's shape, minus its `[line:position=...]`, which this
		// fake does not attempt to reproduce: kustainer's offsets did not fall
		// where a plain scan of the command text puts them, and a position
		// invented here would be one more thing that reads true and is not.
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{
			"code":  "BadRequest",
			"@type": "Kusto.Data.Exceptions.KustoBadRequestException",
			"@message": "Request is invalid and cannot be processed: Syntax error: SYN0002: " +
				"A recognition error occurred. " + kw + " is a KQL keyword and cannot name a column; quote it as ['" + kw + "']",
		}})
		_, _ = w.Write(payload)
		return
	}
	created := ""
	switch {
	case strings.HasPrefix(body.CSL, ".create database "):
		if e.failCreate {
			e.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"BadRequest"}}`))
			return
		}
		created = strings.Fields(body.CSL)[2]
		e.databases[created] = true
	}
	names := make([]string, 0, len(e.databases))
	for name := range e.databases {
		names = append(names, name)
	}
	status := e.status
	e.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"Boom"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case body.CSL == ".show databases":
		rows := make([][]any, 0, len(names))
		for _, name := range names {
			rows = append(rows, []any{name})
		}
		payload, _ := json.Marshal(map[string]any{"Tables": []any{map[string]any{
			"TableName": "Table_0",
			"Columns":   []any{map[string]any{"ColumnName": "DatabaseName", "DataType": "String"}},
			"Rows":      rows,
		}}})
		_, _ = w.Write(payload)
	default:
		// Echo the engine-side database name back so the test can prove the
		// relay maps it home to the Fabric display name.
		payload, _ := json.Marshal(map[string]any{"Tables": []any{map[string]any{
			"TableName": "Table_0",
			"Columns":   []any{map[string]any{"ColumnName": "DatabaseName", "DataType": "String"}},
			"Rows":      [][]any{{body.DB}},
		}}})
		_, _ = w.Write(payload)
	}
}

// kqlKeywords are KQL keywords that cannot stand as a bare column name in a
// schema declaration: real Kusto wants them quoted, ['kind'], and answers the
// bare form with SYN0002.
//
// Provenance matters more than length here, because a keyword listed wrongly
// makes the fake reject KQL that real Kusto accepts — a worse fault than the
// one this gate closes. `kind` is the verified member: CI's rti suite watched
// kustainer answer `.create table Events (ts:datetime, kind:string)` with 400
// KustoBadRequestException, SYN0002, pointing at that token. The rest are
// query-language keywords from Microsoft's identifier-naming rules, which say
// a keyword used as an identifier must be quoted. They are documented rather
// than witnessed, so the list stays conservative and deliberately partial:
// absence from it is not evidence that a name is legal. Type names (string,
// datetime, ...) are left out because only the name half of `name:type` is
// ever checked, so they would add risk without adding reach.
var kqlKeywords = map[string]bool{
	"and": true, "between": true, "by": true, "contains": true,
	"datatable": true, "distinct": true, "endswith": true, "extend": true,
	"false": true, "from": true, "has": true, "in": true, "join": true,
	"kind": true, "let": true, "limit": true, "not": true, "null": true,
	"on": true, "or": true, "order": true, "parse": true, "print": true,
	"project": true, "range": true, "set": true, "sort": true,
	"startswith": true, "summarize": true, "take": true, "then": true,
	"top": true, "true": true, "union": true, "where": true, "with": true,
}

// kqlSchemaKeyword returns the first keyword the command uses as a bare column
// name, or "" if it finds none. It reads the `(name:type, ...)` groups that
// .create table, .create-merge table and datatable all share, and only where
// every member of the group has that shape — so `count()` and other call
// syntax is passed over rather than guessed at. Everything outside such a
// group (a query body, a table name, a database name) is left to the real
// engine, which is the only thing that can settle it.
func kqlSchemaKeyword(csl string) string {
	for i := 0; i < len(csl); i++ {
		if csl[i] != '(' {
			continue
		}
		end := strings.IndexByte(csl[i:], ')')
		if end < 0 {
			break
		}
		group := csl[i+1 : i+end]
		i += end
		fields := strings.Split(group, ",")
		names := make([]string, 0, len(fields))
		for _, f := range fields {
			name, typ, ok := strings.Cut(f, ":")
			name, typ = strings.TrimSpace(name), strings.TrimSpace(typ)
			if !ok || !kustoTableNameOK(name) || !kustoTableNameOK(typ) {
				names = nil
				break
			}
			names = append(names, name)
		}
		for _, name := range names {
			if kqlKeywords[strings.ToLower(name)] {
				return name
			}
		}
	}
	return ""
}

func (e *kustoEngine) sent() []kustoCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]kustoCall(nil), e.calls...)
}

// TestKQLSchemaKeywordGate pins both halves of the gate. The rejections are
// the point of it; the acceptances are what keep it from becoming the larger
// problem, a fake that refuses KQL kustainer would have run.
func TestKQLSchemaKeywordGate(t *testing.T) {
	for _, tc := range []struct{ csl, want string }{
		// The command this gate exists for, as it stood in the ADX activity
		// test, and as CI watched kustainer refuse it.
		{".create table Events (ts:datetime, kind:string)", "kind"},
		{".create-merge table T (a:string, Where:int)", "Where"},
		{".set-or-append T <| datatable (by:string) [\"x\"]", "by"},

		// Accepted: the replacement, and the forms already in this package.
		{".create table Events (DeviceId:string, At:datetime)", ""},
		{".create-merge table Readings (DeviceId:string, Temp:real, At:datetime)", ""},
		{".create-merge table T(a:string)", ""},
		{".show tables", ""},
		{".drop table Nope", ""},
		{"T | count", ""},
		// A keyword outside a schema declaration is the real engine's business:
		// the column may well exist and be quoted at its declaration.
		{"Events | summarize count() by kind", ""},
		// The relay's own create-database command, whose paren group is paths
		// rather than columns. Misreading it would fail every test here.
		{`.create database db_x persist (@"/kustodata/dbs/x/md", @"/kustodata/dbs/x/data")`, ""},
	} {
		if got := kqlSchemaKeyword(tc.csl); got != tc.want {
			t.Errorf("kqlSchemaKeyword(%q) = %q, want %q", tc.csl, got, tc.want)
		}
	}
}

// TestKustoRelayRefusesAKeywordColumn: the gate reaches the tests through the
// relay, in the engine's own error shape, so a command that could not work
// against kustainer cannot pass here either.
func TestKustoRelayRefusesAKeywordColumn(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	attachEngine(t, a)
	eh, _ := seedEventhouse(t, a, ws.ID, "telemetry")

	w := kustoPost(a, ws.ID, eh.ID, "v1", "mgmt",
		`{"db":"telemetry","csl":".create table Events (ts:datetime, kind:string)"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("mgmt = %d %s, want 400 on a keyword column", w.Code, w.Body.String())
	}
	if b := w.Body.String(); !strings.Contains(b, "SYN0002") || !strings.Contains(b, "kind") {
		t.Errorf("body %s does not carry the engine's complaint", b)
	}
}

// attachEngine wires an in-process engine plus a permissive Kusto validator.
func attachEngine(t *testing.T, a *API) *kustoEngine {
	t.Helper()
	e := newKustoEngine()
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	if err := a.SetKQLBackend(srv.URL); err != nil {
		t.Fatal(err)
	}
	return e
}

// seedEventhouse creates an eventhouse the way a real client does (typed
// collection create), so the default child database comes with it.
func seedEventhouse(t *testing.T, a *API, wid, name string) (*store.Item, *store.Item) {
	t.Helper()
	w := do(a.typedCreate("Eventhouse"), admin, http.MethodPost,
		`{"displayName":"`+name+`"}`, map[string]string{"wid": wid})
	if w.Code != http.StatusCreated {
		t.Fatalf("create eventhouse: %d %s", w.Code, w.Body.String())
	}
	var eh store.Item
	if err := json.Unmarshal(w.Body.Bytes(), &eh); err != nil {
		t.Fatal(err)
	}
	dbs, err := a.Store.ListItems(wid, "KQLDatabase")
	if err != nil || len(dbs) == 0 {
		t.Fatalf("eventhouse did not create its default database: %v %v", dbs, err)
	}
	return &eh, dbs[len(dbs)-1]
}

// kustoPost drives the relay the way the mux would, minus the bearer step
// (which the auth tests below cover on its own).
func kustoPost(a *API, wid, ehid, ver, kind, body string) *httptest.ResponseRecorder {
	return kustoPostAs(a, admin, http.MethodPost, wid, ehid, ver, kind, body)
}

func kustoPostAs(a *API, p *auth.Principal, method, wid, ehid, ver, kind, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/kusto/"+wid+"/"+ehid+"/"+ver+"/rest/"+kind, strings.NewReader(body))
	r.SetPathValue("wid", wid)
	r.SetPathValue("ehid", ehid)
	r.SetPathValue("ver", ver)
	r.SetPathValue("kind", kind)
	w := httptest.NewRecorder()
	a.kustoRelay(w, r, p)
	return w
}

// TestEventhouseCreatesDefaultDatabaseAndPublishesQueryUri pins the Fabric-side
// half of the contract: the properties a real client reads before it can speak
// Kusto at all (fabric-docs eventhouse-deploy-with-fabric-api.md).
func TestEventhouseCreatesDefaultDatabaseAndPublishesQueryUri(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	eh, db := seedEventhouse(t, a, ws.ID, "telemetry")

	if db.DisplayName != "telemetry" {
		t.Errorf("default database name = %q, want the eventhouse's own name", db.DisplayName)
	}
	w := do(a.typedGet("Eventhouse"), admin, http.MethodGet, "", map[string]string{"wid": ws.ID, "iid": eh.ID})
	var got struct {
		Properties struct {
			QueryServiceUri     string   `json:"queryServiceUri"`
			IngestionServiceUri string   `json:"ingestionServiceUri"`
			DatabasesItemIds    []string `json:"databasesItemIds"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := "http://example.com/kusto/" + ws.ID + "/" + eh.ID
	if got.Properties.QueryServiceUri != want {
		t.Errorf("queryServiceUri = %q, want %q", got.Properties.QueryServiceUri, want)
	}
	if got.Properties.IngestionServiceUri != want {
		t.Errorf("ingestionServiceUri = %q, want %q", got.Properties.IngestionServiceUri, want)
	}
	if len(got.Properties.DatabasesItemIds) != 1 || got.Properties.DatabasesItemIds[0] != db.ID {
		t.Errorf("databasesItemIds = %v, want [%s]", got.Properties.DatabasesItemIds, db.ID)
	}
}

// TestKQLDatabaseCreationPayloadRoundTrips: the documented create body carries
// parentEventhouseItemId in creationPayload, and GET returns it in properties.
func TestKQLDatabaseCreationPayloadRoundTrips(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	eh, _ := seedEventhouse(t, a, ws.ID, "telemetry")

	body := `{"displayName":"events","creationPayload":{"databaseType":"ReadWrite",` +
		`"parentEventhouseItemId":"` + eh.ID + `","oneLakeStandardStoragePeriod":"P36500D"}}`
	w := do(a.typedCreate("KQLDatabase"), admin, http.MethodPost, body, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusCreated {
		t.Fatalf("create kqlDatabase: %d %s", w.Code, w.Body.String())
	}
	var created store.Item
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	w = do(a.typedGet("KQLDatabase"), admin, http.MethodGet, "", map[string]string{"wid": ws.ID, "iid": created.ID})
	var got struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Properties["parentEventhouseItemId"] != eh.ID {
		t.Errorf("parentEventhouseItemId = %v, want %s", got.Properties["parentEventhouseItemId"], eh.ID)
	}
	if got.Properties["databaseType"] != "ReadWrite" {
		t.Errorf("databaseType = %v", got.Properties["databaseType"])
	}
	if got.Properties["oneLakeStandardStoragePeriod"] != "P36500D" {
		t.Errorf("oneLakeStandardStoragePeriod = %v", got.Properties["oneLakeStandardStoragePeriod"])
	}
	if got.Properties["queryServiceUri"] == "" {
		t.Error("KQL database is missing its queryServiceUri")
	}
	// The eventhouse now lists both databases.
	w = do(a.typedGet("Eventhouse"), admin, http.MethodGet, "", map[string]string{"wid": ws.ID, "iid": eh.ID})
	var eho struct {
		Properties struct {
			DatabasesItemIds []string `json:"databasesItemIds"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &eho)
	if len(eho.Properties.DatabasesItemIds) != 2 {
		t.Errorf("databasesItemIds = %v, want two", eho.Properties.DatabasesItemIds)
	}
}

// TestKustoRelayIsolatesDatabasesPerItem is the core of the sidecar contract:
// the caller names its Fabric database, the engine sees an isolated one, and
// the name comes home in the response.
func TestKustoRelayIsolatesDatabasesPerItem(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	e := attachEngine(t, a)
	eh, db := seedEventhouse(t, a, ws.ID, "telemetry")

	w := kustoPost(a, ws.ID, eh.ID, "v1", "mgmt",
		`{"db":"telemetry","csl":".create-merge table T(a:string)","properties":{"Options":{}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("mgmt: %d %s", w.Code, w.Body.String())
	}
	calls := e.sent()
	if len(calls) != 3 {
		t.Fatalf("engine calls = %d (%v), want show/create/command", len(calls), calls)
	}
	if calls[0].csl != ".show databases" || calls[0].path != "/v1/rest/mgmt" {
		t.Errorf("first call = %+v, want the existence probe", calls[0])
	}
	engineDB := engineDatabaseName(db.ID)
	if !strings.Contains(calls[1].csl, ".create database "+engineDB+" persist") {
		t.Errorf("create = %q, want the documented persist form for %s", calls[1].csl, engineDB)
	}
	if calls[2].db != engineDB {
		t.Errorf("relayed db = %q, want the isolated %q", calls[2].db, engineDB)
	}
	if calls[2].csl != ".create-merge table T(a:string)" {
		t.Errorf("relayed csl = %q, want it verbatim", calls[2].csl)
	}
	if calls[2].properties != `{"Options":{}}` {
		t.Errorf("relayed properties = %q, want them passed through", calls[2].properties)
	}
	if !strings.Contains(w.Body.String(), `"telemetry"`) || strings.Contains(w.Body.String(), engineDB) {
		t.Errorf("response = %s, want the engine name mapped back to the Fabric one", w.Body.String())
	}
	// A second command reuses the remembered database — no re-create.
	before := len(e.sent())
	if w := kustoPost(a, ws.ID, eh.ID, "v1", "query", `{"db":"telemetry","csl":"T | count"}`); w.Code != http.StatusOK {
		t.Fatalf("query: %d %s", w.Code, w.Body.String())
	}
	if got := len(e.sent()) - before; got != 1 {
		t.Errorf("second command made %d engine calls, want 1 (database already ensured)", got)
	}
	if last := e.sent()[len(e.sent())-1]; last.path != "/v1/rest/query" {
		t.Errorf("query went to %q", last.path)
	}
}

// TestKustoRelayVersionsAndMethods: v2 query exists, v2 mgmt does not (real
// Kusto has no /v2/rest/mgmt), and GET is not the protocol.
func TestKustoRelayVersionsAndMethods(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	e := attachEngine(t, a)
	eh, _ := seedEventhouse(t, a, ws.ID, "telemetry")

	if w := kustoPost(a, ws.ID, eh.ID, "v2", "query", `{"db":"telemetry","csl":"T | count"}`); w.Code != http.StatusOK {
		t.Fatalf("v2 query: %d %s", w.Code, w.Body.String())
	}
	if last := e.sent()[len(e.sent())-1]; last.path != "/v2/rest/query" {
		t.Errorf("v2 query relayed to %q", last.path)
	}
	if w := kustoPost(a, ws.ID, eh.ID, "v2", "mgmt", `{"db":"telemetry","csl":".show tables"}`); w.Code != http.StatusNotFound {
		t.Errorf("v2 mgmt = %d, want 404 (no such endpoint in Kusto)", w.Code)
	}
	if w := kustoPost(a, ws.ID, eh.ID, "v3", "query", `{"db":"telemetry","csl":"T"}`); w.Code != http.StatusNotFound {
		t.Errorf("v3 = %d, want 404", w.Code)
	}
	if w := kustoPostAs(a, admin, http.MethodGet, ws.ID, eh.ID, "v1", "query", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", w.Code)
	}
}

// TestKustoRelayRBAC: mgmt mutates and needs Contributor; a query needs only
// Viewer; a stranger gets nothing.
func TestKustoRelayRBAC(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	attachEngine(t, a)
	eh, _ := seedEventhouse(t, a, ws.ID, "telemetry")

	post := func(p *auth.Principal, kind, body string) int {
		return kustoPostAs(a, p, http.MethodPost, ws.ID, eh.ID, "v1", kind, body).Code
	}
	if code := post(viewer, "query", `{"db":"telemetry","csl":"T | count"}`); code != http.StatusOK {
		t.Errorf("viewer query = %d, want 200", code)
	}
	if code := post(viewer, "mgmt", `{"db":"telemetry","csl":".drop table T"}`); code != http.StatusForbidden {
		t.Errorf("viewer mgmt = %d, want 403", code)
	}
	if code := post(nobody, "query", `{"db":"telemetry","csl":"T | count"}`); code != http.StatusForbidden {
		t.Errorf("stranger query = %d, want 403", code)
	}
	if code := post(admin, "query", `{"db":"nope","csl":"T | count"}`); code != http.StatusNotFound {
		t.Errorf("unknown database = %d, want 404", code)
	}
	if code := post(admin, "query", `{"db":"telemetry","csl":"  "}`); code != http.StatusBadRequest {
		t.Errorf("empty csl = %d, want 400", code)
	}
	if code := post(admin, "query", `not json`); code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", code)
	}
}

// TestKustoRelayUnknownWorkspaceOrEventhouse
func TestKustoRelayUnknownWorkspaceOrEventhouse(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	attachEngine(t, a)
	seedEventhouse(t, a, ws.ID, "telemetry")

	if w := kustoPost(a, "no-such-workspace", "x", "v1", "query", `{"csl":"T"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown workspace = %d, want 404", w.Code)
	}
	if w := kustoPost(a, ws.ID, "no-such-eventhouse", "v1", "query", `{"csl":"T"}`); w.Code != http.StatusNotFound {
		t.Errorf("unknown eventhouse = %d, want 404", w.Code)
	}
	// A non-eventhouse item is not a cluster either.
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(nb, nil); err != nil {
		t.Fatal(err)
	}
	if w := kustoPost(a, ws.ID, nb.ID, "v1", "query", `{"csl":"T"}`); w.Code != http.StatusNotFound {
		t.Errorf("notebook as cluster = %d, want 404", w.Code)
	}
}

// TestKustoDefaultDatabaseWhenDbOmitted: `db` is optional for some management
// commands; the eventhouse's own database answers.
func TestKustoDefaultDatabaseWhenDbOmitted(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	e := attachEngine(t, a)
	eh, db := seedEventhouse(t, a, ws.ID, "telemetry")

	if w := kustoPost(a, ws.ID, eh.ID, "v1", "mgmt", `{"csl":".show tables"}`); w.Code != http.StatusOK {
		t.Fatalf("mgmt without db: %d %s", w.Code, w.Body.String())
	}
	if last := e.sent()[len(e.sent())-1]; last.db != engineDatabaseName(db.ID) {
		t.Errorf("relayed db = %q, want the eventhouse default", last.db)
	}
}

// TestKustoWithoutEngineIs501: the honest failure, in Kusto's own envelope.
func TestKustoWithoutEngineIs501(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	eh, _ := seedEventhouse(t, a, ws.ID, "telemetry")

	w := kustoPost(a, ws.ID, eh.ID, "v1", "query", `{"db":"telemetry","csl":"T | count"}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("no engine = %d, want 501", w.Code)
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Error.Code != "KQLEngineNotConfigured" {
		t.Errorf("error code = %q", wire.Error.Code)
	}
}

// TestKustoAuthLayer covers the mounted route end to end: an unwired validator
// 501s, a bad/absent bearer 401s with a challenge, and the path values the
// relay depends on are what the mux extracts.
func TestKustoAuthLayer(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	attachEngine(t, a)
	eh, _ := seedEventhouse(t, a, ws.ID, "telemetry")

	mux := http.NewServeMux()
	a.registerKQL(mux)
	call := func(bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/kusto/"+ws.ID+"/"+eh.ID+"/v1/rest/query",
			strings.NewReader(`{"csl":"T"}`))
		if bearer != "" {
			r.Header.Set("Authorization", bearer)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	a.KQLAuth = nil
	if w := call("Bearer t"); w.Code != http.StatusNotImplemented {
		t.Errorf("no validator = %d, want 501", w.Code)
	}
	a.KQLAuth = &auth.Validator{Issuer: "https://entra.example/tenant/v2.0"}
	w := call("")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer = %d, want 401", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 is missing its WWW-Authenticate challenge")
	}
	if w := call("Bearer not-a-jws"); w.Code != http.StatusUnauthorized {
		t.Errorf("garbage bearer = %d, want 401", w.Code)
	}
}

// TestKustoEngineFailuresSurface: a broken engine is a 502, never a fake result.
func TestKustoEngineFailuresSurface(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	e := attachEngine(t, a)
	eh, _ := seedEventhouse(t, a, ws.ID, "telemetry")

	e.failCreate = true
	if w := kustoPost(a, ws.ID, eh.ID, "v1", "query", `{"db":"telemetry","csl":"T"}`); w.Code != http.StatusBadGateway {
		t.Errorf("failed create = %d, want 502", w.Code)
	}
	e.failCreate = false
	e.status = http.StatusInternalServerError
	if w := kustoPost(a, ws.ID, eh.ID, "v1", "query", `{"db":"telemetry","csl":"T"}`); w.Code != http.StatusBadGateway {
		t.Errorf("failed probe = %d, want 502", w.Code)
	}
	// An unreachable engine is a 502 too.
	e.status = 0
	if err := a.SetKQLBackend("http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if w := kustoPost(a, ws.ID, eh.ID, "v1", "query", `{"db":"telemetry","csl":"T"}`); w.Code != http.StatusBadGateway {
		t.Errorf("unreachable engine = %d, want 502", w.Code)
	}
	// An engine error on the command itself relays its own status verbatim.
	e2 := attachEngine(t, a)
	if w := kustoPost(a, ws.ID, eh.ID, "v1", "query", `{"db":"telemetry","csl":"T"}`); w.Code != http.StatusOK {
		t.Fatalf("warm-up: %d", w.Code)
	}
	e2.status = http.StatusBadRequest
	if w := kustoPost(a, ws.ID, eh.ID, "v1", "query", `{"db":"telemetry","csl":"bad"}`); w.Code != http.StatusBadRequest {
		t.Errorf("engine 400 relayed as %d, want 400", w.Code)
	}
}

// TestKustoDatabaseAlreadyOnEngine: a restarted emulator against a live engine
// must not try to re-create the database (the persist form would fail).
func TestKustoDatabaseAlreadyOnEngine(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	e := attachEngine(t, a)
	eh, db := seedEventhouse(t, a, ws.ID, "telemetry")
	e.databases[engineDatabaseName(db.ID)] = true

	if w := kustoPost(a, ws.ID, eh.ID, "v1", "query", `{"db":"telemetry","csl":"T"}`); w.Code != http.StatusOK {
		t.Fatalf("query: %d %s", w.Code, w.Body.String())
	}
	for _, call := range e.sent() {
		if strings.HasPrefix(call.csl, ".create database") {
			t.Error("re-created a database the engine already had")
		}
	}
}

// TestSetKQLBackend
func TestSetKQLBackend(t *testing.T) {
	a, _ := newAPI(t)
	if err := a.SetKQLBackend("not a url"); err == nil {
		t.Error("SetKQLBackend accepted a non-URL")
	}
	if err := a.SetKQLBackend("http://kustainer:8080"); err != nil {
		t.Fatal(err)
	}
	if a.KQLURL == nil || a.KQLHTTP == nil {
		t.Fatal("backend not attached")
	}
	if err := a.SetKQLBackend(""); err != nil || a.KQLURL != nil {
		t.Errorf("detaching failed: %v", err)
	}
	if _, _, err := a.callKusto(t.Context(), "v1", "query", kustoRequest{}); err == nil {
		t.Error("callKusto with no engine should fail")
	}
}

// TestKustoV1HasValue covers the response-shape reader's edges.
func TestKustoV1HasValue(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"malformed", `{`, false},
		{"no such column", `{"Tables":[{"Columns":[{"ColumnName":"Other"}],"Rows":[["x"]]}]}`, false},
		{"short row", `{"Tables":[{"Columns":[{"ColumnName":"DatabaseName"},{"ColumnName":"X"}],"Rows":[[]]}]}`, false},
		{"non-string cell", `{"Tables":[{"Columns":[{"ColumnName":"DatabaseName"}],"Rows":[[7]]}]}`, false},
		{"hit", `{"Tables":[{"Columns":[{"ColumnName":"DatabaseName"}],"Rows":[["db1"]]}]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := kustoV1HasValue([]byte(tc.payload), "DatabaseName", "db1"); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestKustoBaseURIScheme: the cluster URI must be reachable on whatever
// scheme/host reached us — TLS, plain, or behind a proxy.
func TestKustoBaseURIScheme(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://api.fabric.example/x", nil)
	r.TLS = &tls.ConnectionState{}
	if got := kustoBaseURI(r, "w", "e"); !strings.HasPrefix(got, "https://api.fabric.example/kusto/") {
		t.Errorf("TLS request gave %q", got)
	}
	r = httptest.NewRequest(http.MethodGet, "http://api.fabric.example/x", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := kustoBaseURI(r, "w", "e"); !strings.HasPrefix(got, "https://") {
		t.Errorf("forwarded proto gave %q", got)
	}
}

// TestItemViewLeavesOtherTypesAlone: only RTI items grow a properties object.
func TestItemViewLeavesOtherTypesAlone(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	nb := &store.Item{WorkspaceID: ws.ID, Type: "Notebook", DisplayName: "nb"}
	if err := st.CreateItem(nb, nil); err != nil {
		t.Fatal(err)
	}
	w := do(a.getItem, admin, http.MethodGet, "", map[string]string{"wid": ws.ID, "iid": nb.ID})
	if strings.Contains(w.Body.String(), "properties") {
		t.Errorf("notebook grew properties: %s", w.Body.String())
	}
	// A KQL database with no stored parent still reports its type.
	orphan := &store.Item{WorkspaceID: ws.ID, Type: "KQLDatabase", DisplayName: "orphan"}
	if err := st.CreateItem(orphan, nil); err != nil {
		t.Fatal(err)
	}
	w = do(a.getItem, admin, http.MethodGet, "", map[string]string{"wid": ws.ID, "iid": orphan.ID})
	var got struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Properties["databaseType"] != "ReadWrite" {
		t.Errorf("orphan database properties = %v", got.Properties)
	}
	if _, ok := got.Properties["queryServiceUri"]; ok {
		t.Error("a parentless KQL database must not advertise a cluster URI")
	}
}

// TestEventhouseDuplicateChildNameIsTolerated: a pre-existing KQL database
// with the eventhouse's name must not break the create.
func TestEventhouseDuplicateChildNameIsTolerated(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	clash := &store.Item{WorkspaceID: ws.ID, Type: "KQLDatabase", DisplayName: "telemetry"}
	if err := st.CreateItem(clash, nil); err != nil {
		t.Fatal(err)
	}
	w := do(a.typedCreate("Eventhouse"), admin, http.MethodPost,
		`{"displayName":"telemetry"}`, map[string]string{"wid": ws.ID})
	if w.Code != http.StatusCreated {
		t.Fatalf("create eventhouse: %d %s", w.Code, w.Body.String())
	}
}

// TestAnOversizedEngineResponseIsRefusedNotRelayedShort covers the last read in
// the truncation sweep, and the one with the most surprising blast radius.
//
// callKusto's body is handed to the client as the ENGINE's answer. Truncated,
// it becomes a result set that parses (KQL responses are JSON arrays of tables,
// and a cut one can still be valid) but is missing rows — a query that silently
// returns less data than it matched. Refusing is the only honest option; there
// is no partial answer worth relaying.
func TestAnOversizedEngineResponseIsRefusedNotRelayedShort(t *testing.T) {
	a, _ := newAPI(t)
	// An engine that streams past the ceiling. Written in chunks with no
	// Content-Length, as a genuinely large result set would arrive.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 1<<20)
		for sent := int64(0); sent <= int64(httpx.MaxProxyBody); sent += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	if err := a.SetKQLBackend(srv.URL); err != nil {
		t.Fatal(err)
	}

	status, payload, err := a.callKusto(context.Background(), "v1", "query",
		kustoRequest{CSL: "print 1"})
	if err == nil {
		t.Fatalf("an oversized engine response was relayed: status %d, %d bytes",
			status, len(payload))
	}
	if payload != nil {
		t.Fatalf("a refused relay still returned %d bytes", len(payload))
	}
}

// TestAnEngineResponseInsideTheCeilingIsRelayedWhole keeps the test above from
// passing on a relay that had simply broken.
func TestAnEngineResponseInsideTheCeilingIsRelayedWhole(t *testing.T) {
	a, _ := newAPI(t)
	body := strings.Repeat("k", 2<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	if err := a.SetKQLBackend(srv.URL); err != nil {
		t.Fatal(err)
	}

	status, payload, err := a.callKusto(context.Background(), "v1", "query",
		kustoRequest{CSL: "print 1"})
	if err != nil || status != http.StatusOK {
		t.Fatalf("a 2 MiB engine response failed: status %d, %v", status, err)
	}
	if len(payload) != len(body) {
		t.Fatalf("relayed %d bytes of %d", len(payload), len(body))
	}
}
