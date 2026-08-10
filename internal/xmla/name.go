package xmla

import (
	"fmt"
	"strconv"
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

// DecodeName inverts EncodeName.
//
// Needed on the WRITE path: TOM sends field names back in the same _xHHHH_ form
// this package emits, so a name that had to be escaped on the way out would
// never match an object on the way in. Encoding is total and unambiguous
// (EncodeName escapes every literal underscore), so this is exact rather than
// best-effort.
//
// Anything that is not a well-formed escape is returned verbatim: a name the
// encoder never produced is a name we should not silently rewrite.
func DecodeName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, width := decodeEscape(s, i)
		if width == 0 {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteRune(r)
		i += width
	}
	return b.String()
}

// decodeEscape reads one _xHHHH_ (or surrogate pair) at i, returning the rune
// and the bytes consumed, or width 0 when there is no escape there.
func decodeEscape(s string, i int) (rune, int) {
	hi, ok := hexEscapeAt(s, i)
	if !ok {
		return 0, 0
	}
	if hi >= 0xD800 && hi <= 0xDBFF {
		if lo, ok := hexEscapeAt(s, i+7); ok && lo >= 0xDC00 && lo <= 0xDFFF {
			return 0x10000 + (hi-0xD800)<<10 + (lo - 0xDC00), 14
		}
	}
	return hi, 7
}

func hexEscapeAt(s string, i int) (rune, bool) {
	if i+7 > len(s) || s[i] != '_' || (s[i+1] != 'x' && s[i+1] != 'X') || s[i+6] != '_' {
		return 0, false
	}
	v, err := strconv.ParseUint(s[i+2:i+6], 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(v), true
}
