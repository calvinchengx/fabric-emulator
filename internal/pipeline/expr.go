package pipeline

import (
	"fmt"
	"strconv"
	"strings"
)

// Fabric/ADF pipeline expressions. A value beginning with '@' is an
// expression; '@{...}' segments interpolate inside literal text; '@@' escapes
// a literal '@'. Expressions are function calls, dotted member access, and
// literals over the Fabric function library (a faithful subset) — enough to
// drive real control flow, variables, and activity wiring.
//
// This is a small recursive-descent evaluator, not a reimplementation of the
// whole ADF language; unsupported functions surface as errors (which fail the
// activity) rather than silently mis-evaluating.

// value is any JSON-shaped value: string, float64, bool, nil, []any, map[string]any.
type value = any

// evalContext is what expressions can see: pipeline parameters, live
// variables, prior activity outputs, and the current ForEach item.
type evalContext struct {
	Parameters map[string]value
	Variables  map[string]value
	Activities map[string]value // name -> {"output": ..., "status": ...}
	Item       value            // @item() inside ForEach
	HasItem    bool
}

// evalString resolves a definition string: whole-value '@expr', interpolated
// '@{expr}' text, or a plain literal. Returns the resolved value (which may be
// a non-string for whole-value expressions).
func evalString(s string, ctx *evalContext) (value, error) {
	if !strings.Contains(s, "@") {
		return s, nil
	}
	if strings.HasPrefix(s, "@@") {
		return "@" + s[2:], nil
	}
	// Whole-value expression: '@' + expression, and no earlier literal text.
	if strings.HasPrefix(s, "@") && !strings.HasPrefix(s, "@{") {
		return evalExpr(s[1:], ctx)
	}
	// Otherwise interpolate @{...} runs into the surrounding text.
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "@@") {
			b.WriteByte('@')
			i += 2
			continue
		}
		if strings.HasPrefix(s[i:], "@{") {
			depth, j := 1, i+2
			for ; j < len(s) && depth > 0; j++ {
				if s[j] == '{' {
					depth++
				} else if s[j] == '}' {
					depth--
				}
			}
			v, err := evalExpr(s[i+2:j-1], ctx)
			if err != nil {
				return nil, err
			}
			b.WriteString(toString(v))
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), nil
}

// --- tokenizer ---------------------------------------------------------------

type token struct {
	kind string // ident, num, str, punct
	text string
}

func tokenize(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'':
			// String literal; '' is an escaped quote.
			var b strings.Builder
			i++
			for i < len(s) {
				if s[i] == '\'' {
					if i+1 < len(s) && s[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(s[i])
				i++
			}
			toks = append(toks, token{"str", b.String()})
		case c == '(' || c == ')' || c == ',' || c == '.' || c == '[' || c == ']':
			toks = append(toks, token{"punct", string(c)})
			i++
		case c >= '0' && c <= '9' || (c == '-' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9'):
			j := i + 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			toks = append(toks, token{"num", s[i:j]})
			i = j
		case isIdentStart(c):
			j := i + 1
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			toks = append(toks, token{"ident", s[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q in expression", string(c))
		}
	}
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// --- parser + evaluator ------------------------------------------------------

type parser struct {
	toks []token
	pos  int
	ctx  *evalContext
}

func evalExpr(s string, ctx *evalContext) (v value, err error) {
	// A malformed definition (e.g. a function called with too few arguments)
	// must fail the activity, never crash the server — recover any panic from
	// arg indexing or bad casts into an error.
	defer func() {
		if r := recover(); r != nil {
			v, err = nil, fmt.Errorf("invalid expression %q: %v", s, r)
		}
	}()
	toks, err := tokenize(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, ctx: ctx}
	val, err := p.parsePostfix()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.toks) {
		return nil, fmt.Errorf("trailing tokens in expression %q", s)
	}
	return val, nil
}

func (p *parser) peek() *token {
	if p.pos < len(p.toks) {
		return &p.toks[p.pos]
	}
	return nil
}

// parsePostfix parses a primary then any trailing .member / [index] accessors.
func (p *parser) parsePostfix() (value, error) {
	v, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || t.kind != "punct" {
			break
		}
		switch t.text {
		case ".":
			p.pos++
			mem := p.peek()
			if mem == nil || mem.kind != "ident" {
				return nil, fmt.Errorf("expected member name after '.'")
			}
			p.pos++
			v, err = member(v, mem.text)
			if err != nil {
				return nil, err
			}
		case "[":
			p.pos++
			idx, err := p.parsePostfix()
			if err != nil {
				return nil, err
			}
			if c := p.peek(); c == nil || c.text != "]" {
				return nil, fmt.Errorf("expected ']'")
			}
			p.pos++
			v, err = index(v, idx)
			if err != nil {
				return nil, err
			}
		default:
			return v, nil
		}
	}
	return v, nil
}

func (p *parser) parsePrimary() (value, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	switch t.kind {
	case "str":
		p.pos++
		return t.text, nil
	case "num":
		p.pos++
		n, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	case "ident":
		p.pos++
		switch t.text {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		// Must be a function call.
		if n := p.peek(); n == nil || n.text != "(" {
			return nil, fmt.Errorf("unknown identifier %q (expected a function call)", t.text)
		}
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		return callFunc(t.text, args, p.ctx)
	}
	return nil, fmt.Errorf("unexpected token %q", t.text)
}

func (p *parser) parseArgs() ([]value, error) {
	p.pos++ // consume '('
	var args []value
	if c := p.peek(); c != nil && c.text == ")" {
		p.pos++
		return args, nil
	}
	for {
		v, err := p.parsePostfix()
		if err != nil {
			return nil, err
		}
		args = append(args, v)
		c := p.peek()
		if c == nil {
			return nil, fmt.Errorf("unterminated argument list")
		}
		if c.text == "," {
			p.pos++
			continue
		}
		if c.text == ")" {
			p.pos++
			return args, nil
		}
		return nil, fmt.Errorf("expected ',' or ')' in argument list, got %q", c.text)
	}
}

// member resolves v.name for maps (and the system objects returned by
// pipeline()/activity() which are plain maps).
func member(v value, name string) (value, error) {
	m, ok := v.(map[string]value)
	if !ok {
		if mm, ok2 := v.(map[string]any); ok2 {
			m = mm
		} else {
			return nil, fmt.Errorf("cannot read .%s of non-object", name)
		}
	}
	val, ok := m[name]
	if !ok {
		return nil, fmt.Errorf("no such member %q", name)
	}
	return val, nil
}

func index(v, idx value) (value, error) {
	arr, ok := v.([]value)
	if !ok {
		if aa, ok2 := v.([]any); ok2 {
			arr = aa
		} else {
			return nil, fmt.Errorf("cannot index non-array")
		}
	}
	n, ok := idx.(float64)
	if !ok {
		return nil, fmt.Errorf("array index must be a number")
	}
	i := int(n)
	if i < 0 || i >= len(arr) {
		return nil, fmt.Errorf("index %d out of range", i)
	}
	return arr[i], nil
}
