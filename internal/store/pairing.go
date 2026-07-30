package store

// Item pairing for deployment pipelines (docs/23, "Pairing is persistent
// state, not a name match").
//
// The matching itself is a PURE function over two item sets, deliberately
// separated from the database. Two of the documented inputs are structurally
// unreachable in this emulator today:
//
//   - Duplicate (displayName, type) within one workspace: forbidden by the
//     ux_items_ws_name_type UNIQUE index (see db.go).
//   - Folder location, the documented tie-breaker for those duplicates:
//     items carry no folder membership in this model.
//
// So the ambiguous branch cannot fire through the store as it stands. It is
// still implemented and unit-tested against hand-built item sets, because the
// alternative — assuming uniqueness and silently mis-pairing if either
// premise ever changes — is exactly the failure this design is meant to
// prevent. Ambiguity fails the assignment loudly instead.

import (
	"sort"
	"strings"
)

// matchKey is the documented pairing criterion: item name + item type,
// compared case-insensitively (the uniqueness index is COLLATE NOCASE, so
// the comparison here must agree with it).
func matchKey(it *Item) string {
	return strings.ToLower(it.DisplayName) + "\x00" + strings.ToLower(it.Type)
}

// PairItems matches items between two stages by (displayName, type).
//
// A key present exactly once on each side pairs. A key appearing more than
// once on either side is ambiguous: the caller must refuse rather than guess,
// since picking one arbitrarily would silently bind a promotion path to the
// wrong item. Pairs come back in a deterministic order.
func PairItems(earlier, later []*Item) (pairs [][2]*Item, ambiguous []string) {
	index := func(items []*Item) map[string][]*Item {
		m := make(map[string][]*Item, len(items))
		for _, it := range items {
			m[matchKey(it)] = append(m[matchKey(it)], it)
		}
		return m
	}
	e, l := index(earlier), index(later)

	keys := make([]string, 0, len(e))
	for k := range e {
		if _, both := l[k]; both {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		if len(e[k]) == 1 && len(l[k]) == 1 {
			pairs = append(pairs, [2]*Item{e[k][0], l[k][0]})
			continue
		}
		// Report the human-readable name/type rather than the internal key.
		it := e[k][0]
		ambiguous = append(ambiguous, it.DisplayName+" ("+it.Type+")")
	}
	return pairs, ambiguous
}
