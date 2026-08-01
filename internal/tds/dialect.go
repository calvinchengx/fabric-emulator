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
	"bytes"
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
func dialectFix(typ byte, data []byte, strict bool) (out []byte, reject string) {
	switch typ {
	case PktSQLBatch:
		return fixBatch(data, strict)
	case PktRPC:
		return fixRPC(data, strict)
	}
	return data, ""
}

// strictReject reports a construct real Fabric rejects, when strict mode is on.
// It runs before any rewrite: the question "would Fabric run this at all?"
// comes before "how do we make the sidecar run it?".
func strictReject(sql string, strict bool) string {
	if !strict {
		return ""
	}
	if err := tsql.CheckStrict(sql); err != nil {
		return err.Error()
	}
	return ""
}

// fixRPC rewrites a nested CTE carried inside a procedure parameter
// (sp_prepexec and friends). Every failure path forwards the original: a
// mis-parsed parameter list must cost us the rewrite, never the request.
func fixRPC(data []byte, strict bool) (out []byte, reject string) {
	proc, _ := rpcProc(data)
	if !procsCarryingSQL[proc] {
		return data, ""
	}
	req, err := parseRPC(data)
	if err != nil {
		// Not a shape this file models. Fall back to T6e's behaviour: name the
		// limitation if a statement is visibly in there, rather than let it
		// surface as a bare Msg 156.
		return data, heuristicNestedReject(data, proc)
	}

	idx, flattened := -1, ""
	for i, p := range req.params {
		if !p.isText || p.text == "" {
			continue
		}
		if msg := strictReject(p.text, strict); msg != "" {
			return data, msg
		}
		rewritten, changed, ferr := tsql.Flatten(p.text)
		if ferr != nil {
			var restriction *tsql.RestrictionError
			var shadowed *tsql.ShadowedNameError
			if errors.As(ferr, &restriction) || errors.As(ferr, &shadowed) {
				return data, ferr.Error()
			}
			continue // unparseable parameter: not ours to judge
		}
		if changed {
			idx, flattened = i, rewritten
			break
		}
	}
	if idx < 0 {
		return data, "" // nothing to rewrite
	}

	rebuilt := *req
	rebuilt.params = append([]rpcParam(nil), req.params...)
	rebuilt.params[idx].typeInfo, rebuilt.params[idx].value =
		encodeCharValue(req.params[idx], flattened)
	encoded := rebuilt.encode()

	// Verify our own output before it reaches the engine. This branch is
	// defence in depth against an encoder bug, so it is unreachable while the
	// encoder is correct — rewriteIsFaithful's own logic is tested directly
	// (TestRewriteIsFaithfulRejects*); only this wiring line is uncovered, and
	// deliberately so rather than propped up by a contrived fake.
	if !rewriteIsFaithful(req, encoded, idx, flattened) {
		return data, ""
	}
	return encoded, ""
}

// rewriteIsFaithful re-parses a rewritten RPC and proves it differs from the
// original in exactly one place: the statement parameter, which must decode to
// the SQL we meant to send. Anything else — a lost parameter, a shifted
// boundary, a mangled value — means the encoder is wrong, and the original is
// forwarded instead.
func rewriteIsFaithful(orig *rpcRequest, encoded []byte, idx int, want string) bool {
	got, err := parseRPC(encoded)
	if err != nil ||
		!bytes.Equal(got.headers, orig.headers) ||
		!bytes.Equal(got.proc, orig.proc) ||
		!bytes.Equal(got.flags, orig.flags) ||
		len(got.params) != len(orig.params) {
		return false
	}
	for i := range got.params {
		if i == idx {
			if got.params[i].text != want {
				return false
			}
			continue
		}
		a, b := orig.params[i], got.params[i]
		if !bytes.Equal(a.name, b.name) || a.status != b.status ||
			!bytes.Equal(a.typeInfo, b.typeInfo) || !bytes.Equal(a.value, b.value) {
			return false
		}
	}
	return true
}

func fixBatch(data []byte, strict bool) (out []byte, reject string) {
	raw := sqlBatchQuery(data)
	if msg := strictReject(raw, strict); msg != "" {
		return data, msg
	}
	sql, changed, err := tsql.Flatten(raw)
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

// heuristicNestedReject is the last resort for an RPC whose parameter list this
// file cannot measure exactly — an unmodelled parameter type, say. The
// statement text is recovered with the tracer's printable-run heuristic, which
// cannot be trusted to be exact, so it is only ever used to *reject*, and only
// when the recovered text genuinely parses as a nested-CTE statement. Garbage
// does not parse, so a misread yields no rejection rather than a wrong one.
func heuristicNestedReject(data []byte, proc string) string {
	_, rest := rpcProc(data)
	st, err := tsql.Parse(longestUCS2Text(rest))
	if err != nil || st == nil || !st.HasNestedCTE() {
		return ""
	}
	return fmt.Sprintf("a nested CTE sent as a parameterized statement (%s) could not be rewritten: "+
		"its parameter list uses a type the emulator does not model. Send it without parameters, "+
		"or flatten the CTE by hand", proc)
}

// dialectReject is the TDS response for a statement refused on dialect grounds.
func dialectReject(msg string) []byte {
	return concat(errorToken(50000, msg), done(doneError, 0))
}
