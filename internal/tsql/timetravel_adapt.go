package tsql

// Wiring Phase 2's hint into Adapt (docs/35-warehouse-time-travel.md, Phase 3):
// the SQL analytics endpoint over a Lakehouse.
//
// ParseTimeTravelHint only does the lexical half — find the hint, read its
// timestamp, hand back the statement with the hint cut out. What is still
// missing, and what this file does, is the part the design doc calls the most
// likely way this phase slips: the hint is STATEMENT-WIDE, so every table the
// statement references (every join, every comma-joined table, inside any
// subquery) must resolve to the SAME timestamp — and finding those references
// is new work this package's lexer has never needed before (CTAS and CTE
// flattening are both local, single-point rewrites; this one needs the whole
// statement's table list).
//
// # Division of labour
//
// This package has no idea how to read a Delta table's commit log — that
// knowledge lives in package warehouse, which already has it (Phase 1,
// ReadDeltaTableAsOf). So resolving a table reference is handed to a
// TimeTravelResolver the caller injects: this file finds the references,
// calls the resolver once per distinct table, and does the SQL-text
// surgery — materialising each resolved table as a session-scoped #temp table
// and rewriting the statement's references to point at it. A temp table has
// nothing to write back to, which is what makes the result read-only by
// construction, without a separate check (docs/35, Class A step 3).
//
// # The schema rule this phase exists to get right
//
// Fabric returns a table's CURRENT schema even when asked about the past, and
// fails outright if the statement reaches for a column that had not been
// added yet — it does not hand back NULLs for it. A naive implementation that
// just materialises the old Parquet files and lets the sidecar answer would
// do exactly the wrong thing: a column the old files don't carry silently
// reads back as NULL, which is "a plausible answer to a question Fabric would
// have refused" (docs/35). checkColumnsExistedAsOf exists solely to turn that
// into a refusal, by comparing what the resolver says existed at the
// timestamp against what it says exists today.
//
// # What is deliberately not handled
//
//   - CROSS APPLY / OUTER APPLY are not scanned for table references. Their
//     right-hand side is overwhelmingly a correlated subquery or a
//     table-valued function in real queries, neither of which this phase
//     resolves anyway, and a plain table there is rare enough that getting it
//     wrong (reading today's data for that one reference under the hint) is an
//     acceptable, documented gap rather than reason to grow a real APPLY
//     parser here.
//   - A CTE name cannot collide with this logic: Phase 2 already refuses the
//     hint on anything that does not start with SELECT, and a statement with
//     a WITH prefix starts with WITH — so a CTE can never be in scope when a
//     hint is present, and findTableRefs never needs to shadow one.
//   - A table-valued function call (`FROM dbo.Fn(@x)`) is harmlessly offered
//     to the resolver like any other name; it answers ok=false (nothing in a
//     lakehouse item is named that as a Delta table) and the reference is left
//     untouched, exactly as a #temp table is.

import (
	"fmt"
	"strings"
	"time"
)

// TimeTravelSnapshot is one table's resolved historical version, handed back
// by a TimeTravelResolver so this package can materialise it without itself
// knowing how to read Delta history.
type TimeTravelSnapshot struct {
	// Columns are this table's column names as of the requested instant, in
	// the order Rows carries them — only the ones the SQL surface can
	// represent (the same projection the reflect path already applies).
	Columns []string
	// SQLTypes is Columns' SQL Server type, one per column, e.g. "INT" or
	// "VARCHAR(8000)" — ready to use in a CREATE TABLE column definition.
	SQLTypes []string
	// RowLiterals is the table's rows, each one already rendered as SQL
	// literal expressions parallel to Columns (a quoted string, a numeric
	// literal, or the bare word NULL) and ready to splice into a VALUES list.
	// Rendering happens in the resolver because only it knows the value's
	// real type (a Delta DECIMAL's exact digits, a DATE's calendar day) —
	// this package only ever concatenates text.
	RowLiterals [][]string
	// CurrentColumns are this table's column names TODAY, independent of the
	// hint. Fabric answers a time-travel query with the latest schema, so the
	// gap between CurrentColumns and Columns is exactly the set of columns
	// that did not exist yet at the requested instant — the set
	// checkColumnsExistedAsOf refuses a reference to.
	CurrentColumns []string
}

