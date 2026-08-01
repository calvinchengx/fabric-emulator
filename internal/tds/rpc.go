package tds

// RPC parameter parsing, far enough to rewrite a statement a client sent as a
// parameter rather than as a batch (docs/29-tsql-parity.md, T6g).
//
// T6a measured why this is needed: parameterization, not statement content,
// decides the wire shape. `cursor.execute(sql)` arrives as a SQLBatch, while
// `cursor.execute(sql, params)` arrives as an RPC — the ODBC driver uses
// sp_prepexec — with the statement inside a typed parameter. T6e rewrote the
// first and refused the second by name; this rewrites the second too.
//
// # Why this is written defensively
//
// A SQLBatch is one length-prefixed string: mis-parsing it is nearly
// impossible. An RPC is a parameter list whose every element must be measured
// exactly to reach the next one, and getting a width wrong silently
// mis-frames everything after it. So the rule here is stricter than elsewhere:
//
//   - any type this file does not model → give up, forward untouched;
//   - any inconsistency while walking → give up, forward untouched;
//   - after rewriting, the result is RE-PARSED and compared against the
//     original: every other parameter must be byte-identical and the rewritten
//     one must decode to exactly the SQL we intended. If not → forward the
//     original.
//
// The failure mode of a bug is therefore "the rewrite doesn't happen", never
// "a corrupted request reaches the engine".

import (
	"encoding/binary"
	"errors"
)

var errUnsupportedRPC = errors.New("tds: RPC shape not modelled")

// rpcParam is one parameter of an RPC request, kept as raw slices so an
// untouched parameter re-serialises byte for byte.
type rpcParam struct {
	name     []byte // B_VARCHAR: length byte + UCS-2 name
	status   byte
	typeInfo []byte // the TYPE_INFO block, verbatim
	value    []byte // the value bytes that follow it, verbatim

	// Set for the (N)VARCHAR family, whose values can carry a statement.
	text   string
	isText bool
	plp    bool
	maxLen int
}

// rpcRequest is a parsed RPC message.
type rpcRequest struct {
	headers []byte // ALL_HEADERS, verbatim
	proc    []byte // NameLenProcID or ProcName, verbatim
	flags   []byte // OptionFlags (2 bytes)
	params  []rpcParam
}

// parseRPC walks an RPC payload. It returns errUnsupportedRPC for anything it
// cannot measure exactly, which the caller treats as "forward unchanged".
func parseRPC(data []byte) (*rpcRequest, error) {
	r := &rpcRequest{}
	body := data
	if len(data) >= 4 {
		if total := int(binary.LittleEndian.Uint32(data)); total >= 4 && total <= len(data) {
			r.headers, body = data[:total], data[total:]
		}
	}
	if len(body) < 2 {
		return nil, errUnsupportedRPC
	}
	nameLen := binary.LittleEndian.Uint16(body)
	procEnd := 2 + int(nameLen)*2
	if nameLen == 0xFFFF {
		procEnd = 4
	}
	if procEnd > len(body) {
		return nil, errUnsupportedRPC
	}
	r.proc, body = body[:procEnd], body[procEnd:]

	if len(body) < 2 {
		return nil, errUnsupportedRPC
	}
	r.flags, body = body[:2], body[2:]

	for len(body) > 0 {
		p, rest, err := parseParam(body)
		if err != nil {
			return nil, err
		}
		r.params = append(r.params, p)
		body = rest
	}
	return r, nil
}

func parseParam(b []byte) (rpcParam, []byte, error) {
	var p rpcParam
	if len(b) < 1 {
		return p, nil, errUnsupportedRPC
	}
	nameEnd := 1 + int(b[0])*2
	if nameEnd+1 > len(b) {
		return p, nil, errUnsupportedRPC
	}
	p.name = b[:nameEnd]
	p.status = b[nameEnd]
	b = b[nameEnd+1:]

	tiLen, valLen, meta, err := scanTypeInfo(b)
	if err != nil {
		return p, nil, err
	}
	if tiLen+valLen > len(b) {
		return p, nil, errUnsupportedRPC
	}
	p.typeInfo = b[:tiLen]
	p.value = b[tiLen : tiLen+valLen]
	if meta != nil {
		p.isText, p.plp, p.maxLen = true, meta.plp, meta.maxLen
		p.text = decodeCharValue(p.value, meta)
	}
	return p, b[tiLen+valLen:], nil
}

