package tds

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

func TestAcceptedReadsTheOutcome(t *testing.T) {
	okDone := done(doneFinal, 0)
	if ok, understood := accepted(okDone); !ok || !understood {
		t.Fatalf("clean DONE = (%v, %v); want accepted", ok, understood)
	}
	// An ERROR token means nothing moved.
	failed := concat(errorToken(2714, "There is already an object named 'x'."), done(doneError, 0))
	if ok, understood := accepted(failed); ok || !understood {
		t.Fatalf("ERROR stream = (%v, %v); want a understood rejection", ok, understood)
	}
	// The DONE error bit is the other way a failure is reported.
	if ok, understood := accepted(done(doneError, 0)); ok || !understood {
		t.Fatalf("DONE(error) = (%v, %v); want a understood rejection", ok, understood)
	}
	// A token this file cannot size leaves the outcome unknown — and unknown is
	// never treated as success.
	if ok, understood := accepted([]byte{0x81, 0x01, 0x02}); ok || understood {
		t.Fatalf("COLMETADATA stream = (%v, %v); want unknown", ok, understood)
	}
	if ok, understood := accepted(okDone[:5]); ok || understood {
		t.Fatalf("truncated DONE = (%v, %v); want unknown", ok, understood)
	}
}

func TestObserveBatchOnlyReportsAcceptedWrites(t *testing.T) {
	var got []string
	obs := func(db string, flows []tsql.Flow) {
		for _, f := range flows {
			got = append(got, db+"|"+f.Kind+"|"+strings.Join(f.Target, "."))
		}
	}
	batch := func(q string) []byte { return batchMsg(q) }

	// A write the engine accepted is reported, with the resolved database.
	observeBatch(obs, "wh-guid", PktSQLBatch, batch("SELECT a INTO dbo.dst FROM dbo.src"), done(doneFinal, 0))
	if len(got) != 1 || got[0] != "wh-guid|"+tsql.FlowSelectInto+"|dbo.dst" {
		t.Fatalf("accepted write = %v", got)
	}

	got = nil
	// A read moves nothing; a failed write moved nothing; an unreadable outcome
	// is not success; no database means no item to attribute to.
	observeBatch(obs, "wh-guid", PktSQLBatch, batch("SELECT * FROM dbo.src"), done(doneFinal, 0))
	observeBatch(obs, "wh-guid", PktSQLBatch, batch("DROP TABLE dbo.t"),
		concat(errorToken(3701, "Cannot drop"), done(doneError, 0)))
	observeBatch(obs, "wh-guid", PktSQLBatch, batch("DROP TABLE dbo.t"), []byte{0x81, 0x00})
	observeBatch(obs, "", PktSQLBatch, batch("DROP TABLE dbo.t"), done(doneFinal, 0))
	observeBatch(nil, "wh-guid", PktSQLBatch, batch("DROP TABLE dbo.t"), done(doneFinal, 0))
	if got != nil {
		t.Fatalf("reported something it should not have: %v", got)
	}
}

// TestSpliceObservesAcceptedWrite drives the real splice loop: the observation
// must happen on the byte-forwarding path (the one a real ODBC client uses),
// and only after the backend has answered.
func TestSpliceObservesAcceptedWrite(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	seen := make(chan string, 4)
	go spliceSession(clientA, backendA, false, false,
		func(db string, flows []tsql.Flow) {
			for _, f := range flows {
				seen <- db + "|" + f.Kind + "|" + strings.Join(f.Target, ".") +
					"<-" + strings.Join(f.Sources[0], ".")
			}
		}, "wh-guid")

	// Client sends a CTAS; the fake backend accepts it.
	go func() {
		typ, data, err := ReadMessage(backendB)
		if err != nil || typ != PktSQLBatch {
			return
		}
		_ = data
		_ = WriteMessage(backendB, PktTabular, done(doneFinal, 0))
	}()
	if err := WriteMessage(clientB, PktSQLBatch, batchMsg("CREATE TABLE dbo.gold AS SELECT * FROM dbo.silver")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMessage(clientB); err != nil { // the client's response comes first
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		// The statement reaching the observer is post-dialect-fix — the one the
		// engine actually ran (CTAS rewritten to SELECT … INTO), and the flow
		// names both ends of the movement.
		if got != "wh-guid|"+tsql.FlowSelectInto+"|dbo.gold<-dbo.silver" {
			t.Fatalf("observed %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the splice path never observed the accepted write")
	}
}