// TimeTravelResolver materialises one table reference found under a
// `FOR TIMESTAMP AS OF` hint. table is the reference's own (innermost,
// unqualified, as-written) name — a schema or database prefix is stripped
// before this is called, since the resolver answers for one item and does not
// need to be told its own name back.
//
// ok=false with a nil error means table is not something this resolver can
// time-travel: a name that is not a Delta-backed table in the item it was
// built for (a system view, a cross-database reference, a table-valued
// function call). Adapt leaves such a reference exactly as written, the same
// treatment a #temp table gets by never being offered to the resolver at all.
//
// A non-nil error is a real failure to honour the hint — most notably the
// timestamp predating the table's first commit — and is surfaced to the
// client as a rejection rather than silently forwarding a statement that
// would answer from the wrong data.
type TimeTravelResolver func(table string, asOf time.Time) (snap *TimeTravelSnapshot, ok bool, err error)

// AdaptWithTimeTravel is Adapt, extended to resolve and materialise an
// `OPTION (FOR TIMESTAMP AS OF …)` hint via resolve (docs/35, Phase 3).
//
// resolve is nil on every connection except a lakehouse SQL analytics
// endpoint's, and a nil resolve makes this function identical to Adapt: the
// hint, if any, is left in the statement and reaches the backend unchanged,
// where it fails exactly as it always has (Class A, still open on every other
// surface — the warehouse write path has no history to travel in at all, and
// that gap is Phase 4's to close).
//
// When resolve is non-nil and the statement carries no hint, this is again
// exactly Adapt: the ordinary CTAS/flatten pipeline, untouched.
func AdaptWithTimeTravel(sql string, resolve TimeTravelResolver) (out string, changed bool, err error) {
	if resolve == nil {
		return Adapt(sql)
	}
	hint, herr := ParseTimeTravelHint(sql)
	if herr != nil {
		return sql, false, herr
	}
	if hint == nil {
		return Adapt(sql)
	}

	prologue, rewritten, merr := materializeTimeTravel(hint.Stripped, hint.At, resolve)
	if merr != nil {
		return sql, false, merr
	}
	// The user's own statement, with its table references already pointed at
	// the materialised temporaries, still goes through the ordinary pipeline:
	// it may carry a nested CTE of its own, needing exactly the same
	// flattening any other statement would.
	adapted, _, aerr := adaptStatement(rewritten)
	if aerr != nil {
		return sql, false, aerr
	}
	return prologue + adapted, true, nil
}

// materializeTimeTravel resolves every table reference in stmt (already past
// Phase 2 — the hint is gone) and returns the CREATE+INSERT prologue that
// builds each resolved table's #temp snapshot, plus stmt with those
// references rewritten to name the temporaries.
//
// The prologue is never run through adaptStatement: it is SQL this file wrote
// itself, not a client's, and every CREATE TABLE in it names a #temp table
// (never a plain one), so CTAS's own `CREATE TABLE … AS SELECT` shape cannot
// arise from it — ctasBodyEnd's own test (TestRewriteCTAS…) pins that a bare
// `CREATE TABLE t (…)` with no AS is left alone, and this prologue does not
// even reach adaptStatement to rely on it.
func materializeTimeTravel(stmt string, asOf time.Time, resolve TimeTravelResolver) (prologue, rewritten string, err error) {
	toks, terr := Tokenize(stmt)
	if terr != nil {
		return "", "", terr
	}
	sig := significant(toks)
	refs := findTableRefs(sig)
	if len(refs) == 0 {
		return "", stmt, nil
	}

	resolved := map[string]*resolvedTimeTravelTable{}
	order := make([]string, 0, len(refs))
	for _, r := range refs {
		key := r.base // already normalised (Ident-equivalent) by findTableRefs
		if _, done := resolved[key]; done {
			continue // a self-join: resolve the table once, reuse for every alias
		}
		snap, ok, rerr := resolve(r.rawBase, asOf)
		if rerr != nil {
			return "", "", &TimeTravelError{"unavailable",
				fmt.Sprintf("FOR TIMESTAMP AS OF could not resolve %q: %s", r.name, rerr.Error())}
		}
		if !ok {
			continue // not a Delta table this resolver knows: leave it alone
		}
		tooNew := make(map[string]bool, len(snap.CurrentColumns))
		have := make(map[string]bool, len(snap.Columns))
		for _, c := range snap.Columns {
			have[Ident(c)] = true
		}
		for _, c := range snap.CurrentColumns {
			if !have[Ident(c)] {
				tooNew[Ident(c)] = true
			}
		}
		resolved[key] = &resolvedTimeTravelTable{
			snap: snap, tempName: fmt.Sprintf("#tt%d", len(order)), tooNew: tooNew,
		}
		order = append(order, key)
	}
	if len(resolved) == 0 {
		return "", stmt, nil
	}

	if err := checkColumnsExistedAsOf(sig, refs, resolved); err != nil {
		return "", "", err
	}

	// Splice back to front so earlier byte offsets stay valid — the same
	// discipline adaptDynamicSQL uses for its own edits. A reference with no
	// explicit alias gets one spliced in (`#tt0 AS Customer`) so any OTHER
	// qualifier in the statement — a WHERE clause written against the
	// table's own name — stays valid; a reference that already had an alias
	// just gets its table name swapped, since the alias itself, right after
	// it in the source, is left untouched.
	out := stmt
	for i := len(refs) - 1; i >= 0; i-- {
		r := refs[i]
		rt, ok := resolved[r.base]
		if !ok {
			continue
		}
		repl := rt.tempName
		if r.keepAs != "" {
			repl += " AS " + r.keepAs
		}
		out = out[:r.start] + repl + out[r.end:]
	}

	var b strings.Builder
	for _, key := range order {
		writeTimeTravelMaterialize(&b, resolved[key])
	}
	return b.String(), out, nil
}

