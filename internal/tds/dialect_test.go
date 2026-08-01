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
	out, reject := dialectFix(PktSQLBatch, withHeaders(ucs2Bytes(nestedSQL)), false)
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
		out, reject := dialectFix(PktSQLBatch, in, false)
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
	_, reject := dialectFix(PktSQLBatch, withHeaders(ucs2Bytes(sql)), false)
	if !strings.Contains(reject, "select-only") {
		t.Fatalf("reject = %q", reject)
	}
}

func TestDialectFixRejectsShadowedNames(t *testing.T) {
	sql := "with c as (with c as (select 1 x) select * from c) select * from c"
	_, reject := dialectFix(PktSQLBatch, withHeaders(ucs2Bytes(sql)), false)
	if !strings.Contains(reject, "nesting level") {
		t.Fatalf("reject = %q", reject)
	}
}

// A statement this package cannot parse is forwarded untouched: the engine is
// the authority on its own dialect, and guessing is worse than passing through.
func TestDialectFixForwardsUnparseableStatements(t *testing.T) {
	sql := "with a as (select 'unterminated"
	in := withHeaders(ucs2Bytes(sql))
	out, reject := dialectFix(PktSQLBatch, in, false)
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
	_, reject := dialectFix(PktRPC, rpcByProcID(13, nestedSQL), false) // 13 = sp_prepexec
	if !strings.Contains(reject, "sp_prepexec") || !strings.Contains(reject, "nested CTE") {
		t.Fatalf("reject = %q", reject)
	}
}

func TestDialectFixLeavesOrdinaryRPCsAlone(t *testing.T) {
	for _, data := range [][]byte{
		rpcByProcID(13, "select @P1 as x"),      // parameterized, no nesting
		rpcByProcID(10, "with a as (select 1)"), // sequential CTE only
	} {
		if _, reject := dialectFix(PktRPC, data, false); reject != "" {
			t.Fatalf("ordinary RPC rejected: %s", reject)
		}
	}
}

// Driver metadata RPCs carry no statement and must never be inspected for one.
func TestDialectFixIgnoresMetadataRPC(t *testing.T) {
	name := "sp_datatype_info_100"
	body := ucs2Bytes(name)
	msg := withHeaders(append(lenPrefix(len(name)), body...))
	if _, reject := dialectFix(PktRPC, msg, false); reject != "" {
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
		out, reject := dialectFix(typ, in, false)
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
	if _, reject := dialectFix(PktRPC, withHeaders([]byte{0xFF, 0xFF}), false); reject != "" {
		t.Fatalf("truncated RPC rejected: %s", reject)
	}
}

// Strict mode is a gate, not a default: the same statement must pass without
// it and be refused with it.
func TestStrictModeGatesClassBConstructs(t *testing.T) {
	recursive := "with r as (select 1 n union all select n+1 from r where n < 5) select * from r"
	in := withHeaders(ucs2Bytes(recursive))

	out, reject := dialectFix(PktSQLBatch, in, false)
	if reject != "" {
		t.Fatalf("refused with strict mode OFF: %s", reject)
	}
	if string(out) != string(in) {
		t.Fatal("statement altered with strict mode off")
	}

	out, reject = dialectFix(PktSQLBatch, in, true)
	if !strings.Contains(reject, "recursive-cte") {
		t.Fatalf("not refused with strict mode ON: %q", reject)
	}
	if string(out) != string(in) {
		t.Fatal("a refused statement must be returned untouched")
	}
}

// The gate applies to statements carried as RPC parameters too.
func TestStrictModeGatesClassBInRPCParameter(t *testing.T) {
	recursive := "with r as (select 1 n union all select n+1 from r) select * from r"
	in := spPrepexec(recursive)

	if _, reject := dialectFix(PktRPC, in, false); reject != "" {
		t.Fatalf("refused with strict mode OFF: %s", reject)
	}
	if _, reject := dialectFix(PktRPC, in, true); !strings.Contains(reject, "recursive-cte") {
		t.Fatalf("not refused with strict mode ON: %q", reject)
	}
}

// Strict mode must not disturb what T6 already handles: a nested CTE is still
// rewritten, because Fabric runs it.
func TestStrictModeStillRewritesNestedCTEs(t *testing.T) {
	in := withHeaders(ucs2Bytes(nestedSQL))
	out, reject := dialectFix(PktSQLBatch, in, true)
	if reject != "" {
		t.Fatalf("nested CTE refused in strict mode: %s", reject)
	}
	if !strings.HasPrefix(sqlBatchQuery(out), "with i as (select 1 x), o as (") {
		t.Fatalf("not rewritten: %q", sqlBatchQuery(out))
	}
}

func TestStrictRejectIsInertWhenDisabled(t *testing.T) {
	if msg := strictReject("create trigger t on x after insert as select 1", false); msg != "" {
		t.Fatalf("strict check ran while disabled: %s", msg)
	}
	if msg := strictReject("create trigger t on x after insert as select 1", true); msg == "" {
		t.Fatal("strict check did not run while enabled")
	}
}

// T8: a CTAS must reach the sidecar as SELECT … INTO.
func TestDialectFixRewritesCTAS(t *testing.T) {
	in := withHeaders(ucs2Bytes("create table dst as select a, b from src where x = 1"))
	out, reject := dialectFix(PktSQLBatch, in, false)
	if reject != "" {
		t.Fatalf("reject: %s", reject)
	}
	if got := sqlBatchQuery(out); got != "select a, b into dst from src where x = 1" {
		t.Fatalf("got %q", got)
	}
}

// CTAS carried as a parameterized statement is rewritten too.
func TestDialectFixRewritesCTASInRPC(t *testing.T) {
	in := spPrepexec("create table dst as select a from src where k = @P1")
	out, reject := dialectFix(PktRPC, in, false)
	if reject != "" {
		t.Fatalf("reject: %s", reject)
	}
	req, err := parseRPC(out)
	if err != nil {
		t.Fatal(err)
	}
	if req.params[2].text != "select a into dst from src where k = @P1" {
		t.Fatalf("got %q", req.params[2].text)
	}
}
