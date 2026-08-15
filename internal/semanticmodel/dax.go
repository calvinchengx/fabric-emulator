package semanticmodel

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// A bounded DAX evaluator — the subset the golden fixture (and the SemPy/GX
// tutorial's four assets) needs: `EVALUATE <table>`, `SUMMARIZECOLUMNS`, measure
// references, `SUM`, `DIVIDE`, `COUNTROWS`, `IF`, `ACOS`, `ABS`, `ROUND`, `LOG`, `LOG10`, `INT`, `SIGN`, the infix operators
// (`+ - * / &` and the comparisons) and single-hop relationship filter
// propagation. Not full DAX (no CALCULATE filter modifiers, no time-intelligence,
// no row context beyond aggregation) — unsupported constructs error out rather
// than mis-evaluate. Correctness is gated by captured goldens: tutorial
// fixtures plus Desktop-agreed scalars in desktop_goldens.json (docs/52
// Phase 3). Every-push CI replays those against Go; it does not boot msmdsrv.

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
	tOp                   // + - * / & = <> < <= > >=
)

// opChars are the characters that begin an infix (or unary) operator.
const opChars = "+-*/&=<>"

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
		case strings.IndexByte(opChars, c) >= 0:
			// `-` is always an operator, never the sign of a literal: making it
			// part of the number would lex `[A] -1` as two operands and leave
			// the parser no subtraction to see. Negation is the parser's job.
			op := string(c)
			if i+1 < len(s) && (c == '<' && (s[i+1] == '=' || s[i+1] == '>') || c == '>' && s[i+1] == '=') {
				op = s[i : i+2]
			}
			out = append(out, dtok{tOp, op})
			i += len(op)
		case c >= '0' && c <= '9':
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
type stringLit struct{ v string }
type measureRef struct{ name string }
type columnRef struct{ table, col string }
type funcCall struct {
	name string
	args []scalarExpr
}
type binaryExpr struct {
	op   string
	l, r scalarExpr
}

// daxPrec lists the infix operators by binding strength, loosest group first.
// This is DAX's own order: comparison binds loosest, then `&`, then `+`/`-`,
// then `*`/`/`, with unary `-` tighter than all of them. Every group is
// left-associative.
var daxPrec = [][]string{
	{"=", "<>", "<", "<=", ">", ">="},
	{"&"},
	{"+", "-"},
	{"*", "/"},
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

// parseScalar parses a full scalar expression, operators included.
func (p *daxParser) parseScalar() (scalarExpr, error) { return p.parseBinary(0) }

// parseBinary parses one precedence level of daxPrec by precedence climbing:
// collect a tighter-binding operand, then fold in every operator at this level.
func (p *daxParser) parseBinary(level int) (scalarExpr, error) {
	if level == len(daxPrec) {
		return p.parseUnary()
	}
	lhs, err := p.parseBinary(level + 1)
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || t.kind != tOp || !slices.Contains(daxPrec[level], t.text) {
			return lhs, nil
		}
		p.next()
		rhs, err := p.parseBinary(level + 1)
		if err != nil {
			return nil, err
		}
		lhs = binaryExpr{op: t.text, l: lhs, r: rhs}
	}
}

func (p *daxParser) parseUnary() (scalarExpr, error) {
	t := p.peek()
	if t == nil || t.kind != tOp {
		return p.parsePrimary()
	}
	if t.text != "-" && t.text != "+" {
		return nil, fmt.Errorf("operator %q has no left-hand operand", t.text)
	}
	p.next()
	x, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	if t.text == "+" {
		return x, nil // unary plus is a no-op
	}
	if n, ok := x.(numberLit); ok {
		return numberLit{-n.v}, nil
	}
	// Negation of anything else is `0 - x`, which gets BLANK coercion for free.
	return binaryExpr{op: "-", l: numberLit{0}, r: x}, nil
}