// resolvedTimeTravelTable is one table reference already resolved: its
// snapshot, the #temp name it materialises as, and the columns that exist
// today but did not exist as of the hint's timestamp.
type resolvedTimeTravelTable struct {
	snap     *TimeTravelSnapshot
	tempName string
	tooNew   map[string]bool // Ident(column) -> true
}

// writeTimeTravelMaterialize appends the CREATE TABLE + INSERT statements
// that build rt's #temp snapshot. An empty table still gets its CREATE TABLE,
// since the query may legitimately expect zero rows.
//
// The CREATE is preceded by a drop of any stale table of that name. Sent as a
// plain batch (no sp_executesql around it) a #temp table lives for the whole
// SESSION, so a second time-travel query on one connection collided with the
// first one's "#tt0" -- "There is already an object named '#tt0'" -- and every
// later hint on that session failed. It went unseen because every test issued
// one hint per connection.
func writeTimeTravelMaterialize(b *strings.Builder, rt *resolvedTimeTravelTable) {
	b.WriteString("IF OBJECT_ID('tempdb..")
	b.WriteString(rt.tempName)
	b.WriteString("') IS NOT NULL DROP TABLE ")
	b.WriteString(rt.tempName)
	b.WriteString(";\n")
	b.WriteString("CREATE TABLE ")
	b.WriteString(rt.tempName)
	b.WriteString(" (")
	for i, c := range rt.snap.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(quoteTimeTravelIdent(c))
		b.WriteString(" ")
		b.WriteString(rt.snap.SQLTypes[i])
	}
	b.WriteString(");\n")
	for _, row := range rt.snap.RowLiterals {
		b.WriteString("INSERT INTO ")
		b.WriteString(rt.tempName)
		b.WriteString(" VALUES (")
		b.WriteString(strings.Join(row, ", "))
		b.WriteString(");\n")
	}
}

func quoteTimeTravelIdent(s string) string {
	return "[" + strings.ReplaceAll(s, "]", "]]") + "]"
}

// tableRef is one FROM/JOIN table reference found in a statement.
type tableRef struct {
	name string // the qualified name as written, e.g. "[dbo].[T1]"
	// rawBase is the name's last segment, unquoted but NOT case-folded — what
	// is handed to the resolver. A Delta table's folder name is
	// case-sensitive (it is a OneLake path), so the resolver must see
	// "Customer" exactly, never "customer": lower-casing it here would make
	// every reference to a mixed-case table name unresolvable.
	rawBase string
	base    string // rawBase, normalised (Ident) — the map/alias comparison key
	alias   string // its alias if written, else base — also normalised
	// keepAs is set ONLY when the reference carried no explicit alias, to the
	// table's own last segment AS WRITTEN (not normalised). `FROM
	// dbo.Customer` has no alias at all, so a reference elsewhere in the
	// statement to `Customer.col` uses the table's own name as its implicit
	// alias; replacing the FROM entry with a bare temp-table name would leave
	// that reference dangling. Splicing in `#tt0 AS Customer` instead keeps
	// every other qualifier in the statement valid without this file needing
	// to find and rewrite them too. When an alias WAS written, it already
	// follows the table name in the source untouched — re-adding it here
	// would splice in a second, duplicate `AS alias` — so keepAs stays "".
	keepAs     string
	start, end int // byte range of `name` in the source
}

