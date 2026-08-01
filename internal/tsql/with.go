package tsql

// Parsing the WITH prefix — the shape T6's flattener rewrites.
//
// The grammar recognised here is only what a CTE list is:
//
//	statement := <trivia> [';'] WITH <cte> (',' <cte>)* <tail>
//	cte       := <name> ['(' <columns> ')'] AS '(' [WITH <cte-list>] <body> ')'
//
// <tail> and <body> are captured as raw text and never interpreted: this
// package must not need to understand SELECT to move a CTE definition.

import (
	"fmt"
	"strings"
)

// CTE is one common table expression. Inner is non-nil exactly when the
// definition opens with its own WITH — the nested form SQL Server rejects and
// Fabric accepts.
type CTE struct {
	Name    string // as written, e.g. `a`, `[my cte]`, `"x"`
	Columns string // raw parenthesised column list including parens, or ""
	Inner   *With  // nested CTE list inside this definition, or nil
	Body    string // definition text after Inner (the whole definition if Inner is nil)
}

// Ident returns Name with any quoting removed and case folded, for the
// shadowing comparisons the flattener makes. T-SQL identifiers are
// case-insensitive under the usual collations, and `[a]`, `"a"` and `a` all
// name the same CTE.
func (c *CTE) Ident() string { return Ident(c.Name) }

// Ident normalises an identifier for comparison.
func Ident(name string) string {
	if len(name) >= 2 {
		switch {
		case name[0] == '[' && name[len(name)-1] == ']':
			name = strings.ReplaceAll(name[1:len(name)-1], "]]", "]")
		case name[0] == '"' && name[len(name)-1] == '"':
			name = strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
		}
	}
	return strings.ToLower(name)
}

// With is a CTE list — one WITH clause's definitions, in source order.
type With struct {
	CTEs []*CTE
}

// Statement is a statement whose WITH prefix has been parsed. Leading holds
// the trivia and optional semicolon before WITH (dbt's JSON comment lands
// here, and the `;WITH` idiom's semicolon), so a rewrite can reproduce them
// verbatim.
type Statement struct {
	Leading string
	With    *With
	Tail    string // everything after the CTE list — the statement proper
}

// HasNestedCTE reports whether any CTE in the statement defines its own WITH.
// This is the fast path: a statement without nesting needs no rewriting and
// must be forwarded untouched.
func (s *Statement) HasNestedCTE() bool { return s.With.hasNested() }

func (w *With) hasNested() bool {
	if w == nil {
		return false
	}
	for _, c := range w.CTEs {
		if c.Inner != nil {
			return true
		}
	}
	return false
}

// Parse parses a statement's WITH prefix. It returns (nil, nil) when the
// statement does not begin with WITH — the overwhelmingly common case, which
// callers treat as "nothing to do" rather than an error.
//
// A malformed or merely unfamiliar CTE list is an error, never a partial
// parse: the caller's correct response is to forward the statement unmodified,
// and a half-understood structure is exactly what would corrupt one.
func Parse(sql string) (*Statement, error) {
	toks, err := Tokenize(sql)
	if err != nil {
		return nil, err
	}
	p := &parser{src: sql, toks: toks}

	// Leading trivia, plus the `;WITH` idiom's semicolon.
	p.skipTrivia()
	if p.peekPunct(";") {
		p.next()
		p.skipTrivia()
	}
	if !p.peekWord("with") {
		return nil, nil // not a WITH statement
	}
	leadingEnd := p.tok().Pos
	p.next() // consume WITH

	w, err := p.cteList()
	if err != nil {
		return nil, err
	}
	return &Statement{Leading: sql[:leadingEnd], With: w, Tail: p.rest()}, nil
}

type parser struct {
	src  string
	toks []Token
	i    int
}

func (p *parser) tok() Token {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return Token{Kind: Punct, Pos: len(p.src)}
}