func (p *daxParser) parsePrimary() (scalarExpr, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("expected a scalar expression")
	}
	switch t.kind {
	case tString:
		p.next()
		return stringLit{t.text}, nil
	case tPunct:
		if t.text != "(" {
			break
		}
		p.next()
		x, err := p.parseScalar()
		if err != nil {
			return nil, err
		}
		if c := p.next(); c == nil || c.text != ")" {
			return nil, fmt.Errorf("expected ')' to close a parenthesized expression")
		}
		return x, nil
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

// maxMeasureDepth bounds how far measure references may nest before evaluation
// gives up. A measure's expression is re-parsed and evaluated in place, so
// `M = [M]` — or any A→B→A cycle — recurses without end. Go treats a stack
// overflow as a FATAL error that net/http's per-request recover cannot catch,
// so an unbounded evaluator turns one malformed model into a crash of the whole
// emulator, taking every other in-flight request with it.
//
// 64 is far past any real model (measures referencing measures rarely exceed a
// handful) and far short of the stack.
const maxMeasureDepth = 64

type evalr struct {
	model *Model
	data  Data
	ctx   filterCtx
	// depth counts measure expansions on the current evaluation path.
	depth int
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
	case stringLit:
		return x.v, nil
	case measureRef:
		m := e.model.Measure(x.name)
		if m == nil {
			return nil, fmt.Errorf("no measure [%s]", x.name)
		}
		if e.depth >= maxMeasureDepth {
			return nil, fmt.Errorf("measure [%s]: reference nested more than %d deep, "+
				"which a self-referential or cyclic definition does", x.name, maxMeasureDepth)
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
		// A half-understood expression must not evaluate to its understood
		// half: without this check `[A] + [B] IN {1}` quietly returns [A]+[B].
		if sp.pos != len(sp.toks) {
			return nil, fmt.Errorf("measure [%s]: unsupported DAX from %q onwards",
				x.name, sp.toks[sp.pos].text)
		}
		e.depth++
		defer func() { e.depth-- }()
		return e.scalar(ast)
	case binaryExpr:
		return e.binary(x)
	case funcCall:
		return e.evalFunc(x)
	case columnRef:
		// Real DAX refuses a bare column here too — a filter context gives no
		// row from which to read one value. The likeliest way to arrive is
		// building a label with `&`, so the message names a way out.
		//
		// SELECTEDVALUE leads, because it is the only exit that keeps the
		// caller's intent: grouping returns the raw column when the point was to
		// DERIVE something from it. It is named only now that it exists —
		// pointing at an unimplemented function would have walked the reader
		// into a second `unsupported DAX function` wall.
		//
		// SUM is offered only for a column that can actually be summed, since
		// following that advice on a text column is how someone ends up with a
		// label reading "FY0".
		tbl := strings.Trim(x.table, "'")
		fix := fmt.Sprintf("read its one value with SELECTEDVALUE(%s[%s]), or group by %s[%s] to return it as its own column",
			tbl, x.col, tbl, x.col)
		if t := e.model.Table(tbl); t != nil {
			if c := t.Column(x.col); c != nil && summableType(c.DataType) {
				fix += fmt.Sprintf(", or aggregate it (SUM(%s[%s]))", tbl, x.col)
			}
		}
		return nil, fmt.Errorf("column %s[%s] has no single value in this context: %s", tbl, x.col, fix)
	}
	return nil, fmt.Errorf("unsupported scalar expression %T", expr)
}

// binary evaluates an infix operation. Both operands are evaluated first —
// DAX has no short-circuiting operators.
func (e *evalr) binary(b binaryExpr) (any, error) {
	l, err := e.scalar(b.l)
	if err != nil {
		return nil, err
	}
	r, err := e.scalar(b.r)
	if err != nil {
		return nil, err
	}
	switch b.op {
	case "&":
		return dstr(l) + dstr(r), nil
	case "=":
		return daxCmp(l, r) == 0, nil
	case "<>":
		return daxCmp(l, r) != 0, nil
	case "<":
		return daxCmp(l, r) < 0, nil
	case "<=":
		return daxCmp(l, r) <= 0, nil
	case ">":
		return daxCmp(l, r) > 0, nil
	case ">=":
		return daxCmp(l, r) >= 0, nil
	}

	lf, err := arithNum(l, b.op)
	if err != nil {
		return nil, err
	}
	rf, err := arithNum(r, b.op)
	if err != nil {
		return nil, err
	}
	switch b.op {
	case "+":
		return lf + rf, nil
	case "-":
		return lf - rf, nil
	case "*":
		return lf * rf, nil
	}
	// Only "/" reaches here — the parser builds operators from daxPrec alone.
	if rf == 0 {
		// Real DAX yields Infinity, which has no JSON encoding; erroring keeps
		// the difference visible instead of inventing a number. DIVIDE(a, b)
		// is the blank-on-zero form and works.
		return nil, fmt.Errorf("division by zero — use DIVIDE(a, b) for DAX's blank-on-zero division")
	}
	return lf / rf, nil
}

// summableType reports whether a TMSL/TMDL dataType is one SUM can total. An
// unknown or absent dataType answers false: the point is to avoid recommending
// an aggregation that would silently return zero, so silence is the safe side.
func summableType(t string) bool {
	switch strings.ToLower(t) {
	case "int64", "double", "decimal", "currency":
		return true
	}
	return false
}

// arithNum coerces an operand of +, -, * or / to a number: BLANK counts as 0
// and a numeric string converts, but text that is not a number errors rather
// than silently becoming zero.
func arithNum(v any, op string) (float64, error) {
	if f, ok := asNumber(v); ok {
		return f, nil
	}
	if t, isText := v.(string); isText {
		return 0, fmt.Errorf("cannot apply %q to the text value %q", op, t)
	}
	return 0, fmt.Errorf("cannot apply %q to %T", op, v)
}

// isBlankText reports whether a column value is empty or whitespace-only text.
// That is how every CSV and warehouse export writes a missing number, so a
// numeric column that arrived as text would otherwise refuse the moment one
// value is absent — which in real data is always. It applies to column values
// only: a literal "" written into an expression stays an error, because that is
// deliberate text rather than an absent reading.
func isBlankText(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// asNumber converts a value for arithmetic: BLANK is 0, a boolean is 1 or 0,
// and a numeric string parses. Anything else answers false — never 0, which is
// the coercion that let text total silently.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case nil:
		return 0, true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case string:
		// Exactly what ParseFloat accepts once surrounding whitespace is gone —
		// so "1e3" is 1000 and " 42 " is 42, while "1,234", "$99" and "50%"
		// refuse. The line is deliberate: padding is an artefact of export, but
		// separators, symbols and percent signs are locale-dependent
		// presentation, and guessing a locale is how a sum silently loses a
		// factor of a thousand.
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return numeric(v)
}

// daxCmp orders two values: numerically when both sides are numbers (BLANK
// counting as 0 against one), lexically otherwise.
func daxCmp(l, r any) int {
	lf, lok := numeric(l)
	rf, rok := numeric(r)
	if l == nil && rok {
		lf, lok = 0, true
	}
	if r == nil && lok {
		rf, rok = 0, true
	}
	if lok && rok {
		switch {
		case lf < rf:
			return -1
		case lf > rf:
			return 1
		}
		return 0
	}
	return strings.Compare(dstr(l), dstr(r))
}

// truthy reads a DAX condition: FALSE, BLANK, 0 and "" are false.
func truthy(v any) bool {
	switch n := v.(type) {
	case nil:
		return false
	case bool:
		return n
	}
	if f, ok := numeric(v); ok {
		return f != 0
	}
	return dstr(v) != ""
}

// dstr renders a value the way `&` concatenation does: BLANK is the empty
// string and whole numbers keep no trailing ".0".
func dstr(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case bool:
		if n {
			return "TRUE"
		}
		return "FALSE"
	}
	if f, ok := numeric(v); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func (e *evalr) evalFunc(fc funcCall) (any, error) {
	switch strings.ToUpper(fc.name) {
	case "SUM":
		// The arity check comes first: `SUM()` parses fine into an empty arg
		// list, so indexing before checking panics on a query a client can send.
		if len(fc.args) < 1 {
			return nil, fmt.Errorf("SUM expects a column reference")
		}
		col, ok := fc.args[0].(columnRef)
		if !ok {
			return nil, fmt.Errorf("SUM expects a column reference")
		}
		tbl, err := e.resolveColumn("SUM", col)
		if err != nil {
			return nil, err
		}
		var s float64
		for _, r := range e.activeRows(tbl) {
			v := r[col.col]
			if v == nil || isBlankText(v) {
				continue // BLANK contributes nothing to a sum
			}
			f, ok := asNumber(v)
			if !ok {
				// Every value used to go through a coercion that returned 0 for
				// anything it could not read, so summing a text column reported
				// a confident zero — worse than an error, because a caller acts
				// on a number.
				return nil, fmt.Errorf("cannot sum %s[%s]: the value %q is not a number",
					tbl, col.col, fmt.Sprint(v))
			}
			s += f
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
		num, err := arithNum(a, "DIVIDE")
		if err != nil {
			return nil, err
		}
		den, err := arithNum(b, "DIVIDE")
		if err != nil {
			return nil, err
		}
		if den == 0 {
			return nil, nil // DAX DIVIDE → blank on divide-by-zero
		}
		return num / den, nil
	case "IF":
		if len(fc.args) < 2 {
			return nil, fmt.Errorf("IF expects a condition and a value")
		}
		cond, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		if truthy(cond) {
			return e.scalar(fc.args[1])
		}
		if len(fc.args) > 2 {
			return e.scalar(fc.args[2])
		}
		return nil, nil // omitted else branch → blank
	case "COUNTROWS":
		if len(fc.args) < 1 {
			return nil, fmt.Errorf("COUNTROWS expects a table")
		}
		tr, ok := fc.args[0].(tableRef)
		if !ok {
			return nil, fmt.Errorf("COUNTROWS expects a table")
		}
		tbl, err := e.resolveTable("COUNTROWS", tr.name)
		if err != nil {
			return nil, err
		}
		return float64(len(e.activeRows(tbl))), nil
	case "SELECTEDVALUE":
		return e.selectedValue(fc)
	case "ACOS":
		if len(fc.args) != 1 {
			return nil, fmt.Errorf("ACOS expects 1 argument")
		}
		a, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		f, err := arithNum(a, "ACOS")
		if err != nil {
			return nil, err
		}
		if f < -1 || f > 1 {
			return nil, fmt.Errorf("ACOS argument must be between -1 and 1")
		}
		return math.Acos(f), nil
	case "ABS":
		if len(fc.args) != 1 {
			return nil, fmt.Errorf("ABS expects 1 argument")
		}
		a, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		// DAX ABS(BLANK) is BLANK. arithNum/asNumber treat nil as 0, which
		// would return 0 — wrong. Check before coercing.
		if a == nil {
			return nil, nil
		}
		f, err := arithNum(a, "ABS")
		if err != nil {
			return nil, err
		}
		return math.Abs(f), nil
	case "ROUND":
		if len(fc.args) != 2 {
			return nil, fmt.Errorf("ROUND expects 2 arguments")
		}
		a, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		// DAX ROUND(BLANK, n) is BLANK. arithNum/asNumber treat nil as 0.
		if a == nil {
			return nil, nil
		}
		n, err := arithNum(a, "ROUND")
		if err != nil {
			return nil, err
		}
		b, err := e.scalar(fc.args[1])
		if err != nil {
			return nil, err
		}
		// BLANK digits: Desktop ROUND(1.234, BLANK()) = 1 — treat as 0.
		digits, err := arithNum(b, "ROUND")
		if err != nil {
			return nil, err
		}
		return daxRound(n, digits), nil
	case "LOG":
		if len(fc.args) < 1 || len(fc.args) > 2 {
			return nil, fmt.Errorf("LOG expects 1 or 2 arguments")
		}
		a, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		// Desktop LOG(BLANK()) errors (BLANK coerces to 0). Do not return BLANK.
		n, err := arithNum(a, "LOG")
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, fmt.Errorf("LOG argument must be > 0")
		}
		base := 10.0
		if len(fc.args) == 2 {
			b, err := e.scalar(fc.args[1])
			if err != nil {
				return nil, err
			}
			base, err = arithNum(b, "LOG")
			if err != nil {
				return nil, err
			}
		}
		// Desktop: base 1 is "Division by zero"; base <= 0 is a domain error.
		if base <= 0 || base == 1 {
			return nil, fmt.Errorf("LOG base must be > 0 and not 1")
		}
		out := math.Log(n) / math.Log(base)
		if math.IsNaN(out) || math.IsInf(out, 0) {
			return nil, fmt.Errorf("LOG result is not a number")
		}
		return out, nil
	case "LOG10":
		if len(fc.args) != 1 {
			return nil, fmt.Errorf("LOG10 expects 1 argument")
		}
		a, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		// Desktop LOG10(BLANK()) errors (BLANK coerces to 0). Do not return BLANK.
		n, err := arithNum(a, "LOG10")
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, fmt.Errorf("LOG10 argument must be > 0")
		}
		return math.Log10(n), nil
	case "INT":
		if len(fc.args) != 1 {
			return nil, fmt.Errorf("INT expects 1 argument")
		}
		a, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		// DAX INT(BLANK) is BLANK. arithNum/asNumber treat nil as 0, which
		// would return 0 — wrong. Check before coercing.
		if a == nil {
			return nil, nil
		}
		f, err := arithNum(a, "INT")
		if err != nil {
			return nil, err
		}
		// Excel/DAX INT floors toward −∞ (INT(-2.1) = -3), not truncate
		// toward zero. Desktop 2026-08-15 agrees.
		return math.Floor(f), nil

	case "SIGN":
		if len(fc.args) != 1 {
			return nil, fmt.Errorf("SIGN expects 1 argument")
		}
		a, err := e.scalar(fc.args[0])
		if err != nil {
			return nil, err
		}
		// Desktop SIGN(BLANK()) is BLANK. arithNum would make 0.
		if a == nil {
			return nil, nil
		}
		f, err := arithNum(a, "SIGN")
		if err != nil {
			return nil, err
		}
		switch {
		case f > 0:
			return 1.0, nil
		case f < 0:
			return -1.0, nil
		default:
			return 0.0, nil
		}
	}
	return nil, fmt.Errorf("unsupported DAX function %q", fc.name)
}

