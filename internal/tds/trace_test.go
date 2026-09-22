package tds

import (
	"encoding/binary"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
)

// ucs2Bytes encodes s the way a TDS client does (UTF-16LE).
func ucs2Bytes(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, u := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// withHeaders prefixes the ALL_HEADERS block TDS 7.2+ clients send.
func withHeaders(payload []byte) []byte {
	const hdrLen = 22 // one 18-byte transaction descriptor header + the 4-byte total
	out := binary.LittleEndian.AppendUint32(nil, hdrLen)
	out = append(out, make([]byte, hdrLen-4)...)
	return append(out, payload...)
}

// rpcByProcID builds an RPC message that calls a well-known procedure by id,
// with sql sitting in the parameter area — how a driver sends sp_executesql.
func rpcByProcID(id uint16, sql string) []byte {
	body := binary.LittleEndian.AppendUint16(nil, 0xFFFF)
	body = binary.LittleEndian.AppendUint16(body, id)
	body = binary.LittleEndian.AppendUint16(body, 0) // option flags
	return withHeaders(append(body, ucs2Bytes(sql)...))
}

func TestDescribeRequestSQLBatch(t *testing.T) {
	got := describeRequest(PktSQLBatch, withHeaders(ucs2Bytes("select 1")))
	if !strings.Contains(got, "SQLBatch") || !strings.Contains(got, `"select 1"`) {
		t.Fatalf("got %q", got)
	}
}

func TestDescribeRequestRPCByProcID(t *testing.T) {
	// The case that decides T6a's scope: sp_executesql carrying a nested CTE.
	sql := "with a as (with b as (select 1 x) select * from b) select * from a"
	got := describeRequest(PktRPC, rpcByProcID(10, sql))
	if !strings.Contains(got, "proc=sp_executesql") {
		t.Fatalf("proc not identified: %q", got)
	}
	if !strings.Contains(got, "with a as") {
		t.Fatalf("SQL text not surfaced: %q", got)
	}
}

func TestDescribeRequestRPCByName(t *testing.T) {
	name := "sp_who"
	body := binary.LittleEndian.AppendUint16(nil, uint16(len(name)))
	body = append(body, ucs2Bytes(name)...)
	got := describeRequest(PktRPC, withHeaders(body))
	if !strings.Contains(got, "proc=sp_who") {
		t.Fatalf("got %q", got)
	}
}

func TestDescribeRequestUnknownProcID(t *testing.T) {
	got := describeRequest(PktRPC, rpcByProcID(999, ""))
	if !strings.Contains(got, "proc=#999") {
		t.Fatalf("got %q", got)
	}
}

func TestDescribeRequestOtherTypes(t *testing.T) {
	for _, tc := range []struct {
		typ  byte
		want string
	}{
		{PktAttention, "Attention"},
		{PktBulkLoad, "BulkLoad"},
		{PktLogin7, "type=0x10"},
	} {
		if got := describeRequest(tc.typ, []byte{1, 2, 3}); !strings.Contains(got, tc.want) {
			t.Fatalf("type %#x: got %q, want %q", tc.typ, got, tc.want)
		}
	}
}

// Malformed input is live traffic from third-party drivers: describeRequest
// must degrade, never panic.
func TestDescribeRequestMalformed(t *testing.T) {
	for _, data := range [][]byte{nil, {}, {0x01}, {0xff, 0xff, 0xff, 0xff}, {0x16, 0, 0, 0}} {
		describeRequest(PktRPC, data)
		describeRequest(PktSQLBatch, data)
	}
}

func TestQuotePreviewTruncatesAndFlattens(t *testing.T) {
	if got := quotePreview("select\n  1"); got != `"select 1"` {
		t.Fatalf("newlines not flattened: %q", got)
	}
	long := quotePreview(strings.Repeat("x", 500))
	if !strings.Contains(long, "…(500 bytes)") {
		t.Fatalf("not truncated: %q", long)
	}
}

func TestTraceFuncHookFiresOnlyWhenSet(t *testing.T) {
	traceRequest(PktSQLBatch, withHeaders(ucs2Bytes("select 1"))) // unset: must not panic

	var lines []string
	SetTraceFunc(func(l string) { lines = append(lines, l) })
	t.Cleanup(func() { SetTraceFunc(nil) })

	traceRequest(PktSQLBatch, withHeaders(ucs2Bytes("select 2")))
	if len(lines) != 1 || !strings.Contains(lines[0], "select 2") {
		t.Fatalf("got %v", lines)
	}

	SetTraceFunc(nil)
	traceRequest(PktSQLBatch, withHeaders(ucs2Bytes("select 3")))
	if len(lines) != 1 {
		t.Fatalf("a cleared hook still fired: %v", lines)
	}
}

// The hook is set and cleared while connections are live -- by tests in this
// package, whose Serve goroutines outlive them because Serve offers no way to
// wait for its handlers. This is the shape CI run #1709 failed on, made
// deterministic: without the test's ordering luck, a reader on another
// goroutine and a writer here. Meaningful only under -race, which is how
// `make test-race` runs it; a plain variable fails it there.
func TestTraceHookIsSafeToChangeWhileConnectionsRead(t *testing.T) {
	t.Cleanup(func() { SetTraceFunc(nil) })
	msg := withHeaders(ucs2Bytes("select 1"))

	var fired atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // a connection's handler, reading the hook per message
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				traceRequest(PktSQLBatch, msg)
			}
		}
	}()
	for i := 0; i < 200; i++ { // a test setting and clearing it meanwhile
		SetTraceFunc(func(string) { fired.Add(1) })
		SetTraceFunc(nil)
	}
	SetTraceFunc(func(string) { fired.Add(1) })
	deadline := time.Now().Add(5 * time.Second)
	for fired.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(stop)
	<-done
	if fired.Load() == 0 {
		t.Fatal("the reader never observed an installed hook")
	}
}

// A parameter's text can start at an odd offset; a fixed-parity scan would read
// it straddled and return noise. Both alignments must be tried.
func TestLongestUCS2TextFindsOddAlignedText(t *testing.T) {
	sql := "with o as (with i as (select 1 x) select * from i) select * from o"
	msg := append([]byte{0x00}, ucs2Bytes(sql)...) // shift onto an odd boundary
	if got := longestUCS2Text(msg); got != sql {
		t.Fatalf("odd-aligned text not recovered:\n got %q\nwant %q", got, sql)
	}
	if got := longestUCS2Text(ucs2Bytes(sql)); got != sql {
		t.Fatalf("even-aligned text regressed: %q", got)
	}
}
