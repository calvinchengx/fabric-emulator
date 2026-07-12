// Package tds implements the server side of the Tabular Data Stream (TDS)
// protocol — SQL Server's wire protocol — far enough to terminate a client
// connection, complete an Entra FedAuth login, and relay the post-login
// session to a real T-SQL backend (Babelfish or SQL Server).
//
// This is the "protocol we own"; the T-SQL engine is a bring-your-own sidecar
// reached over TDS with a SQL login (see docs/16-warehouse-tds.md). The novel,
// in-family part is that the login is authenticated with an Entra token
// validated against entra-emulator — which no off-the-shelf SQL engine can do
// against a fake issuer.
package tds

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Packet types (MS-TDS 2.2.3.1.1 — the type byte of the packet header).
const (
	PktSQLBatch  byte = 0x01
	PktRPC       byte = 0x03
	PktTabular   byte = 0x04 // server → client tabular result
	PktAttention byte = 0x06
	PktBulkLoad  byte = 0x07
	PktLogin7    byte = 0x10
	PktSSPI      byte = 0x11
	PktPreLogin  byte = 0x12
)

// Status flags (MS-TDS 2.2.3.1.2).
const (
	statusNormal byte = 0x00
	statusEOM    byte = 0x01 // last packet of a message
)

// headerLen is the fixed TDS packet header size.
const headerLen = 8

// maxPacketData is the default negotiated packet payload (4096 − header). TDS
// messages larger than this span multiple packets; login/prelogin never do.
const maxPacketData = 4096 - headerLen

// Packet is one TDS message frame (header + payload).
type Packet struct {
	Type     byte
	Status   byte
	SPID     uint16
	PacketID byte
	Window   byte
	Data     []byte
}

// EOM reports whether this packet is the last of its message.
func (p *Packet) EOM() bool { return p.Status&statusEOM != 0 }

// ReadPacket reads one TDS packet frame from r.
func ReadPacket(r io.Reader) (*Packet, error) {
	var h [headerLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(h[2:4]))
	if length < headerLen {
		return nil, fmt.Errorf("tds: packet length %d < header length", length)
	}
	data := make([]byte, length-headerLen)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	return &Packet{
		Type:     h[0],
		Status:   h[1],
		SPID:     binary.BigEndian.Uint16(h[4:6]),
		PacketID: h[6],
		Window:   h[7],
		Data:     data,
	}, nil
}

// ReadMessage reads a full TDS message, concatenating packets until EOM. It
// returns the message type and the joined payload.
func ReadMessage(r io.Reader) (typ byte, data []byte, err error) {
	for {
		p, err := ReadPacket(r)
		if err != nil {
			return 0, nil, err
		}
		if typ == 0 {
			typ = p.Type
		}
		data = append(data, p.Data...)
		if p.EOM() {
			return typ, data, nil
		}
	}
}

// WriteMessage writes data as one TDS message of the given type, splitting into
// packets at the default packet size and marking the final packet EOM.
func WriteMessage(w io.Writer, typ byte, data []byte) error {
	first := true
	for first || len(data) > 0 {
		first = false
		chunk := data
		last := true
		if len(chunk) > maxPacketData {
			chunk, data, last = data[:maxPacketData], data[maxPacketData:], false
		} else {
			data = nil
		}
		var h [headerLen]byte
		h[0] = typ
		if last {
			h[1] = statusEOM
		}
		binary.BigEndian.PutUint16(h[2:4], uint16(headerLen+len(chunk)))
		h[6] = 1 // PacketID (single-message counter; 1 is fine here)
		if _, err := w.Write(h[:]); err != nil {
			return err
		}
		if _, err := w.Write(chunk); err != nil {
			return err
		}
	}
	return nil
}
