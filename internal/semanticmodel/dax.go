package semanticmodel

import (
	"fmt"
	"strconv"
	"strings"
)

// A bounded DAX evaluator — the subset the golden fixture (and the SemPy/GX
// tutorial's four assets) needs: `EVALUATE <table>`, `SUMMARIZECOLUMNS`, measure
// references, `SUM`, `DIVIDE`, and single-hop relationship filter propagation.
// Not full DAX (no CALCULATE filter modifiers, no time-intelligence, no row
// context beyond aggregation) — unsupported constructs error out rather than
// mis-evaluate. Correctness is gated by the captured golden fixtures, since no
// live DAX engine can run in CI.

// Result is a query result: ordered column keys + rows keyed by them, matching
// the executeQueries JSON shape ("Table[Col]" / "[Measure]").
type Result struct {
	Columns []string
	Rows    []map[string]any
}

// Evaluate runs a DAX query string against the model + data.
func Evaluate(m *Model, d Data, query string) (*Result, error) {
	toks, err := lex(query)
	if err != nil {
		return nil, err
	}
	p := &daxParser{toks: toks}
	te, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	e := &evalr{model: m, data: d, ctx: filterCtx{}}
	return e.table(te)
}

// --- tokens ------------------------------------------------------------------

type tkind int

const (
	tqTable  tkind = iota // 'Quoted Table'
	tBracket              // [Bracketed]
	tString               // "string"
	tIdent                // identifier
	tNum                  // number
	tPunct                // ( ) ,
)

type dtok struct {
	kind tkind
	text string
}

func lex(s string) ([]dtok, error) {
	var out []dtok
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'' || c == '"' || c == '[':
			close := map[byte]byte{'\'': '\'', '"': '"', '[': ']'}[c]
			j := i + 1
			for j < len(s) && s[j] != close {
				j++
			}
			if j >= len(s) {
				return nil, fmt.Errorf("unterminated %c in DAX", c)
			}
			kind := map[byte]tkind{'\'': tqTable, '"': tString, '[': tBracket}[c]
			out = append(out, dtok{kind, s[i+1 : j]})
			i = j + 1
		case c == '(' || c == ')' || c == ',':
			out = append(out, dtok{tPunct, string(c)})
			i++
		case c >= '0' && c <= '9' || (c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
			j := i + 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			out = append(out, dtok{tNum, s[i:j]})
			i = j
		case isAlpha(c):
			j := i + 1
			for j < len(s) && (isAlpha(s[j]) || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			out = append(out, dtok{tIdent, s[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q in DAX", string(c))
		}
	}
	return out, nil
}

func isAlpha(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// --- AST ---------------------------------------------------------------------

type tableExpr interface{}
type tableRef struct{ name string }
type summarize struct {
	groups  []columnRef
	outputs []outputCol
}
type outputCol struct {
	name string
	expr scalarExpr
}

type scalarExpr interface{}
type numberLit struct{ v float64 }
type measureRef struct{ name string }
type columnRef struct{ table, col string }
type funcCall struct {
	name string
	args []scalarExpr
}

// --- parser ------------------------------------------------------------------

type daxParser struct {
	toks []dtok
	pos  int
}

func (p *daxParser) peek() *dtok {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}
func (p *daxParser) next() *dtok { t := p.peek(); p.pos++; return t }

func (p *daxParser) parseQuery() (tableExpr, error) {
	t := p.next()
	if t == nil || t.kind != tIdent || !strings.EqualFold(t.text, "EVALUATE") {
		return nil, fmt.Errorf("DAX query must start with EVALUATE")
	}
	te, err := p.parseTableExpr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("trailing tokens after DAX table expression")
	}
	return te, nil
}

func (p *daxParser) parseTableExpr() (tableExpr, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("expected a table expression")
	}
	if t.kind == tIdent && strings.EqualFold(t.text, "SUMMARIZECOLUMNS") {
		return p.parseSummarize()
	}
	if t.kind == tqTable || t.kind == tIdent {
		p.next()
		return tableRef{name: t.text}, nil
	}
	return nil, fmt.Errorf("unsupported table expression starting with %q", t.text)
}

func (p *daxParser) parseSummarize() (tableExpr, error) {
	p.next() // SUMMARIZECOLUMNS
	if o := p.next(); o == nil || o.text != "(" {
		return nil, fmt.Errorf("SUMMARIZECOLUMNS expects '('")
	}
	s := summarize{}
	for {
		t := p.peek()
		if t == nil {
			return nil, fmt.Errorf("unterminated SUMMARIZECOLUMNS")
		}
		if t.kind == tString { // "name", <expr> output pair
			p.next()
			if c := p.next(); c == nil || c.text != "," {
				return nil, fmt.Errorf("SUMMARIZECOLUMNS output %q needs an expression", t.text)
			}
			expr, err := p.parseScalar()
			if err != nil {
				return nil, err
			}
			s.outputs = append(s.outputs, outputCol{name: t.text, expr: expr})
		} else { // group column reference
			cr, err := p.parseColumnRef()
			if err != nil {
				return nil, err
			}
			s.groups = append(s.groups, cr)
		}
		sep := p.next()
		if sep == nil {
			return nil, fmt.Errorf("unterminated SUMMARIZECOLUMNS")
		}
		if sep.text == ")" {
			return s, nil
		}
		if sep.text != "," {
			return nil, fmt.Errorf("expected ',' or ')' in SUMMARIZECOLUMNS, got %q", sep.text)
		}
	}
}

// parseColumnRef parses `'Table'[Col]` or `Table[Col]`.
func (p *daxParser) parseColumnRef() (columnRef, error) {
	tbl := p.next()
	if tbl == nil || (tbl.kind != tqTable && tbl.kind != tIdent) {
		return columnRef{}, fmt.Errorf("expected a table name in a column reference")
	}
	col := p.next()
	if col == nil || col.kind != tBracket {
		return columnRef{}, fmt.Errorf("expected [Column] after table %q", tbl.text)
	}
	return columnRef{table: tbl.text, col: col.text}, nil
}

func (p *daxParser) parseScalar() (scalarExpr, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("expected a scalar expression")
	}
	switch t.kind {
	case tNum:
		p.next()
		v, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, err
		}
		return numberLit{v}, nil
	case tBracket: // measure reference
		p.next()
		return measureRef{name: t.text}, nil
	case tqTable, tIdent:
		// column ref (table + [col]), a function call (ident + '('), or a bare table.
		if t.kind == tIdent && p.pos+1 < len(p.toks) && p.toks[p.pos+1].text == "(" {
			return p.parseFuncCall()
		}
		if p.pos+1 < len(p.toks) && p.toks[p.pos+1].kind == tBracket {
			return p.parseColumnRef()
		}
		p.next()
		return tableRef{name: t.text}, nil // bare table (e.g. COUNTROWS(Sales))
	}
	return nil, fmt.Errorf("unexpected token %q in scalar expression", t.text)
}

