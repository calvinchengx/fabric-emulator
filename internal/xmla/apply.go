package xmla

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ApplyWrite applies TOM's write batch to a model.bim and returns the new bytes.
//
// Works on the RAW JSON rather than on semanticmodel.Model, for two reasons.
// The stored definition carries more than this emulator parses (annotations,
// lineage tags, culture blocks), and round-tripping through the parsed model
// would silently delete every one of them. And the write has to be persisted in
// the same serialisation the model was published in, so a reconnect reads back
// what was written rather than a projection of it.
//
// The object keys are the ids this server handed out (see objID), so the
// inversion is exact: tables are model order, columns are table-then-column
// order across the whole model. Nothing here trusts a name the client sent.
//
// An object type this cannot represent is an ERROR, not a skip. TOM reports a
// successful SaveChanges when the server answers, so dropping half a batch
// loses the user's edit while telling them it landed.
func ApplyWrite(bim []byte, cmds []WriteCommand) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(bim, &doc); err != nil {
		return nil, fmt.Errorf("stored model.bim is not JSON: %w", err)
	}
	model, ok := doc["model"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stored model.bim has no model object")
	}
	tables, _ := model["tables"].([]any)

	for _, cmd := range cmds {
		for _, set := range cmd.Sets {
			var err error
			switch set.Object {
			case "Measures":
				err = applyMeasures(tables, set.Rows)
			case "Tables":
				err = applyToTables(tables, set.Rows)
			case "Columns":
				err = applyToColumns(tables, set.Rows)
			case "Annotations":
				err = applyAnnotations(model, set.Rows)
			default:
				err = fmt.Errorf("%s of %s is not implemented", cmd.Kind, set.Object)
			}
			if err != nil {
				return nil, err
			}
		}
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return out, nil
}

// tableAt resolves a TMSCHEMA_TABLES id to the stored table object.
func tableAt(tables []any, id string) (map[string]any, error) {
	n, err := strconv.Atoi(strings.TrimSpace(id))
	if err != nil {
		return nil, fmt.Errorf("table id %q is not a number", id)
	}
	i := n - idBase["TMSCHEMA_TABLES"] - 1
	if i < 0 || i >= len(tables) {
		return nil, fmt.Errorf("no table with id %s", id)
	}
	t, ok := tables[i].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("stored table %d is not an object", i)
	}
	return t, nil
}

// applyMeasures adds or updates measures on the table their TableID names.
func applyMeasures(tables []any, rows []map[string]string) error {
	for _, row := range rows {
		t, err := tableAt(tables, row["TableID"])
		if err != nil {
			return err
		}
		name := row["Name"]
		if name == "" {
			return fmt.Errorf("a measure was sent with no Name")
		}
		measures, _ := t["measures"].([]any)
		target := map[string]any{"name": name}
		found := false
		for _, m := range measures {
			if mm, ok := m.(map[string]any); ok && mm["name"] == name {
				target, found = mm, true
				break
			}
		}
		// Only the fields TOM actually sent: a row carries the CHANGED fields,
		// so writing the whole set would blank everything it left out.
		for wire, key := range map[string]string{
			"Expression": "expression", "Description": "description",
			"FormatString": "formatString", "DisplayFolder": "displayFolder",
			"DataCategory": "dataCategory", "LineageTag": "lineageTag",
		} {
			if v, ok := row[wire]; ok {
				target[key] = v
			}
		}
		if v, ok := row["IsHidden"]; ok {
			target["isHidden"] = strings.EqualFold(v, "true")
		}
		if !found {
			t["measures"] = append(measures, target)
		}
	}
	return nil
}

// applyToTables updates stored tables by id. Only the fields TOM sends for an
// Alter of a table are honoured; anything else is refused by name.
func applyToTables(tables []any, rows []map[string]string) error {
	for _, row := range rows {
		t, err := tableAt(tables, row["ID"])
		if err != nil {
			return err
		}
		if err := setKnown(t, row, "ID", map[string]string{
			"LineageTag": "lineageTag", "SourceLineageTag": "sourceLineageTag",
			"Description": "description", "DataCategory": "dataCategory",
		}, "table"); err != nil {
			return err
		}
	}
	return nil
}

// applyToColumns updates stored columns by id. Column ids are assigned across
// the WHOLE model in table-then-column order, so the walk mirrors dmvRows.
func applyToColumns(tables []any, rows []map[string]string) error {
	flat := []map[string]any{}
	for _, t := range tables {
		tt, ok := t.(map[string]any)
		if !ok {
			continue
		}
		cols, _ := tt["columns"].([]any)
		for _, c := range cols {
			if cc, ok := c.(map[string]any); ok {
				flat = append(flat, cc)
			}
		}
	}
	for _, row := range rows {
		n, err := strconv.Atoi(strings.TrimSpace(row["ID"]))
		if err != nil {
			return fmt.Errorf("column id %q is not a number", row["ID"])
		}
		i := n - idBase["TMSCHEMA_COLUMNS"] - 1
		if i < 0 || i >= len(flat) {
			return fmt.Errorf("no column with id %s", row["ID"])
		}
		if err := setKnown(flat[i], row, "ID", map[string]string{
			"LineageTag": "lineageTag", "SourceLineageTag": "sourceLineageTag",
			"Description": "description", "DataCategory": "dataCategory",
			"FormatString": "formatString", "DisplayFolder": "displayFolder",
		}, "column"); err != nil {
			return err
		}
	}
	return nil
}

// applyAnnotations stores model-level annotations, replacing one of the same
// name rather than accumulating duplicates.
func applyAnnotations(model map[string]any, rows []map[string]string) error {
	anns, _ := model["annotations"].([]any)
	for _, row := range rows {
		name := row["Name"]
		if name == "" {
			return fmt.Errorf("an annotation was sent with no Name")
		}
		replaced := false
		for _, a := range anns {
			if aa, ok := a.(map[string]any); ok && aa["name"] == name {
				aa["value"] = row["Value"]
				replaced = true
				break
			}
		}
		if !replaced {
			anns = append(anns, map[string]any{"name": name, "value": row["Value"]})
		}
	}
	model["annotations"] = anns
	return nil
}

// setKnown copies the mapped wire fields onto a stored object and REFUSES any
// other field. Silently ignoring an unmapped field is how a write reports
// success and loses data, which is the whole hazard of this path.
func setKnown(obj map[string]any, row map[string]string, key string,
	mapping map[string]string, what string) error {
	for wire, v := range row {
		if wire == key {
			continue
		}
		k, ok := mapping[wire]
		if !ok {
			return fmt.Errorf("%s field %q is not implemented", what, wire)
		}
		obj[k] = v
	}
	return nil
}
