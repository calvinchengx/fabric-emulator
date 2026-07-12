package tds

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
)

// Backend runs a T-SQL query and returns its result. It is injected so the TDS
// protocol layer stays independent of the engine: tests use a fake, and the
// real implementation drives a SQL Server sidecar over go-mssqldb (T2b). When a
// Server has no Backend, it answers with the T1 stub instead.
type Backend interface {
	Query(ctx context.Context, sql string) (*Result, error)
}

// ColType is the wire type a result column is encoded as. Integer/float/bit
// columns are emitted with their real TDS type so a typed client reads them as
// numbers/bools; everything else falls back to NVARCHAR text (the client's
// database/sql layer still converts on scan).
type ColType uint8

const (
	ColNVarchar ColType = iota // UTF-16LE text (default; also the fallback)
	ColInt                     // INTNTYPE, 8-byte (BIGINT)
	ColFloat                   // FLTNTYPE, 8-byte IEEE-754
	ColBit                     // BITNTYPE, 1-byte
)

// Column is one result column: its name and wire type.
type Column struct {
	Name string
	Type ColType
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
// row + DONE. Each column carries its real type (integer/float/bit, else
// NVARCHAR text) so a typed client reads the natural Go type; text columns still
// round-trip via the driver's on-scan conversion.
func resultTokens(res *Result) []byte {
	// A resultless batch (SET options, DDL, DML) has no columns: just DONE,
	// with the affected-row count — no COLMETADATA.
	if len(res.Columns) == 0 {
		return done(doneFinal|doneCount, uint64(len(res.Rows)))
	}
	out := []byte{0x81} // COLMETADATA
	out = binary.LittleEndian.AppendUint16(out, uint16(len(res.Columns)))
	for _, c := range res.Columns {
		out = binary.LittleEndian.AppendUint32(out, 0)      // UserType
		out = binary.LittleEndian.AppendUint16(out, 0x0001) // Flags: nullable
		switch c.Type {
		case ColInt:
			out = append(out, 0x26, 0x08) // INTNTYPE, max length 8 (BIGINT)
		case ColFloat:
			out = append(out, 0x6D, 0x08) // FLTNTYPE, max length 8
		case ColBit:
			out = append(out, 0x68, 0x01) // BITNTYPE, max length 1
		default:
			out = append(out, 0xE7) // NVARCHARTYPE
			out = binary.LittleEndian.AppendUint16(out, nvarcharMaxBytes)
			out = append(out, defaultCollation...)
		}
		name := str2ucs2(c.Name)
		out = append(out, byte(len(name)/2))
		out = append(out, name...)
	}
	for _, row := range res.Rows {
		out = append(out, 0xD1) // ROW
		for i, v := range row {
			out = appendCell(out, res.Columns[i].Type, v)
		}
	}
	return append(out, done(doneFinal|doneCount, uint64(len(res.Rows)))...)
}

// appendCell encodes one value per its column type. The variable-length
// nullable types (INTN/FLTN/BITN) use a 1-byte length (0 = NULL); NVARCHAR uses
// a 2-byte length (0xFFFF = NULL).
func appendCell(out []byte, ct ColType, v any) []byte {
	switch ct {
	case ColInt:
		if v == nil {
			return append(out, 0x00)
		}
		out = append(out, 0x08)
		return binary.LittleEndian.AppendUint64(out, uint64(toInt64(v)))
	case ColFloat:
		if v == nil {
			return append(out, 0x00)
		}
		out = append(out, 0x08)
		return binary.LittleEndian.AppendUint64(out, math.Float64bits(toFloat64(v)))
	case ColBit:
		if v == nil {
			return append(out, 0x00)
		}
		b := byte(0)
		if toBool(v) {
			b = 1
		}
		return append(out, 0x01, b)
	default:
		if v == nil {
			return binary.LittleEndian.AppendUint16(out, 0xFFFF)
		}
		b := str2ucs2(fmt.Sprint(v))
		out = binary.LittleEndian.AppendUint16(out, uint16(len(b)))
		return append(out, b...)
	}
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float64:
		return int64(x)
	case bool:
		if x {
			return 1
		}
	}
	return 0
}

func toFloat64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

func toBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case int64:
		return x != 0
	case int:
		return x != 0
	}
	return false
}
