package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	jmespath "github.com/jmespath-community/go-jmespath"
	"github.com/jmespath-community/go-jmespath/pkg/functions"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/pbireport"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Fabric IQ MCP: a remote, read-only MCP server over Power BI reports and
// semantic models, at POST /v1/mcp/fabriciq on the same transport as Core MCP.
//
// Sources, as read on 2026-09-30:
//   - learn.microsoft.com/fabric/iq/connectors/fabric-iq-mcp — the endpoint, the
//     six tool names, delegated-only auth, the X-Variants selector, and the
//     limits (read-only, one model per ExecuteQuery, 250 rows by default).
//   - github.com/microsoft/skills-for-fabric skills/fabriciq/SKILL.md, which
//     Microsoft's page recommends — the argument names (searchQuery,
//     reportObjectId, artifactId, daxQueries, maxRows, queries…), the 1,000-row
//     and 1–4 query bounds, and the response paths agents query
//     (ReportMetadata.Pages[].Visuals, schema.Tables[].Measures,
//     schema.ActiveRelationships[].{PK,FK}, schema.VerifiedAnswers).
//
// Microsoft does not publish the tools' input or output schemas ("call
// tools/list at runtime"), so the schemas and response documents here are built
// to those argument names and paths, not copied from a captured server.

// fabricIQVariant is the current public tool-contract version.
const fabricIQVariant = "Fabric.Routing.FabricIQ.V1"

// Row limits for ExecuteQuery: "Default 250 rows per query, max 1,000".
const (
	iqDefaultRows = 250
	iqMaxRows     = 1000
	iqMaxQueries  = 4
	iqMaxResults  = 50 // DiscoverArtifacts: "Maximum 50 results"
	iqMaxMatches  = 10 // ValueSearch: matches returned per search term (ours)
)

var fabricIQ = &mcpServer{
	name:    "fabric-iq",
	version: "V1",
	instructions: "Microsoft Fabric IQ MCP. Read-only tools over Power BI reports and semantic models: " +
		"DiscoverArtifacts or ResolveFabricItem to find an item, GetReportMetadata and GetSemanticModelSchema " +
		"to read it, ValueSearch to find exact stored values, and ExecuteQuery to run DAX against one semantic model.",
	admit: admitFabricIQ,
}

func (a *API) registerFabricIQ(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/mcp/fabriciq", a.withAuth(a.mcpPost(fabricIQ)))
	mux.HandleFunc("GET /v1/mcp/fabriciq", a.withAuth(a.mcpGet(fabricIQ)))
	mux.HandleFunc("DELETE /v1/mcp/fabriciq", a.withAuth(a.mcpDelete(fabricIQ)))
}

// admitFabricIQ applies the two rules the endpoint has beyond a valid token.
//
// "Service-principal authentication isn't supported. Application-only
// authentication isn't supported." The token itself is accepted for either the
// Fabric or the Power BI audience, as withAuth's validator already does:
// Microsoft's setup notes say "both audiences are valid against its endpoint".
//
// X-Variants selects the tool contract, and V1 is the only one published. A
// request without the header is served V1, the current default; one naming
// another variant is refused rather than answered with a contract it did not
// ask for. The refusal's code and text are ours; Microsoft documents only that
// tools "don't match the documented list" without the right value.
func admitFabricIQ(w http.ResponseWriter, r *http.Request, p *auth.Principal) bool {
	if p.Type == "ServicePrincipal" {
		writeErr(w, http.StatusForbidden, "ServicePrincipalNotSupported",
			"Fabric IQ MCP accepts delegated (user) tokens only; service-principal and application-only "+
				"authentication aren't supported.")
		return false
	}
	if v := r.Header.Get("X-Variants"); v != "" && !strings.EqualFold(v, fabricIQVariant) {
		writeErr(w, http.StatusBadRequest, "UnsupportedVariant",
			fmt.Sprintf("X-Variants %q is not a Fabric IQ tool-contract version; the current one is %s.", v, fabricIQVariant))
		return false
	}
	return true
}

