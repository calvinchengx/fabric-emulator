package tds

// Warehouse write observation (docs/31-flow-observability.md).
//
// The TDS front already parses every statement for dialect adaptation, so it
// can also say what a statement moved: this file reports the statement text of
// a write the engine ACCEPTED, and the caller (internal/server) turns that into
// lineage. Gold is built by dbt over TDS, so without this the flow graph stops
// at silver — the warehouse half of a medallion moves bytes the emulator can
// see but was not recording.
//
// Two rules keep this an observation rather than a guess:
//
//   - the statement is the one the client actually sent (post-dialect-fix, so
//     it is also the one the engine ran);
//   - it is reported only when the engine's response carries no error and the
//     response is one this file fully understands. A statement that failed, or
//     whose outcome cannot be read, produces nothing. Recording an edge for a
//     movement that never happened is the one failure mode worth being
//     conservative about.

import (
	"encoding/binary"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// Observer is notified of the data movements a statement performed, once the
// backend has accepted it. database is the resolved backend database (a Fabric
// item id — see warehouseRouter).
//
// It receives parsed flows rather than SQL so the statement is parsed exactly
// once, here, where the token stream is already in hand.
//
// It is called synchronously on the session goroutine, after the response has
// already been forwarded to the client, so a slow observer delays only the next
// statement on that one connection and never the client's result.
type Observer func(database string, flows []tsql.Flow)

// TDS response token types this file needs to size or recognise.
const (
	tokError      byte = 0xAA
	tokInfo       byte = 0xAB
	tokEnvChange  byte = 0xE3
	tokReturnStat byte = 0x79
	tokOrder      byte = 0xA9
	tokDone       byte = 0xFD
	tokDoneProc   byte = 0xFE
	tokDoneInProc byte = 0xFF
)

// accepted reports whether a response stream says the batch succeeded.
//
// understood is false when the walk meets a token it cannot size (a result set,
// most often) — the outcome is then unknown, and unknown is not success.
func accepted(data []byte) (ok bool, understood bool) {
	for i := 0; i < len(data); {
		switch data[i] {
		case tokError:
			return false, true // the engine rejected it; nothing moved
		case tokInfo, tokEnvChange, tokOrder:
			if i+3 > len(data) {
				return false, false
			}
			i += 3 + int(binary.LittleEndian.Uint16(data[i+1:]))
		case tokReturnStat:
			i += 5 // token + 4-byte value
		case tokDone, tokDoneProc, tokDoneInProc:
			// Status(2) CurCmd(2) DoneRowCount(8).
			if i+13 > len(data) {
				return false, false
			}
			// The error bit (0x0002) is the other way a failure is reported.
			if binary.LittleEndian.Uint16(data[i+1:])&0x0002 != 0 {
				return false, true
			}
			i += 13
		default:
			// A token this file cannot size — a result set, say. The rest of the
			// stream is unreadable from here, so the outcome is unknown.
			return false, false
		}
	}
	return true, true
}

// observeBatch reports the movements of a statement the backend accepted.
func observeBatch(obs Observer, database string, typ byte, payload, response []byte) {
	if obs == nil || database == "" {
		return
	}
	var moving []string
	for _, sql := range statementTexts(typ, payload) {
		if mightMove(sql) {
			moving = append(moving, sql)
		}
	}
	if len(moving) == 0 {
		return
	}
	if ok, understood := accepted(response); !ok || !understood {
		return
	}
	var flows []tsql.Flow
	for _, sql := range moving {
		flows = append(flows, tsql.DataFlows(sql)...)
	}
	if len(flows) == 0 {
		return
	}
	obs(database, flows)
}

// mightMove is the cheap prefilter that keeps the tokenizer off the hot path:
// an ordinary SELECT is the overwhelming majority of statements on this wire
// and moves nothing.
//
// SELECT and WITH are not simply excluded, because SELECT … INTO *is* a write —
// it is what the CTAS rewrite emits, so excluding it would drop precisely the
// statements this file exists to see. They are admitted only when the text
// contains "into" at all, which is a substring test rather than a parse.
func mightMove(sql string) bool {
	switch firstKeyword(sql) {
	case "CREATE", "INSERT", "DROP", "ALTER", "EXEC", "EXECUTE", "SP_RENAME":
		return true
	case "SELECT", "WITH":
		return strings.Contains(strings.ToLower(sql), "into")
	}
	return false
}

// statementTexts pulls the candidate SQL out of whichever message shape carries
// it: a plain batch, or the text parameters of the sp_executesql/sp_prepexec
// family the ODBC driver prefers (the same procs dialectFix rewrites through).
//
// EVERY text parameter is returned, not the first. sp_prepexec's parameter
// order is (@handle, @params, @stmt), so "the first text parameter" is the
// parameter DECLARATION — `@P1 int` — and taking it meant dbt's entire
// warehouse build went unobserved while a plain batch through the same front
// worked fine. dialectFix already iterates for exactly this reason.
func statementTexts(typ byte, data []byte) []string {
	switch typ {
	case PktSQLBatch:
		if q := sqlBatchQuery(data); q != "" {
			return []string{q}
		}
	case PktRPC:
		proc, _ := rpcProc(data)
		if !procsCarryingSQL[proc] {
			return nil
		}
		req, err := parseRPC(data)
		if err != nil {
			return nil
		}
		var out []string
		for _, p := range req.params {
			if p.isText && p.text != "" {
				out = append(out, p.text)
			}
		}
		return out
	}
	return nil
}