// fromListStop are the words that end a table reference's optional bare alias
// (no AS) — every one of them can legally follow a table name in a FROM
// clause and none of them is ever itself an alias a real query would choose,
// so treating them as "not an alias" is the one place this file assumes
// something about the identifiers a consumer writes.
var fromListStop = map[string]bool{
	"where": true, "group": true, "order": true, "having": true,
	"join": true, "inner": true, "left": true, "right": true, "full": true,
	"outer": true, "cross": true, "on": true, "union": true, "except": true,
	"intersect": true, "option": true, "with": true, "for": true,
}

// findTableRefs walks sig for every table named directly after FROM or JOIN —
// including a comma-joined list (`FROM a, b`) and anything nested inside a
// subquery, since depth is never tracked: a reference at any nesting level is
// still a reference the hint's statement-wide scope must resolve.
func findTableRefs(sig []Token) []tableRef {
	var out []tableRef
	for i := 0; i < len(sig); i++ {
		t := sig[i]
		if t.Kind != Word || (!strings.EqualFold(t.Text, "from") && !strings.EqualFold(t.Text, "join")) {
			continue
		}
		i++ // consume FROM/JOIN
		// A subquery, a table-valued function with no name shape we trust, or
		// end of input ends the comma-joined list without a ref.
		for i < len(sig) && (sig[i].Kind == Word || sig[i].Kind == QuotedIdent) {
			start := sig[i].Pos
			name, ni, ok := scanQualifiedName(sig, i)
			if !ok {
				break
			}
			end := tokenEnd(sig[ni-1])
			i = ni
			rawBase := unquoteIdentKeepCase(rawLastSegment(name))
			base := strings.ToLower(rawBase)
			ref := tableRef{name: name, rawBase: rawBase, base: base, start: start, end: end}

			alias, ni2 := scanOptionalAlias(sig, i)
			i = ni2
			if alias != "" {
				// The alias is already in the source, right after the name,
				// and is left completely untouched by the splice below.
				ref.alias = Ident(alias)
			} else {
				ref.alias = Ident(base)
				ref.keepAs = rawLastSegment(name)
			}
			i = skipTableHint(sig, i)

			if !strings.HasPrefix(base, "#") {
				out = append(out, ref)
			}

			if i < len(sig) && sig[i].Kind == Punct && sig[i].Text == "," {
				i++
				continue // another comma-joined table
			}
			break
		}
		i-- // the outer loop's i++ will land back on the token we stopped at
	}
	return out
}

// scanOptionalAlias consumes `AS alias` or a bare `alias` after a table
// reference, returning "" when what follows is not an alias at all — a
// keyword that ends the reference, a table hint, a comma, or anything that
// is not a plain identifier.
func scanOptionalAlias(sig []Token, i int) (string, int) {
	if i >= len(sig) {
		return "", i
	}
	if sig[i].Kind == Word && strings.EqualFold(sig[i].Text, "as") {
		i++
		if i < len(sig) && (sig[i].Kind == Word || sig[i].Kind == QuotedIdent) {
			return sig[i].Text, i + 1
		}
		return "", i
	}
	if sig[i].Kind == QuotedIdent {
		return sig[i].Text, i + 1
	}
	if sig[i].Kind == Word && !fromListStop[strings.ToLower(sig[i].Text)] {
		return sig[i].Text, i + 1
	}
	return "", i
}

// skipTableHint consumes a `WITH (NOLOCK)`-style table hint, if one sits at i.
func skipTableHint(sig []Token, i int) int {
	if i < len(sig) && sig[i].Kind == Word && strings.EqualFold(sig[i].Text, "with") &&
		i+1 < len(sig) && sig[i+1].Kind == Punct && sig[i+1].Text == "(" {
		if end := skipBalanced(sig, i+1); end > 0 {
			return end
		}
	}
	return i
}

// rawLastSegment returns a dotted qualified name's final segment exactly as
// written — `[dbo].[Customer]` becomes `[Customer]` — since it is spliced
// back into the statement as an alias (tableRef.keepAs) rather than compared
// against anything.
func rawLastSegment(qualified string) string {
	if idx := strings.LastIndex(qualified, "."); idx >= 0 {
		return qualified[idx+1:]
	}
	return qualified
}

// unquoteIdentKeepCase strips an identifier's bracket or double-quote
// delimiters without folding its case — unlike Ident, whose lower-casing is
// wrong for a name that is about to be handed to a resolver: a Delta table's
// folder name is a case-sensitive OneLake path, so "Customer" and "customer"
// are different tables, not the same one spelled two ways.
func unquoteIdentKeepCase(name string) string {
	if len(name) >= 2 {
		switch {
		case name[0] == '[' && name[len(name)-1] == ']':
			return strings.ReplaceAll(name[1:len(name)-1], "]]", "]")
		case name[0] == '"' && name[len(name)-1] == '"':
			return strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
		}
	}
	return name
}

