package semanticmodel

import (
	"fmt"
	"strings"
)

// ApplyObjectSecurity removes what a principal's roles hide, so that for them
// "it's as if the secured tables or columns don't exist".
//
// roles are the model roles that admit the principal (Model.RolesFor). The
// rules, from the product:
//
//   - A table or column with metadataPermission none is removed from the model
//     the query and every TMSCHEMA rowset read — referencing it is the same
//     error as referencing a name that was never there.
//   - "Dynamic calculations (measures, KPIs, DetailRows) are automatically
//     restricted if they reference a secured table or column", so a measure that
//     reads a hidden object, directly or through another measure, goes too.
//   - "Relationships that reference a secured column work provided the table the
//     column is in is not secured": a hidden key column leaves the model but
//     stays in the rows, so filters still join through it.
//   - "Row-level security and object-level security cannot be combined from
//     different roles … An error is generated at query time."
//
// Across several roles an object is hidden only when EVERY role hides it. The
// product documents no combination rule for OLS alone; this follows from
// metadataPermission defaulting to read, so a role that does not mention an
// object grants it, and roles are additive. It is an inference.
//
// With no admitting role nothing is hidden: ApplyRowSecurity has already left
// such a principal no rows.
func ApplyObjectSecurity(m *Model, d Data, roles []Role) (*Model, Data, error) {
	if len(roles) == 0 {
		return m, d, nil
	}
	if err := refuseMixedRowAndObjectSecurity(roles); err != nil {
		return nil, nil, err
	}

	hiddenTable := map[string]bool{} // lower-cased table name
	hiddenCol := map[string]bool{}   // lower-cased "table\x1fcolumn"
	for _, t := range m.Tables {
		tk := strings.ToLower(t.Name)
		if everyRole(roles, func(r Role) bool { return r.hidesTable(t.Name) }) {
			hiddenTable[tk] = true
		}
		for _, c := range t.Columns {
			if everyRole(roles, func(r Role) bool { return r.hidesTable(t.Name) || r.hidesColumn(t.Name, c.Name) }) {
				hiddenCol[tk+"\x1f"+strings.ToLower(c.Name)] = true
			}
		}
	}
	if len(hiddenTable) == 0 && len(hiddenCol) == 0 {
		return m, d, nil
	}

	hiddenMeasure := map[string]bool{} // lower-cased measure name
	for _, t := range m.Tables {
		if hiddenTable[strings.ToLower(t.Name)] {
			for _, ms := range t.Measures {
				hiddenMeasure[strings.ToLower(ms.Name)] = true
			}
		}
	}
	// A measure that reads a hidden measure is hidden, so repeat until stable.
	for changed := true; changed; {
		changed = false
		for _, t := range m.Tables {
			for _, ms := range t.Measures {
				mk := strings.ToLower(ms.Name)
				if !hiddenMeasure[mk] && measureReadsHidden(ms.Expression, hiddenTable, hiddenCol, hiddenMeasure) {
					hiddenMeasure[mk] = true
					changed = true
				}
			}
		}
	}

	out := &Model{Name: m.Name, CompatibilityLevel: m.CompatibilityLevel, Expressions: m.Expressions, Roles: m.Roles,
		DirectLakeBehavior: m.DirectLakeBehavior}
	keys := map[string]bool{} // hidden columns that relationships still join on
	for _, r := range m.Relationships {
		if hiddenTable[strings.ToLower(r.FromTable)] || hiddenTable[strings.ToLower(r.ToTable)] {
			continue
		}
		out.Relationships = append(out.Relationships, r)
		keys[strings.ToLower(r.FromTable)+"\x1f"+strings.ToLower(r.FromColumn)] = true
		keys[strings.ToLower(r.ToTable)+"\x1f"+strings.ToLower(r.ToColumn)] = true
	}
	outData := Data{}
	for _, t := range m.Tables {
		tk := strings.ToLower(t.Name)
		if hiddenTable[tk] {
			continue
		}
		nt := Table{Name: t.Name, DirectLake: t.DirectLake}
		var strip []string
		for _, c := range t.Columns {
			ck := tk + "\x1f" + strings.ToLower(c.Name)
			if !hiddenCol[ck] {
				nt.Columns = append(nt.Columns, c)
			} else if !keys[ck] {
				strip = append(strip, c.Name)
			}
		}
		for _, ms := range t.Measures {
			if !hiddenMeasure[strings.ToLower(ms.Name)] {
				nt.Measures = append(nt.Measures, ms)
			}
		}
		out.Tables = append(out.Tables, nt)
		if rows, ok := d[t.Name]; ok {
			outData[t.Name] = withoutColumns(rows, strip)
		}
	}
	return out, outData, nil
}

