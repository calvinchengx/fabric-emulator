package tds

import (
	"bytes"
	"strings"
	"testing"
)

const nestedSQL = "with o as (with i as (select 1 x) select * from i) select * from o"

// The re-encoded payload must be readable by the same decoder the relay uses,
// and must preserve the ALL_HEADERS block byte for byte — it describes the
// request, not the statement.
func TestRewriteBatchPreservesHeadersAndRoundTrips(t *testing.T) {
	orig := withHeaders(ucs2Bytes("select 1"))
	headerLen := 22

	out := rewriteBatch(orig, nestedSQL)

	if got := string(out[:headerLen]); got != string(orig[:headerLen]) {
		t.Fatalf("ALL_HEADERS altered")
	}
	if got := sqlBatchQuery(out); got != nestedSQL {
		t.Fatalf("round-trip:\n got %q\nwant %q", got, nestedSQL)
	}
	// The original must not be aliased or mutated.
	if sqlBatchQuery(orig) != "select 1" {
		t.Fatal("rewriteBatch mutated its input")
	}
}

// Older clients send no ALL_HEADERS; the whole payload is the statement.
func TestRewriteBatchWithoutHeaders(t *testing.T) {
	out := rewriteBatch(ucs2Bytes("select 1"), nestedSQL)
	if got := sqlBatchQuery(out); got != nestedSQL {
		t.Fatalf("got %q", got)
	}
}

// A statement large enough to span several TDS packets must survive the
// re-encode; WriteMessage owns the chunking, so this asserts the payload is
// intact at whatever length results.
func TestRewriteBatchHandlesOversizedStatement(t *testing.T) {
	big := "with o as (with i as (select 1 x /*" + strings.Repeat("p", 20000) + "*/) select * from i) select * from o"
	out := rewriteBatch(withHeaders(ucs2Bytes("select 1")), big)
	if got := sqlBatchQuery(out); got != big {
		t.Fatalf("oversized round-trip lost %d bytes", len(big)-len(got))
	}
	if len(out) <= maxPacketData {
		t.Fatalf("test statement is not actually oversized (%d bytes)", len(out))
	}
}

func TestDialectFixFlattensNestedBatch(t *testing.T) {
	out, reject := dialectFix(PktSQLBatch, withHeaders(ucs2Bytes(nestedSQL)))
	if reject != "" {
		t.Fatalf("unexpected reject: %s", reject)
	}
	got := sqlBatchQuery(out)
	if strings.Count(strings.ToLower(got), "with ") != 1 {
		t.Fatalf("not flattened: %q", got)
	}
	if !strings.HasPrefix(got, "with i as (select 1 x), o as (") {
		t.Fatalf("got %q", got)
	}
}

// Nothing to rewrite must forward the exact bytes, so an untouched statement
// costs no re-encode and cannot be corrupted by one.
func TestDialectFixForwardsUnaffectedBatchesByteIdentical(t *testing.T) {
	for _, sql := range []string{
		"select 1",
		"with a as (select 1 x), b as (select x from a) select * from b",
		"select 'with o as (with i as (select 1) select 1)' as literal",
	} {
		in := withHeaders(ucs2Bytes(sql))
		out, reject := dialectFix(PktSQLBatch, in)
		if reject != "" {
			t.Fatalf("%q rejected: %s", sql, reject)
		}
		if string(out) != string(in) {
			t.Fatalf("%q was re-encoded unnecessarily", sql)
		}
	}
}

// A statement Fabric itself refuses is rejected here, naming the rule.
func TestDialectFixRejectsFabricRestriction(t *testing.T) {
	sql := "with o as (with i as (select 1 x) select * from i) insert into t select * from o"
	_, reject := dialectFix(PktSQLBatch, withHeaders(ucs2Bytes(sql)))
	if !strings.Contains(reject, "select-only") {
		t.Fatalf("reject = %q", reject)
	}
}

