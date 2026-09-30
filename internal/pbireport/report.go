// Package pbireport reads what a Power BI report definition says about itself:
// its pages, the visuals on them and the fields each visual binds, the filters
// at report, page and visual level, the measures the report defines on top of
// its model, and which semantic model it is bound to.
//
// It exists for Fabric IQ MCP's GetReportMetadata, which hands an agent that
// picture so it can write DAX a visual would run. Nothing here renders a report;
// the emulator still stores report definitions byte for byte.
//
// Two definition formats are read, because Fabric returns either:
//
//   - PBIR: a `definition/` folder of JSON files, to Microsoft's published
//     schemas (developer.microsoft.com/json-schemas/fabric/item/report/definition).
//   - PBIR-Legacy: one `report.json` whose config, filters and visual configs are
//     JSON documents embedded as strings.
//
// Both carry field references and filters in the same semantic-query form
// (`Column`/`Measure`/`Aggregation` over a `SourceRef`, `Where` conditions of
// `In`/`Not`/`Comparison`), so one decoder serves both. A filter whose condition
// is not one of those shapes is still reported, as "Advanced" with its raw
// condition, rather than dropped or guessed at.
package pbireport

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Report is a parsed report definition. JSON names are the ones agents query
// through Fabric IQ MCP (ReportMetadata.Pages[].Visuals[].Title, .Filters, .Measures).
type Report struct {
	Format   string           `json:"Format"`
	Dataset  DatasetReference `json:"-"`
	Filters  []Filter         `json:"Filters"`
	Pages    []Page           `json:"Pages"`
	Measures []Measure        `json:"Measures"`
}

// DatasetReference is definition.pbir's datasetReference: a Fabric-hosted model
// named by id in a connection string, or a local definition named by path.
type DatasetReference struct {
	SemanticModelID  string
	ConnectionString string
	Path             string
}

type Page struct {
	Name    string   `json:"Name"`
	Title   string   `json:"Title"`
	Filters []Filter `json:"Filters"`
	Visuals []Visual `json:"Visuals"`
}

type Visual struct {
	Name    string   `json:"Name"`
	Title   string   `json:"Title,omitempty"`
	Type    string   `json:"Type"`
	Fields  []Field  `json:"Fields"`
	Filters []Filter `json:"Filters"`
}

// Field is one field a visual binds, in the role it binds it to.
type Field struct {
	Role        string `json:"Role,omitempty"`
	Kind        string `json:"Kind"` // Column, Measure, Aggregation, HierarchyLevel
	Table       string `json:"Table"`
	Name        string `json:"Name"`
	Aggregation string `json:"Aggregation,omitempty"`
	Reference   string `json:"Reference"` // the DAX that names it
}

// Filter is one filter card. Operator and Values restate a simple condition
// (In, NotIn, a comparison); anything else is Operator "Advanced" with the raw
// Condition kept for the reader.
type Filter struct {
	Name      string          `json:"Name,omitempty"`
	Type      string          `json:"Type,omitempty"`
	Field     string          `json:"Field,omitempty"`
	Operator  string          `json:"Operator,omitempty"`
	Values    []any           `json:"Values,omitempty"`
	Condition json.RawMessage `json:"Condition,omitempty"`
}

// Measure is a report-level measure: defined in the report, not the model, so
// a query that uses it has to redefine it with DEFINE MEASURE.
type Measure struct {
	Table      string `json:"Table"`
	Name       string `json:"Name"`
	Expression string `json:"Expression"`
}