func init() {
	obj := func(required []string, props map[string]any) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	strs := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	queries := strs("Optional JMESPath expressions over the full response, each returned with its result. " +
		"Omit on the first call. regex_match(subject, pattern) is available and ignores case.")
	readOnly := map[string]any{"readOnlyHint": true}

	fabricIQ.tools = []mcpToolSpec{
		{Name: "DiscoverArtifacts", Annotations: readOnly,
			Description: "Search Power BI reports and semantic models you can access by name. Returns the top matches " +
				"(at most 50), reports before semantic models, not an exhaustive listing.",
			InputSchema: obj([]string{"searchQuery"}, map[string]any{
				"searchQuery":   str("Words from the report or semantic model name"),
				"artifactTypes": strs("Report and/or SemanticModel; both when omitted"),
				"maxResults":    num("At most 50"),
			})},
		{Name: "ResolveFabricItem", Annotations: readOnly,
			Description: "Resolve a Power BI or Fabric browser URL, or a bare item GUID, to the item's id, type and workspace.",
			InputSchema: obj([]string{"fabricItemId"}, map[string]any{
				"fabricItemId": str("An item GUID or a report or semantic model browser URL (not a share link)"),
			})},
		{Name: "GetReportMetadata", Annotations: readOnly,
			Description: "Read a report's pages, visuals and the fields they bind, its report, page and visual filters, " +
				"its report-level measures, and the id of the semantic model it is bound to (semanticModel).",
			InputSchema: obj([]string{"reportObjectId"}, map[string]any{
				"reportObjectId": str("The report's id"),
				"queries":        queries,
			})},
		{Name: "GetSemanticModelSchema", Annotations: readOnly,
			Description: "Read a semantic model's tables, columns, measures and active relationships, as you are " +
				"permitted to see them.",
			InputSchema: obj([]string{"artifactId"}, map[string]any{
				"artifactId": str("The semantic model's id"),
				"queries":    queries,
			})},
		{Name: "ValueSearch", Annotations: readOnly,
			Description: "Find the column and exact stored value for named entities, before filtering on them in DAX.",
			InputSchema: obj([]string{"artifactId", "searchTerms"}, map[string]any{
				"artifactId":  str("The semantic model's id"),
				"searchTerms": strs("Values to look for, e.g. a customer or region name"),
				"scope":       strs("Optional tables or 'Table'[Column] references to search within"),
			})},
		{Name: "ExecuteQuery", Annotations: readOnly,
			Description: "Run 1 to 4 DAX queries (one EVALUATE each) against one semantic model. Returns up to maxRows " +
				"rows per query (default 250, at most 1000). DAX only: MDX, DMV and INFO functions are not supported.",
			InputSchema: obj([]string{"artifactId", "daxQueries"}, map[string]any{
				"artifactId": str("The semantic model's id"),
				"daxQueries": strs("1 to 4 DAX queries"),
				"maxRows":    num("Rows returned per query; default 250, at most 1000"),
			})},
	}
	fabricIQ.dispatch = map[string]func(*API, *auth.Principal, map[string]any) mcpToolResult{
		"DiscoverArtifacts":      toolDiscoverArtifacts,
		"ResolveFabricItem":      toolResolveFabricItem,
		"GetReportMetadata":      toolGetReportMetadata,
		"GetSemanticModelSchema": toolGetSemanticModelSchema,
		"ValueSearch":            toolValueSearch,
		"ExecuteQuery":           toolExecuteQuery,
	}
}

// --- shared -----------------------------------------------------------------

// iqTypes are the item types Fabric IQ serves.
var iqTypes = []string{"Report", "SemanticModel"}

