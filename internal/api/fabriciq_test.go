package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric IQ MCP. The suite's `admin` is a service principal, which this server
// refuses, so the tests act as users: `owner` (workspace Admin), `viewer`
// (Viewer, and a member of the retail model's West role), `sharee` (no
// workspace role; the model shared with them for Read) and `stranger`.

var (
	owner  = &auth.Principal{ID: "owner-1", Type: "User", UPN: "owner@contoso.com"}
	sharee = &auth.Principal{ID: "sharee-1", Type: "User"}
)

type iqFixture struct {
	a      *API
	st     *store.Store
	ws     *store.Workspace
	model  *store.Item
	report *store.Item
}

func defPart(path, body string) store.DefinitionPart {
	return store.DefinitionPart{Path: path, PayloadType: "InlineBase64", Payload: b64(body)}
}

// reportParts is a PBIR report over the retail model: one page filtered to
// exclude East, one bar chart of units by territory, and a report measure.
func reportParts(pbir string) []store.DefinitionPart {
	return []store.DefinitionPart{
		defPart("definition.pbir", pbir),
		defPart("definition/report.json", `{"filterConfig":{"filters":[]}}`),
		defPart("definition/reportExtensions.json", `{"name":"extension","entities":[{"name":"Sales",
		  "measures":[{"name":"Double Units","expression":"[TotalUnits] * 2"}]}]}`),
		defPart("definition/pages/pages.json", `{"pageOrder":["p1"]}`),
		defPart("definition/pages/p1/page.json", `{"name":"p1","displayName":"Territories","filterConfig":{"filters":[
		  {"name":"notEast","type":"Categorical",
		   "field":{"Column":{"Expression":{"SourceRef":{"Entity":"Store"}},"Property":"Territory"}},
		   "filter":{"Version":2,"From":[{"Name":"s","Entity":"Store","Type":0}],
		     "Where":[{"Condition":{"Not":{"Expression":{"In":{
		       "Expressions":[{"Column":{"Expression":{"SourceRef":{"Source":"s"}},"Property":"Territory"}}],
		       "Values":[[{"Literal":{"Value":"'East'"}}]]}}}}}]}}]}}`),
		defPart("definition/pages/p1/visuals/v1/visual.json", `{"name":"v1","visual":{"visualType":"clusteredBarChart",
		  "query":{"queryState":{
		    "Category":{"projections":[{"field":{"Column":{"Expression":{"SourceRef":{"Entity":"Store"}},"Property":"Territory"}}}]},
		    "Y":{"projections":[{"field":{"Measure":{"Expression":{"SourceRef":{"Entity":"Sales"}},"Property":"TotalUnits"}}}]}}},
		  "visualContainerObjects":{"title":[{"properties":{"text":{"expr":{"Literal":{"Value":"'Units by territory'"}}}}}]}}}`),
	}
}

func byConnection(modelID string) string {
	return `{"version":"4.0","datasetReference":{"byConnection":{"connectionString":` +
		`"Data Source=powerbi://api.powerbi.com/v1.0/myorg/w;Initial Catalog=Retail;semanticmodelid=` + modelID + `"}}}`
}

func newIQ(t *testing.T) *iqFixture {
	t.Helper()
	a, st := newAPI(t)
	ws := &store.Workspace{DisplayName: "Sales analytics"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: owner.ID, Type: owner.Type}); err != nil {
		t.Fatal(err)
	}
	assignRole(t, st, ws.ID, viewer, store.RoleViewer)
	model := securedRetail(t, st, ws.ID) // "SecuredRetail", West role admitting viewer-1
	report := &store.Item{WorkspaceID: ws.ID, Type: "Report", DisplayName: "Retail Sales Report"}
	if err := st.CreateItem(report, reportParts(byConnection(model.ID))); err != nil {
		t.Fatal(err)
	}
	return &iqFixture{a: a, st: st, ws: ws, model: model, report: report}
}

// iqPost sends one JSON-RPC request to the Fabric IQ endpoint.
func iqPost(a *API, p *auth.Principal, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/mcp/fabriciq", strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	a.mcpPost(fabricIQ)(w, r, p)
	return w
}

