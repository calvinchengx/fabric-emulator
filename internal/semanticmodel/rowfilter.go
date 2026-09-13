package semanticmodel

import (
	"fmt"
	"strconv"
	"strings"
)

// A row-level security filter: a DAX expression evaluated TRUE or FALSE against
// one row of one table — "a DAX filter evaluates TRUE/FALSE for each row. Only
// rows that return TRUE are visible."
//
// SEPARATE FROM THE QUERY EVALUATOR, ON PURPOSE. A filter has row context,
// which the query evaluator has none of; it needs `&&`, `||` and `IN { … }`,
// which the query grammar does not parse; and its answer must be a boolean,
// where a query yields anything. It shares the lexer and nothing else.
//
// THE SUBSET, and nothing past it:
//
//   - column references: 'T'[C], T[C], and [C], all against the permission's
//     own table — a filter reading another table needs RELATED or LOOKUPVALUE,
//     which are not here;
//   - string and number literals, TRUE(), FALSE(), BLANK();
//   - = <> < <= > >=, && and ||, NOT, AND(a,b), OR(a,b), IN { … };
//   - USERPRINCIPALNAME() and USERNAME(), both the caller's UPN, as in the
//     service.
//
// Anything else is refused by name when the filter is compiled — the DAX
// engine's own terms: answer the pinned subset, error outside it. A refusal
// fails the query rather than dropping the rule, since dropping a filter widens.
//
// STRINGS COMPARE CASE-INSENSITIVELY, as DAX's do ("DAX string comparisons are
// case-insensitive by default"). Comparing text with a number is an error, as
// it is in DAX, not a silent FALSE.

// SecurityEnv is what a filter may ask about the caller.
type SecurityEnv struct {
	UPN string
}

// RowFilter is a compiled filter for one table.
type RowFilter struct {
	table string
	root  fnode
}

type fnode interface{}
type fLit struct{ v any }
type fCol struct{ name string }
type fCall struct{ name string }
type fBin struct {
	op   string
	l, r fnode
}
type fNot struct{ x fnode }
type fIn struct {
	x   fnode
	set []fnode
}

// CompileRowFilter parses expr as a filter over table.
func CompileRowFilter(m *Model, table, expr string) (*RowFilter, error) {
	t := m.tableFold(table)
	if t == nil {
		return nil, fmt.Errorf("a row-level security filter names table %q, which the model does not have", table)
	}
	toks, err := lex(expr)
	if err != nil {
		return nil, fmt.Errorf("row-level security filter on %q: %w", t.Name, err)
	}
	p := &filterParser{toks: toks, table: t}
	root, err := p.or()
	if err != nil {
		return nil, fmt.Errorf("row-level security filter on %q: %w", t.Name, err)
	}
	if p.pos != len(toks) {
		return nil, fmt.Errorf("row-level security filter on %q: unsupported DAX from %q onwards", t.Name, toks[p.pos].text)
	}
	return &RowFilter{table: t.Name, root: root}, nil
}

// Admits reports whether the row passes. An evaluation error is returned, never
// read as FALSE: a filter that cannot be evaluated has not said "no", it has said
// nothing, and the caller refuses the query.
func (f *RowFilter) Admits(row Row, env SecurityEnv) (bool, error) {
	v, err := evalFilter(f.root, row, env)
	if err != nil {
		return false, err
	}
	return filterBool(v, "the filter")
}

// tableFold finds a table by name, case-insensitively, as DAX resolves names.
func (m *Model) tableFold(name string) *Table {
	name = strings.Trim(name, "'")
	for i := range m.Tables {
		if strings.EqualFold(m.Tables[i].Name, name) {
			return &m.Tables[i]
		}
	}
	return nil
}

// --- parser ------------------------------------------------------------------

type filterParser struct {
	toks  []dtok
	pos   int
	table *Table
}