// checkColumnsExistedAsOf refuses a reference to a column that exists today
// but did not exist as of the hint's timestamp (docs/35, Class B: Fabric
// fails such a query rather than answering it with NULLs).
//
// A qualified reference (`alias.col`, `alias.*`) is checked against exactly
// that table. An unqualified `*` is checked against every resolved table,
// since it draws columns from all of them. An unqualified bare column name is
// checked against every resolved table too — conservative, since this package
// cannot bind an unqualified column to the one table among several that
// actually owns it, and failing a query Fabric would have allowed is a far
// smaller sin here than the one this check exists to prevent.
func checkColumnsExistedAsOf(sig []Token, refs []tableRef, resolved map[string]*resolvedTimeTravelTable) error {
	aliasMap := make(map[string]*resolvedTimeTravelTable, len(refs))
	for _, r := range refs {
		if rt, ok := resolved[r.base]; ok {
			aliasMap[r.alias] = rt
		}
	}
	if len(aliasMap) == 0 {
		return nil
	}

	for i := 0; i < len(sig); i++ {
		t := sig[i]

		// A bare `*`: only in a SELECT-list position, never arithmetic. The
		// token immediately before it is enough to tell the two apart — a
		// multiplication's left operand is a value (a column, a number, a
		// closing paren), never one of these.
		if t.Kind == Punct && t.Text == "*" {
			if starIsSelectList(sig, i) {
				for _, rt := range resolved {
					if len(rt.tooNew) > 0 {
						return newTooNewColumnError("*", rt)
					}
				}
			}
			continue
		}

		if t.Kind != Word && t.Kind != QuotedIdent {
			continue
		}

		// A qualified pair: `alias.col` or `alias.*`. Consume both tokens of
		// the pair so the bare-identifier arm below never re-examines the
		// right-hand side on its own.
		if i+2 < len(sig) && sig[i+1].Kind == Punct && sig[i+1].Text == "." {
			if rt, ok := aliasMap[Ident(t.Text)]; ok {
				right := sig[i+2]
				switch {
				case right.Kind == Punct && right.Text == "*":
					if len(rt.tooNew) > 0 {
						return newTooNewColumnError(t.Text+".*", rt)
					}
				case right.Kind == Word || right.Kind == QuotedIdent:
					if rt.tooNew[Ident(right.Text)] {
						return newTooNewColumnError(t.Text+"."+right.Text, rt)
					}
				}
			}
			i += 2
			continue
		}
		// The right-hand side of a qualified pair reaches here only when its
		// left side was NOT a known alias (so the pair above did not fire and
		// did not skip past it) — in which case this token is some other
		// table's column, not one of the resolved tables', or it is the
		// qualifier of a pair whose own left we are currently sitting on. A
		// token preceded by a '.' is always the right half of some pair and
		// must not be re-checked as a bare identifier.
		if i > 0 && sig[i-1].Kind == Punct && sig[i-1].Text == "." {
			continue
		}

		id := Ident(t.Text)
		for _, rt := range resolved {
			if rt.tooNew[id] {
				return newTooNewColumnError(t.Text, rt)
			}
		}
	}
	return nil
}

// starIsSelectList reports whether the `*` at sig[i] sits where a SELECT
// list's star does: right after SELECT, DISTINCT, ALL, or a comma separating
// select-list items. Anything else precedes it only in an arithmetic
// expression, which this function must not confuse for a wildcard.
func starIsSelectList(sig []Token, i int) bool {
	if i == 0 {
		return false
	}
	p := sig[i-1]
	if p.Kind == Punct && p.Text == "," {
		return true
	}
	return p.Kind == Word && (strings.EqualFold(p.Text, "select") ||
		strings.EqualFold(p.Text, "distinct") || strings.EqualFold(p.Text, "all"))
}

// newTooNewColumnError reports a reference to a column docs/35's Class B
// table names explicitly: one that exists today but did not exist as of the
// requested instant, which real Fabric fails rather than answering with NULL.
func newTooNewColumnError(ref string, rt *resolvedTimeTravelTable) error {
	return &TimeTravelError{"schema-as-of", fmt.Sprintf(
		"%q references a column added after the requested FOR TIMESTAMP AS OF instant; "+
			"Fabric returns the table's current schema and fails rather than answering with NULL "+
			"for a column that did not exist yet", ref)}
}
