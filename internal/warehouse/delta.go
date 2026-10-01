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
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/format"
)

// Table is a materialised Delta table: column names and rows of Go-typed
// values (bool, int64, float64, string, []byte, Decimal, or nil for NULL).
type Table struct {
	Columns []string
	Rows    [][]any
	// Skipped names the columns the SQL surface cannot represent — the nested
	// types. Recorded rather than silently discarded so a caller can say WHICH
	// column vanished; "some columns might not be available" is a miserable
	// thing to debug without a name.
	Skipped []string
}

// Date is a Delta DATE: a calendar day, no time and no zone.
//
// Same hazard as Decimal below, and the same fix. A DATE is stored as a plain
// INT32 count of days since 1970-01-01, so decoding on the physical kind alone
// yields 20627 — a number that is not wrong-looking, which is what makes it
// dangerous. It reflects to the analytics endpoint as BIGINT, a report shows
// 20627 where a date belongs, and a join against a real date fails with
// "Operand type clash: date is incompatible with bigint" naming neither the
// column nor the cause. Carrying the logical type is what keeps a date a date.
type Date struct{ T time.Time }

func (d Date) String() string { return d.T.Format("2006-01-02") }

// Timestamp is a Delta TIMESTAMP. Physically an INT64 since the epoch, whose
// unit (milli/micro/nano) lives only in the logical annotation — so the same
// int64 is three different instants depending on it.
type Timestamp struct{ T time.Time }

func (t Timestamp) String() string { return t.T.UTC().Format("2006-01-02 15:04:05.999999") }

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
	// ModificationTime is the fallback commit time: a log written by another
	// writer may carry no commitInfo at all, and then the newest file the
	// commit adds is the only record of when it happened.
	Add *struct {
		Path             string `json:"path"`
		ModificationTime int64  `json:"modificationTime"`
	} `json:"add"`
	Remove *struct {
		Path string `json:"path"`
	} `json:"remove"`
	// CommitInfo is where a commit states its own time, and the authoritative
	// answer when present — our writer emits it first in every commit
	// (commitInfoAction, write.go) precisely so a replay can be cut at an
	// instant.
	CommitInfo *struct {
		Timestamp int64 `json:"timestamp"`
	} `json:"commitInfo"`
	// MetaData carries the table's logical schema. Delta matches data files to
	// it BY NAME, so the Parquet files' physical field order is an
	// implementation detail — the schema is what a reader must present.
	MetaData *struct {
		SchemaString string `json:"schemaString"`
	} `json:"metaData"`
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
	return ReadDeltaTableAt(st, itemID, path.Join("Tables", name), name)
}

// ReadDeltaTableAt reads the Delta table rooted at root in the given item. It is
// ReadDeltaTable for a table that does not live under its own Tables/<name>: a
// OneLake shortcut's source is another item's folder, and `name` is only how
// errors say which table it was.
func ReadDeltaTableAt(st *store.Store, itemID, root, name string) (*Table, error) {
	active, schema, err := activeFiles(st, itemID, root)
	if err != nil {
		return nil, err
	}
	return assembleTable(name, active, schema, func(f string) ([]byte, error) {
		p, err := st.GetOneLakePath(itemID, path.Join(root, f))
		if err != nil {
			return nil, fmt.Errorf("delta table %q: missing data file %q", name, f)
		}
		return p.Content, nil
	})
}

// ExternalDelta is how an ADLS Gen2, Amazon S3 or Dataverse shortcut's Delta
// table is read: its _delta_log folder's JSON commit filenames, sorted, and one
// file's bytes by the path a commit or the shortcut's own root names, relative
// to that root. Implemented by *onelake.Service; passed in rather than
// imported, so this package does not depend on how a shortcut's credential is
// resolved (docs/61).
type ExternalDelta interface {
	ExternalDeltaCommits(sc *store.Shortcut) ([]string, error)
	ExternalReadFile(sc *store.Shortcut, remainder string) ([]byte, error)
}