func TestDialectFixRejectsShadowedNames(t *testing.T) {
	sql := "with c as (with c as (select 1 x) select * from c) select * from c"
	_, reject := dialectFix(PktSQLBatch, withHeaders(ucs2Bytes(sql)))
	if !strings.Contains(reject, "nesting level") {
		t.Fatalf("reject = %q", reject)
	}
}

// A statement this package cannot parse is forwarded untouched: the engine is
// the authority on its own dialect, and guessing is worse than passing through.
func TestDialectFixForwardsUnparseableStatements(t *testing.T) {
	sql := "with a as (select 'unterminated"
	in := withHeaders(ucs2Bytes(sql))
	out, reject := dialectFix(PktSQLBatch, in)
	if reject != "" {
		t.Fatalf("unparseable statement rejected: %s", reject)
	}
	if string(out) != string(in) {
		t.Fatal("unparseable statement was altered")
	}
}

// T6a: a parameterized nested CTE arrives as RPC sp_prepexec, which T6e cannot
// rewrite — reject by name rather than let it surface as a bare Msg 156.
func TestDialectFixRejectsNestedCTEInRPC(t *testing.T) {
	_, reject := dialectFix(PktRPC, rpcByProcID(13, nestedSQL)) // 13 = sp_prepexec
	if !strings.Contains(reject, "sp_prepexec") || !strings.Contains(reject, "nested CTE") {
		t.Fatalf("reject = %q", reject)
	}
}

func TestDialectFixLeavesOrdinaryRPCsAlone(t *testing.T) {
	for _, data := range [][]byte{
		rpcByProcID(13, "select @P1 as x"),      // parameterized, no nesting
		rpcByProcID(10, "with a as (select 1)"), // sequential CTE only
	} {
		if _, reject := dialectFix(PktRPC, data); reject != "" {
			t.Fatalf("ordinary RPC rejected: %s", reject)
		}
	}
}

// Driver metadata RPCs carry no statement and must never be inspected for one.
func TestDialectFixIgnoresMetadataRPC(t *testing.T) {
	name := "sp_datatype_info_100"
	body := ucs2Bytes(name)
	msg := withHeaders(append(lenPrefix(len(name)), body...))
	if _, reject := dialectFix(PktRPC, msg); reject != "" {
		t.Fatalf("metadata RPC rejected: %s", reject)
	}
}

func TestDialectRejectIsAnErrorTokenStream(t *testing.T) {
	out := dialectReject("boom")
	if len(out) == 0 || out[0] != 0xAA {
		t.Fatalf("not an ERROR token: %#v", out[:1])
	}
	// The message is UCS-2 inside the token, alongside the server name — search
	// for the encoded bytes rather than the longest printable run.
	if !bytes.Contains(out, str2ucs2("boom")) {
		t.Fatal("message not carried in the token")
	}
	if !bytes.Contains(out, []byte{0xFD}) {
		t.Fatal("no DONE token after the error")
	}
}

// lenPrefix encodes an RPC's procedure-name length (in characters).
func lenPrefix(n int) []byte {
	return []byte{byte(n), byte(n >> 8)}
}

// Message types that carry no statement are forwarded without inspection.
func TestDialectFixIgnoresNonStatementMessages(t *testing.T) {
	for _, typ := range []byte{PktAttention, PktBulkLoad, PktLogin7, PktPreLogin} {
		in := []byte{1, 2, 3}
		out, reject := dialectFix(typ, in)
		if reject != "" || string(out) != string(in) {
			t.Fatalf("type %#x: reject=%q altered=%v", typ, reject, string(out) != string(in))
		}
	}
}

// A truncated RPC header must not panic or be mistaken for a statement.
func TestRPCProcHandlesTruncatedProcID(t *testing.T) {
	if proc, _ := rpcProc(withHeaders([]byte{0xFF, 0xFF})); proc != "?" {
		t.Fatalf("proc = %q, want ?", proc)
	}
	if _, reject := dialectFix(PktRPC, withHeaders([]byte{0xFF, 0xFF})); reject != "" {
		t.Fatalf("truncated RPC rejected: %s", reject)
	}
}
