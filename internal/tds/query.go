package tds

import (
	"context"
	"encoding/binary"
	"fmt"
)

// Backend runs a T-SQL query and returns its result. It is injected so the TDS
// protocol layer stays independent of the engine: tests use a fake, and the
// real implementation drives a SQL Server sidecar over go-mssqldb (T2b). When a
// Server has no Backend, it answers with the T1 stub instead.
type Backend interface {
	Query(ctx context.Context, sql string) (*Result, error)
}

// Column is one result column (its name; types are carried as text — see
// resultTokens).
type Column struct {
	Name string
}

// Result is a query's column set and rows. A nil cell is SQL NULL.
type Result struct {
	Columns []Column
	Rows    [][]any
}

// sqlBatchQuery extracts the query text from a SQLBatch payload. TDS 7.2+
// prefixes an ALL_HEADERS block (a 4-byte total length then headers); the query
// is the UTF-16LE text after it.
func sqlBatchQuery(data []byte) string {
	if len(data) < 4 {
		return ""
	}
	total := int(binary.LittleEndian.Uint32(data))
	if total >= 4 && total <= len(data) {
		return ucs2(data[total:])
	}
	// No ALL_HEADERS (older clients): the whole payload is the query.
	return ucs2(data)
}

// defaultCollation is a 5-byte COLLATION for LCID 1033 (US English), required
// in the COLMETADATA type info of (n)char/(n)varchar columns.
var defaultCollation = []byte{0x09, 0x04, 0x00, 0x00, 0x00}

// nvarcharMaxBytes bounds the declared column width (nvarchar(4000)); values are
// still length-prefixed per row.
const nvarcharMaxBytes = 8000

// resultTokens encodes a Result as a TDS response: COLMETADATA + one ROW per
// row + DONE. Every column is emitted as NVARCHAR — the client's database/sql
// layer converts the text to the caller's scan target (int, string, …), so any
// query round-trips. Real per-column type fidelity is a later refinement.
func resultTokens(res *Result) []byte {
	out := []byte{0x81} // COLMETADATA
	out = binary.LittleEndian.AppendUint16(out, uint16(len(res.Columns)))
	for _, c := range res.Columns {
		out = binary.LittleEndian.AppendUint32(out, 0)      // UserType
		out = binary.LittleEndian.AppendUint16(out, 0x0001) // Flags: nullable
		out = append(out, 0xE7)                             // NVARCHARTYPE
		out = binary.LittleEndian.AppendUint16(out, nvarcharMaxBytes)
		out = append(out, defaultCollation...)
		name := str2ucs2(c.Name)
		out = append(out, byte(len(name)/2))
		out = append(out, name...)
	}
	for _, row := range res.Rows {
		out = append(out, 0xD1) // ROW
		for _, v := range row {
			if v == nil {
				out = binary.LittleEndian.AppendUint16(out, 0xFFFF) // charBin NULL
				continue
			}
			b := str2ucs2(fmt.Sprint(v))
			out = binary.LittleEndian.AppendUint16(out, uint16(len(b)))
			out = append(out, b...)
		}
	}
	return append(out, done(doneFinal|doneCount, uint64(len(res.Rows)))...)
}