// iqItem returns an item the caller may read, of one of the wanted types. A
// missing item and one the caller cannot read get the same answer: "The server
// can't query a report or semantic model that the authenticated identity can't
// access", and saying which would disclose that it exists.
func (a *API) iqItem(p *auth.Principal, id string, want ...string) (*store.Item, store.Access, string) {
	it, err := a.Store.GetItemByID(strings.TrimSpace(id))
	if err == nil {
		if access, err := a.Store.EffectiveItemAccess(it, p.ID); err == nil && access.Has(store.PermRead) {
			if !slices.Contains(want, it.Type) {
				return nil, access, fmt.Sprintf("%s is a %s; this tool takes a %s.", id, it.Type, strings.Join(want, " or "))
			}
			return it, access, ""
		}
	}
	return nil, store.Access{}, fmt.Sprintf("No %s with id %q that you can access.", strings.Join(want, " or "), id)
}

func (a *API) workspaceName(id string) string {
	if ws, err := a.Store.GetWorkspace(id); err == nil {
		return ws.DisplayName
	}
	return ""
}

func (a *API) definitionParts(itemID string) (map[string][]byte, error) {
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(parts))
	for _, part := range parts {
		raw, err := base64.StdEncoding.DecodeString(part.Payload)
		if err != nil {
			return nil, fmt.Errorf("decoding %s: %w", part.Path, err)
		}
		out[part.Path] = raw
	}
	return out, nil
}