// readExternalDeltaTable is ReadDeltaTableAt for a shortcut whose target is not
// OneLake: the same replay and assembly, sourced over HTTP through external
// instead of the store.
func readExternalDeltaTable(external ExternalDelta, sc *store.Shortcut, name string) (*Table, error) {
	active, schema, err := activeFilesExternal(external, sc)
	if err != nil {
		return nil, err
	}
	return assembleTable(name, active, schema, func(f string) ([]byte, error) {
		return external.ExternalReadFile(sc, f)
	})
}

// assembleTable is ReadDeltaTableAt's and readExternalDeltaTable's shared second
// half: given the active Parquet file paths and the schema activeFiles already
// found, read each with `read` and stitch the rows together. Where the bytes
// come from is the only thing that differs between a OneLake table and an
// external shortcut's.
func assembleTable(name string, active []string, schema deltaSchema, read func(string) ([]byte, error)) (*Table, error) {
	if len(active) == 0 {
		return nil, fmt.Errorf("delta table %q has no active data files", name)
	}
	// The projection target: the logical schema minus the columns SQL cannot
	// represent. Computed once, from the schema rather than from any one data
	// file, so every part lands on the identical column list and the rows
	// appended below stay aligned with tbl.Columns.
	cols := omit(schema.Cols, schema.Nested)

	var tbl *Table
	for _, f := range active {
		content, err := read(f)
		if err != nil {
			return nil, err
		}
		part, err := readParquet(content)
		if err != nil {
			return nil, fmt.Errorf("delta table %q: %w", name, err)
		}
		// Present the logical schema's column order, not the Parquet file's:
		// our writer builds the physical schema from a map (alphabetical), and
		// externally-written files may order differently again. Delta matches by
		// name, so the metaData schema is the contract every reader should show.
		if len(cols) > 0 {
			part = project(part, cols)
		}
		if tbl == nil {
			// Skipped comes along. Without it the warning in reflectTable is
			// unreachable from this path even when no projection happens —
			// which is every table with no metaData in its log.
			tbl = &Table{Columns: part.Columns, Skipped: part.Skipped}
		}
		tbl.Rows = append(tbl.Rows, part.Rows...)
	}
	return tbl, nil
}

// activeFiles replays the _delta_log commits (added minus removed) and returns
// the active Parquet file paths (relative to the table root), in commit order.
func activeFiles(st *store.Store, itemID, root string) ([]string, deltaSchema, error) {
	return activeFilesStop(st, itemID, root, nil)
}

// activeFilesStop is activeFiles with a stopping condition: stop is consulted
// before each commit is applied, so a caller can replay a PREFIX of the log —
// which is all time travel is. A nil stop replays everything, which is why
// activeFiles is one line.
func activeFilesStop(st *store.Store, itemID, root string, stop commitStop) ([]string, deltaSchema, error) {
	logDir := path.Join(root, "_delta_log")
	entries, err := st.ListOneLakePaths(itemID, logDir, false)
	if err != nil {
		return nil, deltaSchema{}, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.RelPath, ".json") {
			names = append(names, e.RelPath)
		}
	}
	if len(names) == 0 {
		return nil, deltaSchema{}, fmt.Errorf("no _delta_log commits under %q", root)
	}
	sort.Strings(names) // 000..0.json ordering is lexicographic

	blobs := make([][]byte, len(names))
	for i, c := range names {
		p, err := st.GetOneLakePath(itemID, c)
		if err != nil {
			return nil, deltaSchema{}, err
		}
		blobs[i] = p.Content
	}
	return replayCommits(names, blobs, stop)
}

// activeFilesExternal is activeFiles for an external shortcut's target: the
// same replay, over commits listed and read through external instead of the
// store.
func activeFilesExternal(external ExternalDelta, sc *store.Shortcut) ([]string, deltaSchema, error) {
	names, err := external.ExternalDeltaCommits(sc)
	if err != nil {
		return nil, deltaSchema{}, err
	}
	if len(names) == 0 {
		return nil, deltaSchema{}, fmt.Errorf("no _delta_log commits under %q", sc.Name)
	}
	blobs := make([][]byte, len(names))
	for i, name := range names {
		b, err := external.ExternalReadFile(sc, path.Join("_delta_log", name))
		if err != nil {
			return nil, deltaSchema{}, err
		}
		blobs[i] = b
	}
	return replayCommits(names, blobs, nil)
}

