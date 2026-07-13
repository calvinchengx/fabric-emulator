package tds

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestPacketRoundTrip writes a message and reads it back through the framing.
func TestPacketRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 100)
	var buf bytes.Buffer
	if err := WriteMessage(&buf, PktPreLogin, payload); err != nil {
		t.Fatal(err)
	}
	typ, data, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != PktPreLogin {
		t.Errorf("type = %#x", typ)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("payload round-trip mismatch")
	}
}

// TestPacketMultiPacketMessage forces a message larger than one packet and
// confirms it reassembles (EOM only on the last packet).
func TestPacketMultiPacketMessage(t *testing.T) {
	payload := bytes.Repeat([]byte{0xCD}, maxPacketData*2+7)
	var buf bytes.Buffer
	if err := WriteMessage(&buf, PktSQLBatch, payload); err != nil {
		t.Fatal(err)
	}
	// Expect 3 packets; only the last is EOM.
	var packets int
	rr := bytes.NewReader(buf.Bytes())
	for {
		p, err := ReadPacket(rr)
		if err != nil {
			break
		}
		packets++
		if p.EOM() {
			break
		}
	}
	if packets != 3 {
		t.Errorf("packets = %d, want 3", packets)
	}
	_, data, err := ReadMessage(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, payload) {
		t.Error("multi-packet payload mismatch")
	}
}

// TestPreLoginParseKnownBytes parses a hand-built PRELOGIN with a VERSION,
// ENCRYPTION=OFF, and FEDAUTHREQUIRED=1 option, checking the offset math.
func TestPreLoginParseKnownBytes(t *testing.T) {
	// Option table: 3 entries × 5 bytes + terminator = 16; data starts at 16.
	// VERSION(6) @16, ENCRYPTION(1) @22, FEDAUTHREQUIRED(1) @23.
	data := make([]byte, 24)
	put := func(i int, tok byte, off, ln uint16) {
		data[i] = tok
		binary.BigEndian.PutUint16(data[i+1:i+3], off)
		binary.BigEndian.PutUint16(data[i+3:i+5], ln)
	}
	put(0, plVersion, 16, 6)
	put(5, plEncryption, 22, 1)
	put(10, plFedAuthReq, 23, 1)
	data[15] = plTerminator
	data[22] = EncryptOff
	data[23] = 0x01

	pl, err := ParsePreLogin(data)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Encryption != EncryptOff {
		t.Errorf("encryption = %#x", pl.Encryption)
	}
	if pl.FedAuthReq != 0x01 {
		t.Errorf("fedauthreq = %#x, want 1", pl.FedAuthReq)
	}
	if len(pl.Version) != 6 {
		t.Errorf("version len = %d", len(pl.Version))
	}
}

// TestServerPreLoginRoundTrip builds the server response and parses it back,
// asserting it advertises no-TLS + FedAuth-required so the client does a
// FedAuth login.
func TestServerPreLoginRoundTrip(t *testing.T) {
	pl, err := ParsePreLogin(ServerPreLogin(true))
	if err != nil {
		t.Fatal(err)
	}
	if pl.Encryption != EncryptNotSup {
		t.Errorf("server should advertise ENCRYPT_NOT_SUP, got %#x", pl.Encryption)
	}
	if pl.FedAuthReq != 0x01 {
		t.Errorf("server should require FedAuth, got %#x", pl.FedAuthReq)
	}
	// And without FedAuth required, the flag is cleared.
	pl2, _ := ParsePreLogin(ServerPreLogin(false))
	if pl2.FedAuthReq != 0x00 {
		t.Errorf("fedauthreq should be 0 when not required, got %#x", pl2.FedAuthReq)
	}
}

func TestReadPacketTruncated(t *testing.T) {
	if _, err := ReadPacket(bytes.NewReader([]byte{0x12, 0x01, 0x00})); err == nil {
		t.Error("expected error on short header")
	}
	// Header claims length 100 but the body is missing.
	if _, err := ReadPacket(bytes.NewReader([]byte{0x12, 0x01, 0x00, 0x64, 0, 0, 1, 0})); err == nil {
		t.Error("expected error on missing body")
	}
	// Length below the header size.
	if _, err := ReadPacket(bytes.NewReader([]byte{0x12, 0x01, 0x00, 0x04, 0, 0, 1, 0})); err == nil {
		t.Error("expected error on length < header")
	}
}

func TestParseLogin7Errors(t *testing.T) {
	if _, err := ParseLogin7([]byte{1, 2, 3}); err == nil {
		t.Error("expected header error on short data")
	}
	// A full-size header whose Length field exceeds the payload.
	data := make([]byte, 94)
	binary.LittleEndian.PutUint32(data[0:], 9999)
	if _, err := ParseLogin7(data); err == nil {
		t.Error("expected length-overflow error")
	}
}

func TestFedAuthTokenStream(t *testing.T) {
	// A non-FEDAUTH feature followed by the terminator → no token.
	if tok, err := fedAuthToken([]byte{0x04, 2, 0, 0, 0, 0xAA, 0xBB, featExtTerminator}, 0); err != nil || tok != "" {
		t.Fatalf("non-fedauth stream: tok=%q err=%v", tok, err)
	}
	// Truncated feature header.
	if _, err := fedAuthToken([]byte{0x02, 0xFF}, 0); err == nil {
		t.Error("expected truncated feature-ext error")
	}
	// A feature whose declared data length runs past the buffer.
	if _, err := fedAuthToken([]byte{0x02, 0xFF, 0xFF, 0xFF, 0xFF}, 0); err == nil {
		t.Error("expected feature-ext data-out-of-range error")
	}
	// A FEDAUTH SecurityToken feature carrying a UTF-16LE token "hi".
	fd := []byte{fedAuthLibrarySecurityToken << 1, 4, 0, 0, 0, 'h', 0, 'i', 0}
	dl := make([]byte, 4)
	binary.LittleEndian.PutUint32(dl, uint32(len(fd)))
	stream := append([]byte{featExtFedAuth}, dl...)
	stream = append(stream, fd...)
	stream = append(stream, featExtTerminator)
	if tok, err := fedAuthToken(stream, 0); err != nil || tok != "hi" {
		t.Fatalf("fedauth stream: tok=%q err=%v", tok, err)
	}
}

func TestUCS2Odd(t *testing.T) {
	if ucs2([]byte{0x41}) != "" {
		t.Error("odd-length ucs2 should decode to empty")
	}
}

// TestPreLoginRejectsBadOffsets guards the parser against malformed option
// tables (out-of-range offsets, missing terminator).
func TestPreLoginRejectsBadOffsets(t *testing.T) {
	// Option claims data past the buffer.
	bad := make([]byte, 6)
	bad[0] = plVersion
	binary.BigEndian.PutUint16(bad[1:3], 100) // offset way past end
	binary.BigEndian.PutUint16(bad[3:5], 6)
	bad[5] = plTerminator
	if _, err := ParsePreLogin(bad); err == nil {
		t.Error("expected out-of-range offset error")
	}
	// No terminator.
	if _, err := ParsePreLogin([]byte{plVersion, 0, 8, 0, 1}); err == nil {
		t.Error("expected unterminated-table error")
	}
}
