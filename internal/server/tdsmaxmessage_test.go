package server

// The SECOND listener's accumulated-message ceiling, asserted where it is
// INSTALLED rather than where it is enforced -- the counterpart to
// maxbody_test.go, which does the same for the HTTP outer bound.
//
// WHY THIS FILE EXISTS AT ALL, given internal/tds/maxmessage_test.go already
// covers the read loop. The TDS bound is three links long and the two packages
// each see one end of it: a default constant in internal/tds, resolved from
// FABRIC_MAX_TDS_MESSAGE_BYTES by internal/config, and installed into
// internal/tds's package-level state by New below. maxmessage_test.go tests the
// loop in-package and never builds a Config; config_test.go reads the
// environment and never builds a Server. So the one line that joins them --
// tds.SetMaxMessageBytes(cfg.MaxTDSMessageBytes) -- was asserted by nothing,
// and deleting it left every Go test, the perf checker and the compat checker
// green while the configured ceiling (including the documented 0) became a
// no-op. That is this change's own failure shape one link further out:
// declared, derived, routed, and then not applied.
//
// Both directions are here on purpose. A test that only checks that a small
// ceiling refuses cannot tell "installed" from "already installed by init()",
// which is why the escape-hatch test below lowers the ceiling FIRST and then
// proves a second server raises it back off.

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
)

// tdsCeilingServer is a server configured with a TDS message ceiling. It needs
// SQLTDSAddr set because the ceiling is installed where the SQL endpoint is
// configured -- a Config with no TDS address has no second listener to bound.
//
// The cleanup restores the package default rather than leaving the test's value
// in place: the ceiling is process-wide state (atomic, in internal/tds), and
// every other test in this package that opens a TDS session would otherwise
// inherit whatever this file set last.
func tdsCeilingServer(t *testing.T, max int64) *Server {
	t.Helper()
	cfg := &config.Config{
		EntraIssuer:        "https://unused/t/v2.0",
		SQLTDSAddr:         "127.0.0.1:0",
		MaxTDSMessageBytes: max,
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		tds.SetMaxMessageBytes(tds.DefaultMaxMessageBytes)
	})
	return s
}

// tdsFrame builds one raw TDS packet on the wire. Hand-built rather than via
// tds.WriteMessage because WriteMessage always terminates the message it writes
// -- the EOM-less stream this file is about cannot be produced by it.
func tdsFrame(status byte, payload []byte) []byte {
	h := make([]byte, 8)
	h[0] = 0x01 // PktSQLBatch
	h[1] = status
	binary.BigEndian.PutUint16(h[2:4], uint16(8+len(payload)))
	h[6] = 1
	return append(h, payload...)
}

// eomLess is a FINITE stream of n non-EOM packets of size each. Finite on
// purpose: if the ceiling is not installed, this test must FAIL rather than hang
// to the suite timeout, so the reader runs out instead of going on forever.
func eomLess(n, size int) io.Reader {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		b.Write(tdsFrame(0x00, make([]byte, size)))
	}
	return &b
}

const testCeiling = 4 << 10

// TestAConfiguredServerInstallsTheTDSMessageCeiling. The wiring assertion: the
// number that reached Config has to reach the read loop, which is only
// observable through a read.
func TestAConfiguredServerInstallsTheTDSMessageCeiling(t *testing.T) {
	tdsCeilingServer(t, testCeiling)

	_, data, err := tds.ReadMessage(eomLess(4, 4000))
	if err == nil {
		t.Fatalf("an EOM-less stream was accepted as a %d-byte message after the "+
			"server was configured with a %d-byte ceiling", len(data), testCeiling)
	}
	if !strings.Contains(err.Error(), "MaxMessageBytes") {
		t.Fatalf("the read failed, but not on the ceiling -- so the configured "+
			"ceiling never reached internal/tds and the stream simply ran out: %v", err)
	}
	// The ceiling it names must be the CONFIGURED one, not the package default:
	// an error naming 320 MiB here would mean New installed nothing and init()'s
	// value was doing the refusing.
	if !strings.Contains(err.Error(), "MaxMessageBytes=4096") {
		t.Errorf("the refusal does not name the configured ceiling %d, so some "+
			"other bound is in force: %v", testCeiling, err)
	}
}

// TestATDSMessageUnderTheConfiguredCeilingStillReads. The ceiling must be
// invisible to ordinary multi-packet traffic through a configured server, or it
// is a narrowing of what the warehouse endpoint relays rather than a backstop.
func TestATDSMessageUnderTheConfiguredCeilingStillReads(t *testing.T) {
	tdsCeilingServer(t, testCeiling)

	var wire bytes.Buffer
	payload := bytes.Repeat([]byte{'x'}, 1000)
	wire.Write(tdsFrame(0x00, payload))
	wire.Write(tdsFrame(0x01, payload)) // statusEOM
	typ, data, err := tds.ReadMessage(&wire)
	if err != nil {
		t.Fatalf("a 2000-byte message was refused under a %d-byte ceiling: %v",
			testCeiling, err)
	}
	if typ != 0x01 || len(data) != 2*len(payload) {
		t.Errorf("read type %#x / %d bytes, want %#x / %d -- the packets are no "+
			"longer joined intact", typ, len(data), 0x01, 2*len(payload))
	}
}

// TestAZeroConfiguredCeilingLeavesTheTDSListenerUnbounded is the escape hatch,
// end to end: FABRIC_MAX_TDS_MESSAGE_BYTES=0 resolves to Config 0, and 0 must
// reach internal/tds as "no limit" rather than as "use the default".
//
// It LOWERS the ceiling first and then raises it off, because that ordering is
// what distinguishes a server that installed 0 from a server that installed
// nothing -- the latter would leave the 4 KiB ceiling from the first half in
// force and fail here.
func TestAZeroConfiguredCeilingLeavesTheTDSListenerUnbounded(t *testing.T) {
	tdsCeilingServer(t, testCeiling)
	if _, _, err := tds.ReadMessage(eomLess(4, 4000)); err == nil ||
		!strings.Contains(err.Error(), "MaxMessageBytes") {
		t.Fatalf("precondition: a %d-byte ceiling is not in force, so this test "+
			"cannot show it being lifted: %v", testCeiling, err)
	}

	tdsCeilingServer(t, 0)
	_, _, err := tds.ReadMessage(eomLess(4, 4000))
	if err == nil {
		t.Fatal("an EOM-less stream was read to completion, which cannot happen " +
			"-- the reader is finite and never sets EOM")
	}
	if strings.Contains(err.Error(), "MaxMessageBytes") {
		t.Fatalf("with MaxTDSMessageBytes=0 the listener is still bounded: %v -- "+
			"0 is the documented way to ask for no limit, and a released binary "+
			"whose sidecar returns a larger result set than the default has no "+
			"other way back", err)
	}
	// What a lifted ceiling fails on instead: the stream running out, which is
	// the pre-change behaviour this knob restores.
	if err != io.ErrUnexpectedEOF && err != io.EOF {
		t.Errorf("with the ceiling lifted, the read failed on something other "+
			"than the end of the stream: %v", err)
	}
}
