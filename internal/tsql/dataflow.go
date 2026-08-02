package tsql

// Data-flow extraction: which tables a statement reads and which it writes.
//
// This feeds the warehouse lineage observer (docs/31-flow-observability.md):
// the TDS front already parses every statement for dialect adaptation, so the
// same token stream can say "this CTAS filled dbo.fct_orders from these three
// tables" — an observation of the statement the client actually sent, not an
// inference from results.
//
// The philosophy matches the dialect layer's: only statements this package
// fully understands produce a Flow. "I don't understand this" must never
// become "I'll guess", so an unrecognised statement yields nothing rather
// than a wrong edge.
//
// What is recognised, measured against what dbt-fabric actually ships
// (internal/tsql/ctas.go documents the same shapes):
//
//	CREATE TABLE t [WITH (…)] AS SELECT …      -- Fabric CTAS
//	SELECT … INTO t FROM …                     -- the sidecar's spelling
//	INSERT [INTO] t … SELECT …                 -- appends (VALUES moves no table)
//	CREATE [OR ALTER] VIEW v AS SELECT …       -- no bytes move; recorded so a
//	                                              later CTAS from v can be
//	                                              resolved to v's base tables
//	EXEC('…literal…')                          -- dbt wraps its DDL in dynamic
//	                                              SQL; one level is unwrapped,
//	                                              exactly like Adapt does
//	[EXEC] sp_rename 'old', 'new' [, 'OBJECT'] -- dbt materialises into a temp
//	                                              name and renames; lineage
//	                                              must follow the rename
//	DROP TABLE|VIEW [IF EXISTS] n [, …]        -- so a dropped target's edges
//	                                              can be retired

import "strings"

// Flow kinds.
const (
	FlowCTAS       = "CTAS"
	FlowSelectInto = "SELECT INTO"
	FlowInsert     = "INSERT"
	FlowCreateView = "CREATE VIEW"
	FlowRename     = "RENAME"
	FlowDropTable  = "DROP TABLE"
	FlowDropView   = "DROP VIEW"
)

// Flow is one recognised data movement (or object-identity change). Names are
// slices of unquoted parts as written — up to db.schema.table — because a
// bracketed identifier may itself contain a dot, so a joined string could not
// be split back apart safely.
type Flow struct {
	Kind    string
	Target  []string
	NewName string // FlowRename only: the new (single-part) object name
	Sources [][]string
}

// DataFlows returns every flow recognised in a batch. A batch that cannot be
// tokenized, or contains nothing recognisable, yields nil.
func DataFlows(sql string) []Flow {
	toks, err := Tokenize(sql)
	if err != nil {
		return nil
	}
	return statementFlows(significant(toks), 0)
}

// statementFlows walks a token stream statement by statement. level guards the
// EXEC('…') recursion: dynamic SQL that builds more dynamic SQL is left alone,
// mirroring adaptDynamicSQL.
func statementFlows(sig []Token, level int) []Flow {
	var flows []Flow
	for len(sig) > 0 {
		// Skip statement separators.
		if sig[0].Kind == Punct && sig[0].Text == ";" {
			sig = sig[1:]
			continue
		}
		stmt, rest := splitStatement(sig)
		if f := classify(stmt, level); f != nil {
			flows = append(flows, f...)
		}
		sig = rest
	}
	return flows
}

// splitStatement cuts sig at the first depth-0 semicolon.
func splitStatement(sig []Token) (stmt, rest []Token) {
	depth := 0
	for i, t := range sig {
		if t.Kind != Punct {
			continue
		}
		switch t.Text {
		case "(":
			depth++
		case ")":
			depth--
		case ";":
			if depth == 0 {
				return sig[:i], sig[i+1:]
			}
		}
	}
	return sig, nil
}

// classify recognises one statement. Most statements are not data movement and
// return nil.
func classify(sig []Token, level int) []Flow {
	if len(sig) == 0 {
		return nil
	}
	switch {
	case startsWith(sig, "create", "table"):
		return ctasFlow(sig)
	case startsWith(sig, "create", "view"), startsWith(sig, "create", "or"), startsWith(sig, "alter", "view"):
		return viewFlow(sig)
	case startsWith(sig, "insert"):
		return insertFlow(sig)
	case startsWith(sig, "select"), startsWith(sig, "with"):
		return selectIntoFlow(sig)
	case startsWith(sig, "drop", "table"):
		return dropFlow(sig, FlowDropTable)
	case startsWith(sig, "drop", "view"):
		return dropFlow(sig, FlowDropView)
	case startsWith(sig, "exec"), startsWith(sig, "execute"), startsWith(sig, "sp_rename"):
		return execFlow(sig, level)
	}
	return nil
}