func (p *daxParser) parseFuncCall() (scalarExpr, error) {
	name := p.next().text
	p.next() // '('
	fc := funcCall{name: name}
	if c := p.peek(); c != nil && c.text == ")" {
		p.next()
		return fc, nil
	}
	for {
		arg, err := p.parseScalar()
		if err != nil {
			return nil, err
		}
		fc.args = append(fc.args, arg)
		sep := p.next()
		if sep == nil {
			return nil, fmt.Errorf("unterminated call to %s", name)
		}
		if sep.text == ")" {
			return fc, nil
		}
		if sep.text != "," {
			return nil, fmt.Errorf("expected ',' or ')' in %s(), got %q", name, sep.text)
		}
	}
}

// --- evaluation --------------------------------------------------------------

// filterCtx is an equality filter context: table → column → required value.
type filterCtx map[string]map[string]any

type evalr struct {
	model *Model
	data  Data
	ctx   filterCtx
}

func (e *evalr) table(te tableExpr) (*Result, error) {
	switch t := te.(type) {
	case tableRef:
		return e.evalTableRef(t.name)
	case summarize:
		return e.evalSummarize(t)
	}
	return nil, fmt.Errorf("unsupported table expression %T", te)
}

// evalTableRef returns every row/column of a table (EVALUATE 'Store').
func (e *evalr) evalTableRef(name string) (*Result, error) {
	name = strings.Trim(name, "'")
	tbl := e.model.Table(name)
	if tbl == nil {
		return nil, fmt.Errorf("no table %q", name)
	}
	res := &Result{}
	for _, c := range tbl.Columns {
		res.Columns = append(res.Columns, colKey(name, c.Name))
	}
	for _, r := range e.data.Rows(name) {
		row := map[string]any{}
		for _, c := range tbl.Columns {
			row[colKey(name, c.Name)] = r[c.Name]
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

// evalSummarize groups by the group columns and evaluates each output per group
// under a filter context, dropping all-blank groups (SUMMARIZECOLUMNS semantics).
func (e *evalr) evalSummarize(s summarize) (*Result, error) {
	res := &Result{}
	for _, g := range s.groups {
		res.Columns = append(res.Columns, colKey(strings.Trim(g.table, "'"), g.col))
	}
	for _, o := range s.outputs {
		res.Columns = append(res.Columns, "["+o.name+"]")
	}

	combos, err := e.groupCombos(s.groups)
	if err != nil {
		return nil, err
	}
	for _, combo := range combos {
		e.ctx = combo // set filter context for this group
		row := map[string]any{}
		for _, g := range s.groups {
			row[colKey(strings.Trim(g.table, "'"), g.col)] = combo[strings.Trim(g.table, "'")][g.col]
		}
		allBlank := true
		for _, o := range s.outputs {
			v, err := e.scalar(o.expr)
			if err != nil {
				e.ctx = filterCtx{}
				return nil, err
			}
			row["["+o.name+"]"] = v
			if v != nil {
				allBlank = false
			}
		}
		if !allBlank || len(s.outputs) == 0 {
			res.Rows = append(res.Rows, row)
		}
	}
	e.ctx = filterCtx{}
	return res, nil
}

// groupCombos returns the distinct group-column value combinations, as filter
// contexts. Group columns from the same table are correlated (same source row);
// columns from different tables are cross-joined.
func (e *evalr) groupCombos(groups []columnRef) ([]filterCtx, error) {
	byTable := map[string][]string{}
	var order []string
	for _, g := range groups {
		tn := strings.Trim(g.table, "'")
		if e.model.Table(tn) == nil {
			return nil, fmt.Errorf("no table %q", tn)
		}
		if _, ok := byTable[tn]; !ok {
			order = append(order, tn)
		}
		byTable[tn] = append(byTable[tn], g.col)
	}

	combos := []filterCtx{{}}
	for _, tn := range order {
		var perTable []filterCtx
		seen := map[string]bool{}
		for _, r := range e.data.Rows(tn) {
			key := ""
			sub := map[string]any{}
			for _, col := range byTable[tn] {
				sub[col] = r[col]
				key += fmt.Sprint(r[col]) + "\x1f"
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			perTable = append(perTable, filterCtx{tn: sub})
		}
		// cross-join combos × perTable
		var next []filterCtx
		for _, base := range combos {
			for _, add := range perTable {
				merged := filterCtx{}
				for k, v := range base {
					merged[k] = v
				}
				for k, v := range add {
					merged[k] = v
				}
				next = append(next, merged)
			}
		}
		combos = next
	}
	return combos, nil
}

func (e *evalr) scalar(expr scalarExpr) (any, error) {
	switch x := expr.(type) {
	case numberLit:
		return x.v, nil
	case measureRef:
		m := e.model.Measure(x.name)
		if m == nil {
			return nil, fmt.Errorf("no measure [%s]", x.name)
		}
		toks, err := lex(m.Expression)
		if err != nil {
			return nil, err
		}
		sp := &daxParser{toks: toks}
		ast, err := sp.parseScalar()
		if err != nil {
			return nil, fmt.Errorf("measure [%s]: %w", x.name, err)
		}
		return e.scalar(ast)
	case funcCall:
		return e.evalFunc(x)
	case columnRef:
		return nil, fmt.Errorf("column %s[%s] used outside an aggregation", x.table, x.col)
	}
	return nil, fmt.Errorf("unsupported scalar expression %T", expr)
}

func (e *evalr) evalFunc(fc funcCall) (any, error) {
	switch strings.ToUpper(fc.name) {
	case "SUM":
		col, ok := fc.args[0].(columnRef)
		if !ok {
			return nil, fmt.Errorf("SUM expects a column reference")
		}
		var s float64
		for _, r := range e.activeRows(strings.Trim(col.table, "'")) {
			s += toF(r[col.col])
		}
		return s, nil
	case "DIVIDE":
		if len(fc.args) < 2 {
			return nil, fmt.Errorf("DIVIDE expects 2 arguments")
		}
		a, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		b, err := e.scalar(fc.args[1])
		if err != nil {
			return nil, err
		}
		den := toF(b)
		if den == 0 {
			return nil, nil // DAX DIVIDE → blank on divide-by-zero
		}
		return toF(a) / den, nil
	case "COUNTROWS":
		tr, ok := fc.args[0].(tableRef)
		if !ok {
			return nil, fmt.Errorf("COUNTROWS expects a table")
		}
		return float64(len(e.activeRows(strings.Trim(tr.name, "'")))), nil
	}
	return nil, fmt.Errorf("unsupported DAX function %q", fc.name)
}

// activeRows returns the rows of `table` under the current filter context —
// direct equality constraints on `table`, plus single-hop propagation from a
// related constrained table (star-schema filtering).
func (e *evalr) activeRows(table string) []Row {
	var out []Row
	for _, r := range e.data.Rows(table) {
		if e.matches(table, r) {
			out = append(out, r)
		}
	}
	return out
}

func (e *evalr) matches(table string, r Row) bool {
	for ct, cols := range e.ctx {
		if ct == table {
			for c, v := range cols {
				if !valEq(r[c], v) {
					return false
				}
			}
			continue
		}
		rel := e.model.RelationshipBetween(table, ct)
		if rel == nil {
			continue // unrelated constraint doesn't filter this table (subset)
		}
		myCol, theirCol := rel.ToColumn, rel.FromColumn
		if rel.FromTable == table {
			myCol, theirCol = rel.FromColumn, rel.ToColumn
		}
		if !e.relatedKeyAllowed(ct, theirCol, cols, r[myCol]) {
			return false
		}
	}
	return true
}

// relatedKeyAllowed reports whether `key` matches a row of the related table
// `ct` (joined on `keyCol`) that satisfies ct's constraints `cols`.
func (e *evalr) relatedKeyAllowed(ct, keyCol string, cols map[string]any, key any) bool {
	for _, r := range e.data.Rows(ct) {
		ok := true
		for c, v := range cols {
			if !valEq(r[c], v) {
				ok = false
				break
			}
		}
		if ok && valEq(r[keyCol], key) {
			return true
		}
	}
	return false
}

func colKey(table, col string) string { return table + "[" + col + "]" }

func toF(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func valEq(a, b any) bool {
	af, aok := a.(float64)
	bf, bok := b.(float64)
	if aok && bok {
		return af == bf
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}
