// Package tds implements the server side of the Tabular Data Stream (TDS)
// protocol — SQL Server's wire protocol — far enough to terminate a client
// connection, complete an Entra FedAuth login, and relay the post-login
// session to a real T-SQL backend (SQL Server on Linux; see docs/16).
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
	"math"
	"sync/atomic"
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

// DefaultMaxMessageBytes is the DEFAULT bound on the payload ReadMessage will
// accumulate for a SINGLE TDS message, the one thing the frame format does not
// bound itself.
//
// Each ReadPacket is self-limiting at 64 KiB because the length field is a
// uint16, so the per-frame read looks bounded and is. The MESSAGE loop was not:
// it concatenates packets until one sets the EOM status bit, so a peer that
// simply never sets EOM chooses how much the emulator allocates — the same
// unbounded-by-construction shape the HTTP listener closed with one outer
// ceiling at the root handler (internal/server.Server.boundBodies), here on the
// second listener that bound cannot reach: cmd/fabric-emulator/main.go hands
// the TDS port its own net.Listener, and there is no r.Body there for
// http.MaxBytesReader to wrap.
//
// THE VALUE, so the next reader can recompute it rather than delete it. The
// largest SQL batch literal anywhere in e2e/, internal/tds/ and internal/tsql/
// is 149 bytes — roughly 300 on the wire as UCS-2, i.e. a single packet. The
// direction that can legitimately be large is the other one: splice.go reads
// the sidecar's result sets through this same function, so the ceiling has to
// clear a real SELECT rather than a batch. 320 MiB is httpx.DefaultMaxRequestBody,
// the HTTP listener's default outer bound, which leaves neither of the
// emulator's two listeners the narrower surface and neither unbounded. Spelled
// as a literal rather than imported, because internal/tds has no other reason
// to depend on internal/httpx; the equality is pinned in maxmessage_test.go.
//
// WHY IT IS A DEFAULT AND NOT THE WHOLE STORY. It NARROWS what a released
// binary accepts, on a surface that accepted anything before it, and no
// released binary's accepted inputs may shrink unremarked — the same principle
// that made the HTTP bound FABRIC_MAX_REQUEST_BYTES rather than a constant. The
// effective ceiling is therefore settable once at start-up through
// SetMaxMessageBytes, wired to FABRIC_MAX_TDS_MESSAGE_BYTES in internal/config,
// with 0 meaning no limit at all: someone whose sidecar already returns a result
// set larger than this is doing so successfully today, and the honest migration
// is a documented way to keep doing it rather than a number we guess is enough.
const DefaultMaxMessageBytes = 320 << 20

// noMessageCeiling is what "no limit" is stored AS, rather than being a second
// branch inside the read loop. That keeps `total > limit` the ONLY comparison
// the accumulation loop makes against its bound, which matters twice: the loop
// gets no unlimited-mode path that could drift from the bounded one, and the
// static guard over this file (`unbounded-frame-accumulation` in
// scripts/check_perf_regressions.py) gets no second comparison it could mistake
// for the real one. MaxInt64 bytes is not reachable by any peer — this is
// "unbounded" expressed as a bound.
const noMessageCeiling = math.MaxInt64

// maxMessageBytes is the EFFECTIVE accumulated-message ceiling.
//
// An atomic, and package-level, for exactly the reasons traceFunc is (trace.go):
// the read path is package-level functions called from the goroutine of every
// session — eight call sites across server.go, client.go and splice.go, all
// funnelling through ReadMessage — it is written once at server start, and a
// test replaces it while other tests' sessions are still open.
//
// Initialised in init rather than at its declaration because atomic.Int64 has no
// literal form, and its zero value cannot double as "unset": 0 is the documented
// way to ask for NO limit, so a lazy "0 means use the default" here would make
// FABRIC_MAX_TDS_MESSAGE_BYTES=0 mean 320 MiB — the one value it must not mean.
// Same trap, and the same resolution, as Config.Finish and
// FABRIC_MAX_REQUEST_BYTES (internal/config/config.go).
var maxMessageBytes atomic.Int64

func init() { maxMessageBytes.Store(DefaultMaxMessageBytes) }

// SetMaxMessageBytes installs the effective accumulated-message ceiling. n <= 0
// removes it, which is FABRIC_MAX_TDS_MESSAGE_BYTES=0: the documented escape
// hatch for a peer that legitimately sends, or a sidecar that legitimately
// returns, more than the default.
//
// Safe to call while messages are being read, like SetTraceFunc. It is called
// once, from internal/server, when the TDS endpoint is configured.
func SetMaxMessageBytes(n int64) {
	if n <= 0 {
		maxMessageBytes.Store(noMessageCeiling)
		return
	}
	maxMessageBytes.Store(n)
}

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
// returns the message type and the joined payload. The accumulated payload is
// bounded by the effective accumulated-message ceiling (DefaultMaxMessageBytes,
// or whatever SetMaxMessageBytes was given): a peer that never sets EOM is
// refused rather than allowed to grow the buffer without limit.
//
// This is the single entry point for every TDS read in the package — server.go,
// client.go and splice.go have no other — so the ceiling resolved here is the
// ceiling the whole listener gets.
func ReadMessage(r io.Reader) (typ byte, data []byte, err error) {
	return readMessageBounded(r, maxMessageBytes.Load())
}

// readMessageBounded is ReadMessage with the ceiling as an argument, which is
// what makes the refusal testable: proving it through the real 320 MiB default
// would mean allocating 320 MiB in a unit test to learn nothing the same
// assertion at 4 KiB does not already prove. That the effective ceiling actually
// derives from DefaultMaxMessageBytes, and that this loop actually compares the
// accumulated length against it, are held statically instead — by the
// `unbounded-frame-accumulation` kind in scripts/check_perf_regressions.py,
// which reads THIS function rather than its caller for the comparison.
func readMessageBounded(r io.Reader, limit int64) (typ byte, data []byte, err error) {
	for {
		p, err := ReadPacket(r)
		if err != nil {
			return 0, nil, err
		}
		if typ == 0 {
			typ = p.Type
		}
		// Checked BEFORE the append that would cross it, so the worst case held
		// in memory is the ceiling plus the one packet that tripped it — not the
		// ceiling plus however much more the peer had queued. The bound is
		// inclusive: a message of exactly max bytes is a message, not a refusal.
		if total := int64(len(data)) + int64(len(p.Data)); total > limit {
			return 0, nil, fmt.Errorf(
				"tds: message exceeds MaxMessageBytes=%d (%d bytes accumulated "+
					"with no EOM packet); raise or remove the bound with "+
					"FABRIC_MAX_TDS_MESSAGE_BYTES (0 disables it)", limit, total)
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