// iqCall calls a tool and returns its text and whether it is a tool error.
func iqCall(t *testing.T, a *API, p *auth.Principal, tool string, args map[string]any) (string, bool) {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	w := iqPost(a, p, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(params)+`}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("%s as %s: HTTP %d %s", tool, p.ID, w.Code, w.Body)
	}
	var env struct {
		Result mcpToolResult `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || len(env.Result.Content) == 0 {
		t.Fatalf("%s: %s", tool, w.Body)
	}
	return env.Result.Content[0].Text, env.Result.IsError
}

// iqJSON calls a tool that must succeed and decodes its document.
func iqJSON(t *testing.T, a *API, p *auth.Principal, tool string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := iqCall(t, a, p, tool, args)
	if isErr {
		t.Fatalf("%s as %s: %s", tool, p.ID, text)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("%s: %s", tool, text)
	}
	return doc
}

// iqRefused calls a tool that must fail, and checks the reason.
func iqRefused(t *testing.T, a *API, p *auth.Principal, tool string, args map[string]any, want string) {
	t.Helper()
	text, isErr := iqCall(t, a, p, tool, args)
	if !isErr || !strings.Contains(text, want) {
		t.Errorf("%s as %s %v: %v %q, want a tool error containing %q", tool, p.ID, args, isErr, text, want)
	}
}

func dig(v any, path ...any) any {
	for _, k := range path {
		switch key := k.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[key]
		case int:
			s, _ := v.([]any)
			if key >= len(s) {
				return nil
			}
			v = s[key]
		}
	}
	return v
}

func TestFabricIQServesExactlyTheSixDocumentedToolsReadOnly(t *testing.T) {
	f := newIQ(t)
	w := iqPost(f.a, owner, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, nil)
	var env map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if dig(env, "result", "serverInfo", "name") != "fabric-iq" || w.Header().Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize: %s", w.Body)
	}
	w = iqPost(f.a, owner, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil)
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	var names []string
	for _, tool := range dig(env, "result", "tools").([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
		if dig(tool, "annotations", "readOnlyHint") != true {
			t.Errorf("%v is not marked read-only", tool.(map[string]any)["name"])
		}
	}
	slices.Sort(names)
	want := []string{"DiscoverArtifacts", "ExecuteQuery", "GetReportMetadata", "GetSemanticModelSchema", "ResolveFabricItem", "ValueSearch"}
	if !slices.Equal(names, want) {
		t.Errorf("tools %v, want Microsoft's six %v", names, want)
	}
	// The Core server's tools are not here, and these are not there.
	if text, isErr := iqCall(t, f.a, owner, "list_workspaces", nil); !isErr || !strings.Contains(text, "unknown tool") {
		t.Errorf("a Core tool on the IQ endpoint: %v %s", isErr, text)
	}
}

