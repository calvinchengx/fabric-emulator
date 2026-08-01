package tds

// Request tracing — a development instrument for dialect work
// (docs/29-tsql-parity.md). A SQL rewriter can only rewrite statements it can
// see, and TDS carries a statement two different ways: as a SQLBatch message
// (the text is the payload) or as an RPC message (the text is a parameter of a
// system procedure such as sp_executesql). Which one a given client uses is a
// property of the client's driver, not of the server, so it has to be measured
// rather than assumed. TraceFunc makes that measurable without a debugger.

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unicode"
)

// TraceFunc, when non-nil, receives a one-line description of every
// client→server TDS message. Nil (the default) disables tracing entirely, so
// the cost on the hot path is one nil check.
var TraceFunc func(line string)

// traceRequest describes a client message to TraceFunc, if tracing is on.
func traceRequest(typ byte, data []byte) {
	if TraceFunc == nil {
		return
	}
	TraceFunc(describeRequest(typ, data))
}

// describeRequest renders a client message as one diagnostic line. It is pure
// and total: malformed input yields a best-effort description, never a panic,
// because it runs on live traffic from third-party drivers.
func describeRequest(typ byte, data []byte) string {
	switch typ {
	case PktSQLBatch:
		return fmt.Sprintf("SQLBatch bytes=%d sql=%s", len(data), quotePreview(sqlBatchQuery(data)))
	case PktRPC:
		proc, rest := rpcProc(data)
		return fmt.Sprintf("RPC proc=%s bytes=%d text=%s", proc, len(data), quotePreview(longestUCS2Text(rest)))
	case PktAttention:
		return "Attention (cancel)"
	case PktBulkLoad:
		return fmt.Sprintf("BulkLoad bytes=%d", len(data))
	default:
		return fmt.Sprintf("type=%#x bytes=%d", typ, len(data))
	}
}

// wellKnownProc maps the RPC ProcID shorthand a driver may send instead of a
// name (MS-TDS 2.2.6.6). sp_executesql and the sp_prepare/sp_execute family are
// the ones that carry SQL text in a parameter.
var wellKnownProc = map[uint16]string{
	1: "sp_cursor", 2: "sp_cursoropen", 3: "sp_cursorprepare", 4: "sp_cursorexecute",
	5: "sp_cursorprepexec", 6: "sp_cursorunprepare", 7: "sp_cursorfetch",
	8: "sp_cursoroption", 9: "sp_cursorclose", 10: "sp_executesql", 11: "sp_prepare",
	12: "sp_execute", 13: "sp_prepexec", 14: "sp_prepexecrpc", 15: "sp_unprepare",
}

// rpcProc extracts the procedure a RPC message invokes, plus the bytes that
// follow it (the option flags and parameters). The name is either an inline
// UCS-2 string or, when the length is 0xFFFF, a well-known numeric ProcID.
func rpcProc(data []byte) (proc string, rest []byte) {
	body := afterAllHeaders(data)
	if len(body) < 2 {
		return "?", nil
	}
	nameLen := binary.LittleEndian.Uint16(body)
	if nameLen == 0xFFFF {
		if len(body) < 4 {
			return "?", nil
		}
		id := binary.LittleEndian.Uint16(body[2:])
		if name, ok := wellKnownProc[id]; ok {
			return name, body[4:]
		}
		return fmt.Sprintf("#%d", id), body[4:]
	}
	end := 2 + int(nameLen)*2
	if end > len(body) {
		return "?", nil
	}
	return ucs2(body[2:end]), body[end:]
}

// afterAllHeaders skips the ALL_HEADERS block TDS 7.2+ prefixes to SQLBatch and
// RPC messages, returning the payload proper. Same framing rule as
// sqlBatchQuery.
func afterAllHeaders(data []byte) []byte {
	if len(data) < 4 {
		return data
	}
	total := int(binary.LittleEndian.Uint32(data))
	if total >= 4 && total <= len(data) {
		return data[total:]
	}
	return data
}

// longestUCS2Text returns the longest run of printable UCS-2 text in b — a
// heuristic that surfaces the SQL inside an RPC's parameters without decoding
// TYPE_INFO for every parameter type. Diagnostics only: it identifies *where*
// the statement travels, and is never used to rewrite anything.
//
// Both byte alignments are scanned. A parameter's text does not necessarily
// begin at an even offset within the message (it follows a name, a status byte
// and a TYPE_INFO block of parameter-dependent width), and a UCS-2 scan started
// on the wrong parity reads each character straddling two real ones — turning
// legible SQL into noise. Trying both and keeping the longer run is the cheap
// fix.
func longestUCS2Text(b []byte) string {
	best := ""
	for _, off := range []int{0, 1} {
		if s := scanUCS2(b, off); len(s) > len(best) {
			best = s
		}
	}
	return best
}

func scanUCS2(b []byte, off int) string {
	var best, cur strings.Builder
	flush := func() {
		if cur.Len() > best.Len() {
			best.Reset()
			best.WriteString(cur.String())
		}
		cur.Reset()
	}
	for i := off; i+1 < len(b); i += 2 {
		r := rune(binary.LittleEndian.Uint16(b[i:]))
		if r == '\n' || r == '\r' || r == '\t' || (r < 0x80 && unicode.IsPrint(r)) {
			cur.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return strings.TrimSpace(best.String())
}

// quotePreview renders a statement on one line, truncated, so a trace stays
// greppable. Newlines become spaces: a multi-line CTE must not break the
// one-message-per-line contract.
func quotePreview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 160
	if len(s) > max {
		return fmt.Sprintf("%q…(%d bytes)", s[:max], len(s))
	}
	return fmt.Sprintf("%q", s)
}
