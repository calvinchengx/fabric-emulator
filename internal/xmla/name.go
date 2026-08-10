package xmla

import (
	"fmt"
	"strings"
	"unicode"
)

// EncodeName renders a column name as a legal XML element name using XMLA's
// documented `_xHHHH_` escape.
//
// This is not a nicety. A DAX result's columns are `Time[FiscalYear]` and
// `[TotalUnits]`, and `[`/`]` are illegal in XML names — emitting them raw
// produces a payload that is not well-formed XML at all, which a client
// reports as a parse failure rather than as a bad column. XMLA's answer
// ([MS-SSAS] rowset naming) is to replace each invalid character with the
// four-hex-digit UCS-2 form: the classic example is `Order Details` →
// `Order_x0020_Details`, and here `Time[FiscalYear]` →
// `Time_x005B_FiscalYear_x005D_`.
//
// The true name is not lost: the schema carries it verbatim in `sql:field`,
// so a client can map back without decoding.
func EncodeName(s string) string {
	if s == "" {
		return "_x0020_" // an empty element name is not legal; encode a space
	}
	var b strings.Builder
	for i, r := range s {
		switch {
		// A literal underscore that could start an escape must itself be
		// escaped, or `a_x0020_b` would decode to something the source never
		// contained. Encoding every `_` is the unambiguous choice.
		case r == '_':
			b.WriteString("_x005F_")
		case i == 0 && !isNameStart(r):
			writeEscape(&b, r)
		case i > 0 && !isNameChar(r):
			writeEscape(&b, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func writeEscape(b *strings.Builder, r rune) {
	// UCS-2 form; runes beyond the BMP are emitted as their surrogate pair.
	if r > 0xFFFF {
		r -= 0x10000
		fmt.Fprintf(b, "_x%04X__x%04X_", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		return
	}
	fmt.Fprintf(b, "_x%04X_", r)
}

// isNameStart reports whether r may begin an XML name (underscore excluded
// here deliberately — EncodeName escapes it unconditionally).
func isNameStart(r rune) bool {
	return r == ':' || unicode.IsLetter(r)
}

// isNameChar reports whether r may appear after the first character.
func isNameChar(r rune) bool {
	return isNameStart(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == 0xB7
}
