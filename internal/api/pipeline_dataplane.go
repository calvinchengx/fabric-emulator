package api

// Real, hermetic data-plane leaf activities for the pipeline interpreter:
// Lookup (read rows from a CSV/JSON file in OneLake) and GetMetadata (stat a
// OneLake path). Both compute against real bytes in the storage layer with no
// external engine and no CGO — pure Go, deterministic, offline. Parquet and
// SQL-endpoint sources are a deliberate follow-up (Parquet needs a reader
// dependency; SQL is the warehouse engine's job).

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// readLoc resolves the first present location field (e.g. "source", "dataset")
// into a concrete OneLake location.
func (e *pipelineExecutor) readLoc(tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error), keys ...string) (oneLakeLoc, error) {
	for _, k := range keys {
		if raw, ok := tp[k]; ok {
			return e.resolveLoc(k, raw, resolve)
		}
	}
	return oneLakeLoc{}, fmt.Errorf("no location (%s)", strings.Join(keys, "/"))
}

// lookupActivity reads rows from a CSV or JSON file in OneLake. Format is taken
// from an explicit `format` hint or inferred from the file extension. Honors
// `firstRowOnly` (default true), matching the ADF Lookup output shape.
func (e *pipelineExecutor) lookupActivity(act pipeline.Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (map[string]any, error) {
	loc, err := e.readLoc(tp, resolve, "source", "dataset")
	if err != nil {
		return nil, fmt.Errorf("lookup %q: %w", act.Name, err)
	}
	p, err := e.a.Store.GetOneLakePath(loc.itemID, loc.path)
	if err != nil {
		return nil, fmt.Errorf("lookup %q: source %s not found", act.Name, loc.path)
	}
	if p.IsDir {
		return nil, fmt.Errorf("lookup %q: source %s is a directory", act.Name, loc.path)
	}

	rows, err := parseRows(p.Content, lookupFormat(tp, loc.path))
	if err != nil {
		return nil, fmt.Errorf("lookup %q: %v", act.Name, err)
	}

	firstRowOnly := true
	if raw, ok := tp["firstRowOnly"]; ok {
		_ = json.Unmarshal(raw, &firstRowOnly)
	}
	if firstRowOnly {
		var first any = map[string]any{}
		if len(rows) > 0 {
			first = rows[0]
		}
		return map[string]any{"firstRow": first, "count": len(rows)}, nil
	}
	return map[string]any{"count": len(rows), "value": rows}, nil
}

// lookupFormat picks csv vs json from an explicit hint or the path extension.
func lookupFormat(tp map[string]json.RawMessage, path string) string {
	if raw, ok := tp["format"]; ok {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			var obj struct{ Type string }
			_ = json.Unmarshal(raw, &obj)
			s = obj.Type
		}
		s = strings.ToLower(s)
		switch {
		case strings.Contains(s, "json"):
			return "json"
		case strings.Contains(s, "csv"), strings.Contains(s, "delim"):
			return "csv"
		}
	}
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		return "json"
	}
	return "csv"
}

// parseRows turns file bytes into row objects. CSV: the first record is the
// header; each later record becomes a header-keyed object (string values).
// JSON: an array yields its elements; a single object yields one row.
func parseRows(content []byte, format string) ([]any, error) {
	if format == "json" {
		var v any
		if err := json.Unmarshal(content, &v); err != nil {
			return nil, fmt.Errorf("invalid JSON: %v", err)
		}
		switch t := v.(type) {
		case []any:
			return t, nil
		default:
			return []any{v}, nil
		}
	}
	cr := csv.NewReader(strings.NewReader(string(content)))
	cr.FieldsPerRecord = -1 // tolerate ragged rows; header keys the present cells
	recs, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("invalid CSV: %v", err)
	}
	if len(recs) == 0 {
		return nil, nil
	}
	header := recs[0]
	out := make([]any, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		row := make(map[string]any, len(header))
		for i, h := range header {
			if i < len(rec) {
				row[h] = rec[i]
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// getMetadataActivity stats a OneLake path. Returns only the requested
// `fieldList` fields (or a sensible default set), including childItems for a
// directory. A missing path is reported as exists:false, not an error.
func (e *pipelineExecutor) getMetadataActivity(act pipeline.Activity, tp map[string]json.RawMessage, resolve func(json.RawMessage) (any, error)) (map[string]any, error) {
	loc, err := e.readLoc(tp, resolve, "dataset", "source", "location")
	if err != nil {
		return nil, fmt.Errorf("getMetadata %q: %w", act.Name, err)
	}
	want := map[string]bool{}
	if raw, ok := tp["fieldList"]; ok {
		var fields []string
		_ = json.Unmarshal(raw, &fields)
		for _, f := range fields {
			want[f] = true
		}
	}
	wants := func(f string) bool { return len(want) == 0 || want[f] }

	p, err := e.a.Store.GetOneLakePath(loc.itemID, loc.path)
	if err != nil {
		// Not found: the honest answer to a metadata probe is exists:false.
		return map[string]any{"exists": false}, nil
	}

	out := map[string]any{}
	if wants("exists") {
		out["exists"] = true
	}
	if wants("itemName") {
		out["itemName"] = baseName(loc.path)
	}
	if wants("itemType") {
		out["itemType"] = itemType(p.IsDir)
	}
	if wants("lastModified") && p.ModifiedAt > 0 {
		out["lastModified"] = time.Unix(p.ModifiedAt, 0).UTC().Format(time.RFC3339)
	}
	if !p.IsDir && wants("size") {
		out["size"] = len(p.Content)
	}
	if p.IsDir && wants("childItems") {
		children, err := e.a.Store.ListOneLakePaths(loc.itemID, loc.path, false)
		if err != nil {
			return nil, fmt.Errorf("getMetadata %q: %v", act.Name, err)
		}
		items := []map[string]any{}
		for _, c := range children {
			if c.RelPath == loc.path {
				continue
			}
			items = append(items, map[string]any{"name": baseName(c.RelPath), "type": itemType(c.IsDir)})
		}
		out["childItems"] = items
	}
	return out, nil
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func itemType(isDir bool) string {
	if isDir {
		return "Folder"
	}
	return "File"
}