// ctasFlow: CREATE TABLE name [WITH (…)] AS body.
func ctasFlow(sig []Token) []Flow {
	i := 2
	target, i, ok := scanNameParts(sig, i)
	if !ok || tempObject(target) {
		return nil
	}
	if i < len(sig) && wordIs(sig[i], "with") && i+1 < len(sig) && punctIs(sig[i+1], "(") {
		if i = skipBalanced(sig, i+1); i < 0 {
			return nil
		}
	}
	if i >= len(sig) || !wordIs(sig[i], "as") {
		return nil // a plain CREATE TABLE (…): DDL, no movement
	}
	body := sig[i+1:]
	return []Flow{{Kind: FlowCTAS, Target: target, Sources: bodySources(body)}}
}

// viewFlow: CREATE [OR ALTER] VIEW name [(cols)] AS body.
func viewFlow(sig []Token) []Flow {
	i := 1
	if wordIs(sig[i], "or") { // CREATE OR ALTER
		i++
		if i >= len(sig) || !wordIs(sig[i], "alter") {
			return nil
		}
		i++
	}
	if i >= len(sig) || !wordIs(sig[i], "view") {
		return nil
	}
	i++
	target, i, ok := scanNameParts(sig, i)
	if !ok {
		return nil
	}
	if i < len(sig) && punctIs(sig[i], "(") {
		if i = skipBalanced(sig, i); i < 0 {
			return nil
		}
	}
	if i >= len(sig) || !wordIs(sig[i], "as") {
		return nil
	}
	return []Flow{{Kind: FlowCreateView, Target: target, Sources: bodySources(sig[i+1:])}}
}

// insertFlow: INSERT [INTO] name [(cols)] SELECT|WITH …. INSERT … VALUES moves
// no table, so it yields nothing.
func insertFlow(sig []Token) []Flow {
	i := 1
	if i < len(sig) && wordIs(sig[i], "into") {
		i++
	}
	target, i, ok := scanNameParts(sig, i)
	if !ok || tempObject(target) {
		return nil
	}
	if i < len(sig) && punctIs(sig[i], "(") {
		if i = skipBalanced(sig, i); i < 0 {
			return nil
		}
	}
	if i >= len(sig) || sig[i].Kind != Word ||
		(!wordIs(sig[i], "select") && !wordIs(sig[i], "with")) {
		return nil
	}
	return []Flow{{Kind: FlowInsert, Target: target, Sources: bodySources(sig[i:])}}
}

// selectIntoFlow: a query with a depth-0 INTO — the sidecar's CTAS spelling.
func selectIntoFlow(sig []Token) []Flow {
	depth := 0
	for i := 0; i < len(sig); i++ {
		t := sig[i]
		if t.Kind == Punct {
			switch t.Text {
			case "(":
				depth++
			case ")":
				depth--
			}
			continue
		}
		if depth == 0 && wordIs(t, "into") {
			target, _, ok := scanNameParts(sig, i+1)
			if !ok || tempObject(target) {
				return nil
			}
			return []Flow{{Kind: FlowSelectInto, Target: target, Sources: bodySources(sig)}}
		}
	}
	return nil
}

// dropFlow: DROP TABLE|VIEW [IF EXISTS] n [, n2 …].
func dropFlow(sig []Token, kind string) []Flow {
	i := 2
	if i+1 < len(sig) && wordIs(sig[i], "if") && wordIs(sig[i+1], "exists") {
		i += 2
	}
	var flows []Flow
	for {
		target, j, ok := scanNameParts(sig, i)
		if !ok {
			break
		}
		if !tempObject(target) {
			flows = append(flows, Flow{Kind: kind, Target: target})
		}
		if j < len(sig) && punctIs(sig[j], ",") {
			i = j + 1
			continue
		}
		break
	}
	return flows
}

// execFlow handles the two EXEC shapes that matter here: EXEC('literal') — one
// level of dynamic SQL, exactly like Adapt — and [EXEC] sp_rename 'old','new'.
func execFlow(sig []Token, level int) []Flow {
	i := 0
	if wordIs(sig[i], "exec") || wordIs(sig[i], "execute") {
		i++
	}
	if i >= len(sig) {
		return nil
	}
	// EXEC('…'): recurse into the literal, once.
	if punctIs(sig[i], "(") && i+2 < len(sig) && sig[i+1].Kind == String && punctIs(sig[i+2], ")") {
		if level > 0 {
			return nil
		}
		inner, _, ok := unquoteSQLString(sig[i+1].Text)
		if !ok {
			return nil
		}
		toks, err := Tokenize(inner)
		if err != nil {
			return nil
		}
		return statementFlows(significant(toks), level+1)
	}
	// sp_rename 'old', 'new' [, 'OBJECT']: an object rename. A COLUMN/INDEX
	// rename carries a third argument naming the kind; only an object rename
	// (absent third argument, or the literal OBJECT) moves a table's identity.
	if !wordIs(sig[i], "sp_rename") {
		return nil
	}
	args := stringArgs(sig[i+1:])
	if len(args) < 2 {
		return nil
	}
	if len(args) >= 3 && !strings.EqualFold(args[2], "object") {
		return nil
	}
	old := strings.Split(args[0], ".")
	if len(old) == 0 || len(old) > 3 {
		return nil
	}
	for i := range old {
		old[i] = unbracket(old[i])
	}
	return []Flow{{Kind: FlowRename, Target: old, NewName: unbracket(args[1])}}
}

