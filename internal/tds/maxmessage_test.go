package tds

// The accumulated-message ceiling (MaxMessageBytes), asserted in the three
// directions that matter — the same shape internal/server/maxbody_test.go uses
// for the HTTP outer bound, because this is the same defect on the other
// listener: traffic under the bound is joined intact, a sender that never sets
// EOM is refused rather than accumulated, and the refusal is distinguishable
// from the malformed-frame refusal that already existed.
//
// WHY THE FIRST ONE IS HERE. ReadMessage exists BECAUSE messages span packets:
// the negotiated payload is 4088 bytes and go-mssqldb genuinely sends batches
// and receives result sets larger than one frame. A ceiling that broke the join
// would be a behaviour change dressed as a backstop, and it would look correct
// to a test that only checks that something huge is refused.
//
// WHY THE OVER-CEILING DIRECTION DRIVES readMessageBounded RATHER THAN
// ReadMessage. Proving the refusal through the real 320 MiB constant means
// allocating 320 MiB inside a unit test to learn nothing the same assertion at
// 4 KiB does not already prove. The wiring — that ReadMessage passes
// MaxMessageBytes and not some other number — is pinned statically instead, by
// the `unbounded-frame-accumulation` kind in scripts/check_perf_regressions.py,
// which fails if ReadMessage's body stops naming the constant. The value itself
// is pinned below by TestTheMessageCeilingSitsFarAboveRealTraffic.

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// frame builds one raw TDS packet: header plus payload, EOM set or not. Written
// by hand rather than with WriteMessage because WriteMessage always terminates
// the message it writes — an EOM-less stream, the shape this file is about,
// cannot be produced by it at all.
func frame(typ byte, status byte, payload []byte) []byte {
	h := make([]byte, headerLen)
	h[0] = typ
	h[1] = status
	binary.BigEndian.PutUint16(h[2:4], uint16(headerLen+len(payload)))
	h[6] = 1
	return append(h, payload...)
}

// endlessPackets serves non-EOM packets forever, counting the bytes it has
// handed over. This is the adversary: a client that opens a message and never
// closes it. Before the ceiling landed, ReadMessage would read from this reader
// until the process died.
type endlessPackets struct {
	buf    bytes.Buffer
	served int
}

func (e *endlessPackets) Read(p []byte) (int, error) {
	for e.buf.Len() == 0 {
		// The largest frame the protocol can express: the length field is a
		// uint16, which is exactly why each individual ReadPacket looks
		// self-limiting while the message loop around it was not.
		e.buf.Write(frame(PktSQLBatch, statusNormal, make([]byte, 65535-headerLen)))
	}
	n, err := e.buf.Read(p)
	e.served += n
	return n, err
}

// TestAMultiPacketMessageUnderTheCeilingIsJoinedIntact. The ceiling must be
// invisible to the multi-packet messages this function exists to join, or it is
// not a backstop but a narrowing of what the emulator relays.
func TestAMultiPacketMessageUnderTheCeilingIsJoinedIntact(t *testing.T) {
	var wire bytes.Buffer
	want := make([]byte, 0, 3*maxPacketData)
	for i, b := range []byte{'a', 'b', 'c'} {
		payload := bytes.Repeat([]byte{b}, maxPacketData)
		status := statusNormal
		if i == 2 {
			status = statusEOM
		}
		wire.Write(frame(PktSQLBatch, status, payload))
		want = append(want, payload...)
	}
	typ, data, err := ReadMessage(&wire)
	if err != nil {
		t.Fatalf("a 3-packet message under the ceiling was refused: %v", err)
	}
	if typ != PktSQLBatch {
		t.Errorf("message type %#x, want %#x (the type comes from the FIRST packet)",
			typ, PktSQLBatch)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("the joined payload is %d bytes, want %d — the packets are no "+
			"longer concatenated intact", len(data), len(want))
	}
}

