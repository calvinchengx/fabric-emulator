// Package onelakesec evaluates OneLake security roles into the effective access
// a principal has inside one Fabric item.
//
// WHY THIS IS A PACKAGE AND NOT A HANDLER. Two callers need the same answer and
// must not be able to disagree: the DFS surface, which grants or refuses a path,
// and `securityPolicy/principalAccess`, which hands an engine the row and column
// filters to apply itself. One function, called twice.
//
// WHY `pkg/` AND NOT `internal/`. Go's internal rule would make this
// unimportable by any other module, so the only way to ever reuse it would be to
// extract a repository. See docs/54-onelake-security.md.
//
// THE MODEL, from Microsoft's own description (docs/onelake/security):
//
//   - DENY BY DEFAULT. "All users start with no access to data unless explicitly
//     granted by a OneLake security role." An empty result is the correct answer
//     for an unknown principal, not an error.
//   - A ROLE IS SCOPE + PERMISSION + MEMBERS. Scope is a path within the item;
//     permission is Read or ReadWrite; members are Entra identities OR are
//     derived from item permissions.
//   - EFFECTIVE ACCESS IS CONSOLIDATED across every role the principal is in,
//     because the API returns one entry per path rather than one per role.
//
// MEMBERSHIP HAS TWO KINDS AND BOTH ARE MANDATORY. Explicit Entra members are the
// obvious one. The other is VIRTUAL: membership derived from holding a permission
// on the item, which is how the default roles work — "by using virtualized role
// memberships, all users that have the necessary permissions to view data in the
// item (the ReadAll permission, for example) are included as members of this
// default role". DefaultReader is why a newly created item is readable at all, so
// an evaluator that models only explicit members is not a simplification of the
// product, it is a different product.
package onelakesec

import (
	"sort"
	"strings"
)

// Effect is a decision rule's outcome. The model defines Permit and Deny; the
// product supports "only GRANT type roles", so Deny is parsed and REFUSED at the
// edge rather than silently evaluated here — a rule we accepted and ignored
// would be worse than one we rejected.
type Effect string

const EffectPermit Effect = "Permit"

// Access types. "Currently, the only supported access type is Read", with
// ReadWrite defined for items that support editing.
const (
	AccessRead      = "Read"
	AccessReadWrite = "ReadWrite"
)

// InputPath selects which half of the item to report on, as the
// principalAccess API's `inputPath` does.
const (
	InputTables = "Tables"
	InputFiles  = "Files"
)

// Role is one OneLake security role on one item.
type Role struct {
	Name          string
	DecisionRules []DecisionRule
	Members       Members
}

// DecisionRule grants an Access on a set of paths, optionally narrowed to
// particular rows or columns.
//
// Paths carry the API's wildcard: `*` means everything under the input path.
type DecisionRule struct {
	Effect  Effect
	Paths   []string
	Actions []string
	// Rows is a SQL predicate expressed as the API expresses it: a SELECT the
	// engine runs, not rows we filter. Empty means unrestricted.
	Rows string
	// Columns, when non-empty, is the permitted set. Empty means all columns.
	Columns []string
}

// Members is the two membership kinds, which are a union rather than an
// alternative: a principal is a member if EITHER matches.
type Members struct {
	Entra []string // Entra object IDs
	// ItemAccess lists the item permissions that confer membership. A principal
	// holding any of them is a virtual member. This is how DefaultReader
	// includes everyone with ReadAll without storing them.
	ItemAccess []string
}

// Principal is who is asking, and what they already hold on the item.
//
// ItemAccess is supplied by the caller because it is not OneLake's to know: it
// comes from workspace roles and item permissions, which live on the control
// plane. Keeping it a parameter is what stops this package needing a store.
type Principal struct {
	ObjectID   string
	ItemAccess []string
}

// AccessEntry is one path's effective access, shaped as the principalAccess API
// returns it.
type AccessEntry struct {
	Path    string
	Access  []string
	Rows    string
	Columns []string
	Effect  Effect
}