// stringArgs collects the leading comma-separated string-literal arguments.
func stringArgs(sig []Token) []string {
	var out []string
	for i := 0; i < len(sig); i++ {
		t := sig[i]
		switch {
		case t.Kind == String:
			s, _, ok := unquoteSQLString(t.Text)
			if !ok {
				return out
			}
			out = append(out, s)
		case punctIs(t, ","):
			continue
		case wordIs(t, "@objname") || wordIs(t, "@newname") || wordIs(t, "@objtype") || punctIs(t, "="):
			continue // the named-argument spelling of the same call
		default:
			return out
		}
	}
	return out
}

// bodySources extracts the table references a query body reads: every FROM or
// JOIN operand at any depth, minus CTE aliases, temp/system objects, and
// table-valued functions.
func bodySources(sig []Token) [][]string {
	exclude := map[string]bool{}
	if len(sig) > 0 && wordIs(sig[0], "with") {
		collectCTENames(sig, exclude)
	}
	var out [][]string
	seen := map[string]bool{}
	for i := 0; i < len(sig); i++ {
		t := sig[i]
		if t.Kind != Word || (!wordIs(t, "from") && !wordIs(t, "join")) {
			continue
		}
		j := i + 1
		if j >= len(sig) {
			break
		}
		if punctIs(sig[j], "(") {
			continue // derived table; its own FROM is seen later in this walk
		}
		name, k, ok := scanNameParts(sig, j)
		if !ok {
			continue
		}
		if k < len(sig) && punctIs(sig[k], "(") {
			continue // a table-valued function, not a table
		}
		if tempObject(name) || systemObject(name) {
			continue
		}
		if len(name) == 1 && exclude[strings.ToLower(name[0])] {
			continue // a CTE alias, not a table
		}
		key := strings.ToLower(strings.Join(name, "\x00"))
		if !seen[key] {
			seen[key] = true
			out = append(out, name)
		}
		i = k - 1
	}
	return out
}

// collectCTENames records the aliases a leading WITH clause defines, so a
// FROM that names one is not mistaken for a table read. CTE *bodies* are
// scanned for sources by the caller's walk — they are real reads.
func collectCTENames(sig []Token, into map[string]bool) {
	i := 1 // past WITH
	for i < len(sig) {
		if sig[i].Kind != Word && sig[i].Kind != QuotedIdent {
			return
		}
		into[strings.ToLower(unbracket(sig[i].Text))] = true
		i++
		if i < len(sig) && punctIs(sig[i], "(") { // optional column list
			if i = skipBalanced(sig, i); i < 0 {
				return
			}
		}
		if i >= len(sig) || !wordIs(sig[i], "as") {
			return
		}
		i++
		if i >= len(sig) || !punctIs(sig[i], "(") {
			return
		}
		if i = skipBalanced(sig, i); i < 0 {
			return
		}
		if i < len(sig) && punctIs(sig[i], ",") {
			i++
			continue
		}
		return
	}
}

// scanNameParts reads a qualified name and returns its unquoted parts.
func scanNameParts(sig []Token, i int) ([]string, int, bool) {
	if i >= len(sig) || (sig[i].Kind != Word && sig[i].Kind != QuotedIdent) {
		return nil, i, false
	}
	parts := []string{unbracket(sig[i].Text)}
	i++
	for i+1 < len(sig) && punctIs(sig[i], ".") &&
		(sig[i+1].Kind == Word || sig[i+1].Kind == QuotedIdent) {
		parts = append(parts, unbracket(sig[i+1].Text))
		i += 2
	}
	return parts, i, true
}

// unbracket strips [x] or "x" delimiters and undoes their escape doubling.
func unbracket(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return strings.ReplaceAll(s[1:len(s)-1], "]]", "]")
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// tempObject reports a name whose object part is a temp table or table
// variable — session-scoped, so never a lineage node.
func tempObject(parts []string) bool {
	last := parts[len(parts)-1]
	return last == "" || last[0] == '#' || last[0] == '@'
}

// systemObject reports catalog reads — sys.*, INFORMATION_SCHEMA.* — which are
// metadata, not data movement.
func systemObject(parts []string) bool {
	if len(parts) < 2 {
		return false
	}
	schema := strings.ToLower(parts[len(parts)-2])
	return schema == "sys" || schema == "information_schema"
}

func wordIs(t Token, w string) bool  { return t.Kind == Word && strings.EqualFold(t.Text, w) }
func punctIs(t Token, p string) bool { return t.Kind == Punct && t.Text == p }