// CheckObjectSecurityChains refuses a model whose roles secure a table that
// sits between two others. "Table-level security cannot be set for a model if
// it breaks a relationship chain. An error is generated at design time." The
// model could not have been saved in the service, so it is refused for every
// caller, not only the restricted ones.
//
// A table is in a chain when a relationship carries a filter into it and another
// carries one out of it: it is the "many" side of one relationship and the "one"
// side of another, as B is between A and C in the product's own example.
func CheckObjectSecurityChains(m *Model) error {
	for _, r := range m.Roles {
		for _, tp := range r.TablePermissions {
			if !strings.EqualFold(tp.MetadataPermission, "none") {
				continue
			}
			var in, out string
			for _, rel := range m.Relationships {
				if strings.EqualFold(rel.FromTable, tp.Table) {
					in = rel.Name
				}
				if strings.EqualFold(rel.ToTable, tp.Table) {
					out = rel.Name
				}
			}
			if in != "" && out != "" {
				return fmt.Errorf("role %q secures table %q, which breaks the relationship chain through %q and %q; "+
					"table-level security cannot be set on a table between two others", r.Name, tp.Table, out, in)
			}
		}
	}
	return nil
}

// refuseMixedRowAndObjectSecurity is the product's query-time error for a
// principal whose row filters and hidden objects come from different roles.
func refuseMixedRowAndObjectSecurity(roles []Role) error {
	for i, filtering := range roles {
		if !filtering.filtersRows() {
			continue
		}
		for j, hiding := range roles {
			if i != j && hiding.hidesObjects() {
				return fmt.Errorf("row-level security from role %q and object-level security from role %q cannot be "+
					"combined for one user, because it could introduce unintended access to secured data",
					filtering.Name, hiding.Name)
			}
		}
	}
	return nil
}

func everyRole(roles []Role, f func(Role) bool) bool {
	for _, r := range roles {
		if !f(r) {
			return false
		}
	}
	return true
}

func (r Role) permission(table string) *TablePermission {
	for i := range r.TablePermissions {
		if strings.EqualFold(r.TablePermissions[i].Table, table) {
			return &r.TablePermissions[i]
		}
	}
	return nil
}

func (r Role) hidesTable(table string) bool {
	tp := r.permission(table)
	return tp != nil && strings.EqualFold(tp.MetadataPermission, "none")
}

func (r Role) hidesColumn(table, column string) bool {
	if tp := r.permission(table); tp != nil {
		for _, cp := range tp.ColumnPermissions {
			if strings.EqualFold(cp.Column, column) && strings.EqualFold(cp.MetadataPermission, "none") {
				return true
			}
		}
	}
	return false
}

func (r Role) filtersRows() bool {
	for _, tp := range r.TablePermissions {
		if strings.TrimSpace(tp.FilterExpression) != "" {
			return true
		}
	}
	return false
}

func (r Role) hidesObjects() bool {
	for _, tp := range r.TablePermissions {
		if r.hidesTable(tp.Table) {
			return true
		}
		for _, cp := range tp.ColumnPermissions {
			if r.hidesColumn(tp.Table, cp.Column) {
				return true
			}
		}
	}
	return false
}

// measureReadsHidden reports whether a measure expression names a hidden
// table, a hidden column, or a hidden measure. It reads tokens, not a parse
// tree, so it is deliberately over-inclusive: a bare [Name] matching any hidden
// column also counts, and an expression that does not lex is hidden rather than
// guessed at. Hiding a measure too many is a missing answer; too few is a leak.
func measureReadsHidden(expr string, hiddenTable, hiddenCol, hiddenMeasure map[string]bool) bool {
	toks, err := lex(expr)
	if err != nil {
		return true
	}
	for i, t := range toks {
		next := func(kind tkind, text string) bool {
			return i+1 < len(toks) && toks[i+1].kind == kind && (text == "" || toks[i+1].text == text)
		}
		switch t.kind {
		case tqTable, tIdent:
			tk := strings.ToLower(t.text)
			if next(tBracket, "") {
				if hiddenTable[tk] || hiddenCol[tk+"\x1f"+strings.ToLower(toks[i+1].text)] {
					return true
				}
			} else if !next(tPunct, "(") && hiddenTable[tk] {
				return true
			}
		case tBracket:
			if i > 0 && (toks[i-1].kind == tqTable || toks[i-1].kind == tIdent) {
				continue // read above, with its table
			}
			name := strings.ToLower(t.text)
			if hiddenMeasure[name] {
				return true
			}
			for k := range hiddenCol {
				if strings.HasSuffix(k, "\x1f"+name) {
					return true
				}
			}
		}
	}
	return false
}

func withoutColumns(rows []Row, cols []string) []Row {
	if len(cols) == 0 {
		return rows
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		nr := make(Row, len(r))
		for k, v := range r {
			nr[k] = v
		}
		for _, c := range cols {
			delete(nr, c)
		}
		out = append(out, nr)
	}
	return out
}