// daxRound is Excel/DAX ROUND: half away from zero (ROUND(-1.5, 0) = -2),
// not Go math.Round (half toward +Inf, Round(-1.5) = -1). Digits are themselves
// rounded half-away-from-zero to an integer first (Desktop: ROUND(2.15, 1.5)
// matches ROUND(2.15, 2); ROUND(2.15, 0.5) matches ROUND(2.15, 1)). Negative
// digits round the integer part (ROUND(1234, -2) = 1200).
func daxRound(n, digits float64) float64 {
	return roundHalfAwayPlaces(n, roundHalfAwayInt(digits))
}

func roundHalfAwayInt(f float64) int {
	if f >= 0 {
		return int(math.Floor(f + 0.5))
	}
	return int(math.Ceil(f - 0.5))
}

// roundHalfAwayPlaces shifts via decimal exponent strings so 2.15×10 is
// parsed as 2.15e1 (21.5) rather than IEEE 21.4999…, which would floor to 2.1.
func roundHalfAwayPlaces(n float64, places int) float64 {
	if n == 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return n
	}
	sign := 1.0
	abs := n
	if n < 0 {
		sign = -1
		abs = -n
	}
	dec := strconv.FormatFloat(abs, 'f', -1, 64)
	shifted, err := strconv.ParseFloat(dec+"e"+strconv.Itoa(places), 64)
	if err != nil || math.IsInf(shifted, 0) {
		shifted = abs * math.Pow(10, float64(places))
	}
	rounded := math.Floor(shifted + 0.5)
	out, err := strconv.ParseFloat(strconv.FormatFloat(rounded, 'f', -1, 64)+"e"+strconv.Itoa(-places), 64)
	if err != nil || math.IsInf(out, 0) {
		out = rounded / math.Pow(10, float64(places))
	}
	return sign * out
}

