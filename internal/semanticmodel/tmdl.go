package semanticmodel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// TMDL — the text serialisation Power BI Desktop writes for a semantic model,
// and what a `.pbip` project on disk actually contains. Where TMSL is one
// model.bim JSON document, TMDL is a folder: model.tmdl, tables/<Name>.tmdl,
// relationships.tmdl.
//
// The grammar is indentation-structured, not brace- or bracket-structured:
//
//	table Customer
//		column CustomerId
//			dataType: string
//			sourceColumn: CustomerId
//
//		measure 'Total Revenue' = SUM(Revenue[Revenue])
//
// Three rules carry most of it. An `object name` line opens a block; a
// `property: value` line sets a scalar on the enclosing block; and `name =
// expression` binds an expression, which may continue across the following
// MORE-indented lines (that is how a multi-line DAX measure is written).
//
// What this parser covers is what the emulator's evaluator can act on: tables,
// columns, measures, Direct Lake partitions and relationships. It deliberately
// does NOT cover the rest of the surface — perspectives, roles, cultures,
// calculation groups, annotations, hierarchies — and it ignores those blocks
// rather than failing, because a real .pbip carries them and refusing to load a
// model over a perspective the evaluator would never consult would be
// obstructive. That is a subset, and callers should read it as one.
//
// Reference: docs/18-semantic-model-references.md ("Model format (TMSL/TMDL)").

// tmdlLine is one significant line with its indentation depth.
type tmdlLine struct {
	depth int
	text  string
	file  string
	no    int
}

// ParseTMDL parses a TMDL folder into a Model. `parts` maps a definition path
// (e.g. "definition/tables/Customer.tmdl") to its bytes; non-.tmdl entries are
// ignored, so the caller can hand it a whole item definition.
func ParseTMDL(parts map[string][]byte) (*Model, error) {
	// Deterministic order: a model assembled from a map must not depend on Go's
	// map iteration, or two loads of the same definition could differ.
	paths := make([]string, 0, len(parts))
	for p := range parts {
		if strings.HasSuffix(strings.ToLower(p), ".tmdl") {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no .tmdl parts in definition")
	}
	sort.Strings(paths)

	m := &Model{Expressions: map[string]string{}}
	for _, p := range paths {
		lines := tmdlLines(p, parts[p])
		for i := 0; i < len(lines); {
			consumed, err := parseTMDLBlock(m, lines, i)
			if err != nil {
				return nil, err
			}
			i += consumed
		}
	}
	if len(m.Tables) == 0 {
		return nil, fmt.Errorf("tmdl: no tables in definition")
	}
	if m.CompatibilityLevel == 0 {
		m.CompatibilityLevel = 1550
	}
	return m, nil
}

// tmdlLines strips comments and blank lines and records each line's depth.
// Power BI writes tabs; a hand-authored file may use spaces, so both count,
// with four spaces to a tab.
func tmdlLines(file string, b []byte) []tmdlLine {
	var out []tmdlLine
	for n, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimLeft(line, " \t")
		// `///` is a description, `//` a comment. Neither changes the model.
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			continue
		}
		depth := 0
		for _, r := range line[:len(line)-len(trimmed)] {
			if r == '\t' {
				depth += 4
			} else {
				depth++
			}
		}
		out = append(out, tmdlLine{depth: depth, text: trimmed, file: file, no: n + 1})
	}
	return out
}

// blockAt returns the lines strictly nested under lines[i], and how many lines
// the whole block (header + body) occupies.
func blockAt(lines []tmdlLine, i int) ([]tmdlLine, int) {
	head := lines[i]
	j := i + 1
	for j < len(lines) && lines[j].depth > head.depth {
		j++
	}
	return lines[i+1 : j], j - i
}

// parseTMDLBlock handles one top-level block and returns how many lines it took.
func parseTMDLBlock(m *Model, lines []tmdlLine, i int) (int, error) {
	body, span := blockAt(lines, i)
	kw, name := splitDecl(lines[i].text)

	switch kw {
	case "model":
		for _, l := range body {
			if k, v, ok := splitProp(l.text); ok && k == "compatibilityLevel" {
				m.CompatibilityLevel, _ = strconv.Atoi(v)
			}
		}
		if name != "" && m.Name == "" {
			m.Name = name
		}
	case "table":
		t, err := parseTMDLTable(name, body)
		if err != nil {
			return span, err
		}
		m.Tables = append(m.Tables, *t)
	case "relationship":
		m.Relationships = append(m.Relationships, parseTMDLRelationship(name, body))
	case "expression":
		// `expression Name = <M query>` — the shared source a Direct Lake
		// partition points at.
		if _, expr, ok := splitAssign(lines[i].text); ok {
			m.Expressions[name] = strings.TrimSpace(expr + "\n" + joinBody(body))
		} else {
			m.Expressions[name] = joinBody(body)
		}
	default:
		// database, perspective, role, culture, annotation, ref … — carried by
		// real .pbip projects and irrelevant to the evaluator. Skipped, not an
		// error: see the package note above.
	}
	return span, nil
}

