package server_test

// ADLS Gen2, Amazon S3 and Dataverse shortcuts as tables on a lakehouse's SQL
// analytics endpoint — the same "shortcuts function as tables" rule #526 built
// for OneLake shortcuts, over an external target instead (docs/61).

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/parquet-go/parquet-go"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// fakeBlobTarget is an in-memory Azure Blob / ADLS Gen2-shaped server: plain
// GETs by path, and the List Blobs API (?restype=container&comp=list&prefix=)
// enumerating whatever has been Put under that prefix. Enough to drive the
// reflector's listing and reading without a real storage account.
type fakeBlobTarget struct {
	mu    sync.Mutex
	files map[string][]byte
}

func newFakeBlobTarget() *fakeBlobTarget { return &fakeBlobTarget{files: map[string][]byte{}} }

func (f *fakeBlobTarget) put(path string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = content
}

func (f *fakeBlobTarget) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Query().Get("comp") == "list" {
			prefix := r.URL.Query().Get("prefix")
			var b strings.Builder
			b.WriteString(`<?xml version="1.0" encoding="utf-8"?><EnumerationResults><Blobs>`)
			for name := range f.files {
				if strings.HasPrefix(name, prefix) {
					fmt.Fprintf(&b, "<Blob><Name>%s</Name></Blob>", name)
				}
			}
			b.WriteString(`</Blobs><NextMarker></NextMarker></EnumerationResults>`)
			_, _ = w.Write([]byte(b.String()))
			return
		}
		content, ok := f.files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	}))
}

// putDelta adds one Delta commit (a new Parquet part, added — not replaced —
// by a new _delta_log entry, as a real writer appends) to the fake target under
// root.
func (f *fakeBlobTarget) putDelta(t *testing.T, root string, version int, rows []whRow) {
	t.Helper()
	var buf bytes.Buffer
	pw := parquet.NewGenericWriter[whRow](&buf)
	if _, err := pw.Write(rows); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	part := fmt.Sprintf("part-%d.parquet", version)
	f.put(root+"/"+part, buf.Bytes())
	f.put(fmt.Sprintf("%s/_delta_log/%020d.json", root, version), []byte(fmt.Sprintf(`{"add":{"path":%q}}`, part)))
}

func TestAnExternalShortcutReadsAsATableOnTheEndpoint(t *testing.T) {
	f := newSecFixture(t)
	consumer, _ := f.lakehouse(t)

	target := newFakeBlobTarget()
	ts := target.server()
	t.Cleanup(ts.Close)
	target.putDelta(t, "orders", 0, []whRow{{"us", 80}, {"eu", 60}})

	conn := &store.Connection{DisplayName: "adls", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := f.srv.Store.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Store.CreateShortcut(&store.Shortcut{ItemID: consumer.ID, Path: "Tables", Name: "orders",
		TargetType: "ADLSGen2", TargetLocation: ts.URL, TargetPath: "orders", ConnectionID: conn.ID}); err != nil {
		t.Fatal(err)
	}

	db, err := f.open(t, entra.DaemonClientID, consumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT COUNT(*) FROM dbo.orders`); n != 2 {
		t.Errorf("the external shortcut reads %d row(s), want 2", n)
	}

	// The source changes; the shortcut follows it, as it is not a copy. A commit
	// that only adds a file adds its rows to the table's.
	target.putDelta(t, "orders", 1, []whRow{{"ap", 1}})
	db, err = f.open(t, entra.DaemonClientID, consumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, db, `SELECT COUNT(*) FROM dbo.orders`); n != 3 {
		t.Errorf("after the source changed the external shortcut reads %d row(s), want 3 (2 original + 1 new)", n)
	}
}
