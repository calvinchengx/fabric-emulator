package onelake

// The SAS redaction in trace.go, and the two trace lines it protects
// (docs/64-logging-quality.md, F7).
//
// WHY THIS IS A TEST AND NOT A REVIEW NOTE. The trace is the one place in this
// package whose only observable is the log, so a redactor that silently stopped
// redacting would look exactly like a redactor that works. The end-to-end cases
// at the bottom drive a real request through ServeHTTP and ServeBlob and read
// the emitted line back, which is what pins the redactor to the CALL SITES
// rather than only to the function.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A realistic service SAS, as a storage client appends it.
const aRealSAS = "sv=2022-11-02&ss=bf&srt=sco&sp=rwdlacupiytfx&" +
	"se=2026-10-20T00:00:00Z&st=2026-10-19T00:00:00Z&spr=https&" +
	"sig=9%2FabcDEF123secretsignature%3D"

func TestRedactSASKeepsTheNamesAndDropsTheValues(t *testing.T) {
	got := redactSAS(aRealSAS)

	// The secret must be gone -- this is the whole point.
	for _, leak := range []string{"secretsignature", "9%2FabcDEF123secretsignature%3D"} {
		if strings.Contains(got, leak) {
			t.Fatalf("the signature survived redaction: %q", got)
		}
	}
	// ...and so must its signed inputs, for the reason sasParams records: a
	// reader with `sig` from elsewhere plus these from here holds a credential.
	for _, leak := range []string{"2022-11-02", "rwdlacupiytfx", "2026-10-20T00:00:00Z"} {
		if strings.Contains(got, leak) {
			t.Errorf("a signed SAS input survived redaction: %q in %q", leak, got)
		}
	}
	// But every parameter NAME survives, which is what keeps the trace useful:
	// a missing `se` or a wrong `sp` is the usual reason a client is refused.
	for _, name := range []string{"sv=", "ss=", "srt=", "sp=", "se=", "st=", "spr=", "sig="} {
		if !strings.Contains(got, name) {
			t.Errorf("parameter name %q was lost; the trace can no longer say "+
				"which SAS fields were presented: %q", name, got)
		}
	}
	if n := strings.Count(got, redactedValue); n != 8 {
		t.Errorf("redacted %d of 8 parameters: %q", n, got)
	}
}

// TestAnOrdinaryQuerySurvivesVerbatim. The negative half, and the one that
// decides whether this redactor is worth having rather than merely safe: the
// trace exists to show what the client sent, and these are the parameters that
// explain a OneLake request.
func TestAnOrdinaryQuerySurvivesVerbatim(t *testing.T) {
	for _, q := range []string{
		"resource=filesystem&recursive=true&directory=lake.Lakehouse/Files",
		"action=flush&position=4096&close=true",
		"comp=list&prefix=lake.Lakehouse/Tables/t/_delta_log/&delimiter=%2F",
		"comp=block&blockid=QUFBQQ%3D%3D",
		"restype=container&comp=list&maxresults=5000&marker=abc",
	} {
		if got := redactSAS(q); got != q {
			t.Errorf("an ordinary query was altered:\n  in:  %s\n  out: %s", q, got)
		}
	}
}

// TestRedactionIsCaseInsensitiveOnTheParameterName. Azure's own documentation
// spells these lowercase, but a query parameter name is not case-normalised
// anywhere on the wire, so `SIG=` must not be a bypass.
func TestRedactionIsCaseInsensitiveOnTheParameterName(t *testing.T) {
	got := redactSAS("SIG=secretsignature&Sv=2022-11-02&resource=filesystem")
	if strings.Contains(got, "secretsignature") || strings.Contains(got, "2022-11-02") {
		t.Fatalf("an upper-case SAS parameter was not redacted: %q", got)
	}
	if !strings.Contains(got, "resource=filesystem") {
		t.Errorf("the ordinary parameter was lost: %q", got)
	}
	// The name is echoed as the client spelled it, not normalised -- the trace
	// is a record of the request, not a correction of it.
	if !strings.Contains(got, "SIG=") || !strings.Contains(got, "Sv=") {
		t.Errorf("the parameter names were rewritten: %q", got)
	}
}

// TestTheRedactorDoesNotInventAnAssignment. `?sig` with no `=` carries no
// secret, and `se=` with nothing after it is a client bug worth seeing --
// REDACTED in either place would report something the client never sent, which
// is the one thing a diagnostic must not do.
func TestTheRedactorDoesNotInventAnAssignment(t *testing.T) {
	for _, q := range []string{"sig", "se=", "sig&resource=filesystem", "sv=&sig="} {
		if got := redactSAS(q); got != q {
			t.Errorf("valueless parameter altered:\n  in:  %s\n  out: %s", q, got)
		}
	}
	if got := redactSAS(""); got != "" {
		t.Errorf("empty query = %q", got)
	}
}