// selectedValue implements SELECTEDVALUE(<column>[, <alternate>]): the single
// distinct value of the column under the current filter context, else the
// alternate (BLANK when omitted).
//
// WHY THIS EXISTS. An extension expression cannot read a bare column — a filter
// context supplies no row, and `scalar` refuses one for that reason. That is
// correct DAX, but it leaves no way to DERIVE a label from the column being
// grouped by, which is the first thing anyone reaches for once `&` works:
//
//	SUMMARIZECOLUMNS(T[Year], "Label", "FY" & T[Year])            -- refused
//	SUMMARIZECOLUMNS(T[Year], "Label", "FY" & SELECTEDVALUE(T[Year]))  -- this
//
// It is a lookup, not a new context mechanism: evalSummarize already sets the
// group's filter context before evaluating outputs, and activeRows honours it.
//
// NOT special-cased for the group column, though its value is sitting in the
// context and a direct read would be shorter. Going through activeRows is what
// makes the function correct for a column that is NOT grouped by — where the
// answer is genuinely "more than one value, take the alternate" — and a
// shortcut would quietly answer the grouped case only.
func (e *evalr) selectedValue(fc funcCall) (any, error) {
	if len(fc.args) < 1 {
		return nil, fmt.Errorf("SELECTEDVALUE expects a column reference")
	}
	col, ok := fc.args[0].(columnRef)
	if !ok {
		return nil, fmt.Errorf("SELECTEDVALUE expects a column reference")
	}
	// The alternate is evaluated only when it is needed, so a costly or failing
	// alternate costs nothing on the ordinary single-value path.
	alternate := func() (any, error) {
		if len(fc.args) > 1 {
			return e.scalar(fc.args[1])
		}
		return nil, nil
	}

	tbl, err := e.resolveColumn("SELECTEDVALUE", col)
	if err != nil {
		return nil, err
	}

	var seen []any
	for _, r := range e.activeRows(tbl) {
		v := r[col.col]
		// BLANK IS A VALUE. Real DAX returns it when it is the only one rather
		// than falling through to the alternate, so skipping nils here would
		// answer the alternate for a column that is uniformly blank — a
		// different fact.
		dup := false
		for _, s := range seen {
			if valEq(s, v) {
				dup = true
				break
			}
		}
		if !dup {
			seen = append(seen, v)
			if len(seen) > 1 {
				break // two is already "not exactly one"; the rest cannot change that
			}
		}
	}
	if len(seen) == 1 {
		return seen[0], nil
	}
	return alternate()
}

