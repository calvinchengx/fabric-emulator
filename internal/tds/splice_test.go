package tds

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// batchMsg builds a SQLBatch payload: a minimal ALL_HEADERS block (just the
// 4-byte total-length DWORD) followed by the UTF-16LE query text.
func batchMsg(q string) []byte {
	out := binary.LittleEndian.AppendUint32(nil, 4)
	return append(out, str2ucs2(q)...)
}

func TestSpliceForward(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	defer clientA.Close()
	defer backendA.Close()
	go spliceSession(clientA, backendA, false)

	go func() { _ = WriteMessage(clientB, PktSQLBatch, batchMsg("SELECT 1")) }()
	typ, data, err := ReadMessage(backendB)
	if err != nil || typ != PktSQLBatch {
		t.Fatalf("backend recv type=%#x err=%v", typ, err)
	}
	if q := sqlBatchQuery(data); q != "SELECT 1" {
		t.Errorf("forwarded query = %q, want SELECT 1", q)
	}
	go func() { _ = WriteMessage(backendB, PktTabular, intResult(1)) }()
	rtyp, _, err := ReadMessage(clientB)
	if err != nil || rtyp != PktTabular {
		t.Fatalf("client recv type=%#x err=%v", rtyp, err)
	}
}

func TestSpliceForwardsReadOnlySelect(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	defer clientA.Close()
	defer backendA.Close()
	go spliceSession(clientA, backendA, true) // read-only surface

	// A SELECT on a read-only surface is still forwarded to the engine.
	go func() { _ = WriteMessage(clientB, PktSQLBatch, batchMsg("SELECT amount FROM sales")) }()
	if typ, _, err := ReadMessage(backendB); err != nil || typ != PktSQLBatch {
		t.Fatalf("read-only SELECT not forwarded: type=%#x err=%v", typ, err)
	}
}

func TestSpliceBackendClosed(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	defer clientA.Close()
	backendB.Close() // the engine is gone before any traffic

	done := make(chan error, 1)
	go func() { done <- spliceSession(clientA, backendA, false) }()
	go func() { _ = WriteMessage(clientB, PktSQLBatch, batchMsg("SELECT 1")) }()
	select {
	case <-done: // returned when the forward to the dead backend failed
	case <-time.After(2 * time.Second):
		t.Fatal("spliceSession did not return after the backend closed")
	}
}

func TestSpliceRejectWriteError(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	defer clientA.Close()
	defer backendA.Close()
	defer backendB.Close()

	done := make(chan error, 1)
	go func() { done <- spliceSession(clientA, backendA, true) }()
	// Send a write, then drop the client before it can read the rejection, so the
	// server's reject-write fails and the session ends.
	go func() {
		_ = WriteMessage(clientB, PktSQLBatch, batchMsg("INSERT INTO t VALUES (1)"))
		clientB.Close()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("spliceSession did not return after the client dropped")
	}
}

func TestSpliceReadOnlyRejectsWrite(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	defer clientA.Close()
	defer backendA.Close()
	go spliceSession(clientA, backendA, true) // read-only surface

	go func() { _ = WriteMessage(clientB, PktSQLBatch, batchMsg("INSERT INTO sales VALUES (1)")) }()
	// The client receives a rejection...
	rtyp, rdata, err := ReadMessage(clientB)
	if err != nil {
		t.Fatal(err)
	}
	if rtyp != PktTabular || !containsToken(rdata, 0xAA) {
		t.Errorf("read-only reject: type=%#x, ERROR token present=%v", rtyp, containsToken(rdata, 0xAA))
	}
	// ...and the write never reached the backend.
	_ = backendB.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := ReadMessage(backendB); err == nil {
		t.Error("a write on a read-only surface was forwarded to the engine")
	}
}