// TestRedactSASURLKeepsThePathAndDropsTheSignature. A copy source is the
// sharper case: `x-ms-copy-source` is by convention a full SAS URL, so the
// header alone was a working credential in the log. The blob PATH is what the
// reader needs and it must survive.
func TestRedactSASURLKeepsThePathAndDropsTheSignature(t *testing.T) {
	src := "https://acct.blob.core.windows.net/onelake/ws/item/Files/src.txt?" + aRealSAS
	got := redactSASURL(src)

	if strings.Contains(got, "secretsignature") {
		t.Fatalf("the copy source's signature survived: %q", got)
	}
	if !strings.Contains(got, "/onelake/ws/item/Files/src.txt") {
		t.Errorf("the copy source lost its path, which is what a reader needs: %q", got)
	}
	if !strings.Contains(got, "sig=") {
		t.Errorf("the line no longer shows that a SAS was presented at all: %q", got)
	}
	// A copy source with no query is unchanged -- the in-OneLake form every
	// test in blob_test.go uses.
	plain := "http://onelake.blob.fabric.microsoft.com/onelake/ws/item/Files/src.txt"
	if got := redactSASURL(plain); got != plain {
		t.Errorf("a query-less copy source was altered:\n  in:  %s\n  out: %s", plain, got)
	}
	if got := redactSASURL(""); got != "" {
		t.Errorf("empty source = %q", got)
	}
}

// --------------------------------------------------------------------------
// ...and at the call sites, which is where a redactor stops being theoretical.
// --------------------------------------------------------------------------

// TestTheDFSTraceRedactsTheQueryAndTheRenameSource drives a real request and
// reads the emitted line back. A redactor applied in trace.go and forgotten at
// the call site is the failure mode the unit tests above cannot see.
func TestTheDFSTraceRedactsTheQueryAndTheRenameSource(t *testing.T) {
	t.Setenv("FABRIC_ONELAKE_TRACE", "1")
	buf := captureOneLakeLog(t)
	f := newFixture(t)

	path := "/" + f.ws.ID + "/" + f.it.ID + "/Files/traced.txt"
	f.do("PUT", path+"?resource=file", f.token, []byte("hi"))

	// Built inline rather than through f.do, which takes no headers: the
	// rename source is half of what this test is about.
	r := httptest.NewRequest("GET", path+"?"+aRealSAS, nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("x-ms-rename-source", "/ws/item/Files/other.txt?"+aRealSAS)
	f.svc.ServeHTTP(httptest.NewRecorder(), r)

	out := buf.String()
	if !strings.Contains(out, "onelake-dfs: GET") {
		t.Fatalf("the trace did not fire, so this test asserts nothing:\n%s", out)
	}
	if strings.Contains(out, "secretsignature") {
		t.Fatalf("the DFS trace leaked a SAS signature:\n%s", out)
	}
	if !strings.Contains(out, "sig="+redactedValue) {
		t.Errorf("the DFS trace no longer records that a SAS was presented:\n%s", out)
	}
	// The rename source is a separate argument and a separate mistake.
	if !strings.Contains(out, "/ws/item/Files/other.txt?") {
		t.Errorf("the rename source lost its path:\n%s", out)
	}
	// The tag and format are unchanged, so `grep 'onelake-dfs: '` still works
	// and docs/logging-subsystems.json's tag is still emitted.
	if !strings.Contains(out, "range=") || !strings.Contains(out, "rename=") {
		t.Errorf("the trace format changed; the documented grep no longer selects it:\n%s", out)
	}
}

// TestTheBlobTraceRedactsTheCopySource. The Blob dialect is what delta-rs
// speaks, and `x-ms-copy-source` is how object_store implements rename -- so
// this is the line most likely to carry a real SAS in a real stack.
func TestTheBlobTraceRedactsTheCopySource(t *testing.T) {
	t.Setenv("FABRIC_ONELAKE_TRACE", "1")
	buf := captureOneLakeLog(t)
	f := newFixture(t)

	src := "/" + f.ws.ID + "/" + f.it.ID + "/Files/src.txt"
	if w := f.doBlob("PUT", src, f.token, []byte("payload"), nil); w.Code != http.StatusCreated {
		t.Fatalf("seed = %d", w.Code)
	}
	f.doBlob("PUT", "/"+f.ws.ID+"/"+f.it.ID+"/Files/dst.txt?"+aRealSAS, f.token, nil,
		map[string]string{
			"x-ms-copy-source": "https://acct.blob.core.windows.net/onelake" + src + "?" + aRealSAS,
		})

	out := buf.String()
	if !strings.Contains(out, "onelake-blob: PUT") {
		t.Fatalf("the trace did not fire, so this test asserts nothing:\n%s", out)
	}
	if strings.Contains(out, "secretsignature") {
		t.Fatalf("the Blob trace leaked a SAS signature:\n%s", out)
	}
	if !strings.Contains(out, "sig="+redactedValue) {
		t.Errorf("the Blob trace no longer records that a SAS was presented:\n%s", out)
	}
	if !strings.Contains(out, "/Files/src.txt?") {
		t.Errorf("the copy source lost its path:\n%s", out)
	}
	if !strings.Contains(out, "inm=") || !strings.Contains(out, "copy=") {
		t.Errorf("the trace format changed:\n%s", out)
	}
}
