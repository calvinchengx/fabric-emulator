package tsql

// OPTION (FOR TIMESTAMP AS OF …) — recognition and refusal
// (docs/35-warehouse-time-travel.md, Phase 2).
//
// Fabric spells warehouse time travel as a query hint:
//
//	SELECT * FROM [dbo].[dimension_customer] AS DC
//	OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35.28');
//
// SQL Server has no such hint, so today the sidecar fails the statement with an
// unrecognised-hint syntax error that names nothing about time travel. This file
// does the lexical half: find the hint, read its timestamp, hand back the
// statement with the hint removed, and refuse — in Fabric's own terms — every
// case the documentation says Fabric refuses.
//
// It is deliberately NOT wired into Adapt or CheckStrict. Doing so is Phase 3,
// which also has to resolve the timestamp to a version per table and rewrite the
// references; stripping the hint without that would answer a question about the
// past with today's data, which is the worst failure mode the design note names.
//
// Refused, each mirroring a row of docs/35's Class B table:
//
//   - timestamp-format   — more than three fractional digits, or malformed
//     (Fabric's Msg 22440); `datetime2` would take seven
//   - timestamp-timezone — a `Z` or an offset; the documented format is UTC only
//     and has no designator
//   - hint-once          — the hint appears twice in one statement
//   - select-only        — the statement does not begin with SELECT
//   - view-definition    — the hint sits in a view *definition* (querying a view
//     with it is fine, and is not this case)
//   - non-deterministic  — the value is not a literal (`@ts`, `GETDATE()`,
//     a concatenation)
//
// Everything is decided from tokens, so the hint's spelling is case-insensitive
// and may carry whitespace and comments, while the same words inside a string
// literal or a comment are not a hint at all.

import (
	"fmt"
	"strings"
	"time"
)

// TimeTravelHint is a recognised time-travel hint: the instant asked for, and
// the statement with the hint cut out so the rest can be adapted as usual.
type TimeTravelHint struct {
	At       time.Time // always UTC — the only zone the hint's format admits
	Stripped string    // sql with the hint removed
}

// TimeTravelError reports a time-travel hint that real Fabric would reject.
// Like RestrictionError it is raised instead of a rewrite: the emulator refuses
// what the warehouse refuses rather than quietly being more permissive.
type TimeTravelError struct {
	Rule   string // short identifier, e.g. "timestamp-format"
	Detail string
}

func (e *TimeTravelError) Error() string {
	return fmt.Sprintf("tsql: %s (Fabric time-travel restriction: %s)", e.Detail, e.Rule)
}

// timestampFormatMsg is Fabric's Msg 22440, quoted so a consumer sees the error
// it would have seen in production rather than a Go-flavoured paraphrase.
const timestampFormatMsg = "An error occurred during timestamp conversion. " +
	"Please provide a timestamp in the format yyyy-MM-ddTHH:mm:ss[.fff]"

// ParseTimeTravelHint reports the time-travel hint in sql, if any.
//
// It returns (nil, nil) when the statement carries no hint — including when the
// words merely appear in a literal or a comment, and when the batch does not
// tokenize at all, since an unparseable statement is the engine's business and
// not this function's.
func ParseTimeTravelHint(sql string) (*TimeTravelHint, error) {
	toks, err := Tokenize(sql)
	if err != nil {
		return nil, nil
	}
	sig := significant(toks)
	clauses := findTimeTravelClauses(sig)
	if len(clauses) == 0 {
		return nil, nil
	}
	// Structural refusals first, so a statement with two malformed hints is
	// reported by the reason that does not depend on reading either of them.
	if len(clauses) > 1 {
		return nil, &TimeTravelError{"hint-once",
			fmt.Sprintf("FOR TIMESTAMP AS OF may appear only once per SELECT, found %d", len(clauses))}
	}
	// The view check precedes select-only because CREATE VIEW fails both, and
	// the view rule is the more specific — and more surprising — of the two.
	if isViewDefinition(sig) {
		return nil, &TimeTravelError{"view-definition",
			"FOR TIMESTAMP AS OF cannot appear in a view definition (a view may be queried with it)"}
	}
	if !startsWith(sig, "select") {
		return nil, &TimeTravelError{"select-only",
			"FOR TIMESTAMP AS OF is supported only on statements that begin with SELECT"}
	}

	c := clauses[0]
	// Fabric requires a deterministic value, and the only deterministic form is
	// a literal: one String token, nothing else in the clause.
	if len(c.value) != 1 || c.value[0].Kind != String {
		return nil, &TimeTravelError{"non-deterministic",
			fmt.Sprintf("FOR TIMESTAMP AS OF requires a literal timestamp, not %s",
				describeValue(c.value))}
	}
	lit, _, ok := unquoteSQLString(c.value[0].Text)
	if !ok {
		// Unreachable: the tokenizer only emits String for a well-formed literal.
		return nil, badTimestamp(c.value[0].Text)
	}
	at, err := parseHintTimestamp(lit)
	if err != nil {
		return nil, err
	}
	// Trim spaces and tabs before the hint so the common `… AS DC OPTION (…);`
	// does not leave a gap before the terminator. A newline is kept: dropping it
	// would splice whatever follows onto the end of a preceding -- comment.
	return &TimeTravelHint{
		At:       at,
		Stripped: strings.TrimRight(sql[:c.start], " \t") + sql[c.end:],
	}, nil
}