// TestAnEOMLessStreamIsRefusedRatherThanAccumulated. The defect itself: the loop
// had no cap on the accumulated total, so a sender that never set EOM chose how
// much the emulator allocated.
//
// The tightness assertion is the second half and the more useful one: refusing
// AFTER draining an unbounded amount would pass a test that only checks for an
// error, while leaving the exposure exactly where it was.
func TestAnEOMLessStreamIsRefusedRatherThanAccumulated(t *testing.T) {
	const limit = 4 << 10
	src := &endlessPackets{}
	_, data, err := readMessageBounded(src, limit)
	if err == nil {
		t.Fatalf("an endless EOM-less stream was accepted as a %d-byte message; "+
			"nothing bounds the accumulation", len(data))
	}
	if !strings.Contains(err.Error(), "MaxMessageBytes") {
		t.Errorf("the refusal does not name the ceiling, so a reader cannot act "+
			"on it: %v", err)
	}
	// One oversized frame past the limit is all it may read: the check fires
	// before the append that would cross, so the worst case is the limit plus
	// the packet that tripped it.
	if most := limit + 65535 + headerLen; src.served > most {
		t.Errorf("drained %d bytes before refusing a %d-byte ceiling (worst case "+
			"%d); the refusal is not at the bound", src.served, limit, most)
	}
	// Exactly at the ceiling is NOT over it. An off-by-one here would refuse a
	// message of precisely the documented size, which is the half of an
	// inclusive bound nobody notices until it bites.
	exact := frame(PktSQLBatch, statusEOM, make([]byte, limit))
	if _, got, err := readMessageBounded(bytes.NewReader(exact), limit); err != nil {
		t.Errorf("a message of exactly the ceiling (%d) was refused: %v", limit, err)
	} else if len(got) != limit {
		t.Errorf("a message of exactly the ceiling came back %d bytes, want %d",
			len(got), limit)
	}
}

// TestTheCeilingRefusalIsDistinguishableFromAMalformedFrame.
//
// ReadPacket already refuses a frame whose length field is below the header
// size. Both failures arrive at the same caller as an error from ReadMessage,
// and they want opposite responses: a malformed frame means the peer is not
// speaking TDS, while an oversized message means the peer is speaking it and
// asking for too much. A shared or generic message would send whoever reads the
// log to debug the wrong one.
func TestTheCeilingRefusalIsDistinguishableFromAMalformedFrame(t *testing.T) {
	malformed := []byte{PktSQLBatch, statusEOM, 0x00, 0x04, 0, 0, 1, 0}
	_, _, err := ReadMessage(bytes.NewReader(malformed))
	if err == nil {
		t.Fatal("a packet declaring a length below the header size was accepted")
	}
	if !strings.Contains(err.Error(), "header length") {
		t.Errorf("the malformed-frame refusal changed wording: %v", err)
	}
	if strings.Contains(err.Error(), "MaxMessageBytes") {
		t.Errorf("a malformed frame is being reported as an oversized message: %v", err)
	}

	_, _, over := readMessageBounded(&endlessPackets{}, 4<<10)
	if over == nil {
		t.Fatal("an endless EOM-less stream was accepted")
	}
	if strings.Contains(over.Error(), "header length") {
		t.Errorf("an oversized message is being reported as a malformed frame: %v", over)
	}
}

// TestTheMessageCeilingSitsFarAboveRealTraffic is the value assertion, beside
// the static one in scripts/check_perf_regressions.py.
//
// Two different failures are being prevented. A ceiling set too LOW narrows what
// the warehouse endpoint relays — and the direction that can legitimately be
// large is the response one, since splice.go reads the sidecar's result sets
// through this same function. A ceiling set absurdly high (or to zero, meaning
// unlimited) would leave the shape this closed exactly where it was while
// looking repaired.
func TestTheMessageCeilingSitsFarAboveRealTraffic(t *testing.T) {
	// The largest SQL batch literal anywhere in e2e/, internal/tds/ and
	// internal/tsql/ is 149 bytes, i.e. ~300 bytes on the wire as UCS-2: a
	// single packet. 1 MiB is already thousands of times that, so a ceiling
	// below it could only have been chosen by accident.
	if MaxMessageBytes < 1<<20 {
		t.Errorf("MaxMessageBytes=%d is below 1 MiB; a multi-packet result set "+
			"relayed from the sidecar would be refused", MaxMessageBytes)
	}
	if MaxMessageBytes > 1<<30 {
		t.Errorf("MaxMessageBytes=%d is above 1 GiB, which is not a bound on "+
			"anything a dev-loop emulator should buffer", MaxMessageBytes)
	}
	// A reader who sees this constant should be able to recompute it: it is the
	// HTTP listener's default outer bound (httpx.DefaultMaxRequestBody, 320
	// MiB), so neither of the emulator's two listeners is the narrower surface.
	// Asserted as a literal rather than by importing internal/httpx, which the
	// TDS package has no other reason to depend on.
	if MaxMessageBytes != 320<<20 {
		t.Errorf("MaxMessageBytes=%d no longer matches the HTTP listener's "+
			"default outer bound (320 MiB); if that was deliberate, say so in "+
			"docs/62-performance-regressions.md and update this test",
			MaxMessageBytes)
	}
}