// resolveColumn checks that a column reference names something real and returns
// the unquoted table name. Every function that READS a column goes through it.
//
// WHY IT IS SHARED. A missing column is not absent, it is BLANK on every row —
// `Row` is a map, so a typo reads as nil rather than failing. Each function
// then folds that into its own confident answer: SUM totalled nothing and
// returned 0, SELECTEDVALUE would have collapsed to one distinct BLANK and
// returned it. Both are the worst shape an emulator can have — a number that
// is wrong rather than an error — and both come from the same missing check,
// so it lives in one place and the next column-taking function inherits it.
func (e *evalr) resolveColumn(fn string, ref columnRef) (string, error) {
	tbl := strings.Trim(ref.table, "'")
	t := e.model.Table(tbl)
	if t == nil {
		return "", fmt.Errorf("%s: no table %q in this model", fn, tbl)
	}
	if t.Column(ref.col) == nil {
		return "", fmt.Errorf("%s: table %s has no column %q", fn, tbl, ref.col)
	}
	return tbl, nil
}

// resolveTable is the same guard for a function that takes a whole table.
// COUNTROWS on a name that does not exist counted zero rows and returned 0 —
// the same confident wrong number, one type up.
func (e *evalr) resolveTable(fn, name string) (string, error) {
	tbl := strings.Trim(name, "'")
	if e.model.Table(tbl) == nil {
		return "", fmt.Errorf("%s: no table %q in this model", fn, tbl)
	}
	return tbl, nil
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
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int8:
		return float64(n)
	case int16:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint:
		return float64(n)
	case uint8:
		return float64(n)
	case uint16:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func valEq(a, b any) bool {
	af, aok := numeric(a)
	bf, bok := numeric(b)
	if aok && bok {
		return af == bf
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func numeric(v any) (float64, bool) {
	switch v.(type) {
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return toF(v), true
	default:
		return 0, false
	}
}
