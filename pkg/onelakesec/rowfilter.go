package onelakesec

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// A OneLake security row filter, parsed once and applied by whichever engine
// reads the table: the SQL analytics endpoint renders it as a SQL Server
// predicate (docs/60), Direct Lake evaluates it over the rows it read (docs/54).
// One parser, so the two surfaces cannot disagree about what a filter means.
//
// The grammar is Microsoft's, and nothing beyond it: "All row-level security
// rules take the following form: SELECT * FROM {schema_name}.{table_name} WHERE
// {column_level_boolean_1}…", each boolean being a column, an operator and "a
// static value or set of values", joined by AND or OR, with =, <>, >, >=, <, <=,
// IN, NOT, TRUE, FALSE, and IS BLANK / IS NULL. "RLS roles don't support dynamic
// and multitable queries", and "the maximum number of characters in a row-level
// security rule is 1000". Anything else is refused.
//
// TEXT COMPARES CASE-INSENSITIVELY, for every operator. The syntax page says
// both that RLS "evaluates string data as case insensitive by using the
// following collation for sorting and comparisons:
// Latin1_General_100_CI_AS_KS_WS_SC_UTF8" and that > and < on strings use
// "bitwise comparison". The two cannot both hold; the collation is followed,
// for both engines, so they return the same rows (decided 2026-10-02: inferred).

// RLSCollation is the collation OneLake RLS compares text in.
const RLSCollation = "Latin1_General_100_CI_AS_KS_WS_SC_UTF8"

// UnknownColumnError is ParseRowFilter's failure when a filter names a column
// the table does not have — kept distinguishable from every other refusal,
// because Fabric treats "a column that no longer exists" as schema drift rather
// than a malformed rule.
type UnknownColumnError struct{ Msg string }

func (e *UnknownColumnError) Error() string { return e.Msg }

// FilterColumn is one column of the filtered table: its name as the engine has
// it, and whether it holds text — which only the SQL rendering needs, to apply
// the collation; evaluation reads each value's own type.
type FilterColumn struct {
	Name string
	Text bool
}

// RowFilter is a parsed filter.
type RowFilter struct {
	root    rfNode
	Columns []string // the columns it reads, as the engine names them, in first-use order
}

type rfNode interface{}

type (
	rfOr    struct{ l, r rfNode }
	rfAnd   struct{ l, r rfNode }
	rfNot   struct{ x rfNode }
	rfConst struct{ v bool }
	rfCmp   struct {
		col FilterColumn
		op  string
		v   rfLiteral
	}
	rfIn struct {
		col  FilterColumn
		not  bool
		vals []rfLiteral
	}
	rfIs struct {
		col        FilterColumn
		not, blank bool
	}
)

// rfLiteral is a static value. TRUE and FALSE are numbers, 1 and 0, as SQL
// Server's bit is.
type rfLiteral struct {
	sql  string   // as rendered
	text *string  // a text literal's value
	num  *big.Rat // a number's value
}

var rfDigits = regexp.MustCompile(`^[0-9]+$`)

// ParseRowFilter validates a filter against its table and parses it. schema is
// the table's schema ("dbo" when the item has none); a filter may name it or
// not. columns are keyed by lower-cased name.
func ParseRowFilter(filter, schema, table string, columns map[string]FilterColumn) (*RowFilter, error) {
	if len(filter) > 1000 {
		return nil, fmt.Errorf("the row filter is longer than 1000 characters")
	}
	toks, err := tsql.Tokenize(filter)
	if err != nil {
		return nil, fmt.Errorf("the row filter does not parse: %w", err)
	}
	p := &rfParser{schema: schema, table: table, columns: columns}
	for _, t := range toks {
		if !t.Trivia() {
			p.toks = append(p.toks, t)
		}
	}
	for len(p.toks) > 0 && p.toks[len(p.toks)-1].Text == ";" {
		p.toks = p.toks[:len(p.toks)-1]
	}
	if err := p.header(); err != nil {
		return nil, err
	}
	root, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("the row filter has unsupported SQL from %q onwards", p.toks[p.pos].Text)
	}
	return &RowFilter{root: root, Columns: p.used}, nil
}