// charMeta describes a parsed (N)VARCHAR-family parameter.
type charMeta struct {
	unicode bool
	plp     bool
	maxLen  int
}

// Fixed-width TDS types: the TYPE_INFO is the type byte alone and the value
// width is implied.
var fixedTypeWidth = map[byte]int{
	0x1F: 0, // NULL
	0x30: 1, // INT1
	0x32: 1, // BIT
	0x34: 2, // INT2
	0x38: 4, // INT4
	0x3A: 4, // DATETIM4
	0x3B: 4, // FLT4
	0x3C: 8, // MONEY
	0x3D: 8, // DATETIME
	0x3E: 8, // FLT8
	0x7A: 4, // MONEY4
	0x7F: 8, // INT8
}

// byteLenTypes carry a one-byte maximum length in TYPE_INFO and a one-byte
// actual length before the value.
var byteLenTypes = map[byte]bool{
	0x24: true, // GUID
	0x26: true, // INTN
	0x68: true, // BITN
	0x6D: true, // FLTN
	0x6E: true, // MONEYN
	0x6F: true, // DATETIMN
	0x2F: true, // CHAR
	0x27: true, // VARCHAR
	0x2D: true, // VARBINARY
	0x2C: true, // BINARY
}

// decimalTypes add precision and scale bytes to the byte-length shape.
var decimalTypes = map[byte]bool{0x37: true, 0x3F: true, 0x6A: true, 0x6C: true}

// scaledTemporalTypes carry a scale byte in TYPE_INFO and a one-byte value
// length. DATEN carries no scale.
var scaledTemporalTypes = map[byte]bool{0x29: true, 0x2A: true, 0x2B: true}

// scanTypeInfo measures a parameter's TYPE_INFO block and the value after it.
// meta is non-nil for the (N)VARCHAR family, whose text can be rewritten.
func scanTypeInfo(b []byte) (tiLen, valLen int, meta *charMeta, err error) {
	if len(b) < 1 {
		return 0, 0, nil, errUnsupportedRPC
	}
	t := b[0]

	if w, ok := fixedTypeWidth[t]; ok {
		return 1, w, nil, nil
	}
	if t == 0x28 { // DATEN: type byte only, one-byte value length
		return oneByteValueLen(b, 1)
	}
	if scaledTemporalTypes[t] {
		if len(b) < 2 {
			return 0, 0, nil, errUnsupportedRPC
		}
		return oneByteValueLen(b, 2)
	}
	if decimalTypes[t] {
		if len(b) < 4 { // type + maxlen + precision + scale
			return 0, 0, nil, errUnsupportedRPC
		}
		return oneByteValueLen(b, 4)
	}
	if byteLenTypes[t] {
		if len(b) < 2 { // type + maxlen
			return 0, 0, nil, errUnsupportedRPC
		}
		return oneByteValueLen(b, 2)
	}

	// Two-byte-length family; the char variants also carry a 5-byte collation.
	switch t {
	case 0xA5, 0xAD: // BIGVARBINARY, BIGBINARY
		return twoByteLen(b, 3, &charMeta{})
	case 0xA7, 0xAF: // BIGVARCHAR, BIGCHAR
		return twoByteLen(b, 8, &charMeta{unicode: false})
	case 0xE7, 0xEF: // NVARCHAR, NCHAR
		return twoByteLen(b, 8, &charMeta{unicode: true})
	}
	// TEXT/NTEXT/IMAGE, XML, UDT, SQL_VARIANT and anything unknown: not
	// modelled, so the message is forwarded untouched.
	return 0, 0, nil, errUnsupportedRPC
}

// oneByteValueLen measures a value carrying a single-byte actual length.
func oneByteValueLen(b []byte, tiLen int) (int, int, *charMeta, error) {
	if len(b) < tiLen+1 {
		return 0, 0, nil, errUnsupportedRPC
	}
	return tiLen, 1 + int(b[tiLen]), nil, nil
}

