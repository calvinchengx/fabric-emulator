package warehouse

// Delta writing for callers outside the SQL mirror: the pipeline Copy activity
// needs to land rows into Tables/<name> as a real Delta table, appending to or
// overwriting whatever is already there. writeDeltaSnapshot only ever wrote
// commit zero, so a second write silently collided with the first.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Write modes for WriteDeltaTable.
const (
	WriteAppend    = "append"
	WriteOverwrite = "overwrite"
)

// WriteDeltaTable lands tbl in the item's Tables/<name> as a Delta commit.
//
// Append adds a data file and leaves earlier ones active. Overwrite adds the
// new file and marks every previously-active file removed in the same commit,
// so a reader replaying the log sees only the new rows — the old Parquet stays
// on disk (unreferenced), which is what Delta itself does before a VACUUM.
//
// The table is created on first write, so the caller need not distinguish
// create from append.
func WriteDeltaTable(st *store.Store, wsID, itemID, name, mode string, tbl *Table) error {
	return WriteDeltaTableAs(store.Attribution{}, st, wsID, itemID, name, mode, tbl)
}

// WriteDeltaTableAs is WriteDeltaTable for a caller that knows which unit of
// work is writing — a Copy activity, say — so the resulting file and table
// events can name it.
func WriteDeltaTableAs(attr store.Attribution, st *store.Store, wsID, itemID, name, mode string, tbl *Table) error {
	if tbl == nil || len(tbl.Columns) == 0 {
		return fmt.Errorf("delta write %q: no columns", name)
	}
	if mode == "" {
		mode = WriteOverwrite
	}
	if mode != WriteAppend && mode != WriteOverwrite {
		return fmt.Errorf("delta write %q: unknown mode %q", name, mode)
	}

	kinds := inferKinds(tbl)
	pq, err := encodeParquet(tbl, kinds)
	if err != nil {
		return err
	}

	root := path.Join("Tables", name)
	version, err := nextCommitVersion(st, itemID, root)
	if err != nil {
		return err
	}
	var removes []string
	if mode == WriteOverwrite && version > 0 {
		if removes, _, err = activeFiles(st, itemID, root); err != nil {
			return err
		}
	}

	dataFile := fmt.Sprintf("part-%d.parquet", version)
	if err := st.CreateOneLakePathAs(attr, &store.OneLakePath{
		WorkspaceID: wsID, ItemID: itemID,
		RelPath: path.Join(root, dataFile), Content: pq,
	}, false); err != nil {
		return err
	}
	return st.CreateOneLakePathAs(attr, &store.OneLakePath{
		WorkspaceID: wsID, ItemID: itemID,
		RelPath: path.Join(root, "_delta_log", commitFileName(version)),
		Content: commitJSON(tbl.Columns, kinds, dataFile, len(pq), len(tbl.Rows), removes, version, time.Now().UnixMilli()),
	}, false)
}

// inferKinds types each column from its first non-null value, as the SQL mirror
// does; an all-null column falls back to string.
func inferKinds(tbl *Table) []colKind {
	kinds := make([]colKind, len(tbl.Columns))
	for i := range tbl.Columns {
		kinds[i] = kindString
		for _, row := range tbl.Rows {
			if i < len(row) && row[i] != nil {
				kinds[i] = kindOf(row[i])
				break
			}
		}
	}
	return kinds
}

// nextCommitVersion is one past the highest existing commit in _delta_log, or 0
// for a table that does not exist yet.
func nextCommitVersion(st *store.Store, itemID, root string) (int, error) {
	entries, err := st.ListOneLakePaths(itemID, path.Join(root, "_delta_log"), false)
	if err != nil {
		return 0, err
	}
	next := 0
	for _, e := range entries {
		base := path.Base(e.RelPath)
		if !strings.HasSuffix(base, ".json") {
			continue
		}
		var v int
		if _, err := fmt.Sscanf(strings.TrimSuffix(base, ".json"), "%020d", &v); err != nil {
			continue
		}
		if v >= next {
			next = v + 1
		}
	}
	return next, nil
}

func commitFileName(version int) string { return fmt.Sprintf("%020d.json", version) }

// commitJSON builds one _delta_log commit. protocol/metaData are written on the
// first commit and on an overwrite (which may change the schema); an append
// carries only its add action, as Delta writers do.
func commitJSON(cols []string, kinds []colKind, dataFile string, size, rows int, removes []string, version int, nowMillis int64) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)

	if version == 0 || len(removes) > 0 {
		fields := make([]map[string]any, len(cols))
		for i, c := range cols {
			fields[i] = map[string]any{"name": c, "type": deltaTypeName(kinds[i]), "nullable": true, "metadata": map[string]any{}}
		}
		schemaJSON, _ := json.Marshal(map[string]any{"type": "struct", "fields": fields})
		if version == 0 {
			_ = enc.Encode(map[string]any{"protocol": map[string]any{"minReaderVersion": 1, "minWriterVersion": 2}})
		}
		_ = enc.Encode(map[string]any{"metaData": map[string]any{
			"id":               store.NewID(),
			"format":           map[string]any{"provider": "parquet", "options": map[string]any{}},
			"schemaString":     string(schemaJSON),
			"partitionColumns": []string{},
			"configuration":    map[string]any{},
			"createdTime":      nowMillis,
		}})
	}
	for _, r := range removes {
		_ = enc.Encode(map[string]any{"remove": map[string]any{
			"path": r, "deletionTimestamp": nowMillis, "dataChange": true,
		}})
	}
	// stats is a JSON *string* per the Delta spec, and carries numRecords —
	// what every real writer (delta-rs, Spark) emits and what readers use for
	// data skipping. We omitted it, which also meant our own commits could not
	// report how many rows a write added.
	stats, _ := json.Marshal(map[string]any{"numRecords": rows})
	_ = enc.Encode(map[string]any{"add": map[string]any{
		"path":             dataFile,
		"partitionValues":  map[string]any{},
		"size":             size,
		"modificationTime": nowMillis,
		"dataChange":       true,
		"stats":            string(stats),
	}})
	return buf.Bytes()
}
