// Package semanticmodel parses a Fabric semantic model's TMSL definition
// (`model.bim`) into a tabular model — tables, columns, measures (as DAX
// expression strings), and relationships — and holds its table data. It is the
// model layer the DAX evaluator (evaluator.go) runs over.
//
// Pure Go, no engine: parsing + a plain in-memory model. The measure
// expressions are kept verbatim; interpreting them is the evaluator's job.
package semanticmodel

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Column is a table column with its TMSL data type (int64/string/double/…).
type Column struct {
	Name         string
	DataType     string
	SourceColumn string
}

// DirectLakePartition maps a model table to a Delta entity through a shared
// TMSL expression. It is nil for import/calculated tables.
type DirectLakePartition struct {
	EntityName       string
	SchemaName       string
	ExpressionSource string
}

// Measure is a model measure: a name and its DAX expression.
type Measure struct {
	Name       string
	Expression string
}

// Relationship is a single-column relationship between two tables (Fabric
// relationships are single-column; the fixture uses StoreId and MonthKey).
type Relationship struct {
	Name                                     string
	FromTable, FromColumn, ToTable, ToColumn string
}

// Table is a model table with its columns and measures.
type Table struct {
	Name       string
	Columns    []Column
	Measures   []Measure
	DirectLake *DirectLakePartition
}

// Model is the parsed tabular model.
type Model struct {
	Name               string
	CompatibilityLevel int
	Tables             []Table
	Relationships      []Relationship
	Expressions        map[string]string
	// Roles are the model's security roles: row-level filters and object-level
	// visibility, for the principals they name. A model with ANY role is
	// secured — a principal they apply to who is in none of them sees nothing.
	Roles []Role
}

// Role is one model role. Its rules apply only to principals without Write
// permission on the model: "RLS only restricts data access for users with
// Viewer permissions. It doesn't apply to workspace Admin, Member, or
// Contributor roles."
type Role struct {
	Name             string
	ModelPermission  string
	Members          []RoleMember
	TablePermissions []TablePermission
}

// RoleMember names a principal in a role: by UPN (Name) and, where given, by
// object id (ID). Type is TOM's RoleMemberType — auto, user or group.
type RoleMember struct {
	Name             string
	ID               string
	IdentityProvider string
	Type             string
}

// TablePermission narrows one table for a role: a DAX row filter, the table's
// own visibility, and its columns' visibility.
type TablePermission struct {
	Table              string
	FilterExpression   string
	MetadataPermission string
	ColumnPermissions  []ColumnPermission
}

// ColumnPermission is one column's visibility for a role.
type ColumnPermission struct {
	Column             string
	MetadataPermission string
}

// tmsl mirrors the model.bim shape we consume (unknown keys, like the "//"
// comment or annotations, are ignored by encoding/json).
type tmsl struct {
	Name               string `json:"name"`
	CompatibilityLevel int    `json:"compatibilityLevel"`
	Model              struct {
		Expressions []struct {
			Name       string          `json:"name"`
			Expression json.RawMessage `json:"expression"`
		} `json:"expressions"`
		Tables []struct {
			Name    string `json:"name"`
			Columns []struct {
				Name         string `json:"name"`
				DataType     string `json:"dataType"`
				SourceColumn string `json:"sourceColumn"`
			} `json:"columns"`
			Measures []struct {
				Name       string `json:"name"`
				Expression string `json:"expression"`
			} `json:"measures"`
			Partitions []struct {
				Mode   string `json:"mode"`
				Source struct {
					Type             string `json:"type"`
					EntityName       string `json:"entityName"`
					SchemaName       string `json:"schemaName"`
					ExpressionSource string `json:"expressionSource"`
				} `json:"source"`
			} `json:"partitions"`
		} `json:"tables"`
		Relationships []struct {
			Name       string `json:"name"`
			FromTable  string `json:"fromTable"`
			FromColumn string `json:"fromColumn"`
			ToTable    string `json:"toTable"`
			ToColumn   string `json:"toColumn"`
		} `json:"relationships"`
		// The Roles object (TMSL reference), with OLS's metadataPermission and
		// columnPermissions from compatibility level 1400.
		Roles []struct {
			Name            string `json:"name"`
			ModelPermission string `json:"modelPermission"`
			Members         []struct {
				MemberName       string `json:"memberName"`
				MemberID         string `json:"memberId"`
				IdentityProvider string `json:"identityProvider"`
				MemberType       string `json:"memberType"`
			} `json:"members"`
			TablePermissions []struct {
				Name               string          `json:"name"`
				FilterExpression   json.RawMessage `json:"filterExpression"`
				MetadataPermission string          `json:"metadataPermission"`
				ColumnPermissions  []struct {
					Name               string `json:"name"`
					MetadataPermission string `json:"metadataPermission"`
				} `json:"columnPermissions"`
			} `json:"tablePermissions"`
		} `json:"roles"`
	} `json:"model"`
}