// Effective consolidates every role the principal belongs to into one entry per
// path. Deny-by-default: a principal in no role gets an empty result.
//
// CONSOLIDATION IS A UNION, and that is the semantics the API describes — "this
// API consolidates a principal's permissions across roles, providing an
// effective access view". Two roles granting the same path therefore combine
// rather than compete, which matters most for the restrictions:
//
//   - ROW filters union. Being in a role with no row filter means unrestricted
//     rows, because the union of "some rows" and "all rows" is all rows.
//     Intersecting instead would let adding a role take access away, which the
//     Permit-only model cannot express.
//   - COLUMN sets union for the same reason, and an unrestricted grant clears
//     any narrowing from another role.
func Effective(roles []Role, p Principal, input string) []AccessEntry {
	byPath := map[string]*AccessEntry{}
	// unrestrictedRows/Cols track whether ANY matching rule granted the path
	// without a restriction, which erases restrictions from the others.
	unrestrictedRows := map[string]bool{}
	unrestrictedCols := map[string]bool{}

	for _, role := range roles {
		if !isMember(role.Members, p) {
			continue
		}
		for _, rule := range role.DecisionRules {
			if rule.Effect != EffectPermit {
				continue
			}
			for _, raw := range rule.Paths {
				path, ok := normalisePath(raw, input)
				if !ok {
					continue
				}
				e := byPath[path]
				if e == nil {
					e = &AccessEntry{Path: path, Effect: EffectPermit}
					byPath[path] = e
				}
				for _, a := range rule.Actions {
					e.Access = addOnce(e.Access, a)
				}
				if rule.Rows == "" {
					unrestrictedRows[path] = true
				} else {
					e.Rows = unionRows(e.Rows, rule.Rows)
				}
				if len(rule.Columns) == 0 {
					unrestrictedCols[path] = true
				} else {
					for _, c := range rule.Columns {
						e.Columns = addOnce(e.Columns, c)
					}
				}
			}
		}
	}

	out := make([]AccessEntry, 0, len(byPath))
	for path, e := range byPath {
		if unrestrictedRows[path] {
			e.Rows = ""
		}
		if unrestrictedCols[path] {
			e.Columns = nil
		}
		sort.Strings(e.Access)
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// isMember: explicit Entra membership OR a virtual membership conferred by an
// item permission the principal holds.
func isMember(m Members, p Principal) bool {
	for _, id := range m.Entra {
		if strings.EqualFold(id, p.ObjectID) && p.ObjectID != "" {
			return true
		}
	}
	for _, need := range m.ItemAccess {
		for _, has := range p.ItemAccess {
			if strings.EqualFold(need, has) {
				return true
			}
		}
	}
	return false
}

// normalisePath maps a rule's scope onto the requested half of the item, and
// reports whether it belongs there at all.
//
// `*` is the API's "everything under this input path". A rule scoped to Files
// must not surface when the caller asked for Tables, or an engine would filter
// a table by a rule written for a folder.
func normalisePath(raw, input string) (string, bool) {
	p := strings.Trim(strings.TrimSpace(raw), "/")
	if p == "" || p == "*" {
		return input, true
	}
	if strings.EqualFold(p, input) {
		return input, true
	}
	if strings.HasPrefix(strings.ToLower(p), strings.ToLower(input)+"/") {
		return input + p[len(input):], true
	}
	// A bare scope like `dbo/Customers` is relative to the requested half.
	if !strings.EqualFold(firstSegment(p), InputTables) &&
		!strings.EqualFold(firstSegment(p), InputFiles) {
		return input + "/" + p, true
	}
	return "", false
}

func firstSegment(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}

// unionRows combines two row predicates the way the API expresses a union:
// as SQL, because the engine is what runs it.
func unionRows(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "", a == b:
		return a
	}
	return a + " UNION " + b
}

func addOnce(xs []string, v string) []string {
	for _, x := range xs {
		if strings.EqualFold(x, v) {
			return xs
		}
	}
	return append(xs, v)
}
