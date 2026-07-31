// Package warehouse reflects lakehouse Delta tables from OneLake into a real
// T-SQL engine so the warehouse SQL endpoint can query them. This file reads a
// Delta table (its _delta_log + Parquet data files) into a neutral column/row
// form, in pure Go — no CGO, so it lives in the emulator binary.
package warehouse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"path"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"
)

// Table is a materialised Delta table: column names and rows of Go-typed
// values (bool, int64, float64, string, []byte, Decimal, or nil for NULL).
type Table struct {
	Columns []string
	Rows    [][]any
}

// Decimal is an exact DECIMAL(precision, scale) value.
//
// Parquet stores a decimal as an *unscaled integer* plus a logical annotation
// carrying the scale: decimal(10,2) 1.50 is the int64 150. Decoding on the
// physical kind alone therefore yields 150, and every downstream sum is wrong
// by 10^scale — silently, which is worse than failing. Carrying the unscaled
// integer with its scale (rather than converting to float64) keeps the value
// exact, which is the whole point of asking for a decimal.
type Decimal struct {
	Unscaled  *big.Int
	Precision int
	Scale     int
}

// String renders the value with its scale applied ("150" scale 2 -> "1.50"),
// which is also a valid SQL decimal literal.
func (d Decimal) String() string {
	if d.Unscaled == nil {
		return "0"
	}
	if d.Scale <= 0 {
		return d.Unscaled.String()
	}
	neg := d.Unscaled.Sign() < 0
	digits := new(big.Int).Abs(d.Unscaled).String()
	if len(digits) <= d.Scale { // pad to 0.00ddd
		digits = strings.Repeat("0", d.Scale-len(digits)+1) + digits
	}
	out := digits[:len(digits)-d.Scale] + "." + digits[len(digits)-d.Scale:]
	if neg {
		out = "-" + out
	}
	return out
}

// deltaAction is one line of a _delta_log commit (only the parts we use).
type deltaAction struct {
	Add    *struct{ Path string } `json:"add"`
	Remove *struct{ Path string } `json:"remove"`
}

// ReadParquetBytes reads a single standalone Parquet file's rows into a Table
// (flat schemas — the same reader ReadDeltaTable uses per data file). Exported
// for callers that address a bare .parquet file directly (e.g. the pipeline
// Lookup activity), rather than a Tables/<name> Delta table.
func ReadParquetBytes(data []byte) (*Table, error) {
	return readParquet(data)
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
	// A decimal's scale lives in the schema's logical annotation, not in the
	// values, so capture it per column before reading any rows.
	decs := make([]*format.DecimalType, len(fields))
	for i, f := range fields {
		cols[i] = f.Name()
		if lt := f.Type().LogicalType(); lt != nil {
			decs[i] = lt.Decimal
		}
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
						out[c] = goValue(v, decs[c])
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

// goValue converts a parquet Value to a Go primitive (nil for NULL). dec is the
// column's DECIMAL annotation when it has one, which must win over the physical
// kind: the same INT64 is a plain long or an unscaled decimal depending on it.
func goValue(v parquet.Value, dec *format.DecimalType) any {
	if v.IsNull() {
		return nil
	}
	if dec != nil {
		return decimalValue(v, dec)
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

// decimalValue decodes one DECIMAL value. Parquet allows four physical
// encodings for the unscaled integer, and delta-rs picks by precision (INT32
// to 9, INT64 to 18, byte array beyond), so all of them have to be handled or
// wide decimals silently fall through to a string.
func decimalValue(v parquet.Value, dec *format.DecimalType) Decimal {
	d := Decimal{Precision: int(dec.Precision), Scale: int(dec.Scale)}
	switch v.Kind() {
	case parquet.Int32:
		d.Unscaled = big.NewInt(int64(v.Int32()))
	case parquet.Int64:
		d.Unscaled = big.NewInt(v.Int64())
	case parquet.ByteArray, parquet.FixedLenByteArray:
		// Big-endian two's complement, so a leading high bit means negative.
		b := v.ByteArray()
		n := new(big.Int).SetBytes(b)
		if len(b) > 0 && b[0]&0x80 != 0 {
			n.Sub(n, new(big.Int).Lsh(big.NewInt(1), uint(len(b)*8)))
		}
		d.Unscaled = n
	default:
		d.Unscaled = big.NewInt(0)
	}
	return d
}
