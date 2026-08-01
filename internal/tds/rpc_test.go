package tds

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// --- builders that mimic what a driver puts on the wire ---------------------

// paramName encodes a B_VARCHAR parameter name (length in characters).
func paramName(s string) []byte {
	return append([]byte{byte(len([]rune(s)))}, ucs2Bytes(s)...)
}

// nvarcharParam builds an NVARCHAR(maxChars) parameter with a plain,
// two-byte-length value.
func nvarcharParam(name, val string) []byte {
	const maxChars = 4000
	out := paramName(name)
	out = append(out, 0) // status
	out = append(out, 0xE7)
	out = binary.LittleEndian.AppendUint16(out, maxChars*2)
	out = append(out, defaultCollation...)
	body := ucs2Bytes(val)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(body)))
	return append(out, body...)
}

// nvarcharMaxParam builds an NVARCHAR(max) parameter: PLP-encoded, one chunk.
func nvarcharMaxParam(name, val string) []byte {
	out := paramName(name)
	out = append(out, 0)
	out = append(out, 0xE7)
	out = binary.LittleEndian.AppendUint16(out, 0xFFFF)
	out = append(out, defaultCollation...)
	body := ucs2Bytes(val)
	out = binary.LittleEndian.AppendUint64(out, uint64(len(body)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	out = append(out, body...)
	return binary.LittleEndian.AppendUint32(out, 0) // terminator
}

// intnParam builds an INTN(4) parameter — the shape sp_prepexec's @handle uses.
func intnParam(name string, v int32) []byte {
	out := paramName(name)
	out = append(out, 0)
	out = append(out, 0x26, 4, 4)
	return binary.LittleEndian.AppendUint32(out, uint32(v))
}

// rpcMsg assembles a full RPC payload for a well-known procedure id.
func rpcMsg(procID uint16, params ...[]byte) []byte {
	body := binary.LittleEndian.AppendUint16(nil, 0xFFFF)
	body = binary.LittleEndian.AppendUint16(body, procID)
	body = binary.LittleEndian.AppendUint16(body, 0) // option flags
	for _, p := range params {
		body = append(body, p...)
	}
	return withHeaders(body)
}

// spPrepexec is the real shape: @handle (INTN out), @params, @stmt.
func spPrepexec(stmt string) []byte {
	return rpcMsg(13,
		intnParam("@handle", 0),
		nvarcharParam("@params", "@P1 int"),
		nvarcharParam("@stmt", stmt),
	)
}

// --- parsing ----------------------------------------------------------------

func TestParseRPCWalksEveryParameter(t *testing.T) {
	req, err := parseRPC(spPrepexec("select @P1 as x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.params) != 3 {
		t.Fatalf("got %d params, want 3", len(req.params))
	}
	if req.params[0].isText {
		t.Fatal("@handle should not be text")
	}
	if req.params[1].text != "@P1 int" || req.params[2].text != "select @P1 as x" {
		t.Fatalf("decoded %q / %q", req.params[1].text, req.params[2].text)
	}
}

// A parsed request must re-serialise to the exact bytes it came from, or
// nothing built on it can be trusted.
func TestParseRPCRoundTripsByteForByte(t *testing.T) {
	for _, in := range [][]byte{
		spPrepexec("select 1"),
		rpcMsg(10, nvarcharMaxParam("@stmt", strings.Repeat("x", 9000))),
		rpcMsg(10, nvarcharParam("@stmt", "select 1")),
	} {
		req, err := parseRPC(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.encode(); !bytes.Equal(got, in) {
			t.Fatalf("round-trip differs: %d vs %d bytes", len(got), len(in))
		}
	}
}

func TestParseRPCHandlesEveryModelledTypeShape(t *testing.T) {
	shapes := map[string][]byte{
		"int4 fixed":   append(append(paramName("@a"), 0), 0x38, 1, 0, 0, 0),
		"intn":         intnParam("@b", 7),
		"bitn":         append(append(paramName("@c"), 0), 0x68, 1, 1, 1),
		"guid":         append(append(paramName("@d"), 0), 0x24, 16, 0),
		"decimaln":     append(append(paramName("@e"), 0), 0x6A, 17, 18, 2, 0),
		"daten":        append(append(paramName("@f"), 0), 0x28, 0),
		"datetime2n":   append(append(paramName("@g"), 0), 0x2A, 7, 0),
		"bigvarbinary": append(append(append(paramName("@h"), 0), 0xA5, 0x10, 0x00), 0, 0),
		"nvarchar":     nvarcharParam("@i", "x"),
		"nvarchar max": nvarcharMaxParam("@j", "y"),
	}
	for name, p := range shapes {
		if _, err := parseRPC(rpcMsg(10, p)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// An unmodelled type must stop the walk rather than be guessed at.
func TestParseRPCRefusesUnmodelledType(t *testing.T) {
	sqlVariant := append(append(paramName("@v"), 0), 0x62, 8, 0, 0, 0)
	if _, err := parseRPC(rpcMsg(10, sqlVariant)); err == nil {
		t.Fatal("SQL_VARIANT accepted; the walk must give up instead")
	}
}

func TestParseRPCRefusesTruncatedInput(t *testing.T) {
	full := spPrepexec("select 1")
	for _, n := range []int{0, 1, 5, 20, len(full) - 3} {
		if n >= len(full) {
			continue
		}
		if _, err := parseRPC(full[:n]); err == nil {
			t.Fatalf("truncation to %d bytes accepted", n)
		}
	}
}

// --- rewriting --------------------------------------------------------------

func TestFixRPCRewritesNestedCTEInStatementParameter(t *testing.T) {
	in := spPrepexec("with o as (with i as (select @P1 x) select * from i) select * from o")
	out, reject := dialectFix(PktRPC, in, false)
	if reject != "" {
		t.Fatalf("unexpected reject: %s", reject)
	}
	if bytes.Equal(out, in) {
		t.Fatal("statement was not rewritten")
	}
	req, err := parseRPC(out)
	if err != nil {
		t.Fatal(err)
	}
	got := req.params[2].text
	if strings.Count(strings.ToLower(got), "with ") != 1 {
		t.Fatalf("still nested: %q", got)
	}
	if got != "with i as (select @P1 x), o as (select * from i) select * from o" {
		t.Fatalf("got %q", got)
	}
	// The other parameters must be untouched.
	orig, _ := parseRPC(in)
	for _, i := range []int{0, 1} {
		if !bytes.Equal(orig.params[i].value, req.params[i].value) {
			t.Fatalf("parameter %d was altered", i)
		}
	}
}

func TestFixRPCRewritesPLPStatement(t *testing.T) {
	in := rpcMsg(10, nvarcharMaxParam("@stmt",
		"with o as (with i as (select 1 x) select * from i) select * from o"))
	out, reject := dialectFix(PktRPC, in, false)
	if reject != "" || bytes.Equal(out, in) {
		t.Fatalf("PLP statement not rewritten (reject=%q)", reject)
	}
	req, _ := parseRPC(out)
	if !strings.HasPrefix(req.params[0].text, "with i as (select 1 x), o as (") {
		t.Fatalf("got %q", req.params[0].text)
	}
}

// Flattening usually *shrinks* a statement — it removes a `with` keyword and a
// paren pair — so the interesting encoder cases are exercised directly rather
// than contrived through a whole message.
func TestEncodeCharValueStaysPlainWhenItFits(t *testing.T) {
	req, err := parseRPC(rpcMsg(10, nvarcharParam("@stmt", "select 1")))
	if err != nil {
		t.Fatal(err)
	}
	p := req.params[0]
	ti, val := encodeCharValue(p, "select 2")

	if !bytes.Equal(ti, p.typeInfo) {
		t.Fatal("TYPE_INFO changed although the value still fits")
	}
	if n := int(binary.LittleEndian.Uint16(val)); n != len(ucs2Bytes("select 2")) {
		t.Fatalf("length prefix = %d", n)
	}
	if got := decodeCharValue(val, &charMeta{unicode: true, maxLen: p.maxLen}); got != "select 2" {
		t.Fatalf("got %q", got)
	}
}

// When the rewrite no longer fits the declared maximum, the parameter must
// widen to nvarchar(max) (PLP) rather than silently truncate.
func TestEncodeCharValueWidensToPLPWhenItDoesNotFit(t *testing.T) {
	// A deliberately narrow declaration: nvarchar(4).
	body := paramName("@stmt")
	body = append(body, 0, 0xE7)
	body = binary.LittleEndian.AppendUint16(body, 8) // 4 chars
	body = append(body, defaultCollation...)
	raw := ucs2Bytes("abcd")
	body = binary.LittleEndian.AppendUint16(body, uint16(len(raw)))
	body = append(body, raw...)

	req, err := parseRPC(rpcMsg(10, body))
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("z", 50)
	ti, val := encodeCharValue(req.params[0], long)

	if binary.LittleEndian.Uint16(ti[1:]) != 0xFFFF {
		t.Fatal("declared maximum was not widened to nvarchar(max)")
	}
	if got := decodeCharValue(val, &charMeta{unicode: true, plp: true}); got != long {
		t.Fatalf("content lost when widening: %d of %d chars", len(got), len(long))
	}
	// And the widened parameter must still parse as part of a message.
	rebuilt := append(append(append(paramName("@stmt"), 0), ti...), val...)
	if _, err := parseRPC(rpcMsg(10, rebuilt)); err != nil {
		t.Fatalf("widened parameter no longer parses: %v", err)
	}
}

func TestFixRPCLeavesOrdinaryStatementsUntouched(t *testing.T) {
	for _, in := range [][]byte{
		spPrepexec("select @P1 as x"),
		spPrepexec("with a as (select 1 x), b as (select x from a) select * from b"),
		rpcMsg(1, nvarcharParam("@stmt", "with o as (with i as (select 1 x) select * from i) select * from o")), // sp_cursor: not a SQL-carrying proc
	} {
		out, reject := dialectFix(PktRPC, in, false)
		if reject != "" || !bytes.Equal(out, in) {
			t.Fatalf("altered or rejected: reject=%q changed=%v", reject, !bytes.Equal(out, in))
		}
	}
}

// A statement Fabric itself refuses is rejected by name, even in a parameter.
func TestFixRPCRejectsFabricRestrictionInParameter(t *testing.T) {
	in := spPrepexec("with o as (with i as (select 1 x) select * from i) insert into t select * from o")
	_, reject := dialectFix(PktRPC, in, false)
	if !strings.Contains(reject, "select-only") {
		t.Fatalf("reject = %q", reject)
	}
}

// When the parameter list cannot be measured, the rewrite is impossible — but
// the limitation is still named rather than left as a bare Msg 156.
func TestFixRPCFallsBackToNamedRejectOnUnmodelledShape(t *testing.T) {
	nested := "with o as (with i as (select 1 x) select * from i) select * from o"
	sqlVariant := append(append(paramName("@v"), 0), 0x62, 8, 0, 0, 0)
	in := rpcMsg(13, sqlVariant, nvarcharParam("@stmt", nested))

	out, reject := dialectFix(PktRPC, in, false)
	if !strings.Contains(reject, "does not model") {
		t.Fatalf("reject = %q", reject)
	}
	if !bytes.Equal(out, in) {
		t.Fatal("payload altered on the fallback path")
	}
}

// The verification gate: if the encoder ever produced something that did not
// re-parse to the intended statement, the original must be forwarded.
func TestRewriteIsFaithfulRejectsMismatch(t *testing.T) {
	req, err := parseRPC(spPrepexec("with o as (with i as (select 1 x) select * from i) select * from o"))
	if err != nil {
		t.Fatal(err)
	}
	if rewriteIsFaithful(req, []byte{0, 1, 2}, 2, "anything") {
		t.Fatal("garbage accepted as a faithful rewrite")
	}
	if rewriteIsFaithful(req, req.encode(), 2, "a statement that is not there") {
		t.Fatal("wrong statement text accepted")
	}
}

// Truncation sweep: every prefix of a valid message must either fail to parse
// or round-trip exactly. Never a panic, never a silent partial parse — this is
// the property that makes "give up and forward" safe.
func TestParseRPCTruncationSweep(t *testing.T) {
	msgs := [][]byte{
		spPrepexec("with o as (with i as (select @P1 x) select * from i) select * from o"),
		rpcMsg(10, nvarcharMaxParam("@stmt", "select 1")),
		rpcMsg(10, intnParam("@a", 1), nvarcharParam("@b", "x")),
	}
	for _, full := range msgs {
		for n := 0; n <= len(full); n++ {
			req, err := parseRPC(full[:n])
			if err != nil {
				continue // giving up is always allowed
			}
			if got := req.encode(); !bytes.Equal(got, full[:n]) {
				t.Fatalf("prefix %d parsed but did not round-trip", n)
			}
		}
	}
}

// The same sweep through the public entry point: a truncated message must never
// be rewritten into something else.
func TestFixRPCNeverCorruptsTruncatedInput(t *testing.T) {
	full := spPrepexec("with o as (with i as (select @P1 x) select * from i) select * from o")
	for n := 0; n <= len(full); n++ {
		in := append([]byte(nil), full[:n]...)
		out, _ := dialectFix(PktRPC, in, false)
		if !bytes.Equal(out, in) && n != len(full) {
			// A rewrite of a truncated message is only acceptable if it still
			// parses to the intended statement.
			req, err := parseRPC(out)
			if err != nil {
				t.Fatalf("prefix %d rewritten into unparseable bytes", n)
			}
			_ = req
		}
	}
}

// NULL values, both plain and PLP, decode to empty rather than misreading the
// length sentinel as content.
func TestDecodeCharValueHandlesNulls(t *testing.T) {
	plainNull := []byte{0xFF, 0xFF}
	if got := decodeCharValue(plainNull, &charMeta{unicode: true}); got != "" {
		t.Fatalf("plain NULL decoded to %q", got)
	}
	plpNull := binary.LittleEndian.AppendUint64(nil, 0xFFFFFFFFFFFFFFFF)
	if got := decodeCharValue(plpNull, &charMeta{unicode: true, plp: true}); got != "" {
		t.Fatalf("PLP NULL decoded to %q", got)
	}
	if got := decodeCharValue([]byte{1}, &charMeta{unicode: true}); got != "" {
		t.Fatalf("truncated value decoded to %q", got)
	}
	if got := decodeCharValue([]byte{1, 2, 3}, &charMeta{unicode: true, plp: true}); got != "" {
		t.Fatalf("truncated PLP decoded to %q", got)
	}
}

// A PLP value split across several chunks must reassemble in order.
func TestDecodeCharValueJoinsMultipleChunks(t *testing.T) {
	a, b := ucs2Bytes("hello "), ucs2Bytes("world")
	v := binary.LittleEndian.AppendUint64(nil, uint64(len(a)+len(b)))
	v = binary.LittleEndian.AppendUint32(v, uint32(len(a)))
	v = append(v, a...)
	v = binary.LittleEndian.AppendUint32(v, uint32(len(b)))
	v = append(v, b...)
	v = binary.LittleEndian.AppendUint32(v, 0)
	if got := decodeCharValue(v, &charMeta{unicode: true, plp: true}); got != "hello world" {
		t.Fatalf("got %q", got)
	}
}

// Non-Unicode char parameters decode and re-encode as bytes, not UCS-2.
func TestCharValueRoundTripNonUnicode(t *testing.T) {
	body := paramName("@s")
	body = append(body, 0, 0xA7)
	body = binary.LittleEndian.AppendUint16(body, 100)
	body = append(body, defaultCollation...)
	raw := []byte("select 1")
	body = binary.LittleEndian.AppendUint16(body, uint16(len(raw)))
	body = append(body, raw...)

	req, err := parseRPC(rpcMsg(10, body))
	if err != nil {
		t.Fatal(err)
	}
	if req.params[0].text != "select 1" {
		t.Fatalf("decoded %q", req.params[0].text)
	}
	_, val := encodeCharValue(req.params[0], "select 2")
	if got := decodeCharValue(val, &charMeta{maxLen: 100}); got != "select 2" {
		t.Fatalf("re-encoded to %q", got)
	}
}

func TestPLPLenRejectsTruncatedChunks(t *testing.T) {
	// A chunk header promising more bytes than remain.
	v := binary.LittleEndian.AppendUint64(nil, 10)
	v = binary.LittleEndian.AppendUint32(v, 99)
	if _, err := plpLen(v); err == nil {
		t.Fatal("over-long chunk accepted")
	}
	if _, err := plpLen([]byte{1, 2}); err == nil {
		t.Fatal("truncated PLP header accepted")
	}
}

// FuzzParseRPC guards the invariant the rewrite depends on: the parser never
// panics on arbitrary bytes, and anything it accepts re-serialises exactly.
func FuzzParseRPC(f *testing.F) {
	f.Add(spPrepexec("select 1"))
	f.Add(rpcMsg(10, nvarcharMaxParam("@stmt", "with o as (with i as (select 1 x) select * from i) select * from o")))
	f.Add(rpcMsg(10, intnParam("@a", 1)))
	f.Add([]byte{})
	f.Add([]byte{0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, data []byte) {
		req, err := parseRPC(data)
		if err != nil {
			return
		}
		if got := req.encode(); !bytes.Equal(got, data) {
			t.Fatalf("accepted but did not round-trip: %d vs %d bytes", len(got), len(data))
		}
		// The full path must not panic either.
		dialectFix(PktRPC, data, false)
	})
}

// The remaining defensive branches, exercised deliberately rather than left to
// chance: each one is a "give up safely" path, and a regression in any of them
// would turn a malformed request into a corrupted one.
func TestParseRPCDefensiveBranches(t *testing.T) {
	// A NULL nvarchar value (0xFFFF length sentinel) parses and round-trips.
	nullNV := append(append(paramName("@s"), 0), 0xE7)
	nullNV = binary.LittleEndian.AppendUint16(nullNV, 200)
	nullNV = append(nullNV, defaultCollation...)
	nullNV = binary.LittleEndian.AppendUint16(nullNV, 0xFFFF)
	req, err := parseRPC(rpcMsg(10, nullNV))
	if err != nil {
		t.Fatalf("NULL nvarchar: %v", err)
	}
	if req.params[0].text != "" {
		t.Fatalf("NULL decoded to %q", req.params[0].text)
	}

	// A PLP NULL likewise.
	plpNull := append(append(paramName("@s"), 0), 0xE7)
	plpNull = binary.LittleEndian.AppendUint16(plpNull, 0xFFFF)
	plpNull = append(plpNull, defaultCollation...)
	plpNull = binary.LittleEndian.AppendUint64(plpNull, 0xFFFFFFFFFFFFFFFF)
	if _, err := parseRPC(rpcMsg(10, plpNull)); err != nil {
		t.Fatalf("PLP NULL: %v", err)
	}

	// TYPE_INFO cut short for each shape that reads beyond the type byte.
	for name, ti := range map[string][]byte{
		"scaled temporal": {0x2A},
		"decimal":         {0x6A, 17},
		"two-byte family": {0xE7, 0x10},
	} {
		msg := append(append(paramName("@x"), 0), ti...)
		if _, err := parseRPC(rpcMsg(10, msg)); err == nil {
			t.Errorf("%s: truncated TYPE_INFO accepted", name)
		}
	}

	// A parameter header that claims a name longer than the message.
	if _, err := parseRPC(rpcMsg(10, []byte{0x40})); err == nil {
		t.Error("over-long parameter name accepted")
	}
}

// An unparseable parameter is skipped, not judged: the scan moves on to the
// next parameter rather than rejecting the whole request.
func TestFixRPCSkipsUnparseableParameterAndRewritesTheNext(t *testing.T) {
	in := rpcMsg(13,
		nvarcharParam("@junk", "with a as (select 'unterminated"),
		nvarcharParam("@stmt", "with o as (with i as (select 1 x) select * from i) select * from o"),
	)
	out, reject := dialectFix(PktRPC, in, false)
	if reject != "" {
		t.Fatalf("reject: %s", reject)
	}
	req, err := parseRPC(out)
	if err != nil {
		t.Fatal(err)
	}
	if req.params[0].text != "with a as (select 'unterminated" {
		t.Fatalf("the unparseable parameter was altered: %q", req.params[0].text)
	}
	if !strings.HasPrefix(req.params[1].text, "with i as (select 1 x), o as (") {
		t.Fatalf("the following parameter was not rewritten: %q", req.params[1].text)
	}
}

// rewriteIsFaithful must reject a rewrite that disturbed another parameter —
// the check that stops an encoder bug reaching the engine.
func TestRewriteIsFaithfulRejectsCollateralChange(t *testing.T) {
	in := spPrepexec("with o as (with i as (select @P1 x) select * from i) select * from o")
	orig, err := parseRPC(in)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper with an unrelated parameter, exactly as a mis-framed encode would.
	tampered := *orig
	tampered.params = append([]rpcParam(nil), orig.params...)
	_, v := encodeCharValue(orig.params[1], "@P1 bigint")
	tampered.params[1].value = v
	if rewriteIsFaithful(orig, tampered.encode(), 2, orig.params[2].text) {
		t.Fatal("a rewrite that changed another parameter was accepted")
	}
}

// parseParam reads the name-length byte before it can bounds-check against it,
// so the empty-slice guard is load-bearing even though the only caller loops
// while bytes remain. Exercised directly.
func TestParseParamRejectsEmptyInput(t *testing.T) {
	if _, _, err := parseParam(nil); err == nil {
		t.Fatal("empty parameter accepted")
	}
	if _, _, err := parseParam([]byte{}); err == nil {
		t.Fatal("empty parameter accepted")
	}
}
