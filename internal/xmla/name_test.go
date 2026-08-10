package xmla

import (
	"encoding/xml"
	"strings"
	"testing"
)

// markup is every character that could end an element name and begin something
// else. If none of these can leave EncodeName, no caller-supplied column name
// can add markup to the response, whatever else it contains.
const markup = `<>&"'`

// EncodeName's output alphabet excludes markup, for EVERY rune in the BMP.
//
// This is the invariant CodeQL's go/reflected-xss cannot see: it follows the
// `b.WriteRune(r)` passthrough without the isNameChar guard that governs it,
// reports the DMV column path as reflected XSS, and alert 65 is dismissed as a
// false positive on the strength of this test. If this ever fails, that
// dismissal is wrong and the alert should be reopened rather than re-dismissed.
func TestEncodeNameNeverEmitsMarkup(t *testing.T) {
	checked := 0
	for r := rune(0); r <= 0xFFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // surrogate halves are not runes; Go yields U+FFFD
		}
		// Both positions matter: EncodeName applies a different predicate to
		// the first character than to the rest.
		for _, s := range []string{string(r), "a" + string(r)} {
			if i := strings.IndexAny(EncodeName(s), markup); i >= 0 {
				t.Fatalf("EncodeName(%q) = %q leaks %q at %d",
					s, EncodeName(s), EncodeName(s)[i], i)
			}
		}
		checked++
	}
	if checked < 0xF000 {
		t.Fatalf("only %d runes checked; the loop is not covering the BMP", checked)
	}
	// Astral planes take the surrogate-pair path, which is a separate branch.
	for _, s := range []string{"\U0001F600", "a\U0001F600", "\U0010FFFF"} {
		if strings.ContainsAny(EncodeName(s), markup) {
			t.Errorf("EncodeName(%q) = %q leaks markup", s, EncodeName(s))
		}
	}
}

// The same property at the writer, not the encoder: a hostile column name
// reaches the response as an escape sequence and the payload still parses.
//
// Driven through Rowset directly rather than through DMV, because DMV now
// refuses non-identifier names outright — this asserts the layer underneath
// that guard, which is what actually holds if the guard is ever loosened.
func TestHostileColumnNameStaysWellFormed(t *testing.T) {
	rs := Rowset{
		Name:    "Table",
		Columns: []string{`<script>alert(1)</script>`, "Name"},
		Rows:    [][]string{{`</row><script>alert(2)</script>`, "ok"}},
	}
	body := rs.ExecuteResponse()
	if strings.Contains(string(body), "<script>") {
		t.Error("a raw <script> element reached the response body")
	}
	// Well-formed is the stronger claim: absence of one substring is not proof
	// the document survived. The trailing status byte is not part of the XML.
	if err := xml.Unmarshal(trimPayloadByte(body), new(any)); err != nil {
		t.Fatalf("hostile column name produced unparseable XML: %v", err)
	}
}

func trimPayloadByte(b []byte) []byte {
	if len(b) > 0 && b[len(b)-1] == payloadComplete {
		return b[:len(b)-1]
	}
	return b
}
