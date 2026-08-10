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
// of already-stringified cells.
//
// THE INLINE SCHEMA IS A TYPE CONTRACT, not decoration. An earlier version of
// this comment said "every XMLA value crosses as xsd:string ... which keeps the
// emitted XSD honest about what we actually send". That was wrong, and it was
// measured wrong against real sempy: the client re-types FROM the schema and
// casts, so a string-typed ID fails as
//
//	InvalidCastException: Unable to cast object of type 'System.String'
//	to type 'System.UInt64'
//
// Types is optional and parallel to Columns; an empty entry means xsd:string,
// which keeps every existing caller correct.
type Rowset struct {
	// Name is emitted as <root name="...">. TOM's AmoDataAdapter renames the
	// DataSet's tables from these, one per rowset, and
	// AdjustTableNames BAILS OUT ENTIRELY if the count of names does not match
	// the count of tables — so an unnamed or absent rowset silently breaks
	// naming for every OTHER rowset in the same batch.
	Name    string
	Columns []string
	Types   []string
	Rows    [][]string
}

// xsdType is the declared type for column i: explicit if given, else string.
func (r Rowset) xsdType(i int) string {
	if i < len(r.Types) && r.Types[i] != "" {
		return r.Types[i]
	}
	return "xsd:string"
}

// rootName renders the name attribute, omitting it entirely when unset so that
// callers with a single unnamed rowset emit exactly what they did before.
func rootName(n string) string {
	if n == "" {
		return ""
	}
	return ` name="` + escape(n) + `"`
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
// RootFragment is the <root> element alone, for a BATCH response where many
// rowsets share one envelope. Same bytes the single-rowset path emits inside
// its envelope, so the two cannot drift.
func (r Rowset) RootFragment() []byte { return r.rootElement() }

// rootElement is ONE <root> rowset: the name attribute, the inline XSD the
// client reads before any row, and the rows. Shared by the single-rowset
// envelope and by a batch, so the two cannot drift — the schema and the naming
// are exactly the parts a copy would get subtly wrong.
func (r Rowset) rootElement() []byte {
	var b strings.Builder
	b.WriteString(`<root` + rootName(r.Name) + ` xmlns="urn:schemas-microsoft-com:xml-analysis:rowset" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema" ` +
		`xmlns:sql="urn:schemas-microsoft-com:xml-sql" ` +
		`xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">`)
	b.WriteString(`<xsd:schema targetNamespace="urn:schemas-microsoft-com:xml-analysis:rowset" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema" ` +
		`xmlns:sql="urn:schemas-microsoft-com:xml-sql" ` +
		`elementFormDefault="qualified">` +
		`<xsd:element name="root">` +
		`<xsd:complexType><xsd:sequence>` +
		`<xsd:element name="row" type="row" minOccurs="0" maxOccurs="unbounded"/>` +
		`</xsd:sequence></xsd:complexType></xsd:element>` +
		`<xsd:complexType name="row"><xsd:sequence>`)
	for i, c := range r.Columns {
		// sql:field carries the true name; name= must be a legal XML name, so
		// it is the encoded form (see EncodeName).
		b.WriteString(`<xsd:element sql:field="` + escape(c) + `" name="` + EncodeName(c) +
			`" type="` + r.xsdType(i) + `" minOccurs="0"/>`)
	}
	b.WriteString(`</xsd:sequence></xsd:complexType></xsd:schema>`)
	// A cell absent from a short row is omitted rather than emitted empty:
	// minOccurs="0" makes absence legal, and "" is a value.
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
	b.WriteString(`</root>`)
	return []byte(b.String())
}

func (r Rowset) envelope(responseElement string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` +
		`<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/">` +
		`<soap:Body>` +
		`<` + responseElement + ` xmlns="urn:schemas-microsoft-com:xml-analysis">` +
		`<return>`)
	b.Write(r.rootElement())
	b.WriteString(`</return></` + responseElement + `>` +
		`</soap:Body></soap:Envelope>`)
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
