package server

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// A OneLake security row filter, translated into a SQL Server predicate
// (docs/60, stage 3).
//
// The grammar is Microsoft's, and nothing beyond it: "All row-level security
// rules take the following form: SELECT * FROM {schema_name}.{table_name} WHERE
// {column_level_boolean_1}…", each boolean being a column, an operator and "a
// static value or set of values", joined by AND or OR, with =, <>, >, >=, <, <=,
// IN, NOT, TRUE, FALSE, and IS BLANK / IS NULL. "RLS roles don't support dynamic
// and multitable queries", and "the maximum number of characters in a row-level
// security rule is 1000". Anything else is refused — and a refused filter grants
// no rows, as Fabric's does: "Queries with invalid RLS syntax, or RLS syntax that
// doesn't match the underlying table, result in no rows being shown".

// unknownColumnError is translateRowFilter's specific failure when a filter
// names a column the table does not have — kept distinguishable, via
// errors.As, from every other parse failure. The sync treats the two
// differently: Fabric documents invalid RLS syntax as "no rows being shown",
// which is what the rest of this grammar's refusals get, but a filter that
// references "a column that no longer exists" as the security sync entering
// an error state — evidence of schema drift after the role was authored, not
// of a malformed rule (docs/60).
type unknownColumnError struct{ msg string }

func (e *unknownColumnError) Error() string { return e.msg }

// rlsCollation is the collation OneLake RLS compares text in: "Row-level
// security evaluates string data as case insensitive by using the following
// collation … Latin1_General_100_CI_AS_KS_WS_SC_UTF8".
const rlsCollation = "Latin1_General_100_CI_AS_KS_WS_SC_UTF8"

// endpointColumn is one column of an endpoint table: its name as the engine has
// it, and its declared type, for a predicate function's parameter.
type endpointColumn struct {
	Name, Type string
	Text       bool
}

// rowFilter is a translated filter: a boolean T-SQL expression over `r.[col]`,
// and the columns it reads.
type rowFilter struct {
	Expr    string
	Columns []string // actual column names, in first-use order
}

var rlsDigits = regexp.MustCompile(`^[0-9]+$`)

// translateRowFilter validates a filter against its table and translates it.
func translateRowFilter(filter, table string, columns map[string]endpointColumn) (rowFilter, error) {
	if len(filter) > 1000 {
		return rowFilter{}, fmt.Errorf("the row filter is longer than 1000 characters")
	}
	toks, err := tsql.Tokenize(filter)
	if err != nil {
		return rowFilter{}, fmt.Errorf("the row filter does not parse: %w", err)
	}
	p := &filterParser{table: table, columns: columns}
	for _, t := range toks {
		if !t.Trivia() {
			p.toks = append(p.toks, t)
		}
	}
	for len(p.toks) > 0 && p.toks[len(p.toks)-1].Text == ";" {
		p.toks = p.toks[:len(p.toks)-1]
	}
	if err := p.header(); err != nil {
		return rowFilter{}, err
	}
	expr, err := p.or()
	if err != nil {
		return rowFilter{}, err
	}
	if p.pos != len(p.toks) {
		return rowFilter{}, fmt.Errorf("the row filter has unsupported SQL from %q onwards", p.toks[p.pos].Text)
	}
	return rowFilter{Expr: expr, Columns: p.used}, nil
}

type filterParser struct {
	toks    []tsql.Token
	pos     int
	table   string
	columns map[string]endpointColumn
	used    []string
}

func (p *filterParser) peek() (tsql.Token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return tsql.Token{}, false
}

// word reports whether the next token is the keyword w, consuming it if so.
func (p *filterParser) word(w string) bool {
	if t, ok := p.peek(); ok && t.Kind == tsql.Word && strings.EqualFold(t.Text, w) {
		p.pos++
		return true
	}
	return false
}

func (p *filterParser) punct(s string) bool {
	if t, ok := p.peek(); ok && t.Kind == tsql.Punct && t.Text == s {
		p.pos++
		return true
	}
	return false
}

func (p *filterParser) expect(ok bool, what string) error {
	if ok {
		return nil
	}
	if t, more := p.peek(); more {
		return fmt.Errorf("the row filter expects %s, not %q", what, t.Text)
	}
	return fmt.Errorf("the row filter ends where it expects %s", what)
}

