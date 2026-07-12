// Package warehouse reflects lakehouse Delta tables from OneLake into a real
// T-SQL engine so the warehouse SQL endpoint can query them. This file reads a
// Delta table (its _delta_log + Parquet data files) into a neutral column/row
// form, in pure Go — no CGO, so it lives in the emulator binary.
package warehouse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/parquet-go/parquet-go"
)

// Table is a materialised Delta table: column names and rows of Go-typed
// values (bool, int64, float64, string, []byte, or nil for NULL).
type Table struct {
	Columns []string
	Rows    [][]any
}

// deltaAction is one line of a _delta_log commit (only the parts we use).
type deltaAction struct {
	Add    *struct{ Path string } `json:"add"`
	Remove *struct{ Path string } `json:"remove"`
}

// ReadDeltaTable reads the Delta table under Tables/<name> in the given item
// (a lakehouse) from OneLake: it replays the _delta_log to find the active
// Parquet files, then reads their rows. Only the common shape delta-rs/Spark
// write for small tables is supported (JSON commits, no checkpoint yet).
func ReadDeltaTable(st *store.Store, itemID, name string) (*Table, error) {
	root := path.Join("Tables", name)
	active, err := activeFiles(st, itemID, root)
	if err != nil {
		return nil, err
	}
	if len(active) == 0 {
		return nil, fmt.Errorf("delta table %q has no active data files", name)
	}

	var tbl *Table
	for _, f := range active {
		p, err := st.GetOneLakePath(itemID, path.Join(root, f))
		if err != nil {
			return nil, fmt.Errorf("delta table %q: missing data file %q", name, f)
		}
		part, err := readParquet(p.Content)
		if err != nil {
			return nil, fmt.Errorf("delta table %q: %w", name, err)
		}
		if tbl == nil {
			tbl = &Table{Columns: part.Columns}
		}
		tbl.Rows = append(tbl.Rows, part.Rows...)
	}
	return tbl, nil
}

// activeFiles replays the _delta_log commits (added minus removed) and returns
// the active Parquet file paths (relative to the table root), in commit order.
func activeFiles(st *store.Store, itemID, root string) ([]string, error) {
	logDir := path.Join(root, "_delta_log")
	entries, err := st.ListOneLakePaths(itemID, logDir, false)
	if err != nil {
		return nil, err
	}
	var commits []string
	for _, e := range entries {
		if strings.HasSuffix(e.RelPath, ".json") {
			commits = append(commits, e.RelPath)
		}
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("no _delta_log commits under %q", root)
	}
	sort.Strings(commits) // 000..0.json ordering is lexicographic

	var order []string
	active := map[string]bool{}
	for _, c := range commits {
		p, err := st.GetOneLakePath(itemID, c)
		if err != nil {
			return nil, err
		}
		for _, line := range bytes.Split(p.Content, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var a deltaAction
			if err := json.Unmarshal(line, &a); err != nil {
				return nil, fmt.Errorf("bad _delta_log line in %q: %w", c, err)
			}
			switch {
			case a.Add != nil:
				if !active[a.Add.Path] {
					active[a.Add.Path] = true
					order = append(order, a.Add.Path)
				}
			case a.Remove != nil:
				active[a.Remove.Path] = false
			}
		}
	}
	out := make([]string, 0, len(order))
	for _, f := range order {
		if active[f] {
			out = append(out, f)
		}
	}
	return out, nil
}

// readParquet reads a Parquet file into a Table (flat schemas — Delta tables
// are structs of primitives).
func readParquet(data []byte) (*Table, error) {
	pf, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open parquet: %w", err)
	}
	fields := pf.Schema().Fields()
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = f.Name()
	}
	tbl := &Table{Columns: cols}

	for _, rg := range pf.RowGroups() {
		rows := rg.Rows()
		buf := make([]parquet.Row, 64)
		for {
			n, err := rows.ReadRows(buf)
			for i := 0; i < n; i++ {
				out := make([]any, len(cols))
				for _, v := range buf[i] {
					c := v.Column()
					if c >= 0 && c < len(out) {
						out[c] = goValue(v)
					}
				}
				tbl.Rows = append(tbl.Rows, out)
			}
			if err != nil {
				break // io.EOF (or a read error) ends the row group
			}
		}
		_ = rows.Close()
	}
	return tbl, nil
}

// goValue converts a parquet Value to a Go primitive (nil for NULL).
func goValue(v parquet.Value) any {
	if v.IsNull() {
		return nil
	}
	switch v.Kind() {
	case parquet.Boolean:
		return v.Boolean()
	case parquet.Int32:
		return int64(v.Int32())
	case parquet.Int64:
		return v.Int64()
	case parquet.Float:
		return float64(v.Float())
	case parquet.Double:
		return v.Double()
	case parquet.ByteArray, parquet.FixedLenByteArray:
		return string(v.ByteArray())
	default:
		return v.String()
	}
}
