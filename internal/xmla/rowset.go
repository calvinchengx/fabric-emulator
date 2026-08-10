// Package xmla serialises results onto the XMLA wire: the
// `urn:schemas-microsoft-com:xml-analysis:rowset` payload that Microsoft's own
// ADOMD.NET client consumes, wrapped for either Execute or Discover.
//
// Scope note: this package is the *serialisation* half of XMLA. The transport —
// routing, the session handshake, clusterResolve, the four connect gates — is
// e2e/xmla's, and is measured there against a real ADOMD.NET client rather than
// guessed. What lives here is the projection from the emulator's own model
// (internal/semanticmodel) onto rowsets, which is why it holds no HTTP.
//
// The envelope shape below is not read off the XSD; it is the shape a real
// ADOMD.NET client has been observed to ACCEPT (e2e/xmla, PR #152). Two details
// are load-bearing and neither is obvious from the spec:
//
//   - The inline XSD is mandatory. The client reads the schema to learn the row
//     shape BEFORE it reads any row, so a rowset without it is rejected as
//     "unrecognizable" rather than as empty.
//   - The payload ends with a trailing 0x00 byte. ReadResponsePayloadImpl
//     switches on the final byte: 0 = complete, 1 = LRO continuation, anything
//     else = TransportProtocolError.
package xmla

import (
	"encoding/xml"
	"strings"
)

// Rowset is a tabular result headed for the wire: ordered column names and rows
// of already-stringified cells. Every XMLA value crosses as xsd:string here —
// the client re-types from the schema it is given, and a single string column
// type keeps the emitted XSD honest about what we actually send.
type Rowset struct {
	Columns []string
	Rows    [][]string
}

// Trailing byte the client's payload reader switches on (0 = complete).
const payloadComplete = 0x00

// ExecuteResponse wraps the rowset as a response to an Execute — a DAX
// `EVALUATE` or a `$SYSTEM.TMSCHEMA_*` DMV query, which arrive by the same
// route and differ only in the statement.
func (r Rowset) ExecuteResponse() []byte { return r.envelope("ExecuteResponse") }

// DiscoverResponse wraps the same rowset as a response to a Discover.
func (r Rowset) DiscoverResponse() []byte { return r.envelope("DiscoverResponse") }

// envelope emits the SOAP envelope, the inline XSD and the rows. Execute and
// Discover take an identical rowset under a different wrapper, so this is one
// function rather than two that would drift — the client rejects each
// differently ("not a rowset" vs "unrecognizable"), which is exactly the kind
// of divergence a copy would hide.
func (r Rowset) envelope(responseElement string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">` +
		`<soap:Body>` +
		`<` + responseElement + ` xmlns="urn:schemas-microsoft-com:xml-analysis">` +
		`<return>` +
		`<root xmlns="urn:schemas-microsoft-com:xml-analysis:rowset" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema" ` +
		`xmlns:sql="urn:schemas-microsoft-com:xml-sql" ` +
		`xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">`)

	// The schema the client reads before any row.
	b.WriteString(`<xsd:schema targetNamespace="urn:schemas-microsoft-com:xml-analysis:rowset" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema" ` +
		`xmlns:sql="urn:schemas-microsoft-com:xml-sql" ` +
		`elementFormDefault="qualified">` +
		`<xsd:element name="root">` +
		`<xsd:complexType><xsd:sequence>` +
		`<xsd:element name="row" type="row" minOccurs="0" maxOccurs="unbounded"/>` +
		`</xsd:sequence></xsd:complexType></xsd:element>` +
		`<xsd:complexType name="row"><xsd:sequence>`)
	for _, c := range r.Columns {
		// sql:field carries the true name; name= must be a legal XML name, so
		// it is the encoded form (see EncodeName).
		b.WriteString(`<xsd:element sql:field="` + escape(c) + `" name="` + EncodeName(c) +
			`" type="xsd:string" minOccurs="0"/>`)
	}
	b.WriteString(`</xsd:sequence></xsd:complexType></xsd:schema>`)

	// Rows. A cell absent from a short row is omitted rather than emitted
	// empty: minOccurs="0" makes absence legal, and "" is a value.
	for _, row := range r.Rows {
		b.WriteString(`<row>`)
		for i, c := range r.Columns {
			if i >= len(row) {
				break
			}
			n := EncodeName(c)
			b.WriteString(`<` + n + `>` + escape(row[i]) + `</` + n + `>`)
		}
		b.WriteString(`</row>`)
	}

	b.WriteString(`</root></return></` + responseElement + `></soap:Body></soap:Envelope>`)
	return append([]byte(b.String()), payloadComplete)
}

// escape XML-escapes text. Not optional here: DAX measure expressions are
// routine rowset payloads and routinely contain <, > and &, so an unescaped
// writer produces malformed XML on ordinary model content rather than on
// adversarial input.
func escape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