// ParseTMSL parses a model.bim payload into a Model.
func ParseTMSL(b []byte) (*Model, error) {
	var t tmsl
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("invalid TMSL model: %w", err)
	}
	if len(t.Model.Tables) == 0 {
		return nil, fmt.Errorf("model has no tables")
	}
	m := &Model{Name: t.Name, CompatibilityLevel: t.CompatibilityLevel, Expressions: map[string]string{}}
	for _, expression := range t.Model.Expressions {
		text, err := expressionText(expression.Expression)
		if err != nil {
			return nil, fmt.Errorf("expression %q: %w", expression.Name, err)
		}
		m.Expressions[expression.Name] = text
	}
	for _, tb := range t.Model.Tables {
		table := Table{Name: tb.Name}
		for _, c := range tb.Columns {
			table.Columns = append(table.Columns, Column{Name: c.Name, DataType: c.DataType, SourceColumn: c.SourceColumn})
		}
		for _, ms := range tb.Measures {
			table.Measures = append(table.Measures, Measure{Name: ms.Name, Expression: ms.Expression})
		}
		for _, partition := range tb.Partitions {
			if strings.EqualFold(partition.Mode, "directLake") {
				if t.CompatibilityLevel < 1604 {
					return nil, fmt.Errorf("Direct Lake table %q requires compatibilityLevel 1604 or higher", tb.Name)
				}
				if table.DirectLake != nil {
					return nil, fmt.Errorf("table %q has multiple Direct Lake partitions", tb.Name)
				}
				if !strings.EqualFold(partition.Source.Type, "entity") || partition.Source.EntityName == "" || partition.Source.ExpressionSource == "" {
					return nil, fmt.Errorf("Direct Lake table %q requires an entity source and expressionSource", tb.Name)
				}
				table.DirectLake = &DirectLakePartition{
					EntityName: partition.Source.EntityName, SchemaName: partition.Source.SchemaName,
					ExpressionSource: partition.Source.ExpressionSource,
				}
			}
		}
		m.Tables = append(m.Tables, table)
	}
	for _, r := range t.Model.Relationships {
		m.Relationships = append(m.Relationships, Relationship{
			Name: r.Name, FromTable: r.FromTable, FromColumn: r.FromColumn,
			ToTable: r.ToTable, ToColumn: r.ToColumn,
		})
	}
	for _, r := range t.Model.Roles {
		role := Role{Name: r.Name, ModelPermission: r.ModelPermission}
		for _, mb := range r.Members {
			role.Members = append(role.Members, RoleMember{Name: mb.MemberName, ID: mb.MemberID,
				IdentityProvider: mb.IdentityProvider, Type: mb.MemberType})
		}
		for _, tp := range r.TablePermissions {
			perm := TablePermission{Table: tp.Name, MetadataPermission: tp.MetadataPermission}
			if len(tp.FilterExpression) > 0 && string(tp.FilterExpression) != "null" {
				text, err := expressionText(tp.FilterExpression)
				if err != nil {
					return nil, fmt.Errorf("role %q table permission %q filterExpression: %w", r.Name, tp.Name, err)
				}
				perm.FilterExpression = text
			}
			for _, cp := range tp.ColumnPermissions {
				perm.ColumnPermissions = append(perm.ColumnPermissions,
					ColumnPermission{Column: cp.Name, MetadataPermission: cp.MetadataPermission})
			}
			role.TablePermissions = append(role.TablePermissions, perm)
		}
		m.Roles = append(m.Roles, role)
	}
	return m, nil
}

func expressionText(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var lines []string
	if err := json.Unmarshal(raw, &lines); err != nil {
		return "", fmt.Errorf("must be a string or string array")
	}
	return strings.Join(lines, "\n"), nil
}

// Table returns the named table (nil if absent). Table names match TMSL exactly.
func (m *Model) Table(name string) *Table {
	name = strings.Trim(name, "'")
	for i := range m.Tables {
		if m.Tables[i].Name == name {
			return &m.Tables[i]
		}
	}
	return nil
}

// Measure resolves a measure by name across the whole model (measure names are
// model-unique, so `[TotalUnits]` need not name its table).
func (m *Model) Measure(name string) *Measure {
	for ti := range m.Tables {
		for mi := range m.Tables[ti].Measures {
			if m.Tables[ti].Measures[mi].Name == name {
				return &m.Tables[ti].Measures[mi]
			}
		}
	}
	return nil
}

// Column returns the named column of a table (nil if absent).
func (t *Table) Column(name string) *Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

// RelationshipBetween returns a relationship directly connecting two tables in
// either direction (nil if none).
func (m *Model) RelationshipBetween(a, b string) *Relationship {
	for i := range m.Relationships {
		r := &m.Relationships[i]
		if (r.FromTable == a && r.ToTable == b) || (r.FromTable == b && r.ToTable == a) {
			return r
		}
	}
	return nil
}
