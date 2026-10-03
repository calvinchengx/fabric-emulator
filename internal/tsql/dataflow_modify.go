package tsql

// Statements that change a table's contents or shape without moving data from
// another table: INSERT … VALUES, UPDATE, DELETE, TRUNCATE, MERGE, ALTER TABLE,
// and a plain CREATE TABLE (…).
//
// Lineage has no use for these -- no edge is drawn from nothing -- which is why
// they were not recognised before. Warehouse time travel does
// (docs/35-warehouse-time-travel.md, Phase 4): a version is a commit per
// data-changing statement, and a statement that was not recognised is a version
// that was never written, silently. Each is reported as a FlowModify naming the
// table it changed; the lineage observer ignores the kind.
//
// The same rule as the rest of this file holds: only a statement fully
// understood yields a flow. A target written as an alias (UPDATE a SET … FROM
// dbo.t a) is resolved through the statement's own FROM list; a name that
// matches no alias is taken as the table it says it is.

import "strings"

// FlowModify is a statement that changed a table in place.
const FlowModify = "MODIFY"

// modifyFlow recognises the in-place writers. nil means "not one of them".
func modifyFlow(sig []Token) []Flow {
	switch {
	case startsWith(sig, "update"):
		if len(sig) > 1 && wordIs(sig[1], "statistics") {
			return nil
		}
		return updateFlow(sig)
	case startsWith(sig, "delete"):
		return deleteFlow(sig)
	case startsWith(sig, "truncate", "table"):
		return nameFlow(sig, 2)
	case startsWith(sig, "merge"):
		return mergeFlow(sig)
	case startsWith(sig, "alter", "table"):
		return nameFlow(sig, 2)
	}
	return nil
}

func nameFlow(sig []Token, i int) []Flow {
	target, _, ok := scanNameParts(sig, i)
	if !ok || tempObject(target) {
		return nil
	}
	return []Flow{{Kind: FlowModify, Target: target}}
}

// skipTop steps over an optional TOP (n) [PERCENT] / TOP n.
func skipTop(sig []Token, i int) int {
	if i >= len(sig) || !wordIs(sig[i], "top") {
		return i
	}
	i++
	if i < len(sig) && punctIs(sig[i], "(") {
		if j := skipBalanced(sig, i); j >= 0 {
			i = j
		}
	} else if i < len(sig) {
		i++
	}
	if i < len(sig) && wordIs(sig[i], "percent") {
		i++
	}
	return i
}

// UPDATE [TOP (n)] target SET …
func updateFlow(sig []Token) []Flow {
	target, _, ok := scanNameParts(sig, skipTop(sig, 1))
	if !ok {
		return nil
	}
	return resolvedModify(sig, target)
}

// DELETE [TOP (n)] [FROM] target [FROM …]
func deleteFlow(sig []Token) []Flow {
	i := skipTop(sig, 1)
	if i < len(sig) && wordIs(sig[i], "from") {
		i++
	}
	target, _, ok := scanNameParts(sig, i)
	if !ok {
		return nil
	}
	return resolvedModify(sig, target)
}

// MERGE [TOP (n)] [INTO] target [[AS] alias] USING …
func mergeFlow(sig []Token) []Flow {
	i := skipTop(sig, 1)
	if i < len(sig) && wordIs(sig[i], "into") {
		i++
	}
	target, _, ok := scanNameParts(sig, i)
	if !ok || tempObject(target) {
		return nil
	}
	return []Flow{{Kind: FlowModify, Target: target}}
}

// resolvedModify maps an UPDATE/DELETE target that is an alias back to the table
// the statement's FROM list gave that alias.
func resolvedModify(sig []Token, target []string) []Flow {
	if len(target) == 1 {
		if real, ok := aliasInFrom(sig, target[0]); ok {
			target = real
		}
	}
	if tempObject(target) {
		return nil
	}
	return []Flow{{Kind: FlowModify, Target: target}}
}