func (p *filterParser) peek() *dtok {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

func (p *filterParser) isOp(text string) bool {
	t := p.peek()
	return t != nil && t.kind == tOp && t.text == text
}

func (p *filterParser) isPunct(text string) bool {
	t := p.peek()
	return t != nil && t.kind == tPunct && t.text == text
}

func (p *filterParser) expectPunct(text string) error {
	if !p.isPunct(text) {
		got := "the end of the filter"
		if t := p.peek(); t != nil {
			got = fmt.Sprintf("%q", t.text)
		}
		return fmt.Errorf("expected %q, got %s", text, got)
	}
	p.pos++
	return nil
}

func (p *filterParser) or() (fnode, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.isOp("||") {
		p.pos++
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = fBin{op: "||", l: l, r: r}
	}
	return l, nil
}

func (p *filterParser) and() (fnode, error) {
	l, err := p.cmp()
	if err != nil {
		return nil, err
	}
	for p.isOp("&&") {
		p.pos++
		r, err := p.cmp()
		if err != nil {
			return nil, err
		}
		l = fBin{op: "&&", l: l, r: r}
	}
	return l, nil
}

var filterCmpOps = map[string]bool{"=": true, "<>": true, "<": true, "<=": true, ">": true, ">=": true}

func (p *filterParser) cmp() (fnode, error) {
	l, err := p.operand()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t != nil && t.kind == tOp && filterCmpOps[t.text] {
		p.pos++
		r, err := p.operand()
		if err != nil {
			return nil, err
		}
		return fBin{op: t.text, l: l, r: r}, nil
	}
	if t := p.peek(); t != nil && t.kind == tIdent && strings.EqualFold(t.text, "IN") {
		p.pos++
		if err := p.expectPunct("{"); err != nil {
			return nil, err
		}
		in := fIn{x: l}
		for {
			v, err := p.operand()
			if err != nil {
				return nil, err
			}
			in.set = append(in.set, v)
			if p.isPunct(",") {
				p.pos++
				continue
			}
			if err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return in, nil
		}
	}
	return l, nil
}

func (p *filterParser) operand() (fnode, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("the filter ends where a value was expected")
	}
	switch t.kind {
	case tString:
		p.pos++
		return fLit{t.text}, nil
	case tNum:
		p.pos++
		return numberNode(t.text, false)
	case tOp:
		if t.text == "-" && p.pos+1 < len(p.toks) && p.toks[p.pos+1].kind == tNum {
			p.pos += 2
			return numberNode(p.toks[p.pos-1].text, true)
		}
	case tPunct:
		if t.text == "(" {
			p.pos++
			x, err := p.or()
			if err != nil {
				return nil, err
			}
			return x, p.expectPunct(")")
		}
	case tBracket:
		p.pos++
		return p.column(t.text)
	case tqTable, tIdent:
		if t.kind == tIdent && strings.EqualFold(t.text, "NOT") {
			return p.call(t.text) // NOT [C] would otherwise read as a table named NOT
		}
		if p.pos+1 < len(p.toks) && p.toks[p.pos+1].kind == tBracket {
			if !strings.EqualFold(t.text, p.table.Name) {
				return nil, fmt.Errorf("column %s[%s] belongs to another table; a filter on %q reads only its own "+
					"columns (RELATED and LOOKUPVALUE are not supported)", t.text, p.toks[p.pos+1].text, p.table.Name)
			}
			p.pos += 2
			return p.column(p.toks[p.pos-1].text)
		}
		if t.kind == tIdent {
			return p.call(t.text)
		}
	}
	return nil, fmt.Errorf("unexpected %q in a row-level security filter", t.text)
}

func numberNode(text string, negative bool) (fnode, error) {
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid number %q", text)
	}
	if negative {
		f = -f
	}
	return fLit{f}, nil
}

func (p *filterParser) column(name string) (fnode, error) {
	for _, c := range p.table.Columns {
		if strings.EqualFold(c.Name, name) {
			return fCol{name: c.Name}, nil
		}
	}
	return nil, fmt.Errorf("table %q has no column [%s]", p.table.Name, name)
}

// call parses a function or the NOT operator.
func (p *filterParser) call(name string) (fnode, error) {
	p.pos++
	upper := strings.ToUpper(name)
	if upper == "NOT" && !p.isPunct("(") {
		x, err := p.cmp()
		if err != nil {
			return nil, err
		}
		return fNot{x}, nil
	}
	if err := p.expectPunct("("); err != nil {
		return nil, fmt.Errorf("unsupported %q in a row-level security filter", name)
	}
	var args []fnode
	for !p.isPunct(")") {
		arg, err := p.or()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		if !p.isPunct(",") {
			break
		}
		p.pos++
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	arity := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s expects %d argument(s), got %d", upper, n, len(args))
		}
		return nil
	}
	switch upper {
	case "TRUE", "FALSE", "BLANK", "USERPRINCIPALNAME", "USERNAME":
		if err := arity(0); err != nil {
			return nil, err
		}
		return fCall{name: upper}, nil
	case "NOT":
		if err := arity(1); err != nil {
			return nil, err
		}
		return fNot{args[0]}, nil
	case "AND", "OR":
		if err := arity(2); err != nil {
			return nil, err
		}
		op := "&&"
		if upper == "OR" {
			op = "||"
		}
		return fBin{op: op, l: args[0], r: args[1]}, nil
	}
	return nil, fmt.Errorf("function %s is not supported in a row-level security filter", upper)
}