// Parse reads a report definition from its parts, keyed by path.
func Parse(parts map[string][]byte) (*Report, error) {
	r := &Report{Filters: []Filter{}, Pages: []Page{}, Measures: []Measure{}}
	if raw, ok := parts["definition.pbir"]; ok {
		if err := r.readPBIR(raw); err != nil {
			return nil, fmt.Errorf("definition.pbir: %w", err)
		}
	}
	var err error
	switch {
	case hasPrefix(parts, "definition/"):
		r.Format = "PBIR"
		err = r.readEnhanced(parts)
	case parts["report.json"] != nil:
		r.Format = "PBIR-Legacy"
		err = r.readLegacy(parts["report.json"])
	default:
		return nil, fmt.Errorf("the definition has neither a definition/ folder nor a report.json")
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func hasPrefix(parts map[string][]byte, prefix string) bool {
	for p := range parts {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

var semanticModelID = regexp.MustCompile(`(?i)(?:^|;)\s*semanticmodelid\s*=\s*([0-9a-f-]{36})`)

func (r *Report) readPBIR(raw []byte) error {
	var doc struct {
		DatasetReference struct {
			ByPath *struct {
				Path string `json:"path"`
			} `json:"byPath"`
			ByConnection *struct {
				ConnectionString string `json:"connectionString"`
			} `json:"byConnection"`
		} `json:"datasetReference"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if c := doc.DatasetReference.ByConnection; c != nil {
		r.Dataset.ConnectionString = c.ConnectionString
		if m := semanticModelID.FindStringSubmatch(c.ConnectionString); m != nil {
			r.Dataset.SemanticModelID = strings.ToLower(m[1])
		}
	}
	if p := doc.DatasetReference.ByPath; p != nil {
		r.Dataset.Path = p.Path
	}
	return nil
}

// --- PBIR -------------------------------------------------------------------

type filterConfig struct {
	Filters []rawFilter `json:"filters"`
}

type rawFilter struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Field      json.RawMessage `json:"field"`
	Expression json.RawMessage `json:"expression"` // PBIR-Legacy's name for field
	Filter     *filterDef      `json:"filter"`
}

type filterDef struct {
	From  []entitySource `json:"From"`
	Where []struct {
		Condition json.RawMessage `json:"Condition"`
	} `json:"Where"`
}

type entitySource struct {
	Name   string `json:"Name"`
	Entity string `json:"Entity"`
}

func (r *Report) readEnhanced(parts map[string][]byte) error {
	if raw, ok := parts["definition/report.json"]; ok {
		var doc struct {
			FilterConfig filterConfig `json:"filterConfig"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("definition/report.json: %w", err)
		}
		r.Filters = filters(doc.FilterConfig.Filters)
	}
	if raw, ok := parts["definition/reportExtensions.json"]; ok {
		var doc struct {
			Entities []extensionEntity `json:"entities"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("definition/reportExtensions.json: %w", err)
		}
		r.Measures = measures(doc.Entities)
	}

	// Pages: definition/pages/<page>/page.json, visuals beneath each.
	pages := map[string]*Page{}
	visuals := map[string][]string{}
	for p := range parts {
		// definition/pages/<page>/page.json and
		// definition/pages/<page>/visuals/<visual>/visual.json.
		s := strings.Split(p, "/")
		if len(s) < 4 || s[0] != "definition" || s[1] != "pages" {
			continue
		}
		switch {
		case len(s) == 4 && s[3] == "page.json":
			pages[s[2]] = nil
		case len(s) == 6 && s[3] == "visuals" && s[5] == "visual.json":
			visuals[s[2]] = append(visuals[s[2]], p)
		}
	}
	for name := range pages {
		key := "definition/pages/" + name + "/page.json"
		var doc struct {
			Name         string       `json:"name"`
			DisplayName  string       `json:"displayName"`
			FilterConfig filterConfig `json:"filterConfig"`
		}
		if err := json.Unmarshal(parts[key], &doc); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		pg := &Page{Name: name, Title: doc.DisplayName, Filters: filters(doc.FilterConfig.Filters), Visuals: []Visual{}}
		sort.Strings(visuals[name])
		for _, vp := range visuals[name] {
			v, err := enhancedVisual(parts[vp])
			if err != nil {
				return fmt.Errorf("%s: %w", vp, err)
			}
			pg.Visuals = append(pg.Visuals, v)
		}
		pages[name] = pg
	}
	for _, name := range pageOrder(parts, pages) {
		r.Pages = append(r.Pages, *pages[name])
	}
	return nil
}

// pageOrder is pages.json's pageOrder, then any page it does not list by name.
func pageOrder(parts map[string][]byte, pages map[string]*Page) []string {
	var doc struct {
		PageOrder []string `json:"pageOrder"`
	}
	_ = json.Unmarshal(parts["definition/pages/pages.json"], &doc)
	var out []string
	for _, n := range doc.PageOrder {
		if pages[n] != nil && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	var rest []string
	for n := range pages {
		if !slices.Contains(out, n) {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func enhancedVisual(raw []byte) (Visual, error) {
	var doc struct {
		Name   string `json:"name"`
		Visual struct {
			VisualType string `json:"visualType"`
			Query      struct {
				QueryState map[string]struct {
					Projections []struct {
						Field json.RawMessage `json:"field"`
					} `json:"projections"`
				} `json:"queryState"`
			} `json:"query"`
			VisualContainerObjects map[string]json.RawMessage `json:"visualContainerObjects"`
		} `json:"visual"`
		FilterConfig filterConfig `json:"filterConfig"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Visual{}, err
	}
	v := Visual{Name: doc.Name, Type: doc.Visual.VisualType, Fields: []Field{},
		Filters: filters(doc.FilterConfig.Filters), Title: title(doc.Visual.VisualContainerObjects["title"])}
	roles := make([]string, 0, len(doc.Visual.Query.QueryState))
	for role := range doc.Visual.Query.QueryState {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		for _, p := range doc.Visual.Query.QueryState[role].Projections {
			if f, ok := field(p.Field, nil); ok {
				f.Role = role
				v.Fields = append(v.Fields, f)
			}
		}
	}
	return v, nil
}

// title is a visual's title text, from its title formatting object's literal.
func title(raw json.RawMessage) string {
	var objs []struct {
		Properties struct {
			Text struct {
				Expr struct {
					Literal *struct {
						Value string `json:"Value"`
					} `json:"Literal"`
				} `json:"expr"`
			} `json:"text"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &objs) != nil {
		return ""
	}
	for _, o := range objs {
		if l := o.Properties.Text.Expr.Literal; l != nil {
			if s, ok := literal(l.Value).(string); ok {
				return s
			}
		}
	}
	return ""
}

// --- PBIR-Legacy --------------------------------------------------------------

func (r *Report) readLegacy(raw []byte) error {
	var doc struct {
		Config   string `json:"config"`
		Filters  string `json:"filters"`
		Sections []struct {
			Name             string `json:"name"`
			DisplayName      string `json:"displayName"`
			Filters          string `json:"filters"`
			VisualContainers []struct {
				Config  string `json:"config"`
				Filters string `json:"filters"`
			} `json:"visualContainers"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("report.json: %w", err)
	}
	if doc.Config != "" {
		var cfg struct {
			ModelExtensions []struct {
				Entities []extensionEntity `json:"entities"`
			} `json:"modelExtensions"`
		}
		if err := json.Unmarshal([]byte(doc.Config), &cfg); err != nil {
			return fmt.Errorf("report.json config: %w", err)
		}
		for _, x := range cfg.ModelExtensions {
			r.Measures = append(r.Measures, measures(x.Entities)...)
		}
	}
	var err error
	if r.Filters, err = legacyFilters(doc.Filters); err != nil {
		return fmt.Errorf("report.json filters: %w", err)
	}
	for _, s := range doc.Sections {
		pg := Page{Name: s.Name, Title: s.DisplayName, Visuals: []Visual{}}
		if pg.Filters, err = legacyFilters(s.Filters); err != nil {
			return fmt.Errorf("report.json section %q filters: %w", s.Name, err)
		}
		for i, vc := range s.VisualContainers {
			v, err := legacyVisual(vc.Config)
			if err != nil {
				return fmt.Errorf("report.json section %q visual %d config: %w", s.Name, i, err)
			}
			if v.Filters, err = legacyFilters(vc.Filters); err != nil {
				return fmt.Errorf("report.json section %q visual %d filters: %w", s.Name, i, err)
			}
			pg.Visuals = append(pg.Visuals, v)
		}
		r.Pages = append(r.Pages, pg)
	}
	return nil
}

func legacyFilters(s string) ([]Filter, error) {
	if strings.TrimSpace(s) == "" {
		return []Filter{}, nil
	}
	var fs []rawFilter
	if err := json.Unmarshal([]byte(s), &fs); err != nil {
		return nil, err
	}
	return filters(fs), nil
}

// legacyVisual reads a visual container's config: fields come from the
// prototype query's Select, placed in the roles its projections name them in.
func legacyVisual(config string) (Visual, error) {
	var doc struct {
		Name         string `json:"name"`
		SingleVisual struct {
			VisualType  string `json:"visualType"`
			Projections map[string][]struct {
				QueryRef string `json:"queryRef"`
			} `json:"projections"`
			PrototypeQuery struct {
				From   []entitySource `json:"From"`
				Select []json.RawMessage
			} `json:"prototypeQuery"`
			VCObjects map[string]json.RawMessage `json:"vcObjects"`
		} `json:"singleVisual"`
	}
	if err := json.Unmarshal([]byte(config), &doc); err != nil {
		return Visual{}, err
	}
	sv := doc.SingleVisual
	v := Visual{Name: doc.Name, Type: sv.VisualType, Fields: []Field{}, Title: title(sv.VCObjects["title"])}
	aliases := aliasMap(sv.PrototypeQuery.From)
	byName := map[string]Field{}
	for _, sel := range sv.PrototypeQuery.Select {
		var named struct {
			Name string `json:"Name"`
		}
		_ = json.Unmarshal(sel, &named)
		if f, ok := field(sel, aliases); ok {
			byName[named.Name] = f
		}
	}
	roles := make([]string, 0, len(sv.Projections))
	for role := range sv.Projections {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		for _, p := range sv.Projections[role] {
			if f, ok := byName[p.QueryRef]; ok {
				f.Role = role
				v.Fields = append(v.Fields, f)
			}
		}
	}
	return v, nil
}

// --- shared: fields, filters, literals, measures ------------------------------

type extensionEntity struct {
	Name     string `json:"name"`
	Measures []struct {
		Name       string `json:"name"`
		Expression string `json:"expression"`
	} `json:"measures"`
}

func measures(es []extensionEntity) []Measure {
	out := []Measure{}
	for _, e := range es {
		for _, m := range e.Measures {
			out = append(out, Measure{Table: e.Name, Name: m.Name, Expression: m.Expression})
		}
	}
	return out
}

func aliasMap(from []entitySource) map[string]string {
	m := map[string]string{}
	for _, f := range from {
		m[f.Name] = f.Entity
	}
	return m
}

// expr is the subset of a semantic-query expression container this package reads.
type expr struct {
	SourceRef *struct {
		Entity string `json:"Entity"`
		Source string `json:"Source"`
	} `json:"SourceRef"`
	Column      *property `json:"Column"`
	Measure     *property `json:"Measure"`
	Aggregation *struct {
		Function   int             `json:"Function"`
		Expression json.RawMessage `json:"Expression"`
	} `json:"Aggregation"`
	HierarchyLevel *struct {
		Level      string `json:"Level"`
		Expression struct {
			Hierarchy *struct {
				Expression json.RawMessage `json:"Expression"`
				Hierarchy  string          `json:"Hierarchy"`
			} `json:"Hierarchy"`
		} `json:"Expression"`
	} `json:"HierarchyLevel"`
	Literal *struct {
		Value string `json:"Value"`
	} `json:"Literal"`
	In *struct {
		Expressions []json.RawMessage   `json:"Expressions"`
		Values      [][]json.RawMessage `json:"Values"`
	} `json:"In"`
	Not *struct {
		Expression json.RawMessage `json:"Expression"`
	} `json:"Not"`
	Comparison *struct {
		ComparisonKind int             `json:"ComparisonKind"`
		Left           json.RawMessage `json:"Left"`
		Right          json.RawMessage `json:"Right"`
	} `json:"Comparison"`
}

type property struct {
	Expression json.RawMessage `json:"Expression"`
	Property   string          `json:"Property"`
}

// aggregateNames is QueryAggregateFunction, in schema order.
var aggregateNames = []string{"Sum", "Average", "DistinctCount", "Min", "Max", "Count", "Median", "StandardDeviation", "Variance"}

// aggregateDAX is the DAX function each aggregate is written as, where one exists.
var aggregateDAX = map[string]string{"Sum": "SUM", "Average": "AVERAGE", "DistinctCount": "DISTINCTCOUNT",
	"Min": "MIN", "Max": "MAX", "Count": "COUNT", "Median": "MEDIAN", "StandardDeviation": "STDEV.P", "Variance": "VAR.P"}

// comparisonOps is QueryComparisonKind, in schema order.
var comparisonOps = []string{"=", ">", ">=", "<", "<="}

func quoteTable(t string) string { return "'" + strings.ReplaceAll(t, "'", "''") + "'" }

// entity resolves a SourceRef to its table: named directly, or through the
// query's From alias.
func entity(raw json.RawMessage, aliases map[string]string) string {
	var e expr
	if json.Unmarshal(raw, &e) != nil || e.SourceRef == nil {
		return ""
	}
	if e.SourceRef.Entity != "" {
		return e.SourceRef.Entity
	}
	if t, ok := aliases[e.SourceRef.Source]; ok {
		return t
	}
	return e.SourceRef.Source
}

// field decodes a field reference.
func field(raw json.RawMessage, aliases map[string]string) (Field, bool) {
	var e expr
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil {
		return Field{}, false
	}
	switch {
	case e.Column != nil:
		t := entity(e.Column.Expression, aliases)
		return Field{Kind: "Column", Table: t, Name: e.Column.Property, Reference: quoteTable(t) + "[" + e.Column.Property + "]"}, true
	case e.Measure != nil:
		t := entity(e.Measure.Expression, aliases)
		return Field{Kind: "Measure", Table: t, Name: e.Measure.Property, Reference: quoteTable(t) + "[" + e.Measure.Property + "]"}, true
	case e.Aggregation != nil:
		inner, ok := field(e.Aggregation.Expression, aliases)
		if !ok {
			return Field{}, false
		}
		fn := "Unknown"
		if e.Aggregation.Function >= 0 && e.Aggregation.Function < len(aggregateNames) {
			fn = aggregateNames[e.Aggregation.Function]
		}
		ref := inner.Reference
		if d, ok := aggregateDAX[fn]; ok {
			ref = d + "(" + inner.Reference + ")"
		}
		return Field{Kind: "Aggregation", Table: inner.Table, Name: inner.Name, Aggregation: fn, Reference: ref}, true
	case e.HierarchyLevel != nil && e.HierarchyLevel.Expression.Hierarchy != nil:
		h := e.HierarchyLevel.Expression.Hierarchy
		t := entity(h.Expression, aliases)
		return Field{Kind: "HierarchyLevel", Table: t, Name: e.HierarchyLevel.Level,
			Reference: quoteTable(t) + "[" + e.HierarchyLevel.Level + "]"}, true
	}
	return Field{}, false
}

func filters(raw []rawFilter) []Filter {
	out := []Filter{}
	for _, rf := range raw {
		fieldRaw := rf.Field
		if len(fieldRaw) == 0 {
			fieldRaw = rf.Expression
		}
		f := Filter{Name: rf.Name, Type: rf.Type}
		if fld, ok := field(fieldRaw, nil); ok {
			f.Field = fld.Reference
		}
		if rf.Filter != nil && len(rf.Filter.Where) > 0 {
			aliases := aliasMap(rf.Filter.From)
			cond := rf.Filter.Where[0].Condition
			if len(rf.Filter.Where) == 1 && restate(&f, cond, aliases) {
				out = append(out, f)
				continue
			}
			f.Operator = "Advanced"
			f.Condition = cond
		}
		out = append(out, f)
	}
	return out
}

// restate fills Operator and Values for a condition simple enough to state as
// a single column compared against values: In, Not In, or one comparison.
func restate(f *Filter, cond json.RawMessage, aliases map[string]string) bool {
	var e expr
	if json.Unmarshal(cond, &e) != nil {
		return false
	}
	switch {
	case e.In != nil && len(e.In.Expressions) == 1:
		vals, ok := inValues(e.In.Values)
		if !ok {
			return false
		}
		if fld, ok := field(e.In.Expressions[0], aliases); ok {
			f.Field = fld.Reference
		}
		f.Operator, f.Values = "In", vals
		return true
	case e.Not != nil:
		var inner Filter
		if !restate(&inner, e.Not.Expression, aliases) || inner.Operator != "In" {
			return false
		}
		if inner.Field != "" {
			f.Field = inner.Field
		}
		f.Operator, f.Values = "NotIn", inner.Values
		return true
	case e.Comparison != nil:
		k := e.Comparison.ComparisonKind
		var right expr
		if k < 0 || k >= len(comparisonOps) || json.Unmarshal(e.Comparison.Right, &right) != nil || right.Literal == nil {
			return false
		}
		if fld, ok := field(e.Comparison.Left, aliases); ok {
			f.Field = fld.Reference
		}
		f.Operator, f.Values = comparisonOps[k], []any{literal(right.Literal.Value)}
		return true
	}
	return false
}

func inValues(tuples [][]json.RawMessage) ([]any, bool) {
	out := make([]any, 0, len(tuples))
	for _, t := range tuples {
		if len(t) != 1 {
			return nil, false
		}
		var e expr
		if json.Unmarshal(t[0], &e) != nil || e.Literal == nil {
			return nil, false
		}
		out = append(out, literal(e.Literal.Value))
	}
	return out, true
}

// literal decodes a semantic-query literal: 'text' (quotes doubled inside),
// 24L integer, 2.4D double, 2.4M decimal, true/false, null, datetime'…'.
func literal(v string) any {
	switch {
	case v == "null":
		return nil
	case v == "true":
		return true
	case v == "false":
		return false
	case strings.HasPrefix(v, "datetime'") && strings.HasSuffix(v, "'"):
		return v[len("datetime'") : len(v)-1]
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	case strings.HasSuffix(v, "L"):
		if n, err := strconv.ParseInt(v[:len(v)-1], 10, 64); err == nil {
			return n
		}
	case strings.HasSuffix(v, "D") || strings.HasSuffix(v, "M"):
		if n, err := strconv.ParseFloat(v[:len(v)-1], 64); err == nil {
			return n
		}
	}
	return v
}