// ident reads a bare or bracketed name.
func (p *filterParser) ident() (string, bool) {
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

// header reads `SELECT * FROM [dbo.]<table> WHERE`. The table "must exactly
// match the name of the table, or the RLS shows no rows".
func (p *filterParser) header() error {
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
		if name != "dbo" {
			return fmt.Errorf("the row filter names schema %q; this lakehouse's tables are in dbo", name)
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

func (p *filterParser) or() (string, error) {
	left, err := p.and()
	if err != nil {
		return "", err
	}
	for p.word("OR") {
		right, err := p.and()
		if err != nil {
			return "", err
		}
		left = "(" + left + " OR " + right + ")"
	}
	return left, nil
}

func (p *filterParser) and() (string, error) {
	left, err := p.factor()
	if err != nil {
		return "", err
	}
	for p.word("AND") {
		right, err := p.factor()
		if err != nil {
			return "", err
		}
		left = "(" + left + " AND " + right + ")"
	}
	return left, nil
}

func (p *filterParser) factor() (string, error) {
	switch {
	case p.word("NOT"):
		inner, err := p.factor()
		if err != nil {
			return "", err
		}
		return "(NOT " + inner + ")", nil
	case p.punct("("):
		inner, err := p.or()
		if err != nil {
			return "", err
		}
		return inner, p.expect(p.punct(")"), `")"`)
	case p.word("TRUE"):
		return "(1 = 1)", nil
	case p.word("FALSE"):
		return "(1 = 0)", nil
	}
	return p.comparison()
}

// comparison reads `<column> <op> <value>`, `<column> [NOT] IN (<values>)` or
// `<column> IS [NOT] NULL|BLANK`.
func (p *filterParser) comparison() (string, error) {
	col, err := p.column()
	if err != nil {
		return "", err
	}
	switch {
	case p.word("IS"):
		not := p.word("NOT")
		var test string
		switch {
		case p.word("NULL"):
			test = col + " IS NULL"
		case p.word("BLANK"):
			// BLANK is not defined further; read as SQL's absence of a value,
			// NULL or empty text. An inference.
			test = "(" + col + " IS NULL OR CAST(" + col + " AS nvarchar(max)) = N'')"
		default:
			return "", p.expect(false, "NULL or BLANK")
		}
		if not {
			return "(NOT " + test + ")", nil
		}
		return "(" + test + ")", nil
	case p.word("NOT"):
		if err := p.expect(p.word("IN"), "IN"); err != nil {
			return "", err
		}
		list, err := p.list()
		if err != nil {
			return "", err
		}
		return "(" + col + " NOT IN " + list + ")", nil
	case p.word("IN"):
		list, err := p.list()
		if err != nil {
			return "", err
		}
		return "(" + col + " IN " + list + ")", nil
	}
	op, err := p.operator()
	if err != nil {
		return "", err
	}
	v, err := p.value()
	if err != nil {
		return "", err
	}
	return "(" + col + " " + op + " " + v + ")", nil
}

// column reads `<column>` or `<table>.<column>`, which must be the filter's
// table and one of its columns.
func (p *filterParser) column() (string, error) {
	name, ok := p.ident()
	if err := p.expect(ok, "a column"); err != nil {
		return "", err
	}
	if p.punct(".") {
		if name != p.table {
			return "", fmt.Errorf("the row filter reads table %q, which is not %q: multitable filters are not supported", name, p.table)
		}
		if name, ok = p.ident(); !ok {
			return "", p.expect(false, "a column")
		}
	}
	c, ok := p.columns[strings.ToLower(name)]
	if !ok {
		return "", &unknownColumnError{fmt.Sprintf("the row filter reads column %q, which table %q does not have", name, p.table)}
	}
	seen := false
	for _, u := range p.used {
		seen = seen || u == c.Name
	}
	if !seen {
		p.used = append(p.used, c.Name)
	}
	ref := "r." + sqlIdent(c.Name)
	if c.Text {
		ref += " COLLATE " + rlsCollation
	}
	return ref, nil
}

func (p *filterParser) operator() (string, error) {
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
func (p *filterParser) value() (string, error) {
	t, ok := p.peek()
	switch {
	case ok && t.Kind == tsql.String:
		p.pos++
		if strings.HasPrefix(t.Text, "N") || strings.HasPrefix(t.Text, "n") {
			return t.Text, nil
		}
		return "N" + t.Text, nil
	case ok && t.Kind == tsql.Word && rlsDigits.MatchString(t.Text):
		return p.number(), nil
	case ok && t.Kind == tsql.Punct && t.Text == "-":
		p.pos++
		if n, more := p.peek(); more && n.Kind == tsql.Word && rlsDigits.MatchString(n.Text) {
			return "-" + p.number(), nil
		}
		return "", p.expect(false, "a number")
	case p.word("TRUE"):
		return "1", nil
	case p.word("FALSE"):
		return "0", nil
	}
	return "", p.expect(false, "a static value")
}

// number reads digits and an optional fraction, which tokenize as a word, a
// full stop and a word.
func (p *filterParser) number() string {
	n := p.toks[p.pos].Text
	p.pos++
	if p.pos+1 < len(p.toks) && p.toks[p.pos].Text == "." && rlsDigits.MatchString(p.toks[p.pos+1].Text) {
		n += "." + p.toks[p.pos+1].Text
		p.pos += 2
	}
	return n
}

func (p *filterParser) list() (string, error) {
	if err := p.expect(p.punct("("), `"("`); err != nil {
		return "", err
	}
	var vs []string
	for {
		v, err := p.value()
		if err != nil {
			return "", err
		}
		vs = append(vs, v)
		if !p.punct(",") {
			break
		}
	}
	return "(" + strings.Join(vs, ", ") + ")", p.expect(p.punct(")"), `")"`)
}

// unquoteIdent removes a bracketed or double-quoted name's delimiters and
// escapes, keeping its case: the table "must exactly match".
func unquoteIdent(q string) string {
	if q[0] == '[' {
		return strings.ReplaceAll(q[1:len(q)-1], "]]", "]")
	}
	return strings.ReplaceAll(q[1:len(q)-1], `""`, `"`)
}