// parseHintTimestamp reads Fabric's documented `yyyy-MM-ddTHH:mm:ss[.fff]`.
func parseHintTimestamp(lit string) (time.Time, error) {
	// A zone designator is checked before the format, so `…35Z` is refused for
	// the reason it is actually wrong (UTC only) rather than as a typo. Only the
	// text after the T is examined, since the date's own hyphens are separators.
	if t := strings.IndexAny(lit, "Tt"); t >= 0 {
		if rest := lit[t+1:]; strings.ContainsAny(rest, "Zz+-") {
			return time.Time{}, &TimeTravelError{"timestamp-timezone",
				fmt.Sprintf("time travel is UTC only, so %q cannot carry a time zone offset or designator", lit)}
		}
	}
	layout := "2006-01-02T15:04:05"
	if dot := strings.IndexByte(lit, '.'); dot >= 0 {
		digits := 0
		for digits < len(lit)-dot-1 && isDigit(lit[dot+1+digits]) {
			digits++
		}
		// At most three, at least one, and nothing trailing them: Fabric's
		// format admits ".fff" and no other shape.
		if digits == 0 || digits > 3 || dot+1+digits != len(lit) {
			return time.Time{}, badTimestamp(lit)
		}
		layout += "." + strings.Repeat("0", digits)
	}
	ts, err := time.Parse(layout, lit)
	if err != nil {
		return time.Time{}, badTimestamp(lit)
	}
	return ts.UTC(), nil
}

func badTimestamp(lit string) error {
	return &TimeTravelError{"timestamp-format", fmt.Sprintf("%s (got %q)", timestampFormatMsg, lit)}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// describeValue names a rejected value expression well enough for the error to
// be actionable, without pretending to have parsed it.
func describeValue(value []Token) string {
	if len(value) == 0 {
		return "an empty value"
	}
	var b strings.Builder
	for _, t := range value {
		b.WriteString(t.Text)
	}
	return fmt.Sprintf("%q", b.String())
}

// isViewDefinition reports a statement whose body becomes a stored view.
func isViewDefinition(sig []Token) bool {
	return startsWith(sig, "create", "view") ||
		startsWith(sig, "alter", "view") ||
		startsWith(sig, "create", "or", "alter", "view")
}

// ttClause is one recognised FOR TIMESTAMP AS OF clause: the byte range to cut
// out of the source, and the significant tokens of its value expression.
type ttClause struct {
	start, end int
	value      []Token
}

// findTimeTravelClauses locates every FOR TIMESTAMP AS OF inside an OPTION(…)
// group. The clause is looked for at the group's top level rather than only
// immediately after the paren, so `OPTION (LABEL = 'x', FOR TIMESTAMP AS OF …)`
// is seen too — dbt-fabric already ships a LABEL hint on every query, and a hint
// this file failed to see would be a statement the emulator ran against the
// present while the consumer asked about the past.
//
// The byte range removes the whole OPTION(…) group when the time-travel clause
// is the only thing in it, and just the clause — with one adjacent comma — when
// it is not, so surviving hints are left exactly as written.
func findTimeTravelClauses(sig []Token) []ttClause {
	var out []ttClause
	for i := 0; i < len(sig); i++ {
		if sig[i].Kind != Word || !strings.EqualFold(sig[i].Text, "option") {
			continue
		}
		if i+1 >= len(sig) || sig[i+1].Kind != Punct || sig[i+1].Text != "(" {
			continue // a column or alias merely named "option"
		}
		open := i + 1
		shut := skipBalanced(sig, open) - 1 // index of the matching ")"
		if shut < open {
			continue // never closes: not something to rewrite from
		}
		bounds := optionClauses(sig, open, shut)
		for _, b := range bounds {
			if !matchAt(sig, b[0], "for", "timestamp", "as", "of") {
				continue
			}
			c := ttClause{value: sig[b[0]+4 : b[1]]}
			switch {
			case len(bounds) == 1:
				c.start, c.end = sig[i].Pos, tokenEnd(sig[shut])
			case b[1] < shut: // a comma follows the clause
				c.start, c.end = sig[b[0]].Pos, tokenEnd(sig[b[1]])
			default: // the clause is last, so take the comma before it
				c.start, c.end = sig[b[0]-1].Pos, tokenEnd(sig[b[1]-1])
			}
			out = append(out, c)
		}
		i = shut
	}
	return out
}

// optionClauses splits the hints inside OPTION(…) — the tokens between open and
// shut — into [start, end) index ranges at commas belonging to the group itself.
func optionClauses(sig []Token, open, shut int) [][2]int {
	var out [][2]int
	depth, start := 0, open+1
	for j := open; j < shut; j++ {
		if sig[j].Kind != Punct {
			continue
		}
		switch sig[j].Text {
		case "(":
			depth++
		case ")":
			depth--
		case ",":
			if depth == 1 {
				out = append(out, [2]int{start, j})
				start = j + 1
			}
		}
	}
	return append(out, [2]int{start, shut})
}

func tokenEnd(t Token) int { return t.Pos + len(t.Text) }