func TestFabricIQRefusesServicePrincipalsAndUnknownVariants(t *testing.T) {
	f := newIQ(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	w := iqPost(f.a, admin, ping, nil)
	if w.Code != http.StatusForbidden || errorCode(t, w) != "ServicePrincipalNotSupported" {
		t.Errorf("service principal: %d %s", w.Code, w.Body)
	}
	for _, v := range []string{"", fabricIQVariant, strings.ToLower(fabricIQVariant)} {
		if w := iqPost(f.a, owner, ping, map[string]string{"X-Variants": v}); w.Code != http.StatusOK {
			t.Errorf("X-Variants %q: %d %s", v, w.Code, w.Body)
		}
	}
	w = iqPost(f.a, owner, ping, map[string]string{"X-Variants": "Fabric.Routing.FabricIQ.V2"})
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "UnsupportedVariant" ||
		!strings.Contains(w.Body.String(), fabricIQVariant) {
		t.Errorf("unknown variant: %d %s", w.Code, w.Body)
	}
	// GET and DELETE are held to the same rules.
	r := httptest.NewRequest("GET", "/v1/mcp/fabriciq", nil)
	rec := httptest.NewRecorder()
	f.a.mcpGet(fabricIQ)(rec, r, admin)
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET as a service principal: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	f.a.mcpDelete(fabricIQ)(rec, httptest.NewRequest("DELETE", "/v1/mcp/fabriciq", nil), owner)
	if rec.Code != http.StatusNoContent {
		t.Errorf("DELETE: %d", rec.Code)
	}
}

func TestDiscoverArtifactsFindsWhatTheCallerCanReadReportsFirst(t *testing.T) {
	f := newIQ(t)
	doc := iqJSON(t, f.a, owner, "DiscoverArtifacts", map[string]any{"searchQuery": "retail"})
	arts := doc["Artifacts"].([]any)
	if len(arts) != 2 || dig(arts, 0, "Type") != "Report" || dig(arts, 1, "Type") != "SemanticModel" {
		t.Fatalf("want the report before the model: %v", arts)
	}
	if dig(arts, 0, "SemanticModelId") != f.model.ID || dig(arts, 0, "WorkspaceName") != "Sales analytics" {
		t.Errorf("the report should name its model and workspace: %v", arts[0])
	}
	// An exact name outranks a name that only contains the query.
	doc = iqJSON(t, f.a, owner, "DiscoverArtifacts", map[string]any{"searchQuery": "SecuredRetail"})
	if dig(doc, "Artifacts", 0, "ArtifactId") != f.model.ID {
		t.Errorf("exact name first: %v", doc)
	}
	// Every word, across name, description and workspace name.
	doc = iqJSON(t, f.a, owner, "DiscoverArtifacts", map[string]any{"searchQuery": "analytics report"})
	if doc["Count"].(float64) != 1 {
		t.Errorf("words across fields: %v", doc)
	}
	doc = iqJSON(t, f.a, owner, "DiscoverArtifacts", map[string]any{"searchQuery": "retail",
		"artifactTypes": []any{"SemanticModel"}, "maxResults": 1})
	if doc["Count"].(float64) != 1 || dig(doc, "Artifacts", 0, "Type") != "SemanticModel" {
		t.Errorf("type filter: %v", doc)
	}
	// No workspace role, but the model shared for Read: found. The report is not shared.
	if err := f.st.PutItemAccess(store.ItemAccess{ItemID: f.model.ID, PrincipalID: sharee.ID,
		PrincipalType: "User", Permissions: []string{store.PermRead}}); err != nil {
		t.Fatal(err)
	}
	doc = iqJSON(t, f.a, sharee, "DiscoverArtifacts", map[string]any{"searchQuery": "retail"})
	if doc["Count"].(float64) != 1 || dig(doc, "Artifacts", 0, "ArtifactId") != f.model.ID {
		t.Errorf("a model shared directly: %v", doc)
	}
	doc = iqJSON(t, f.a, stranger, "DiscoverArtifacts", map[string]any{"searchQuery": "retail"})
	if doc["Count"].(float64) != 0 {
		t.Errorf("a stranger found %v", doc)
	}
	for args, want := range map[string]map[string]any{
		"searchQuery is required":     {"searchQuery": " "},
		"not Report or SemanticModel": {"searchQuery": "x", "artifactTypes": []any{"Dashboard"}},
		"must be a list":              {"searchQuery": "x", "artifactTypes": 7},
		"from 1 to 50":                {"searchQuery": "x", "maxResults": 51},
	} {
		iqRefused(t, f.a, owner, "DiscoverArtifacts", want, args)
	}
}

func TestResolveFabricItemReadsGUIDsAndBrowserURLs(t *testing.T) {
	f := newIQ(t)
	lake := &store.Item{WorkspaceID: f.ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := f.st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		f.report.ID: "Report",
		"https://app.powerbi.com/groups/" + f.ws.ID + "/reports/" + f.report.ID + "/ReportSection?experience=power-bi": "Report",
		"https://app.fabric.microsoft.com/groups/" + f.ws.ID + "/datasets/" + f.model.ID + "/details":                  "SemanticModel",
		"https://app.powerbi.com/groups/me/semanticmodels/" + f.model.ID:                                               "SemanticModel",
	} {
		doc := iqJSON(t, f.a, owner, "ResolveFabricItem", map[string]any{"fabricItemId": in})
		if doc["itemType"] != want || doc["workspaceId"] != f.ws.ID || doc["instructions"] == nil {
			t.Errorf("%s: %v", in, doc)
		}
	}
	for in, want := range map[string]string{
		"":            "fabricItemId is required",
		"not a thing": "neither an item GUID nor a URL",
		lake.ID:       "is a Lakehouse",
		"https://app.powerbi.com/groups/me/apps/" + f.ws.ID + "/reports/" + f.report.ID: "Workspace-app URLs",
		"https://app.powerbi.com/links/abc123":                                          "Share links",
		"https://app.powerbi.com/home":                                                  "not a report or semantic model URL",
		// The item exists, but not in the workspace the URL names.
		"https://app.powerbi.com/groups/" + lake.ID + "/reports/" + f.report.ID: "that you can access",
	} {
		iqRefused(t, f.a, owner, "ResolveFabricItem", map[string]any{"fabricItemId": in}, want)
	}
	iqRefused(t, f.a, stranger, "ResolveFabricItem", map[string]any{"fabricItemId": f.report.ID}, "that you can access")
}

func TestGetReportMetadataDescribesPagesVisualsFiltersAndTheModel(t *testing.T) {
	f := newIQ(t)
	doc := iqJSON(t, f.a, viewer, "GetReportMetadata", map[string]any{"reportObjectId": f.report.ID})
	if doc["semanticModel"] != f.model.ID {
		t.Errorf("semanticModel = %v, want the model id from definition.pbir", doc["semanticModel"])
	}
	page := dig(doc, "ReportMetadata", "Pages", 0)
	if dig(page, "Title") != "Territories" || dig(page, "Visuals", 0, "Title") != "Units by territory" {
		t.Errorf("page: %v", page)
	}
	if dig(page, "Filters", 0, "Operator") != "NotIn" || dig(page, "Filters", 0, "Field") != "'Store'[Territory]" {
		t.Errorf("page filter: %v", dig(page, "Filters"))
	}
	if dig(doc, "ReportMetadata", "Measures", 0, "Expression") != "[TotalUnits] * 2" {
		t.Errorf("report measures: %v", dig(doc, "ReportMetadata", "Measures"))
	}

	// queries: the skill's own example shape, with regex_match ignoring case.
	doc = iqJSON(t, f.a, viewer, "GetReportMetadata", map[string]any{"reportObjectId": f.report.ID, "queries": []any{
		"ReportMetadata.Pages[].Title",
		"ReportMetadata.Pages[].Visuals[?regex_match(to_string(@), 'UNITS|revenue')] | [] | [:10].Title",
	}})
	res := doc["Results"].([]any)
	if len(res) != 2 || dig(res, 0, "Result", 0) != "Territories" || dig(res, 1, "Result", 0) != "Units by territory" {
		t.Errorf("queries: %v", doc)
	}
	iqRefused(t, f.a, viewer, "GetReportMetadata", map[string]any{"reportObjectId": f.report.ID, "queries": []any{"Pages[?"}}, "JMESPath")
	iqRefused(t, f.a, viewer, "GetReportMetadata", map[string]any{"reportObjectId": f.report.ID, "queries": 3}, "list of JMESPath")
	iqRefused(t, f.a, viewer, "GetReportMetadata", map[string]any{"reportObjectId": f.model.ID}, "is a SemanticModel")
	iqRefused(t, f.a, stranger, "GetReportMetadata", map[string]any{"reportObjectId": f.report.ID}, "that you can access")
}

func TestAReportBoundByPathOrToNothingSaysSo(t *testing.T) {
	f := newIQ(t)
	byPath := &store.Item{WorkspaceID: f.ws.ID, Type: "Report", DisplayName: "By path"}
	if err := f.st.CreateItem(byPath, reportParts(`{"datasetReference":{"byPath":{"path":"../SecuredRetail.SemanticModel"}}}`)); err != nil {
		t.Fatal(err)
	}
	if doc := iqJSON(t, f.a, owner, "GetReportMetadata", map[string]any{"reportObjectId": byPath.ID}); doc["semanticModel"] != f.model.ID {
		t.Errorf("byPath resolves by name in the workspace: %v", doc["semanticModel"])
	}
	for name, pbir := range map[string]string{
		"gone":    byConnection("00000000-0000-0000-0000-000000000000"),
		"no path": `{"datasetReference":{"byPath":{"path":"../Nothing.SemanticModel"}}}`,
	} {
		it := &store.Item{WorkspaceID: f.ws.ID, Type: "Report", DisplayName: name}
		if err := f.st.CreateItem(it, reportParts(pbir)); err != nil {
			t.Fatal(err)
		}
		doc := iqJSON(t, f.a, owner, "GetReportMetadata", map[string]any{"reportObjectId": it.ID})
		if doc["semanticModel"] != nil || doc["Warnings"] == nil {
			t.Errorf("%s: an unresolved model must be null with a warning: %v", name, doc)
		}
	}
	broken := &store.Item{WorkspaceID: f.ws.ID, Type: "Report", DisplayName: "broken"}
	if err := f.st.CreateItem(broken, []store.DefinitionPart{defPart("definition.pbir", `{}`)}); err != nil {
		t.Fatal(err)
	}
	iqRefused(t, f.a, owner, "GetReportMetadata", map[string]any{"reportObjectId": broken.ID}, "could not be read")
}

func TestGetSemanticModelSchemaIsTheModelAsTheCallerMaySeeIt(t *testing.T) {
	f := newIQ(t)
	doc := iqJSON(t, f.a, owner, "GetSemanticModelSchema", map[string]any{"artifactId": f.model.ID})
	schema := doc["schema"].(map[string]any)
	var tables []string
	for _, tb := range schema["Tables"].([]any) {
		tables = append(tables, tb.(map[string]any)["Name"].(string))
	}
	if !slices.Contains(tables, "Store") || !slices.Contains(tables, "Sales") {
		t.Errorf("tables %v", tables)
	}
	rels := schema["ActiveRelationships"].([]any)
	if !slices.ContainsFunc(rels, func(r any) bool {
		return dig(r, "PK") == "'Store'[StoreId]" && dig(r, "FK") == "'Sales'[StoreId]"
	}) {
		t.Errorf("relationships should name the one side PK and the many side FK: %v", rels)
	}
	if schema["CustomInstructions"] != nil || len(schema["VerifiedAnswers"].([]any)) != 0 {
		t.Errorf("prep-for-AI objects are not modelled and must be present and empty: %v", schema)
	}
	// The skill's queries: measures by keyword, and the priority paths.
	doc = iqJSON(t, f.a, owner, "GetSemanticModelSchema", map[string]any{"artifactId": f.model.ID, "queries": []any{
		"schema.Tables[].Measures[?regex_match(to_string(@), 'units delta')].{Name: Name, Expression: Expression} | []",
		"schema.VerifiedAnswers[].{Title: Title, Question: Question}",
		"schema.CustomInstructions",
	}})
	if dig(doc, "Results", 0, "Result", 0, "Name") != "Units Delta" {
		t.Errorf("measure search: %v", doc)
	}

	// Object-level security: a role that hides PostalCode hides it from its member.
	ws2 := &store.Workspace{DisplayName: "OLS"}
	if err := f.st.CreateWorkspace(ws2, store.Principal{ID: owner.ID, Type: owner.Type}); err != nil {
		t.Fatal(err)
	}
	assignRole(t, f.st, ws2.ID, viewer, store.RoleViewer)
	hidden := securedRetailWith(t, f.st, ws2.ID, `"roles":[{"name":"NoPostal","modelPermission":"read",
	  "members":[{"memberId":"viewer-1"}],
	  "tablePermissions":[{"name":"Store","columnPermissions":[{"name":"PostalCode","metadataPermission":"none"}]}]}],`)
	storeCols := func(p *auth.Principal) []any {
		d := iqJSON(t, f.a, p, "GetSemanticModelSchema", map[string]any{"artifactId": hidden.ID,
			"queries": []any{"schema.Tables[?Name == 'Store'] | [0].Columns[].Name"}})
		return dig(d, "Results", 0, "Result").([]any)
	}
	if slices.Contains(storeCols(viewer), any("PostalCode")) {
		t.Errorf("viewer sees a column object-level security hides")
	}
	if !slices.Contains(storeCols(owner), any("PostalCode")) {
		t.Errorf("the owner (Write) is not restricted and should see PostalCode")
	}
	iqRefused(t, f.a, owner, "GetSemanticModelSchema", map[string]any{"artifactId": f.report.ID}, "is a Report")
}

func TestValueSearchReturnsTheModelsOwnSpellingWithinTheCallersRows(t *testing.T) {
	f := newIQ(t)
	doc := iqJSON(t, f.a, owner, "ValueSearch", map[string]any{"artifactId": f.model.ID, "searchTerms": []any{"west", "store"}})
	west := dig(doc, "Results", 0, "Matches", 0)
	if dig(west, "Value") != "West" || dig(west, "ColumnReference") != "'Store'[Territory]" || dig(west, "MatchType") != "Exact" {
		t.Errorf("west: %v", west)
	}
	stores := dig(doc, "Results", 1, "Matches").([]any)
	if len(stores) != 4 || dig(stores, 0, "MatchType") != "Contains" {
		t.Errorf("'store' should find the four store names by containment: %v", stores)
	}
	// Row-level security: the viewer's rows are the West stores only.
	doc = iqJSON(t, f.a, viewer, "ValueSearch", map[string]any{"artifactId": f.model.ID, "searchTerms": []any{"East"}})
	if n := len(dig(doc, "Results", 0, "Matches").([]any)); n != 0 {
		t.Errorf("viewer found %d East values the West role hides", n)
	}
	doc = iqJSON(t, f.a, owner, "ValueSearch", map[string]any{"artifactId": f.model.ID, "searchTerms": []any{"East"}})
	if n := len(dig(doc, "Results", 0, "Matches").([]any)); n != 1 {
		t.Errorf("owner found %d East values, want 1", n)
	}
	// scope narrows the search; a name the model lacks is refused.
	doc = iqJSON(t, f.a, owner, "ValueSearch", map[string]any{"artifactId": f.model.ID, "searchTerms": []any{"West"},
		"scope": []any{"'Store'[Store]"}})
	if n := len(dig(doc, "Results", 0, "Matches").([]any)); n != 0 {
		t.Errorf("scope to Store[Store] still matched Territory: %v", doc)
	}
	doc = iqJSON(t, f.a, owner, "ValueSearch", map[string]any{"artifactId": f.model.ID, "searchTerms": []any{"West"},
		"scope": []any{"Store"}})
	if n := len(dig(doc, "Results", 0, "Matches").([]any)); n != 1 {
		t.Errorf("scope to the Store table: %v", doc)
	}
	for want, args := range map[string]map[string]any{
		"not a column of this model": {"artifactId": f.model.ID, "searchTerms": []any{"x"}, "scope": []any{"Store[Nope]"}},
		"not a table of this model":  {"artifactId": f.model.ID, "searchTerms": []any{"x"}, "scope": []any{"Nope"}},
		"scope must be a list":       {"artifactId": f.model.ID, "searchTerms": []any{"x"}, "scope": 1},
		"searchTerms is required":    {"artifactId": f.model.ID},
	} {
		iqRefused(t, f.a, owner, "ValueSearch", args, want)
	}
}

func TestExecuteQueryRunsDAXAsTheCallerWithReadAlone(t *testing.T) {
	f := newIQ(t)
	q := `EVALUATE SUMMARIZECOLUMNS('Store'[Territory], "Units", [TotalUnits]) ORDER BY [Units] DESC`

	// The viewer holds Read and no Build. executeQueries refuses them; Fabric IQ
	// does not ("You don't need … Build permission"), and their role still applies.
	if w := do(f.a.executeQueries, viewer, "POST", `{"queries":[{"query":"EVALUATE 'Store'"}]}`,
		map[string]string{"datasetId": f.model.ID}); w.Code != http.StatusForbidden {
		t.Fatalf("executeQueries without Build = %d, the premise of this test", w.Code)
	}
	doc := iqJSON(t, f.a, viewer, "ExecuteQuery", map[string]any{"artifactId": f.model.ID, "daxQueries": []any{q}})
	rows := dig(doc, "Results", 0, "Rows").([]any)
	if len(rows) != 1 || dig(rows, 0, "Store[Territory]") != "West" {
		t.Errorf("viewer's rows %v, want West only", rows)
	}
	// The owner sees every territory, sorted by the ORDER BY.
	doc = iqJSON(t, f.a, owner, "ExecuteQuery", map[string]any{"artifactId": f.model.ID, "daxQueries": []any{q}})
	rows = dig(doc, "Results", 0, "Rows").([]any)
	if len(rows) != 3 || dig(rows, 0, "[Units]").(float64) < dig(rows, 2, "[Units]").(float64) {
		t.Errorf("owner's rows %v, want three territories, largest first", rows)
	}
	// maxRows truncates and says so; RowCount is the full count.
	doc = iqJSON(t, f.a, owner, "ExecuteQuery", map[string]any{"artifactId": f.model.ID, "daxQueries": []any{q}, "maxRows": 1})
	r0 := dig(doc, "Results", 0).(map[string]any)
	if r0["Truncated"] != true || r0["RowCount"].(float64) != 3 || len(r0["Rows"].([]any)) != 1 {
		t.Errorf("truncation: %v", r0)
	}
	// A failing query reports beside the others; only an all-failed call is an error.
	text, isErr := iqCall(t, f.a, owner, "ExecuteQuery", map[string]any{"artifactId": f.model.ID,
		"daxQueries": []any{q, "EVALUATE 'NoSuchTable'"}})
	if isErr || !strings.Contains(text, `"Error"`) {
		t.Errorf("one bad query of two: %v %s", isErr, text)
	}
	for want, qs := range map[string]string{
		"MDX and DMV queries are not supported": "SELECT [Measures].[TotalUnits] ON 0 FROM [Model]",
		"MDX and DMV":                           "SELECT * FROM $SYSTEM.TMSCHEMA_TABLES",
		"INFO functions are not supported":      "EVALUATE INFO.TABLES()",
	} {
		iqRefused(t, f.a, owner, "ExecuteQuery", map[string]any{"artifactId": f.model.ID, "daxQueries": []any{qs}}, want)
	}
	for want, args := range map[string]map[string]any{
		"1 to 4 DAX queries":  {"artifactId": f.model.ID, "daxQueries": []any{q, q, q, q, q}},
		"1 to 4":              {"artifactId": f.model.ID},
		"from 1 to 1000":      {"artifactId": f.model.ID, "daxQueries": []any{q}, "maxRows": 1001},
		"whole number":        {"artifactId": f.model.ID, "daxQueries": []any{q}, "maxRows": 2.5},
		"that you can access": {"artifactId": "no-such-model", "daxQueries": []any{q}},
	} {
		iqRefused(t, f.a, owner, "ExecuteQuery", args, want)
	}
	iqRefused(t, f.a, stranger, "ExecuteQuery", map[string]any{"artifactId": f.model.ID, "daxQueries": []any{q}}, "that you can access")
}

func TestAModelThatCannotBeReadIsAToolErrorNotACrash(t *testing.T) {
	f := newIQ(t)
	empty := &store.Item{WorkspaceID: f.ws.ID, Type: "SemanticModel", DisplayName: "Empty"}
	if err := f.st.CreateItem(empty, nil); err != nil {
		t.Fatal(err)
	}
	for tool, args := range map[string]map[string]any{
		"GetSemanticModelSchema": {"artifactId": empty.ID},
		"ValueSearch":            {"artifactId": empty.ID, "searchTerms": []any{"x"}},
		"ExecuteQuery":           {"artifactId": empty.ID, "daxQueries": []any{"EVALUATE 'Store'"}},
	} {
		iqRefused(t, f.a, owner, tool, args, "could not be read")
	}
}
