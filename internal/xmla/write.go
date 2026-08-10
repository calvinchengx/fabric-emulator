package xmla

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// The WRITE path: what TOM sends from `Model.SaveChanges()`.
//
// MEASURED 2026-08-10 against semantic-link-labs 0.17.0 driving
// `connect_semantic_model(readonly=False)`. It is NOT the TMSL JSON that
// docs/32 planned for. TOM sends
//
//	<Execute><Command>
//	  <Batch Transaction="true" xmlns=".../2003/engine">
//	    <Create xmlns=".../2014/engine">
//	      <DatabaseID>…</DatabaseID>
//	      <Measures><xs:schema…/><row><TableID>1003</TableID>…</row></Measures>
//	      <Annotations>…</Annotations>
//	    </Create>
//	    <Alter …><Tables>…</Tables><Columns>…</Columns></Alter>
//	  </Batch>
//	</Command></Execute>
//
// which is the SAME rowset shape this package emits for Discover, inbound: per
// object type an inline schema followed by <row> elements carrying only the
// changed fields, keyed by the ids this server handed out (see objID).
//
// The schema is skipped rather than parsed. It restates the column types we
// already declare on the way out, and reading it would mean trusting the client
// to describe our own id space back to us.

// WriteCommand is one <Create> or <Alter> from the batch, in document order.
type WriteCommand struct {
	Kind       string // "Create" or "Alter"
	DatabaseID string
	Sets       []WriteSet
}

// WriteSet is one object-type element and the rows it carries.
type WriteSet struct {
	Object string              // "Measures", "Tables", "Columns", "Annotations", …
	Rows   []map[string]string // element name -> text, only the fields sent
}

// ParseWriteBatch reads TOM's write batch.
//
// An element it does not understand is KEPT rather than dropped: the caller
// decides what it can apply and faults on the rest, because silently ignoring
// half a transaction is the failure that reads as success at the client and
// loses the user's edit.
func ParseWriteBatch(payload string) ([]WriteCommand, error) {
	dec := xml.NewDecoder(strings.NewReader(payload))
	var out []WriteCommand
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("write batch is not well-formed XML: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || (se.Name.Local != "Create" && se.Name.Local != "Alter") {
			continue
		}
		cmd, err := parseWriteCommand(dec, se)
		if err != nil {
			return nil, err
		}
		out = append(out, cmd)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("write batch carries no Create or Alter command")
	}
	return out, nil
}

func parseWriteCommand(dec *xml.Decoder, start xml.StartElement) (WriteCommand, error) {
	cmd := WriteCommand{Kind: start.Name.Local}
	for {
		tok, err := dec.Token()
		if err != nil {
			return cmd, fmt.Errorf("%s command ended early: %w", cmd.Kind, err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return cmd, nil
			}
		case xml.StartElement:
			if t.Name.Local == "DatabaseID" {
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return cmd, err
				}
				cmd.DatabaseID = v
				continue
			}
			set, err := parseWriteSet(dec, t)
			if err != nil {
				return cmd, err
			}
			cmd.Sets = append(cmd.Sets, set)
		}
	}
}

func parseWriteSet(dec *xml.Decoder, start xml.StartElement) (WriteSet, error) {
	set := WriteSet{Object: start.Name.Local}
	for {
		tok, err := dec.Token()
		if err != nil {
			return set, fmt.Errorf("%s ended early: %w", set.Object, err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return set, nil
			}
		case xml.StartElement:
			// Cheap subtree skip. Not load-bearing for correctness: the
			// `row` filter below already excludes everything in it.
			if t.Name.Local == "schema" {
				if err := dec.Skip(); err != nil {
					return set, err
				}
				continue
			}
			if t.Name.Local != "row" {
				if err := dec.Skip(); err != nil {
					return set, err
				}
				continue
			}
			row, err := parseWriteRow(dec, t)
			if err != nil {
				return set, err
			}
			set.Rows = append(set.Rows, row)
		}
	}
}

func parseWriteRow(dec *xml.Decoder, start xml.StartElement) (map[string]string, error) {
	row := map[string]string{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return row, fmt.Errorf("row ended early: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return row, nil
			}
		case xml.StartElement:
			var v string
			if err := dec.DecodeElement(&v, &t); err != nil {
				return row, err
			}
			// The field name is the ELEMENT name, decoded rather than taken
			// raw: TOM sends back the same _xHHHH_ encoding this package emits
			// for names that are not legal XML, so `Order_x0020_Details` has to
			// come back as `Order Details` or it will never match an object.
			row[DecodeName(t.Name.Local)] = v
		}
	}
}
