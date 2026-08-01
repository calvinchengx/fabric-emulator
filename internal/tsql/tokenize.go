// Package tsql tokenises T-SQL far enough to find, and later rewrite, a
// statement's WITH prefix (docs/29-tsql-parity.md, T6). It is deliberately not
// a SQL parser: it understands *lexical* structure — comments, string
// literals, quoted identifiers, parenthesis nesting — plus the shape of a CTE
// list, and nothing whatsoever about SELECT bodies.
//
// Lexical fidelity is the entire point. A regex for `with` corrupts any
// statement that merely contains the word inside a string literal, a comment,
// or an identifier — and those are not hypothetical: T6a measured dbt
// prefixing every statement it sends with a JSON comment
// (`/* {"app": "dbt", …} */`), so the scanner cannot even assume WITH is the
// first token.
//
// The package is protocol-free (no TDS types), so it can be exercised on plain
// strings and reused by T8's CTAS rewrite.
package tsql

import "fmt"

// Kind classifies a token. The distinctions that matter are exactly those that
// let the parser skip text it must not interpret.
type Kind int

const (
	Space       Kind = iota // run of whitespace
	Comment                 // -- to end of line, or a /* */ block (nestable)
	String                  // 'literal', N'literal' ('' escapes a quote)
	QuotedIdent             // [ident] or "ident" (]] and "" escape)
	Word                    // bare identifier, keyword, or number
	Punct                   // any other single character
)

func (k Kind) String() string {
	switch k {
	case Space:
		return "space"
	case Comment:
		return "comment"
	case String:
		return "string"
	case QuotedIdent:
		return "quoted-ident"
	case Word:
		return "word"
	}
	return "punct"
}

// Token is one lexical unit, carrying its raw source text and byte offset so a
// rewrite can splice the original rather than re-render it.
type Token struct {
	Kind Kind
	Text string
	Pos  int
}

// Trivia reports whether the token carries no syntactic meaning — whitespace
// or a comment. The parser skips trivia; a rewriter preserves it.
func (t Token) Trivia() bool { return t.Kind == Space || t.Kind == Comment }

// Tokenize splits src into tokens. An unterminated string, quoted identifier,
// or block comment is an error rather than a best-effort guess: the caller's
// correct response is to leave the statement alone, and a scanner that
// silently resynchronised would instead hand a rewriter a false structure.
func Tokenize(src string) ([]Token, error) {
	var toks []Token
	for i := 0; i < len(src); {
		start := i
		switch c := src[i]; {
		case isSpace(c):
			for i < len(src) && isSpace(src[i]) {
				i++
			}
			toks = append(toks, Token{Space, src[start:i], start})

		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			toks = append(toks, Token{Comment, src[start:i], start})

		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end, err := blockComment(src, i)
			if err != nil {
				return nil, err
			}
			i = end
			toks = append(toks, Token{Comment, src[start:i], start})

		case c == '\'':
			end, err := delimited(src, i, '\'', '\'')
			if err != nil {
				return nil, err
			}
			i = end
			toks = append(toks, Token{String, src[start:i], start})

		// N'…' is a Unicode string literal, not the identifier N followed by a
		// string — check before the Word case claims the N.
		case (c == 'N' || c == 'n') && i+1 < len(src) && src[i+1] == '\'':
			end, err := delimited(src, i+1, '\'', '\'')
			if err != nil {
				return nil, err
			}
			i = end
			toks = append(toks, Token{String, src[start:i], start})

		case c == '[':
			end, err := delimited(src, i, '[', ']')
			if err != nil {
				return nil, err
			}
			i = end
			toks = append(toks, Token{QuotedIdent, src[start:i], start})

		case c == '"':
			end, err := delimited(src, i, '"', '"')
			if err != nil {
				return nil, err
			}
			i = end
			toks = append(toks, Token{QuotedIdent, src[start:i], start})

		case isWordByte(c):
			for i < len(src) && isWordByte(src[i]) {
				i++
			}
			toks = append(toks, Token{Word, src[start:i], start})

		default:
			i++
			toks = append(toks, Token{Punct, src[start:i], start})
		}
	}
	return toks, nil
}

// blockComment returns the offset just past a /* */ block starting at i.
// T-SQL nests block comments, so depth is tracked rather than scanning for the
// first "*/" — otherwise `/* a /* b */ c */` would terminate early and the
// trailing `c */` would be lexed as code.
func blockComment(src string, i int) (int, error) {
	depth := 0
	for i < len(src) {
		switch {
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			depth++
			i += 2
		case src[i] == '*' && i+1 < len(src) && src[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i, nil
			}
		default:
			i++
		}
	}
	return 0, fmt.Errorf("tsql: unterminated block comment")
}

// delimited returns the offset just past a run opened at i by `open` and
// closed by `close`, where a doubled closing character escapes itself
// ('it”s', [a]]b], "x""y").
func delimited(src string, i int, open, close byte) (int, error) {
	i++ // consume the opener
	for i < len(src) {
		if src[i] == close {
			if i+1 < len(src) && src[i+1] == close {
				i += 2 // doubled: an escaped literal delimiter
				continue
			}
			return i + 1, nil
		}
		i++
	}
	return 0, fmt.Errorf("tsql: unterminated %c%c literal", open, close)
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' }

// isWordByte covers what T-SQL allows in a bare identifier or number,
// including the @ and # prefixes for variables and temp tables.
func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '@' || c == '#' || c == '$'
}
