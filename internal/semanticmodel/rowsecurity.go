package semanticmodel

import (
	"fmt"
	"strings"
)

// ApplyRowSecurity narrows data to what a principal's roles let it see.
//
// roles are the model roles that admit the principal (Model.RolesFor). The
// rules, from the product:
//
//   - NO ROLE, NO ROWS. "Users who aren't assigned to any RLS role typically see
//     no data." Every table comes back empty rather than the query refused, which
//     is what that sentence describes.
//   - ROLES ARE ADDITIVE. A table is narrowed only if EVERY admitting role
//     filters it; one role that leaves the table alone leaves it whole, and a row
//     survives if ANY role's filter admits it.
//   - FILTERS TRAVEL RELATIONSHIPS, ONE DIRECTION BY DEFAULT. A narrowed table
//     on a relationship's "one" side narrows the "many" side to rows whose key
//     survives — so a filter on a dimension reaches the facts — and that repeats
//     along chains until nothing changes. Only active relationships carry it.
//
// What is refused rather than guessed, since each would otherwise serve rows a
// filter should have removed: a relationship that filters security in both
// directions, and a many-to-many relationship.
func ApplyRowSecurity(m *Model, d Data, roles []Role, env SecurityEnv) (Data, error) {
	out := Data{}
	if len(roles) == 0 {
		for _, t := range m.Tables {
			out[t.Name] = []Row{}
		}
		return out, nil
	}
	for _, r := range m.Relationships {
		if r.Inactive {
			continue
		}
		if strings.EqualFold(r.SecurityFilteringBehavior, "bothDirections") {
			return nil, fmt.Errorf("relationship %q applies security filters in both directions, which this "+
				"emulator does not model; ignoring it would serve rows the filter removes", r.Name)
		}
		if strings.EqualFold(r.FromCardinality, "many") && strings.EqualFold(r.ToCardinality, "many") {
			return nil, fmt.Errorf("relationship %q is many-to-many, which this emulator does not propagate "+
				"security filters across", r.Name)
		}
	}

	narrowed := map[string]bool{}
	for _, t := range m.Tables {
		rows := d[t.Name]
		filters, err := tableFilters(m, t.Name, roles)
		if err != nil {
			return nil, err
		}
		if filters == nil {
			out[t.Name] = rows
			continue
		}
		kept := []Row{}
		for _, row := range rows {
			for _, f := range filters {
				ok, err := f.Admits(row, env)
				if err != nil {
					return nil, err
				}
				if ok {
					kept = append(kept, row)
					break
				}
			}
		}
		out[t.Name] = kept
		narrowed[t.Name] = true
	}

	// Propagate "one" → "many" until a pass narrows nothing new. Each pass can
	// only remove rows or newly mark a table narrowed, so it terminates.
	for changed := true; changed; {
		changed = false
		for _, r := range m.Relationships {
			one, many := m.tableFold(r.ToTable), m.tableFold(r.FromTable)
			if r.Inactive || one == nil || many == nil || !narrowed[one.Name] {
				continue
			}
			keys := map[string]bool{}
			for _, row := range out[one.Name] {
				keys[relKey(row[r.ToColumn])] = true
			}
			kept := []Row{}
			for _, row := range out[many.Name] {
				if keys[relKey(row[r.FromColumn])] {
					kept = append(kept, row)
				}
			}
			if len(kept) != len(out[many.Name]) || !narrowed[many.Name] {
				out[many.Name] = kept
				narrowed[many.Name] = true
				changed = true
			}
		}
	}
	return out, nil
}

// tableFilters is the compiled filters for a table across the principal's
// roles, or nil when any one role leaves the table unfiltered.
func tableFilters(m *Model, table string, roles []Role) ([]*RowFilter, error) {
	var filters []*RowFilter
	for _, role := range roles {
		var expr string
		for _, tp := range role.TablePermissions {
			if strings.EqualFold(tp.Table, table) {
				expr = strings.TrimSpace(tp.FilterExpression)
			}
		}
		if expr == "" {
			return nil, nil
		}
		f, err := CompileRowFilter(m, table, expr)
		if err != nil {
			return nil, fmt.Errorf("role %q: %w", role.Name, err)
		}
		filters = append(filters, f)
	}
	return filters, nil
}

// relKey makes relationship keys comparable the way the engine joins them:
// numbers by value whatever their Go type, text case-insensitively.
func relKey(v any) string {
	if f, ok := numeric(v); ok {
		return fmt.Sprintf("n:%v", f)
	}
	if s, ok := v.(string); ok {
		return "s:" + strings.ToLower(s)
	}
	return fmt.Sprintf("v:%v", v)
}