// --- evaluation --------------------------------------------------------------

// evalFilter evaluates a node the parser built; fBin is the only node left once
// the others are handled.
func evalFilter(n fnode, row Row, env SecurityEnv) (any, error) {
	switch x := n.(type) {
	case fLit:
		return x.v, nil
	case fCol:
		return row[x.name], nil
	case fCall:
		switch x.name {
		case "TRUE":
			return true, nil
		case "FALSE":
			return false, nil
		case "USERPRINCIPALNAME", "USERNAME":
			return env.UPN, nil
		}
		return nil, nil // BLANK
	case fNot:
		v, err := evalFilter(x.x, row, env)
		if err != nil {
			return nil, err
		}
		b, err := filterBool(v, "NOT")
		return !b, err
	case fIn:
		v, err := evalFilter(x.x, row, env)
		if err != nil {
			return nil, err
		}
		for _, s := range x.set {
			sv, err := evalFilter(s, row, env)
			if err != nil {
				return nil, err
			}
			c, err := filterCompare(v, sv)
			if err != nil {
				return nil, err
			}
			if c == 0 {
				return true, nil
			}
		}
		return false, nil
	}
	return evalBin(n.(fBin), row, env)
}

func evalBin(x fBin, row Row, env SecurityEnv) (any, error) {
	{
		l, err := evalFilter(x.l, row, env)
		if err != nil {
			return nil, err
		}
		r, err := evalFilter(x.r, row, env)
		if err != nil {
			return nil, err
		}
		if x.op == "&&" || x.op == "||" {
			lb, err := filterBool(l, x.op)
			if err != nil {
				return nil, err
			}
			rb, err := filterBool(r, x.op)
			if err != nil {
				return nil, err
			}
			if x.op == "&&" {
				return lb && rb, nil
			}
			return lb || rb, nil
		}
		c, err := filterCompare(l, r)
		if err != nil {
			return nil, err
		}
		switch x.op {
		case "=":
			return c == 0, nil
		case "<>":
			return c != 0, nil
		case "<":
			return c < 0, nil
		case "<=":
			return c <= 0, nil
		case ">":
			return c > 0, nil
		}
		return c >= 0, nil // ">=": the parser admits nothing else
	}
}

// filterBool reads a logical operand. BLANK is FALSE; anything that is not a
// boolean is an error rather than a guess at truthiness.
func filterBool(v any, where string) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case nil:
		return false, nil
	}
	return false, fmt.Errorf("%s needs TRUE or FALSE, got %v", where, v)
}

// filterCompare orders two values the way DAX does: numbers numerically,
// strings case-insensitively, booleans FALSE before TRUE, BLANK as the empty
// value of whatever it meets. Text against a number is an error.
func filterCompare(l, r any) (int, error) {
	l, r = blankAgainst(l, r), blankAgainst(r, l)
	switch lv := l.(type) {
	case string:
		rv, ok := r.(string)
		if !ok {
			return 0, fmt.Errorf("cannot compare the text %q with %v", lv, r)
		}
		return strings.Compare(strings.ToLower(lv), strings.ToLower(rv)), nil
	case bool:
		rv, ok := r.(bool)
		if !ok {
			return 0, fmt.Errorf("cannot compare TRUE/FALSE with %v", r)
		}
		return boolOrder(lv) - boolOrder(rv), nil
	}
	lf, lok := numeric(l)
	rf, rok := numeric(r)
	if !lok || !rok {
		return 0, fmt.Errorf("cannot compare %v with %v", l, r)
	}
	switch {
	case lf < rf:
		return -1, nil
	case lf > rf:
		return 1, nil
	}
	return 0, nil
}

// blankAgainst replaces BLANK — nil, or a column's whitespace-only text read
// against a number — with the empty value of the other side's type.
func blankAgainst(v, other any) any {
	if _, otherNum := numeric(other); otherNum && isBlankText(v) {
		v = nil
	}
	if v != nil {
		return v
	}
	switch other.(type) {
	case string:
		return ""
	case bool:
		return false
	}
	return float64(0)
}

func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}
