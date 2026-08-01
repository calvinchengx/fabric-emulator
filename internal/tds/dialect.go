package tds

// Dialect adaptation on the wire (docs/29-tsql-parity.md, T6e).
//
// This is the one place the emulator rewrites a client's SQL rather than
// relaying it verbatim, so the rules from docs/29 are enforced here in code:
//
//	rewrite  — a nested CTE becomes the sequential form SQL Server accepts,
//	           which Fabric would have run as written;
//	reject   — anything Fabric itself refuses, or that cannot be rewritten
//	           faithfully, is answered with an error naming the limitation;
//	forward  — everything else is passed through byte-identical, including
//	           statements this package fails to parse. "I don't understand
//	           this" must never become "I'll guess".
//
// Nothing here fabricates a *result*: the rows still come from the real engine
// running real T-SQL. Only the dialect of the statement changes.

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// dialectFix inspects a client→server message and returns the payload to
// forward, or a non-empty reject message to answer with instead.
//
// A nil error path is deliberate: only a statement this package fully
// understands is altered.
func dialectFix(typ byte, data []byte) (out []byte, reject string) {
	switch typ {
	case PktSQLBatch:
		return fixBatch(data)
	case PktRPC:
		return data, rpcNestedCTEReject(data)
	}
	return data, ""
}

func fixBatch(data []byte) (out []byte, reject string) {
	sql, changed, err := tsql.Flatten(sqlBatchQuery(data))
	switch {
	case err != nil:
		// A statement Fabric itself refuses, or one that cannot be flattened
		// without changing its meaning: say so, by name.
		var restriction *tsql.RestrictionError
		var shadowed *tsql.ShadowedNameError
		if errors.As(err, &restriction) || errors.As(err, &shadowed) {
			return data, err.Error()
		}
		// Anything else is a parse failure — forward untouched and let the
		// engine be the authority on its own dialect.
		return data, ""
	case changed:
		return rewriteBatch(data, sql), ""
	default:
		return data, ""
	}
}

// rewriteBatch rebuilds a SQLBatch payload around new statement text. The
// ALL_HEADERS block (transaction descriptor and friends) is copied verbatim —
// it describes the request, not the statement, and a client that sent one
// expects it echoed in shape. Packet framing is not this function's concern:
// WriteMessage re-chunks whatever length results, so a rewrite that grows past
// one packet still goes out correctly.
func rewriteBatch(data []byte, sql string) []byte {
	var headers []byte
	if len(data) >= 4 {
		if total := int(binary.LittleEndian.Uint32(data)); total >= 4 && total <= len(data) {
			headers = data[:total]
		}
	}
	out := make([]byte, 0, len(headers)+len(sql)*2)
	out = append(out, headers...)
	return append(out, str2ucs2(sql)...)
}

// procsCarryingSQL are the system procedures whose parameters carry a
// statement. T6a measured the ODBC driver using sp_prepexec for every
// parameterized query.
var procsCarryingSQL = map[string]bool{
	"sp_executesql": true, "sp_prepare": true, "sp_prepexec": true,
	"sp_prepexecrpc": true, "sp_cursorprepare": true, "sp_cursorprepexec": true,
}

// rpcNestedCTEReject returns a message when a parameterized statement carries a
// nested CTE, which T6e cannot rewrite (the statement lives inside a typed
// parameter; rewriting it is T6g). Rejecting by name beats forwarding it to
// surface as a bare Msg 156 with no hint of the cause.
//
// The statement text is recovered with the same printable-run heuristic the
// tracer uses, which cannot be trusted to be exact — so it is only ever used to
// *reject*, and only when the recovered text actually parses as a nested-CTE
// statement. Garbage does not parse, so a misread yields no rejection rather
// than a wrong one.
func rpcNestedCTEReject(data []byte) string {
	proc, rest := rpcProc(data)
	if !procsCarryingSQL[proc] {
		return ""
	}
	st, err := tsql.Parse(longestUCS2Text(rest))
	if err != nil || st == nil || !st.HasNestedCTE() {
		return ""
	}
	return fmt.Sprintf("a nested CTE sent as a parameterized statement (%s) is not rewritten by the "+
		"emulator yet; send it without parameters, or flatten the CTE by hand", proc)
}

// dialectReject is the TDS response for a statement refused on dialect grounds.
func dialectReject(msg string) []byte {
	return concat(errorToken(50000, msg), done(doneError, 0))
}