// ParseRowFilters parses a consolidated row restriction. Several grants
// filtering one table reach a reader joined by " UNION " — Effective writes
// them that way — and a row survives if any of them admits it.
func ParseRowFilters(rows, schema, table string, columns map[string]FilterColumn) ([]*RowFilter, error) {
	var out []*RowFilter
	for _, part := range strings.Split(rows, " UNION ") {
		f, err := ParseRowFilter(part, schema, table, columns)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// --- parser ------------------------------------------------------------------

type rfParser struct {
	toks          []tsql.Token
	pos           int
	schema, table string
	columns       map[string]FilterColumn
	used          []string
}

func (p *rfParser) peek() (tsql.Token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return tsql.Token{}, false
}

// word reports whether the next token is the keyword w, consuming it if so.
func (p *rfParser) word(w string) bool {
	if t, ok := p.peek(); ok && t.Kind == tsql.Word && strings.EqualFold(t.Text, w) {
		p.pos++
		return true
	}
	return false
}

func (p *rfParser) punct(s string) bool {
	if t, ok := p.peek(); ok && t.Kind == tsql.Punct && t.Text == s {
		p.pos++
		return true
	}
	return false
}

func (p *rfParser) expect(ok bool, what string) error {
	if ok {
		return nil
	}
	if t, more := p.peek(); more {
		return fmt.Errorf("the row filter expects %s, not %q", what, t.Text)
	}
	return fmt.Errorf("the row filter ends where it expects %s", what)
}

// ident reads a bare or bracketed name.
func (p *rfParser) ident() (string, bool) {
	t, ok := p.peek()
	if !ok {
		return "", false
	}
	switch t.Kind {
	case tsql.Word:
		p.pos++
		return t.Text, true
	case tsql.QuotedIdent:
		p.pos++
		return unquoteIdent(t.Text), true
	}
	return "", false
}

// header reads `SELECT * FROM [<schema>.]<table> WHERE`. The table "must exactly
// match the name of the table, or the RLS shows no rows".
func (p *rfParser) header() error {
	if err := p.expect(p.word("SELECT"), "SELECT"); err != nil {
		return err
	}
	if err := p.expect(p.punct("*"), "*"); err != nil {
		return err
	}
	if err := p.expect(p.word("FROM"), "FROM"); err != nil {
		return err
	}
	name, ok := p.ident()
	if err := p.expect(ok, "a table name"); err != nil {
		return err
	}
	if p.punct(".") {
		if name != p.schema {
			return fmt.Errorf("the row filter names schema %q; the table is in %q", name, p.schema)
		}
		if name, ok = p.ident(); !ok {
			return p.expect(false, "a table name")
		}
	}
	if name != p.table {
		return fmt.Errorf("the row filter is written for table %q, not %q", name, p.table)
	}
	return p.expect(p.word("WHERE"), "WHERE")
}

func (p *rfParser) or() (rfNode, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.word("OR") {
		right, err := p.and()
		if err != nil {
			return nil, err
		}
		left = rfOr{left, right}
	}
	return left, nil
}

func (p *rfParser) and() (rfNode, error) {
	left, err := p.factor()
	if err != nil {
		return nil, err
	}
	for p.word("AND") {
		right, err := p.factor()
		if err != nil {
			return nil, err
		}
		left = rfAnd{left, right}
	}
	return left, nil
}

func (p *rfParser) factor() (rfNode, error) {
	switch {
	case p.word("NOT"):
		inner, err := p.factor()
		if err != nil {
			return nil, err
		}
		return rfNot{inner}, nil
	case p.punct("("):
		inner, err := p.or()
		if err != nil {
			return nil, err
		}
		return inner, p.expect(p.punct(")"), `")"`)
	case p.word("TRUE"):
		return rfConst{true}, nil
	case p.word("FALSE"):
		return rfConst{false}, nil
	}
	return p.comparison()
}

// comparison reads `<column> <op> <value>`, `<column> [NOT] IN (<values>)` or
// `<column> IS [NOT] NULL|BLANK`.
func (p *rfParser) comparison() (rfNode, error) {
	col, err := p.column()
	if err != nil {
		return nil, err
	}
	switch {
	case p.word("IS"):
		not := p.word("NOT")
		switch {
		case p.word("NULL"):
			return rfIs{col: col, not: not}, nil
		case p.word("BLANK"):
			return rfIs{col: col, not: not, blank: true}, nil
		}
		return nil, p.expect(false, "NULL or BLANK")
	case p.word("NOT"):
		if err := p.expect(p.word("IN"), "IN"); err != nil {
			return nil, err
		}
		vals, err := p.list()
		if err != nil {
			return nil, err
		}
		return rfIn{col: col, not: true, vals: vals}, nil
	case p.word("IN"):
		vals, err := p.list()
		if err != nil {
			return nil, err
		}
		return rfIn{col: col, vals: vals}, nil
	}
	op, err := p.operator()
	if err != nil {
		return nil, err
	}
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	return rfCmp{col: col, op: op, v: v}, nil
}

// column reads `<column>` or `<table>.<column>`, which must be the filter's
// table and one of its columns.
func (p *rfParser) column() (FilterColumn, error) {
	name, ok := p.ident()
	if err := p.expect(ok, "a column"); err != nil {
		return FilterColumn{}, err
	}
	if p.punct(".") {
		if name != p.table {
			return FilterColumn{}, fmt.Errorf("the row filter reads table %q, which is not %q: multitable filters are not supported", name, p.table)
		}
		if name, ok = p.ident(); !ok {
			return FilterColumn{}, p.expect(false, "a column")
		}
	}
	c, ok := p.columns[strings.ToLower(name)]
	if !ok {
		return FilterColumn{}, &UnknownColumnError{fmt.Sprintf("the row filter reads column %q, which table %q does not have", name, p.table)}
	}
	seen := false
	for _, u := range p.used {
		seen = seen || u == c.Name
	}
	if !seen {
		p.used = append(p.used, c.Name)
	}
	return c, nil
}

func (p *rfParser) operator() (string, error) {
	for _, op := range []string{"<>", ">=", "<=", "=", ">", "<"} {
		start := p.pos
		matched := true
		for _, ch := range op {
			if !p.punct(string(ch)) {
				matched = false
				break
			}
		}
		if matched {
			return op, nil
		}
		p.pos = start
	}
	return "", p.expect(false, "a comparison operator")
}

// value reads a static value: text, a number, or TRUE/FALSE.
func (p *rfParser) value() (rfLiteral, error) {
	t, ok := p.peek()
	switch {
	case ok && t.Kind == tsql.String:
		p.pos++
		raw := t.Text
		if strings.HasPrefix(raw, "N") || strings.HasPrefix(raw, "n") {
			raw = raw[1:]
		}
		s := strings.ReplaceAll(raw[1:len(raw)-1], "''", "'")
		rendered := t.Text
		if !strings.HasPrefix(rendered, "N") && !strings.HasPrefix(rendered, "n") {
			rendered = "N" + rendered
		}
		return rfLiteral{sql: rendered, text: &s}, nil
	case ok && t.Kind == tsql.Word && rfDigits.MatchString(t.Text):
		return numberLiteral(p.number()), nil
	case ok && t.Kind == tsql.Punct && t.Text == "-":
		p.pos++
		if n, more := p.peek(); more && n.Kind == tsql.Word && rfDigits.MatchString(n.Text) {
			return numberLiteral("-" + p.number()), nil
		}
		return rfLiteral{}, p.expect(false, "a number")
	case p.word("TRUE"):
		return numberLiteral("1"), nil
	case p.word("FALSE"):
		return numberLiteral("0"), nil
	}
	return rfLiteral{}, p.expect(false, "a static value")
}

func numberLiteral(text string) rfLiteral {
	r, _ := new(big.Rat).SetString(text) // digits with an optional fraction and sign always parse
	return rfLiteral{sql: text, num: r}
}

// number reads digits and an optional fraction, which tokenize as a word, a
// full stop and a word.
func (p *rfParser) number() string {
	n := p.toks[p.pos].Text
	p.pos++
	if p.pos+1 < len(p.toks) && p.toks[p.pos].Text == "." && rfDigits.MatchString(p.toks[p.pos+1].Text) {
		n += "." + p.toks[p.pos+1].Text
		p.pos += 2
	}
	return n
}

func (p *rfParser) list() ([]rfLiteral, error) {
	if err := p.expect(p.punct("("), `"("`); err != nil {
		return nil, err
	}
	var vs []rfLiteral
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		vs = append(vs, v)
		if !p.punct(",") {
			break
		}
	}
	return vs, p.expect(p.punct(")"), `")"`)
}

// unquoteIdent removes a bracketed or double-quoted name's delimiters and
// escapes, keeping its case: the table "must exactly match".
func unquoteIdent(q string) string {
	if q[0] == '[' {
		return strings.ReplaceAll(q[1:len(q)-1], "]]", "]")
	}
	return strings.ReplaceAll(q[1:len(q)-1], `""`, `"`)
}

// --- SQL rendering -----------------------------------------------------------

// SQL renders the filter as a boolean SQL Server expression. ref renders a
// column reference — the endpoint's `r.[col]`, with the collation on text.
func (f *RowFilter) SQL(ref func(FilterColumn) string) string { return rfSQL(f.root, ref) }

func rfSQL(n rfNode, ref func(FilterColumn) string) string {
	switch x := n.(type) {
	case rfOr:
		return "(" + rfSQL(x.l, ref) + " OR " + rfSQL(x.r, ref) + ")"
	case rfAnd:
		return "(" + rfSQL(x.l, ref) + " AND " + rfSQL(x.r, ref) + ")"
	case rfNot:
		return "(NOT " + rfSQL(x.x, ref) + ")"
	case rfConst:
		if x.v {
			return "(1 = 1)"
		}
		return "(1 = 0)"
	case rfCmp:
		return "(" + ref(x.col) + " " + x.op + " " + x.v.sql + ")"
	case rfIn:
		var vs []string
		for _, v := range x.vals {
			vs = append(vs, v.sql)
		}
		op := " IN "
		if x.not {
			op = " NOT IN "
		}
		return "(" + ref(x.col) + op + "(" + strings.Join(vs, ", ") + "))"
	}
	is := n.(rfIs) // the parser builds nothing else
	col := ref(is.col)
	test := col + " IS NULL"
	if is.blank {
		// BLANK is not defined further; read as SQL's absence of a value, NULL or
		// empty text. An inference.
		test = "(" + col + " IS NULL OR CAST(" + col + " AS nvarchar(max)) = N'')"
	}
	if is.not {
		return "(NOT " + test + ")"
	}
	return "(" + test + ")"
}

// --- evaluation --------------------------------------------------------------

// Admits evaluates the filter over one row, read by column name. SQL's
// three-valued logic applies, so a comparison with NULL is unknown, its NOT is
// unknown too, and an unknown row is not admitted — the same rows the endpoint's
// predicate keeps. A value the filter cannot be compared with is an error, never
// a guess: the endpoint fails the query there too.
//
// Values are the types a Delta read produces: string, the integer and float
// kinds, bool, a *big.Rat (decimals), time.Time (dates and timestamps), and nil.
func (f *RowFilter) Admits(row func(column string) any) (bool, error) {
	v, err := rfEval(f.root, row)
	return v == rfTrue, err
}

type rfTri int

const (
	rfFalse rfTri = iota
	rfTrue
	rfUnknown
)

func rfBool(b bool) rfTri {
	if b {
		return rfTrue
	}
	return rfFalse
}

func rfEval(n rfNode, row func(string) any) (rfTri, error) {
	switch x := n.(type) {
	case rfOr, rfAnd:
		var l, r rfNode
		and := false
		if o, ok := x.(rfOr); ok {
			l, r = o.l, o.r
		} else {
			a := x.(rfAnd)
			l, r, and = a.l, a.r, true
		}
		lv, err := rfEval(l, row)
		if err != nil {
			return rfUnknown, err
		}
		rv, err := rfEval(r, row)
		if err != nil {
			return rfUnknown, err
		}
		if and {
			switch {
			case lv == rfFalse || rv == rfFalse:
				return rfFalse, nil
			case lv == rfTrue && rv == rfTrue:
				return rfTrue, nil
			}
			return rfUnknown, nil
		}
		switch {
		case lv == rfTrue || rv == rfTrue:
			return rfTrue, nil
		case lv == rfFalse && rv == rfFalse:
			return rfFalse, nil
		}
		return rfUnknown, nil
	case rfNot:
		v, err := rfEval(x.x, row)
		switch v {
		case rfTrue:
			return rfFalse, err
		case rfFalse:
			return rfTrue, err
		}
		return rfUnknown, err
	case rfConst:
		return rfBool(x.v), nil
	case rfCmp:
		cell := row(x.col.Name)
		if cell == nil {
			return rfUnknown, nil
		}
		c, err := rfCompare(cell, x.v)
		if err != nil {
			return rfUnknown, fmt.Errorf("column %q: %w", x.col.Name, err)
		}
		switch x.op {
		case "=":
			return rfBool(c == 0), nil
		case "<>":
			return rfBool(c != 0), nil
		case "<":
			return rfBool(c < 0), nil
		case "<=":
			return rfBool(c <= 0), nil
		case ">":
			return rfBool(c > 0), nil
		}
		return rfBool(c >= 0), nil // ">=": the parser admits nothing else
	case rfIn:
		cell := row(x.col.Name)
		if cell == nil {
			return rfUnknown, nil
		}
		in := false
		for _, v := range x.vals {
			c, err := rfCompare(cell, v)
			if err != nil {
				return rfUnknown, fmt.Errorf("column %q: %w", x.col.Name, err)
			}
			in = in || c == 0
		}
		return rfBool(in != x.not), nil
	}
	is := n.(rfIs)
	cell := row(is.col.Name)
	hit := cell == nil
	if s, ok := cell.(string); ok && is.blank {
		hit = s == ""
	}
	return rfBool(hit != is.not), nil
}

// rfCompare orders a cell against a literal the way SQL Server would after its
// implicit conversion: text case-insensitively, ignoring trailing spaces as
// its padded comparison does; numbers exactly; a date or timestamp against
// ISO-8601 text. A pairing SQL Server would refuse to convert is an error.
func rfCompare(cell any, lit rfLiteral) (int, error) {
	if s, ok := cell.(string); ok && lit.text != nil {
		a := strings.ToLower(strings.TrimRight(s, " "))
		b := strings.ToLower(strings.TrimRight(*lit.text, " "))
		return strings.Compare(a, b), nil
	}
	if t, ok := cell.(time.Time); ok {
		if lit.text == nil {
			return 0, fmt.Errorf("a date or time cannot be compared with the number %s", lit.sql)
		}
		lt, err := parseRFTime(*lit.text)
		if err != nil {
			return 0, err
		}
		return t.Compare(lt), nil
	}
	n, err := rfNumber(cell)
	if err != nil {
		return 0, err
	}
	want := lit.num
	if lit.text != nil {
		if want, err = textNumber(*lit.text); err != nil {
			return 0, err
		}
	}
	return n.Cmp(want), nil
}

// rfNumber reads a numeric cell exactly. Text in a numeric comparison is
// converted, as SQL Server converts it, and fails the same way when it is not a
// number.
func rfNumber(cell any) (*big.Rat, error) {
	switch v := cell.(type) {
	case int:
		return new(big.Rat).SetInt64(int64(v)), nil
	case int8:
		return new(big.Rat).SetInt64(int64(v)), nil
	case int16:
		return new(big.Rat).SetInt64(int64(v)), nil
	case int32:
		return new(big.Rat).SetInt64(int64(v)), nil
	case int64:
		return new(big.Rat).SetInt64(v), nil
	case float32:
		return floatRat(float64(v))
	case float64:
		return floatRat(v)
	case bool:
		if v {
			return big.NewRat(1, 1), nil
		}
		return new(big.Rat), nil
	case *big.Rat:
		return v, nil
	case string:
		return textNumber(v)
	}
	return nil, fmt.Errorf("a %T value cannot be compared by a row filter", cell)
}

func floatRat(f float64) (*big.Rat, error) {
	r := new(big.Rat)
	if r.SetFloat64(f) == nil {
		return nil, fmt.Errorf("the value %v is not a finite number", f)
	}
	return r, nil
}

func textNumber(s string) (*big.Rat, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return nil, fmt.Errorf("the text %q cannot be converted to a number", s)
	}
	return r, nil
}

var rfTimeLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02"}

// parseRFTime reads an ISO-8601 date or timestamp, the forms SQL Server
// converts unambiguously whatever its language setting. A zoneless value is
// UTC, as a Delta timestamp is.
func parseRFTime(s string) (time.Time, error) {
	for _, layout := range rfTimeLayouts {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("the text %q is not an ISO-8601 date or time", s)
}