func argStrings(args map[string]any, key string) ([]string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false
	}
	switch t := v.(type) {
	case string:
		return []string{t}, true
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// argInt reads an optional integer argument; ok is false when present but not one.
func argInt(args map[string]any, key string, def int) (int, bool) {
	v, present := args[key]
	if !present || v == nil {
		return def, true
	}
	n, isNum := v.(float64)
	if !isNum || n != float64(int(n)) {
		return 0, false
	}
	return int(n), true
}

// iqResult renders a response document, applying `queries` when given.
func iqResult(doc any, args map[string]any) mcpToolResult {
	qs, present := argStrings(args, "queries")
	if _, given := args["queries"]; given && !present {
		return mcpErr("queries must be a list of JMESPath expressions.")
	}
	if len(qs) == 0 {
		return mcpJSON(doc)
	}
	// Search generic JSON, so the paths are the marshalled names.
	b, err := json.Marshal(doc)
	if err != nil {
		return mcpErr(err.Error())
	}
	var data any
	_ = json.Unmarshal(b, &data)
	results := make([]map[string]any, 0, len(qs))
	for _, q := range qs {
		v, err := jmespathSearch(q, data)
		if err != nil {
			return mcpErr(fmt.Sprintf("JMESPath %q: %v", q, err))
		}
		results = append(results, map[string]any{"Query": q, "Result": v})
	}
	return mcpJSON(map[string]any{"Results": results})
}

// regexMatch is added to JMESPath's standard functions for `queries`. The
// Fabric IQ skill's example queries use it (`[?regex_match(to_string(@),
// 'revenue|sales')]`). It is not standard JMESPath. Its semantics are ours:
// Go regular expressions, matching anywhere in the subject, ignoring case —
// the skill writes lowercase keywords against names like "Total Revenue".
var regexMatch = functions.FunctionEntry{
	Name: "regex_match",
	Arguments: []functions.ArgSpec{
		{Types: []functions.JpType{functions.JpString}},
		{Types: []functions.JpType{functions.JpString}},
	},
	Handler: func(args []any) (any, error) {
		re, err := regexp.Compile("(?i)" + args[1].(string))
		if err != nil {
			return nil, err
		}
		return re.MatchString(args[0].(string)), nil
	},
}

func jmespathSearch(expr string, data any) (any, error) {
	jp, err := jmespath.Compile(expr, regexMatch)
	if err != nil {
		return nil, err
	}
	return jp.Search(data)
}

// --- DiscoverArtifacts ------------------------------------------------------

type iqArtifact struct {
	ArtifactID      string `json:"ArtifactId"`
	Name            string `json:"Name"`
	Type            string `json:"Type"`
	Description     string `json:"Description,omitempty"`
	WorkspaceID     string `json:"WorkspaceId"`
	WorkspaceName   string `json:"WorkspaceName"`
	SemanticModelID string `json:"SemanticModelId,omitempty"`
	score           int
}

// toolDiscoverArtifacts searches every report and semantic model the caller can
// read — through a workspace role or shared with them directly, since "you don't
// need a workspace role" — by name. Ranking is ours: an exact name, then a name
// containing the query, then one whose name, description and workspace hold
// every word of it; reports before semantic models ("Prefer reports over
// standalone semantic models"), then by name.
func toolDiscoverArtifacts(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	q := strings.ToLower(strings.TrimSpace(arg(args, "searchQuery")))
	if q == "" {
		return mcpErr("searchQuery is required: words from the report or semantic model name.")
	}
	types := iqTypes
	if ts, ok := argStrings(args, "artifactTypes"); ok {
		types = nil
		for _, t := range ts {
			switch strings.ToLower(t) {
			case "report":
				types = append(types, "Report")
			case "semanticmodel", "dataset":
				types = append(types, "SemanticModel")
			default:
				return mcpErr(fmt.Sprintf("artifactTypes: %q is not Report or SemanticModel.", t))
			}
		}
	} else if _, given := args["artifactTypes"]; given {
		return mcpErr("artifactTypes must be a list of Report and/or SemanticModel.")
	}
	limit, ok := argInt(args, "maxResults", iqMaxResults)
	if !ok || limit < 1 || limit > iqMaxResults {
		return mcpErr(fmt.Sprintf("maxResults must be a whole number from 1 to %d.", iqMaxResults))
	}

	workspaces, err := a.Store.ListAllWorkspaces()
	if err != nil {
		return mcpErr(err.Error())
	}
	words := strings.Fields(q)
	var found []iqArtifact
	for _, ws := range workspaces {
		items, err := a.Store.ListItems(ws.ID, "")
		if err != nil {
			return mcpErr(err.Error())
		}
		for _, it := range items {
			if !slices.Contains(types, it.Type) {
				continue
			}
			name := strings.ToLower(it.DisplayName)
			hay := name + " " + strings.ToLower(it.Description) + " " + strings.ToLower(ws.DisplayName)
			score := 0
			switch {
			case name == q:
				score = 3
			case strings.Contains(name, q):
				score = 2
			case allIn(words, hay):
				score = 1
			default:
				continue
			}
			if access, err := a.Store.EffectiveItemAccess(it, p.ID); err != nil || !access.Has(store.PermRead) {
				continue
			}
			art := iqArtifact{ArtifactID: it.ID, Name: it.DisplayName, Type: it.Type, Description: it.Description,
				WorkspaceID: ws.ID, WorkspaceName: ws.DisplayName, score: score}
			if it.Type == "Report" {
				art.SemanticModelID, _ = a.reportModel(it)
			}
			found = append(found, art)
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		x, y := found[i], found[j]
		if x.score != y.score {
			return x.score > y.score
		}
		if x.Type != y.Type {
			return x.Type == "Report"
		}
		return strings.ToLower(x.Name) < strings.ToLower(y.Name)
	})
	if len(found) > limit {
		found = found[:limit]
	}
	if found == nil {
		found = []iqArtifact{}
	}
	return mcpJSON(map[string]any{"Artifacts": found, "Count": len(found)})
}

func allIn(words []string, hay string) bool {
	for _, w := range words {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// --- ResolveFabricItem ------------------------------------------------------

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// toolResolveFabricItem maps a bare GUID or a browser URL
// (…/groups/<workspace>/reports/<id>, …/datasets/<id>, …/semanticmodels/<id>)
// to the item. Workspace-app URLs and share links are refused by name: the
// emulator has no workspace apps, and Microsoft's page says not to use share links.
func toolResolveFabricItem(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	in := strings.TrimSpace(arg(args, "fabricItemId"))
	if in == "" {
		return mcpErr("fabricItemId is required: an item GUID or a report or semantic model URL.")
	}
	id, wsFromURL := in, ""
	if !guidPattern.MatchString(in) {
		u, err := url.Parse(in)
		if err != nil || u.Host == "" {
			return mcpErr(fmt.Sprintf("%q is neither an item GUID nor a URL.", in))
		}
		segs := strings.Split(strings.Trim(u.Path, "/"), "/")
		switch {
		case slices.Contains(segs, "apps"):
			return mcpErr("Workspace-app URLs are not supported by this emulator, which has no workspace apps; " +
				"use the report's own URL or id.")
		case slices.Contains(segs, "links") || u.Query().Has("pbi_source") && slices.Contains(segs, "view"):
			return mcpErr("Share links are not supported; use the report or semantic model URL from the browser's address bar.")
		}
		id = ""
		for i := 0; i+3 < len(segs); i++ {
			if segs[i] == "groups" {
				switch strings.ToLower(segs[i+2]) {
				case "reports", "datasets", "semanticmodels":
					wsFromURL, id = segs[i+1], segs[i+3]
				}
			}
		}
		if id == "" {
			return mcpErr(fmt.Sprintf("%q is not a report or semantic model URL (…/groups/<workspace>/reports/<id> "+
				"or …/datasets/<id>).", in))
		}
	}
	it, _, msg := a.iqItem(p, id, iqTypes...)
	if msg != "" {
		return mcpErr(msg)
	}
	if wsFromURL != "" && wsFromURL != "me" && !strings.EqualFold(wsFromURL, it.WorkspaceID) {
		return mcpErr(fmt.Sprintf("No Report or SemanticModel with id %q that you can access.", id))
	}
	out := map[string]any{"fabricItemId": it.ID, "itemType": it.Type, "displayName": it.DisplayName,
		"workspaceId": it.WorkspaceID}
	if it.Type == "Report" {
		out["instructions"] = "Call GetReportMetadata with reportObjectId=" + it.ID +
			"; its semanticModel field is the id to pass to GetSemanticModelSchema, ValueSearch and ExecuteQuery."
	} else {
		out["instructions"] = "Call GetSemanticModelSchema with artifactId=" + it.ID + "."
	}
	return mcpJSON(out)
}

// --- GetReportMetadata ------------------------------------------------------

// reportModel resolves the semantic model a report is bound to: by id from
// definition.pbir's byConnection, or by name from byPath (../<Name>.SemanticModel)
// in the report's own workspace. "" when the definition names neither, or names
// a model that is not there.
func (a *API) reportModel(it *store.Item) (string, error) {
	parts, err := a.definitionParts(it.ID)
	if err != nil {
		return "", err
	}
	r, err := pbireport.Parse(parts)
	if err != nil {
		return "", err
	}
	return a.boundModel(it, r), nil
}

func (a *API) boundModel(it *store.Item, r *pbireport.Report) string {
	if id := r.Dataset.SemanticModelID; id != "" {
		if m, err := a.Store.GetItemByID(id); err == nil && m.Type == "SemanticModel" {
			return m.ID
		}
		return ""
	}
	if p := r.Dataset.Path; p != "" {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "../"), ".SemanticModel")
		models, err := a.Store.ListItems(it.WorkspaceID, "SemanticModel")
		if err == nil {
			for _, m := range models {
				if m.DisplayName == name {
					return m.ID
				}
			}
		}
	}
	return ""
}

func toolGetReportMetadata(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	it, _, msg := a.iqItem(p, arg(args, "reportObjectId"), "Report")
	if msg != "" {
		return mcpErr(msg)
	}
	parts, err := a.definitionParts(it.ID)
	if err != nil {
		return mcpErr(err.Error())
	}
	r, err := pbireport.Parse(parts)
	if err != nil {
		return mcpErr("The report's definition could not be read: " + err.Error())
	}
	meta := map[string]any{
		"Id": it.ID, "Name": it.DisplayName, "WorkspaceId": it.WorkspaceID, "WorkspaceName": a.workspaceName(it.WorkspaceID),
		"Format": r.Format, "Filters": r.Filters, "Pages": r.Pages, "Measures": r.Measures,
	}
	doc := map[string]any{"ReportMetadata": meta}
	if id := a.boundModel(it, r); id != "" {
		doc["semanticModel"] = id
	} else {
		doc["semanticModel"] = nil
		doc["Warnings"] = []string{"The report's definition.pbir does not name a semantic model that exists " +
			"in this emulator, so there is no model id to query."}
	}
	return iqResult(doc, args)
}

// --- GetSemanticModelSchema -------------------------------------------------

type iqColumn struct {
	Name string `json:"Name"`
	Type string `json:"Type"`
}

type iqMeasure struct {
	Name       string `json:"Name"`
	Expression string `json:"Expression"`
}

type iqTable struct {
	Name     string      `json:"Name"`
	Columns  []iqColumn  `json:"Columns"`
	Measures []iqMeasure `json:"Measures"`
}

type iqRelationship struct {
	PK string `json:"PK"`
	FK string `json:"FK"`
}

// loadModelSchema is loadSemanticModel without the data: the model as this
// caller may see it, object-level security applied, for the schema tool.
func (a *API) loadModelSchema(itemID string, p *auth.Principal) (*semanticmodel.Model, error) {
	m, err := a.parseModelDefinition(itemID)
	if err != nil {
		return nil, err
	}
	if len(m.Roles) == 0 {
		return m, nil
	}
	if err := semanticmodel.CheckObjectSecurityChains(m); err != nil {
		return nil, err
	}
	restricted, err := a.rolesRestrict(itemID, m, p)
	if err != nil || !restricted {
		return m, err
	}
	roles, err := admittingRoles(m, p)
	if err != nil {
		return nil, err
	}
	m, _, err = semanticmodel.ApplyObjectSecurity(m, semanticmodel.Data{}, roles)
	return m, err
}

func daxRef(table, col string) string {
	return "'" + strings.ReplaceAll(table, "'", "''") + "'[" + col + "]"
}

// toolGetSemanticModelSchema returns the model's tables, columns and measures,
// and its active relationships as PK (the one side) and FK (the many side).
// CustomInstructions and VerifiedAnswers are Power BI's "prep data for AI"
// objects, which this emulator does not model: they are present and empty, so
// a query for them answers rather than fails.
func toolGetSemanticModelSchema(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	it, _, msg := a.iqItem(p, arg(args, "artifactId"), "SemanticModel")
	if msg != "" {
		return mcpErr(msg)
	}
	m, err := a.loadModelSchema(it.ID, p)
	if err != nil {
		return mcpErr("The semantic model could not be read: " + err.Error())
	}
	tables := make([]iqTable, 0, len(m.Tables))
	for _, t := range m.Tables {
		tb := iqTable{Name: t.Name, Columns: []iqColumn{}, Measures: []iqMeasure{}}
		for _, c := range t.Columns {
			tb.Columns = append(tb.Columns, iqColumn{Name: c.Name, Type: c.DataType})
		}
		for _, ms := range t.Measures {
			tb.Measures = append(tb.Measures, iqMeasure{Name: ms.Name, Expression: ms.Expression})
		}
		tables = append(tables, tb)
	}
	rels := []iqRelationship{}
	for _, r := range m.Relationships {
		if r.Inactive {
			continue
		}
		one, many := daxRef(r.ToTable, r.ToColumn), daxRef(r.FromTable, r.FromColumn)
		if strings.EqualFold(r.FromCardinality, "one") && !strings.EqualFold(r.ToCardinality, "one") {
			one, many = many, one
		}
		rels = append(rels, iqRelationship{PK: one, FK: many})
	}
	doc := map[string]any{
		"schema": map[string]any{
			"Name": it.DisplayName, "Tables": tables, "ActiveRelationships": rels,
			"CustomInstructions": nil, "VerifiedAnswers": []any{},
		},
		"semanticModel": map[string]any{"Id": it.ID, "Name": it.DisplayName, "WorkspaceId": it.WorkspaceID},
	}
	return iqResult(doc, args)
}

// --- ValueSearch ------------------------------------------------------------

type iqMatch struct {
	Table           string `json:"Table"`
	Column          string `json:"Column"`
	Value           any    `json:"Value"`
	ColumnReference string `json:"ColumnReference"`
	MatchType       string `json:"MatchType"` // Exact or Contains
}

// toolValueSearch finds stored text values matching each term, so a DAX filter
// uses the model's own spelling. It reads the rows the caller may see — row-
// and object-level security applied by the same loader ExecuteQuery uses — and
// returns exact (case-insensitive) matches before values containing the term,
// at most iqMaxMatches per term. `scope`, when given, limits the search to the
// named tables or 'Table'[Column] references; the meaning is inferred from the
// argument name, which is all Microsoft documents of it.
func toolValueSearch(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	it, _, msg := a.iqItem(p, arg(args, "artifactId"), "SemanticModel")
	if msg != "" {
		return mcpErr(msg)
	}
	terms, ok := argStrings(args, "searchTerms")
	if !ok || len(terms) == 0 {
		return mcpErr("searchTerms is required: the values to look for.")
	}
	m, data, err := a.loadSemanticModel(context.Background(), it.ID, p)
	if err != nil {
		return mcpErr("The semantic model could not be read: " + err.Error())
	}
	inScope, errMsg := valueScope(m, args)
	if errMsg != "" {
		return mcpErr(errMsg)
	}
	results := make([]map[string]any, 0, len(terms))
	for _, term := range terms {
		needle := strings.ToLower(strings.TrimSpace(term))
		var exact, partial []iqMatch
		seen := map[string]bool{}
		for _, t := range m.Tables {
			for _, c := range t.Columns {
				if !inScope(t.Name, c.Name) {
					continue
				}
				for _, row := range data[t.Name] {
					s, isText := row[c.Name].(string)
					key := t.Name + "\x00" + c.Name + "\x00" + s
					if !isText || needle == "" || seen[key] {
						continue
					}
					low := strings.ToLower(s)
					switch {
					case low == needle:
						exact = append(exact, iqMatch{t.Name, c.Name, s, daxRef(t.Name, c.Name), "Exact"})
					case strings.Contains(low, needle):
						partial = append(partial, iqMatch{t.Name, c.Name, s, daxRef(t.Name, c.Name), "Contains"})
					default:
						continue
					}
					seen[key] = true
				}
			}
		}
		matches := append(exact, partial...)
		if len(matches) > iqMaxMatches {
			matches = matches[:iqMaxMatches]
		}
		if matches == nil {
			matches = []iqMatch{}
		}
		results = append(results, map[string]any{"SearchTerm": term, "Matches": matches})
	}
	return mcpJSON(map[string]any{"Results": results})
}

var scopeColumn = regexp.MustCompile(`^'?([^'\[]+)'?\[([^\]]+)\]$`)

// valueScope turns `scope` into a predicate over (table, column), refusing a
// name the model does not have rather than silently searching nothing.
func valueScope(m *semanticmodel.Model, args map[string]any) (func(table, col string) bool, string) {
	scope, ok := argStrings(args, "scope")
	if _, given := args["scope"]; given && !ok {
		return nil, "scope must be a list of table names or 'Table'[Column] references."
	}
	if len(scope) == 0 {
		return func(string, string) bool { return true }, ""
	}
	tables, cols := map[string]bool{}, map[string]bool{}
	for _, s := range scope {
		s = strings.TrimSpace(s)
		if g := scopeColumn.FindStringSubmatch(s); g != nil {
			t := m.Table(g[1])
			if t == nil || t.Column(g[2]) == nil {
				return nil, fmt.Sprintf("scope: %s is not a column of this model.", s)
			}
			cols[t.Name+"\x00"+g[2]] = true
			continue
		}
		t := m.Table(strings.Trim(s, "'"))
		if t == nil {
			return nil, fmt.Sprintf("scope: %s is not a table of this model.", s)
		}
		tables[t.Name] = true
	}
	return func(table, col string) bool { return tables[table] || cols[table+"\x00"+col] }, ""
}

// --- ExecuteQuery -----------------------------------------------------------

var (
	mdxOrDMV = regexp.MustCompile(`(?is)^\s*(SELECT\b|WITH\s+MEMBER\b)|\$SYSTEM\.`)
	infoFunc = regexp.MustCompile(`(?i)\bINFO\.[A-Z]+\s*\(`)
)

// toolExecuteQuery runs 1–4 DAX queries against one semantic model, as the
// caller: the rows are the ones their security roles admit, through the same
// loader executeQueries uses. Unlike executeQueries it does not require Build:
// "You don't need a workspace role or Build permission on the semantic model";
// Read on it is enough.
//
// Each query is answered on its own: one that fails reports its Error beside
// the others' rows, and the call is an error only when every query failed. How
// the real server reports a partial failure is not documented; that choice is ours.
func toolExecuteQuery(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	it, access, msg := a.iqItem(p, arg(args, "artifactId"), "SemanticModel")
	if msg != "" {
		return mcpErr(msg)
	}
	queries, ok := argStrings(args, "daxQueries")
	if !ok || len(queries) == 0 || len(queries) > iqMaxQueries {
		return mcpErr(fmt.Sprintf("daxQueries must hold 1 to %d DAX queries.", iqMaxQueries))
	}
	maxRows, ok := argInt(args, "maxRows", iqDefaultRows)
	if !ok || maxRows < 1 || maxRows > iqMaxRows {
		return mcpErr(fmt.Sprintf("maxRows must be a whole number from 1 to %d.", iqMaxRows))
	}
	ctx := context.Background()
	model, data, err := a.loadSemanticModel(ctx, it.ID, p)
	if err != nil {
		return mcpErr("The semantic model could not be read: " + err.Error())
	}
	if len(model.Roles) > 0 && a.DAXURL != nil && !access.Has(store.PermWrite) {
		return mcpErr("The attached DAX engine does not apply security roles, so a principal they restrict " +
			"is not served through it.")
	}
	results := make([]map[string]any, 0, len(queries))
	failed := 0
	for i, q := range queries {
		res := map[string]any{"QueryIndex": i}
		var rows []map[string]any
		switch {
		case mdxOrDMV.MatchString(q):
			err = fmt.Errorf("MDX and DMV queries are not supported; send a DAX query (EVALUATE …)")
		case infoFunc.MatchString(q):
			err = fmt.Errorf("INFO functions are not supported; read the model with GetSemanticModelSchema")
		default:
			rows, err = a.evalDAX(ctx, it.ID, model, data, q, true)
		}
		if err != nil {
			failed++
			res["Error"] = err.Error()
			results = append(results, res)
			continue
		}
		res["RowCount"] = len(rows)
		res["Truncated"] = len(rows) > maxRows
		if len(rows) > maxRows {
			rows = rows[:maxRows]
		}
		res["Rows"] = rows
		results = append(results, res)
	}
	a.publishQuery(it, len(queries), failed > 0)
	out := mcpJSON(map[string]any{"Results": results})
	out.IsError = failed == len(queries)
	return out
}