func (p *parser) next()      { p.i++ }
func (p *parser) done() bool { return p.i >= len(p.toks) }

func (p *parser) skipTrivia() {
	for !p.done() && p.tok().Trivia() {
		p.next()
	}
}

func (p *parser) peekWord(w string) bool {
	t := p.tok()
	return t.Kind == Word && strings.EqualFold(t.Text, w)
}

func (p *parser) peekPunct(s string) bool {
	t := p.tok()
	return t.Kind == Punct && t.Text == s
}

// rest returns the unconsumed source from the current token onward.
func (p *parser) rest() string {
	if p.done() {
		return ""
	}
	return p.src[p.tok().Pos:]
}

// cteList parses `<cte> (',' <cte>)*`, stopping at the first token that cannot
// continue the list. WITH has already been consumed.
func (p *parser) cteList() (*With, error) {
	w := &With{}
	for {
		c, err := p.cte()
		if err != nil {
			return nil, err
		}
		w.CTEs = append(w.CTEs, c)
		p.skipTrivia()
		if !p.peekPunct(",") {
			return w, nil
		}
		p.next() // consume the comma
		p.skipTrivia()
	}
}

// cte parses `<name> ['(' cols ')'] AS '(' [WITH …] body ')'`.
func (p *parser) cte() (*CTE, error) {
	p.skipTrivia()
	name := p.tok()
	if name.Kind != Word && name.Kind != QuotedIdent {
		return nil, fmt.Errorf("tsql: expected a CTE name, got %s %q", name.Kind, name.Text)
	}
	p.next()
	c := &CTE{Name: name.Text}

	// Optional column list.
	p.skipTrivia()
	if p.peekPunct("(") {
		start := p.tok().Pos
		end, err := p.skipParens()
		if err != nil {
			return nil, err
		}
		c.Columns = p.src[start:end]
		p.skipTrivia()
	}

	if !p.peekWord("as") {
		return nil, fmt.Errorf("tsql: expected AS after CTE name %q", name.Text)
	}
	p.next()
	p.skipTrivia()
	if !p.peekPunct("(") {
		return nil, fmt.Errorf("tsql: expected ( after AS for CTE %q", name.Text)
	}

	// Descend into the definition rather than skipping it: a nested WITH is
	// exactly what we are here to find.
	p.next() // consume (
	p.skipTrivia()
	if p.peekWord("with") {
		p.next()
		inner, err := p.cteList()
		if err != nil {
			return nil, err
		}
		c.Inner = inner
	}
	bodyStart := p.tok().Pos
	closePos, err := p.skipToCloseParen()
	if err != nil {
		return nil, fmt.Errorf("tsql: CTE %q: %w", name.Text, err)
	}
	c.Body = p.src[bodyStart:closePos]
	return c, nil
}

// skipParens consumes a balanced parenthesised run starting at the current
// token, returning the offset just past its closing paren.
func (p *parser) skipParens() (int, error) {
	depth := 0
	for !p.done() {
		t := p.tok()
		if t.Kind == Punct {
			switch t.Text {
			case "(":
				depth++
			case ")":
				depth--
				if depth == 0 {
					p.next()
					return t.Pos + 1, nil
				}
			}
		}
		p.next()
	}
	return 0, fmt.Errorf("tsql: unbalanced parentheses")
}

// skipToCloseParen consumes tokens until the paren opened by the caller closes,
// returning that closing paren's offset. Parens inside strings, identifiers and
// comments are already tokenised as those kinds, so they cannot unbalance it.
func (p *parser) skipToCloseParen() (int, error) {
	depth := 1
	for !p.done() {
		t := p.tok()
		if t.Kind == Punct {
			switch t.Text {
			case "(":
				depth++
			case ")":
				depth--
				if depth == 0 {
					p.next()
					return t.Pos, nil
				}
			}
		}
		p.next()
	}
	return 0, fmt.Errorf("unbalanced parentheses")
}