// aliasInFrom finds, among the depth-0 FROM/JOIN table references, the one the
// name aliases.
func aliasInFrom(sig []Token, name string) ([]string, bool) {
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
		if depth != 0 || (!wordIs(t, "from") && !wordIs(t, "join")) {
			continue
		}
		// A FROM list may hold several comma-separated references.
		j := i + 1
		for {
			ref, next, ok := scanNameParts(sig, j)
			if !ok {
				break
			}
			if next < len(sig) && wordIs(sig[next], "as") {
				next++
			}
			alias := ""
			if next < len(sig) && sig[next].Kind == Word && !aliasStop[strings.ToLower(sig[next].Text)] {
				alias = sig[next].Text
				next++
			} else if next < len(sig) && sig[next].Kind == QuotedIdent {
				alias = unbracket(sig[next].Text)
				next++
			}
			if alias != "" && strings.EqualFold(alias, name) || alias == "" && strings.EqualFold(ref[len(ref)-1], name) {
				return ref, true
			}
			if next < len(sig) && punctIs(sig[next], ",") {
				j = next + 1
				continue
			}
			break
		}
	}
	return nil, false
}

// aliasStop are the words that can follow a table reference without being its
// alias.
var aliasStop = map[string]bool{
	"where": true, "on": true, "join": true, "inner": true, "left": true, "right": true,
	"full": true, "cross": true, "outer": true, "group": true, "order": true, "set": true,
	"union": true, "option": true, "using": true, "when": true, "with": true, "for": true,
	"output": true, "having": true, "except": true, "intersect": true,
}

// cteLedFlow reads a statement that opens with a WITH clause. The clause leads
// a query or any DML statement; the verb after the CTE list says which. A query
// is the existing SELECT … INTO reading. For DML the verb's own reader runs on
// the part after the clause, and a CTE the statement writes THROUGH
// (WITH c AS (SELECT … FROM dbo.t) DELETE FROM c) is resolved to the base table
// its body reads, since that is the table whose contents changed.
func cteLedFlow(sig []Token) []Flow {
	verb, bodies := cteClause(sig)
	if verb < 0 {
		return nil
	}
	rest := sig[verb:]
	switch {
	case startsWith(rest, "select"):
		return selectIntoFlow(sig)
	case startsWith(rest, "insert"):
		flows := insertFlow(rest)
		for i := range flows {
			if flows[i].Kind == FlowInsert {
				// The CTE bodies are where the rows come from.
				flows[i].Sources = bodySources(sig)
			}
		}
		return flows
	}
	flows := modifyFlow(rest)
	for i := range flows {
		t := flows[i].Target
		if len(t) != 1 {
			continue
		}
		if body, ok := bodies[strings.ToLower(t[0])]; ok {
			if src := bodySources(body); len(src) > 0 {
				flows[i].Target = src[0]
			}
		}
	}
	return flows
}

// cteClause finds the index of the verb that follows a leading WITH clause
// (-1 when the clause is malformed) and each CTE's body tokens by lower-cased
// name.
func cteClause(sig []Token) (verb int, bodies map[string][]Token) {
	bodies = map[string][]Token{}
	i := 1 // past WITH
	for i < len(sig) {
		if sig[i].Kind != Word && sig[i].Kind != QuotedIdent {
			return -1, nil
		}
		name := strings.ToLower(unbracket(sig[i].Text))
		i++
		if i < len(sig) && punctIs(sig[i], "(") { // optional column list
			if i = skipBalanced(sig, i); i < 0 {
				return -1, nil
			}
		}
		if i+1 >= len(sig) || !wordIs(sig[i], "as") || !punctIs(sig[i+1], "(") {
			return -1, nil
		}
		end := skipBalanced(sig, i+1)
		if end < 0 {
			return -1, nil
		}
		bodies[name] = sig[i+2 : end-1]
		i = end
		if i < len(sig) && punctIs(sig[i], ",") {
			i++
			continue
		}
		return i, bodies
	}
	return -1, nil
}