// twoByteLen measures the BIG*/N* family. tiLen already accounts for the type
// byte, the 2-byte maximum length, and a collation when the type has one.
func twoByteLen(b []byte, tiLen int, meta *charMeta) (int, int, *charMeta, error) {
	if len(b) < 3 {
		return 0, 0, nil, errUnsupportedRPC
	}
	maxLen := int(binary.LittleEndian.Uint16(b[1:]))
	meta.maxLen = maxLen
	if len(b) < tiLen {
		return 0, 0, nil, errUnsupportedRPC
	}
	if maxLen == 0xFFFF { // nvarchar(max) etc: partially length-prefixed
		meta.plp = true
		n, err := plpLen(b[tiLen:])
		if err != nil {
			return 0, 0, nil, err
		}
		return tiLen, n, meta, nil
	}
	if len(b) < tiLen+2 {
		return 0, 0, nil, errUnsupportedRPC
	}
	n := int(binary.LittleEndian.Uint16(b[tiLen:]))
	if n == 0xFFFF { // NULL
		return tiLen, 2, meta, nil
	}
	return tiLen, 2 + n, meta, nil
}

// plpLen measures a partially-length-prefixed value: an 8-byte total (or the
// UNKNOWN sentinel), then length-prefixed chunks ended by a zero-length chunk.
func plpLen(b []byte) (int, error) {
	if len(b) < 8 {
		return 0, errUnsupportedRPC
	}
	total := binary.LittleEndian.Uint64(b)
	if total == 0xFFFFFFFFFFFFFFFF { // PLP_NULL
		return 8, nil
	}
	i := 8
	for {
		if i+4 > len(b) {
			return 0, errUnsupportedRPC
		}
		chunk := int(binary.LittleEndian.Uint32(b[i:]))
		i += 4
		if chunk == 0 { // terminator
			return i, nil
		}
		if i+chunk > len(b) {
			return 0, errUnsupportedRPC
		}
		i += chunk
	}
}

// decodeCharValue extracts the text of an (N)VARCHAR value.
func decodeCharValue(v []byte, meta *charMeta) string {
	var raw []byte
	switch {
	case meta.plp:
		if len(v) < 8 || binary.LittleEndian.Uint64(v) == 0xFFFFFFFFFFFFFFFF {
			return ""
		}
		i := 8
		for i+4 <= len(v) {
			chunk := int(binary.LittleEndian.Uint32(v[i:]))
			i += 4
			if chunk == 0 || i+chunk > len(v) {
				break
			}
			raw = append(raw, v[i:i+chunk]...)
			i += chunk
		}
	default:
		if len(v) < 2 || binary.LittleEndian.Uint16(v) == 0xFFFF {
			return ""
		}
		raw = v[2:]
	}
	if meta.unicode {
		return ucs2(raw)
	}
	return string(raw)
}

// encodeCharValue re-encodes text in the same shape the client used, widening
// to PLP when the rewritten statement no longer fits the declared maximum.
// It returns the value bytes and the TYPE_INFO to pair with them.
func encodeCharValue(p rpcParam, text string) (typeInfo, value []byte) {
	var raw []byte
	unicode := len(p.typeInfo) > 0 && (p.typeInfo[0] == 0xE7 || p.typeInfo[0] == 0xEF)
	if unicode {
		raw = str2ucs2(text)
	} else {
		raw = []byte(text)
	}

	if !p.plp && len(raw) <= p.maxLen {
		value = binary.LittleEndian.AppendUint16(nil, uint16(len(raw)))
		return p.typeInfo, append(value, raw...)
	}

	// PLP: either the client already used it, or the rewrite outgrew maxLen and
	// the declared maximum has to widen with it.
	typeInfo = append([]byte(nil), p.typeInfo...)
	binary.LittleEndian.PutUint16(typeInfo[1:], 0xFFFF)
	value = binary.LittleEndian.AppendUint64(nil, uint64(len(raw)))
	value = binary.LittleEndian.AppendUint32(value, uint32(len(raw)))
	value = append(value, raw...)
	return typeInfo, binary.LittleEndian.AppendUint32(value, 0) // terminator
}

// encode re-serialises a parsed RPC request.
func (r *rpcRequest) encode() []byte {
	out := append([]byte(nil), r.headers...)
	out = append(out, r.proc...)
	out = append(out, r.flags...)
	for _, p := range r.params {
		out = append(out, p.name...)
		out = append(out, p.status)
		out = append(out, p.typeInfo...)
		out = append(out, p.value...)
	}
	return out
}