func parseTMDLTable(name string, body []tmdlLine) (*Table, error) {
	t := &Table{Name: unquote(name)}
	for i := 0; i < len(body); {
		sub, span := blockAt(body, i)
		kw, decl := splitDecl(body[i].text)
		switch kw {
		case "column":
			col := Column{Name: unquote(decl)}
			for _, l := range sub {
				if k, v, ok := splitProp(l.text); ok {
					switch k {
					case "dataType":
						col.DataType = v
					case "sourceColumn":
						col.SourceColumn = unquote(v)
					}
				}
			}
			if col.SourceColumn == "" {
				col.SourceColumn = col.Name
			}
			t.Columns = append(t.Columns, col)
		case "measure":
			// `measure Name = <DAX>`, the DAX optionally continuing across the
			// following more-indented lines.
			lhs, expr, ok := splitAssign(body[i].text)
			mname := unquote(strings.TrimSpace(strings.TrimPrefix(lhs, "measure")))
			full := strings.TrimSpace(expr)
			if cont := joinBody(exprBody(sub)); cont != "" {
				full = strings.TrimSpace(full + " " + cont)
			}
			if !ok || full == "" {
				return nil, fmt.Errorf("tmdl: measure %q in table %q has no expression",
					mname, t.Name)
			}
			t.Measures = append(t.Measures, Measure{Name: mname, Expression: full})
		case "partition":
			if dl := parseTMDLPartition(sub); dl != nil {
				t.DirectLake = dl
			}
		}
		i += span
	}
	return t, nil
}

// parseTMDLPartition reads a Direct Lake partition's entity binding. An import
// partition has no entityName and yields nil, matching TMSL's shape.
func parseTMDLPartition(body []tmdlLine) *DirectLakePartition {
	dl := &DirectLakePartition{}
	for _, l := range body {
		if k, v, ok := splitProp(l.text); ok {
			switch k {
			case "entityName":
				dl.EntityName = unquote(v)
			case "schemaName":
				dl.SchemaName = unquote(v)
			case "expressionSource":
				dl.ExpressionSource = unquote(v)
			}
		}
	}
	if dl.EntityName == "" {
		return nil
	}
	return dl
}

func parseTMDLRelationship(name string, body []tmdlLine) Relationship {
	r := Relationship{Name: unquote(name)}
	for _, l := range body {
		k, v, ok := splitProp(l.text)
		if !ok {
			continue
		}
		// TMDL writes endpoints as Table.Column, where TMSL splits them across
		// fromTable/fromColumn.
		switch k {
		case "fromColumn":
			r.FromTable, r.FromColumn = splitRef(v)
		case "toColumn":
			r.ToTable, r.ToColumn = splitRef(v)
		case "fromTable":
			r.FromTable = unquote(v)
		case "toTable":
			r.ToTable = unquote(v)
		}
	}
	return r
}

// --- small helpers -----------------------------------------------------------

// splitDecl splits "table Customer" into ("table", "Customer"). For a line that
// also assigns ("measure 'X' = DAX") the name half is returned unsplit; the
// caller uses splitAssign for those.
func splitDecl(s string) (string, string) {
	if i := strings.Index(s, "="); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	kw, rest, _ := strings.Cut(s, " ")
	return kw, strings.TrimSpace(rest)
}

// splitProp reads "dataType: string". It must not swallow "measure X = ..." or
// a DAX line containing a colon, so it only accepts a bare identifier key.
func splitProp(s string) (string, string, bool) {
	k, v, ok := strings.Cut(s, ":")
	if !ok {
		return "", "", false
	}
	k = strings.TrimSpace(k)
	if k == "" || strings.ContainsAny(k, " \t'\"=[](),") {
		return "", "", false
	}
	return k, strings.TrimSpace(v), true
}

func splitAssign(s string) (string, string, bool) {
	l, r, ok := strings.Cut(s, "=")
	return strings.TrimSpace(l), strings.TrimSpace(r), ok
}

// exprBody drops property lines from a measure's block, keeping only the
// continuation of its expression. formatString/displayFolder and friends are
// properties, not DAX.
func exprBody(body []tmdlLine) []tmdlLine {
	var out []tmdlLine
	for _, l := range body {
		if _, _, ok := splitProp(l.text); ok {
			continue
		}
		out = append(out, l)
	}
	return out
}

func joinBody(body []tmdlLine) string {
	parts := make([]string, 0, len(body))
	for _, l := range body {
		parts = append(parts, l.text)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// splitRef splits a TMDL "Table.Column" endpoint, honouring quoting on either
// side ('Daily Revenue'.Country).
func splitRef(s string) (string, string) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "'") {
		if end := strings.Index(s[1:], "'"); end >= 0 {
			tbl := s[1 : 1+end]
			col := strings.TrimPrefix(strings.TrimSpace(s[2+end:]), ".")
			return tbl, unquote(col) // the column half may be quoted too
		}
	}
	tbl, col, _ := strings.Cut(s, ".")
	return unquote(tbl), unquote(col)
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '\'' && s[len(s)-1] == '\'') ||
		(s[0] == '"' && s[len(s)-1] == '"')) {
		return s[1 : len(s)-1]
	}
	return s
}