// replayCommits is activeFiles' and activeFilesExternal's shared core: turn a
// list of _delta_log commits' CONTENTS, already read and in commit order, into
// the active data file paths and the logical schema. Only how the bytes were
// fetched differs between a OneLake table and a shortcut's external target;
// the replay — later metaData supersedes earlier, add/remove — is one rule
// either way.
func replayCommits(names []string, blobs [][]byte, stop commitStop) ([]string, deltaSchema, error) {
	var order []string
	var schema deltaSchema
	active := map[string]bool{}
	for i, content := range blobs {
		actions, err := parseCommit(names[i], content)
		if err != nil {
			return nil, deltaSchema{}, err
		}
		// Asked BEFORE anything is applied: a commit is either wholly in the
		// replay or wholly out of it. Delta's atomicity is per commit, and a
		// half-applied one is a file set that never existed.
		if stop != nil {
			ts, dated := commitTimestamp(actions)
			done, err := stop(names[i], ts, dated)
			if err != nil {
				return nil, deltaSchema{}, err
			}
			if done {
				break
			}
		}
		for _, a := range actions {
			// A later metaData supersedes an earlier one (schema evolution).
			if a.MetaData != nil {
				if sc := schemaColumns(a.MetaData.SchemaString); len(sc.Cols) > 0 {
					schema = sc
				}
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
	return out, schema, nil
}

// commitStop decides, for one _delta_log commit, whether the replay should end
// BEFORE that commit is applied. It is handed the commit's file name (for
// errors), its timestamp in epoch millis, and whether the commit recorded one
// at all — an undated commit cannot be placed in time, and only the caller
// knows whether that is fatal.
type commitStop func(name string, ts int64, dated bool) (bool, error)

// parseCommit decodes one commit's NDJSON into its actions, in order.
func parseCommit(name string, content []byte) ([]deltaAction, error) {
	var out []deltaAction
	for _, line := range bytes.Split(content, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var a deltaAction
		if err := json.Unmarshal(line, &a); err != nil {
			return nil, fmt.Errorf("bad _delta_log line in %q: %w", name, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// commitTimestamp is when a commit happened, in epoch millis: its commitInfo
// timestamp, or else the largest modificationTime among the files it adds.
//
// The fallback is not a guess — Delta writers that omit commitInfo still stamp
// every add, and the newest of them is when the commit landed. Zero counts as
// absent in both: in a log this emulator stamps from its own clock it means
// "not recorded" far more often than it means 1970.
func commitTimestamp(actions []deltaAction) (int64, bool) {
	for _, a := range actions {
		if a.CommitInfo != nil && a.CommitInfo.Timestamp > 0 {
			return a.CommitInfo.Timestamp, true
		}
	}
	var newest int64
	for _, a := range actions {
		if a.Add != nil && a.Add.ModificationTime > newest {
			newest = a.Add.ModificationTime
		}
	}
	return newest, newest > 0
}

// ReadDeltaTableAsOf is ReadDeltaTable at a point in time: the rows of
// Tables/<name> as they stood at the last commit made at or before asOf
// (docs/35 Phase 1 — what `OPTION (FOR TIMESTAMP AS OF …)` needs underneath).
//
// Nothing is reconstructed. A commit that supersedes a file only marks it
// removed, so replaying a PREFIX of the log yields exactly the file set that
// was live then, with the schema of that prefix's newest metaData — both still
// on disk. That is why time travel is cheap for a Delta table and impossible
// for a warehouse table, which keeps no version history at all.
//
// A timestamp before the table existed is an error rather than an empty table:
// Fabric refuses it, and answering "no rows" for a table that had not been
// created yet is the kind of plausible wrong answer time travel exists to
// avoid.
func ReadDeltaTableAsOf(st *store.Store, itemID, name string, asOf time.Time) (*Table, error) {
	root := path.Join("Tables", name)
	asOfMillis := asOf.UnixMilli()

	var applied, first int64
	var haveFirst bool
	stop := func(commit string, ts int64, dated bool) (bool, error) {
		if !dated {
			return false, fmt.Errorf("delta table %q: commit %q records no timestamp, so it cannot be read as of a point in time", name, commit)
		}
		if !haveFirst {
			first, haveFirst = ts, true
		}
		if ts > asOfMillis {
			return true, nil
		}
		applied++
		return false, nil
	}

	active, schema, err := activeFilesStop(st, itemID, root, stop)
	if err != nil {
		return nil, err
	}
	if applied == 0 {
		return nil, fmt.Errorf("delta table %q did not exist at %s: its first commit is at %s",
			name, asOf.UTC().Format(time.RFC3339), time.UnixMilli(first).UTC().Format(time.RFC3339))
	}
	return assembleTable(name, active, schema, func(f string) ([]byte, error) {
		p, err := st.GetOneLakePath(itemID, path.Join(root, f))
		if err != nil {
			return nil, fmt.Errorf("delta table %q: missing data file %q", name, f)
		}
		return p.Content, nil
	})
}

// deltaSchema is the logical schema from a metaData action: every field in
// order, and which of them are nested.
type deltaSchema struct {
	Cols   []string
	Nested []string
}

// schemaColumns pulls the field names, in order, out of a Delta metaData
// schemaString ({"type":"struct","fields":[{"name":…},…]}), and separates the
// nested ones.
//
// The nested set comes from the SCHEMA rather than from what a Parquet reader
// happened to skip, because the schema is the only order-independent answer.
// Deciding it from the first data file instead looks equivalent and is not:
// after a schema evolution that adds a nested column, the oldest file — which
// is first in commit order — does not carry that column at all, so it skips
// nothing, and the nested column would be re-added for every later file.
func schemaColumns(schemaString string) deltaSchema {
	var s struct {
		Fields []struct {
			Name string          `json:"name"`
			Type json.RawMessage `json:"type"`
		} `json:"fields"`
	}
	var out deltaSchema
	if json.Unmarshal([]byte(schemaString), &s) != nil {
		return out
	}
	out.Cols = make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		if f.Name == "" {
			continue
		}
		out.Cols = append(out.Cols, f.Name)
		// The Delta protocol's own distinction: a primitive's type is a JSON
		// STRING ("long", "date", "decimal(9,2)"), while struct/array/map are
		// JSON OBJECTS.
		if t := bytes.TrimSpace(f.Type); len(t) > 0 && t[0] == '{' {
			out.Nested = append(out.Nested, f.Name)
		}
	}
	return out
}

// project reorders a part's columns onto the table's logical schema, filling a
// column the part does not carry with nulls. Delta resolves data files to the
// schema by name; the Parquet field order is not the contract.
func project(part *Table, cols []string) *Table {
	idx := make(map[string]int, len(part.Columns))
	for i, c := range part.Columns {
		idx[c] = i
	}
	// Skipped is carried across. reflectTable's "not representable … omitted"
	// warning is guarded on it, so dropping it here silenced that warning for
	// exactly the tables it was written for.
	out := &Table{Columns: cols, Rows: make([][]any, len(part.Rows)), Skipped: part.Skipped}
	for r, row := range part.Rows {
		vals := make([]any, len(cols))
		for i, c := range cols {
			if j, ok := idx[c]; ok && j < len(row) {
				vals[i] = row[j]
			}
		}
		out.Rows[r] = vals
	}
	return out
}

// omit drops `names` from `cols`, preserving order.
//
// A column the reader SKIPPED is not a column a part merely lacks, and the
// difference is the whole point of this function. The logical schema still
// names the nested fields, so projecting onto it re-added every one of them —
// with nils, because nothing maps to them. They then reached CREATE TABLE, took
// sqlType's varchar default (no non-null value is ever seen) and served as
// nullable varchar full of NULL: the opposite of omitting them, and the
// opposite of what v0.16.0's release notes promised.
func omit(cols, names []string) []string {
	if len(names) == 0 {
		return cols
	}
	drop := make(map[string]bool, len(names))
	for _, c := range names {
		drop[c] = true
	}
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if !drop[c] {
			out = append(out, c)
		}
	}
	return out
}

// readParquet reads a Parquet file into a Table (flat schemas — Delta tables
// are structs of primitives).
func readParquet(data []byte) (*Table, error) {
	pf, err := parquet.OpenFile(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("open parquet: %w", err)
	}
	// Columns are built from the top-level LEAF fields only, and each one
	// remembers which PARQUET leaf index carries it.
	//
	// A nested field (struct/array/map) occupies several leaf indices, and the
	// reader used to assign values by position into a slice sized by top-level
	// field count. One nested column therefore took a leaf belonging to
	// something else and shifted every column after it — a `bigint` reading
	// back as another column's string, with the real value dropped and nothing
	// raised. Fabric's SQL analytics endpoint does not represent these types at
	// all ("Types that aren't listed in the table aren't represented as the
	// table columns"), so omitting the column is both the faithful answer and
	// the safe one: what remains is correct.
	//
	// The logical annotation is the only place a date, a timestamp's unit, a
	// decimal's scale, or "these bytes are text" is recorded — the physical kind
	// cannot distinguish any of them.
	var (
		cols    []string
		lts     []*format.LogicalType
		skipped []string
	)
	byLeaf := map[int]int{} // parquet leaf index -> position in cols
	leaf := 0
	for _, f := range pf.Schema().Fields() {
		width := leafCount(f)
		if f.Leaf() {
			byLeaf[leaf] = len(cols)
			cols = append(cols, f.Name())
			lts = append(lts, f.Type().LogicalType())
		} else {
			skipped = append(skipped, f.Name())
		}
		leaf += width
	}
	tbl := &Table{Columns: cols, Skipped: skipped}

	for _, rg := range pf.RowGroups() {
		rows := rg.Rows()
		buf := make([]parquet.Row, 64)
		for {
			n, err := rows.ReadRows(buf)
			for i := 0; i < n; i++ {
				out := make([]any, len(cols))
				for _, v := range buf[i] {
					// By LEAF index, not by position: the two coincide only
					// when every top-level field is a leaf.
					if pos, ok := byLeaf[v.Column()]; ok {
						out[pos] = goValue(v, lts[pos])
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

// leafCount is how many parquet leaf columns a node occupies: 1 for a
// primitive, and the sum of its children otherwise. It is what lets a nested
// field be SKIPPED without shifting the fields after it.
func leafCount(n parquet.Node) int {
	if n.Leaf() {
		return 1
	}
	total := 0
	for _, f := range n.Fields() {
		total += leafCount(f)
	}
	return total
}

// logicalValue reads one member out of parquet-go 0.32's LogicalType union,
// which replaced the old struct-of-optional-pointers. Generic because every
// caller asks the same question of a different member, and a nil annotation
// must answer "no" rather than panic.
func logicalValue[T format.LogicalTypeValue](lt *format.LogicalType) (T, bool) {
	var zero T
	if lt == nil {
		return zero, false
	}
	v, ok := lt.Value.(T)
	return v, ok
}

// isTextAnnotation reports whether the schema SAYS these bytes are text.
// Three annotations mean text — STRING, JSON and ENUM — and they are listed
// here rather than inline so the byte-array branch keeps reading as one
// question.
func isTextAnnotation(lt *format.LogicalType) bool {
	if lt == nil {
		return false
	}
	switch lt.Value.(type) {
	case *format.StringType, *format.JsonType, *format.EnumType:
		return true
	}
	return false
}

// goValue converts a parquet Value to a Go value (nil for NULL). lt is the
// column's logical annotation, which must win over the physical kind: the same
// INT32 is a plain int or a date, the same INT64 a long, an unscaled decimal or
// an instant, and the same BYTE_ARRAY text or opaque bytes. Reading the
// physical kind alone answers every one of those the same way, which is exactly
// the bug this exists to prevent.
func goValue(v parquet.Value, lt *format.LogicalType) any {
	if v.IsNull() {
		return nil
	}
	// parquet-go 0.32 turned LogicalType from a struct of optional pointers
	// into a real thrift union with one Value, so the annotation is read by
	// type rather than by nil-check. Same three questions, same order.
	if lt != nil {
		switch t := lt.Value.(type) {
		case *format.DecimalType:
			return decimalValue(v, t)
		case *format.DateType:
			return Date{T: epochDay.AddDate(0, 0, int(v.Int32()))}
		case *format.TimestampType:
			return Timestamp{T: timestampTime(v.Int64(), t)}
		}
	}
	switch v.Kind() {
	case parquet.Boolean:
		return v.Boolean()
	case parquet.Int32:
		// A Delta smallint/tinyint is ALSO an INT32 physically — the width
		// lives only in the annotation, INT(16,true) or INT(8,true). Reading
		// the kind alone reflects both as `int` where Fabric says `smallint`:
		// one width too wide, with nothing to notice. Same shape as the date
		// bug, and the reason the annotation is consulted first.
		//
		// Fabric maps 8-bit to `smallint` too, not to `tinyint`: T-SQL tinyint
		// is not supported for persisted storage, and its own unsupported-types
		// table says "tinyint -> Use smallint".
		//
		// Unsigned is deliberately excluded. Delta has no unsigned types, so
		// this is defensive — but a uint16 up to 65535 does not fit an int16,
		// and reflecting one width too WIDE is lossless where narrowing is not.
		if it, ok := logicalValue[*format.IntType](lt); ok && it.BitWidth <= 16 && it.IsSigned {
			return int16(v.Int32())
		}
		// int32, not int64: a widened int reflects as BIGINT where Fabric says
		// int. The physical width is the logical one here.
		return v.Int32()
	case parquet.Int64:
		return v.Int64()
	case parquet.Float:
		// float32, not float64. Fabric maps Delta FLOAT/REAL to `real` and
		// DOUBLE to `float`; widening here made both reflect as `float`, so a
		// 4-byte column silently became 8 bytes wide. Unlike the integer
		// widths, this one is visible in the PHYSICAL kind — FLOAT vs DOUBLE —
		// so no annotation is needed to tell them apart.
		return v.Float()
	case parquet.Double:
		return v.Double()
	case parquet.ByteArray, parquet.FixedLenByteArray:
		// Only text when the schema SAYS it is text. Everything else is bytes,
		// including UUID and BSON: stringifying them loses the distinction and
		// can produce invalid UTF-8 in a string column.
		//
		// This used to fall through to string for EVERY non-nil annotation, so
		// isTextAnnotation could not change an outcome and a mutation to it
		// failed no test (#339). The oracle is not a new Fabric measurement:
		// Delta declares no UUID or BSON type, so a column carrying either
		// annotation is `binary` in the Delta log, and docs/16 already measures
		// `binary` -> `varbinary` against the real endpoint. Text is one type too
		// permissive, which is the direction that ships quietly.
		if isTextAnnotation(lt) {
			return string(v.ByteArray())
		}
		return append([]byte(nil), v.ByteArray()...)
	default:
		return v.String()
	}
}

// epochDay is the zero of a Parquet DATE.
var epochDay = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// timestampTime applies the annotation's unit. Getting this wrong is silent:
// microseconds read as milliseconds land in 1970 rather than failing.
func timestampTime(n int64, ts *format.TimestampType) time.Time {
	switch ts.Unit.Value.(type) {
	case *format.NanoSeconds:
		return time.Unix(0, n).UTC()
	case *format.MilliSeconds:
		return time.UnixMilli(n).UTC()
	default: // Micros is the Delta/Spark default
		return time.UnixMicro(n).UTC()
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
