package semanticmodel

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// CreateOrReplaceTMSL builds a TMSL command that publishes this model into
// msmdsrv (docs/52 Phase 2). Import rows from data.json become DATATABLE
// calculated partitions — VertiPaq will not read our data.json, and pbix-mcp
// is verification tooling, not a runtime dependency (docs/33).
//
// Direct Lake tables are refused: Desktop / SSAS cannot host Fabric's
// OneLake entity source, and silently dropping them would publish a
// different model than the item.
func CreateOrReplaceTMSL(m *Model, data Data) ([]byte, error) {
	if m == nil || len(m.Tables) == 0 {
		return nil, fmt.Errorf("model has no tables")
	}
	name := strings.TrimSpace(m.Name)
	if name == "" {
		return nil, fmt.Errorf("model has no name")
	}
	level := m.CompatibilityLevel
	if level == 0 {
		level = 1550
	}
	tables := make([]map[string]any, 0, len(m.Tables))
	for _, t := range m.Tables {
		if t.DirectLake != nil {
			return nil, fmt.Errorf("table %q is Direct Lake; msmdsrv cannot host Fabric OneLake partitions", t.Name)
		}
		expr, err := dataTableExpr(t, data.Rows(t.Name))
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", t.Name, err)
		}
		cols := make([]map[string]any, 0, len(t.Columns))
		for _, c := range t.Columns {
			col := map[string]any{"name": c.Name, "dataType": c.DataType}
			if c.DataType == "" {
				col["dataType"] = "string"
			}
			cols = append(cols, col)
		}
		table := map[string]any{
			"name":    t.Name,
			"columns": cols,
			"partitions": []map[string]any{{
				"name": t.Name,
				"mode": "import",
				"source": map[string]any{
					"type":       "calculated",
					"expression": expr,
				},
			}},
		}
		if len(t.Measures) > 0 {
			ms := make([]map[string]any, 0, len(t.Measures))
			for _, measure := range t.Measures {
				ms = append(ms, map[string]any{
					"name":       measure.Name,
					"expression": measure.Expression,
				})
			}
			table["measures"] = ms
		}
		tables = append(tables, table)
	}
	rels := make([]map[string]any, 0, len(m.Relationships))
	for _, r := range m.Relationships {
		rels = append(rels, map[string]any{
			"name":       r.Name,
			"fromTable":  r.FromTable,
			"fromColumn": r.FromColumn,
			"toTable":    r.ToTable,
			"toColumn":   r.ToColumn,
		})
	}
	payload := map[string]any{
		"createOrReplace": map[string]any{
			"object": map[string]any{"database": name},
			"database": map[string]any{
				"name":               name,
				"compatibilityLevel": level,
				"model": map[string]any{
					"culture":       "en-US",
					"tables":        tables,
					"relationships": rels,
				},
			},
		},
	}
	return json.Marshal(payload)
}

func dataTableExpr(t Table, rows []Row) (string, error) {
	if len(t.Columns) == 0 {
		return "", fmt.Errorf("has no columns")
	}
	var b strings.Builder
	b.WriteString("DATATABLE(\n")
	for i, c := range t.Columns {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "    %s, %s", daxString(c.Name), daxDataType(c.DataType))
	}
	b.WriteString(",\n    {\n")
	for i, row := range rows {
		if i > 0 {
			b.WriteString(",\n")
		}
		b.WriteString("        {")
		for j, c := range t.Columns {
			if j > 0 {
				b.WriteString(", ")
			}
			lit, err := daxLiteral(row[c.Name], c.DataType)
			if err != nil {
				return "", fmt.Errorf("row %d column %s: %w", i, c.Name, err)
			}
			b.WriteString(lit)
		}
		b.WriteString("}")
	}
	b.WriteString("\n    }\n)")
	return b.String(), nil
}

func daxDataType(tmsl string) string {
	switch strings.ToLower(tmsl) {
	case "int64", "int", "integer", "int32", "int16":
		return "INTEGER"
	case "double", "decimal", "currency":
		return "DOUBLE"
	case "boolean", "bool":
		return "BOOLEAN"
	case "datetime", "date", "time":
		return "DATETIME"
	default:
		return "STRING"
	}
}

func daxString(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func daxLiteral(v any, dataType string) (string, error) {
	if v == nil {
		return "BLANK()", nil
	}
	switch daxDataType(dataType) {
	case "INTEGER":
		n, err := asInt(v)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(n, 10), nil
	case "DOUBLE":
		f, err := asFloat(v)
		if err != nil {
			return "", err
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	case "BOOLEAN":
		b, err := asBool(v)
		if err != nil {
			return "", err
		}
		if b {
			return "TRUE()", nil
		}
		return "FALSE()", nil
	case "DATETIME":
		return asDateTime(v)
	default:
		return daxString(fmt.Sprint(v)), nil
	}
}

func asInt(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case int64:
		return n, nil
	case float64:
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("not an integer: %v", n)
		}
		return int64(n), nil
	case json.Number:
		return n.Int64()
	case string:
		return strconv.ParseInt(n, 10, 64)
	default:
		return 0, fmt.Errorf("not an integer: %T", v)
	}
}

func asFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case json.Number:
		return n.Float64()
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return 0, fmt.Errorf("not a number: %T", v)
	}
}

func asBool(v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		return strconv.ParseBool(b)
	default:
		return false, fmt.Errorf("not a boolean: %T", v)
	}
}

func asDateTime(v any) (string, error) {
	var parsed time.Time
	switch t := v.(type) {
	case time.Time:
		parsed = t
	case string:
		var err error
		parsed, err = time.Parse(time.RFC3339, t)
		if err != nil {
			parsed, err = time.Parse("2006-01-02", t)
		}
		if err != nil {
			return "", fmt.Errorf("not a datetime: %q", t)
		}
	default:
		return "", fmt.Errorf("not a datetime: %T", v)
	}
	return fmt.Sprintf("DATE(%d,%d,%d)", parsed.Year(), int(parsed.Month()), parsed.Day()), nil
}
