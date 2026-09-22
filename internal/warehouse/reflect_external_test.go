package warehouse

import (
	"context"
	"fmt"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

// fakeExternal is an ExternalDelta backed by an in-memory map, keyed by path
// relative to the shortcut's own root — what a real *onelake.Service resolves
// down to, tested against an actual target in internal/onelake.
type fakeExternal struct {
	files map[string][]byte // "_delta_log/0....json" -> content, "part-0.parquet" -> content
	calls int
}

func (f *fakeExternal) ExternalDeltaCommits(sc *store.Shortcut) ([]string, error) {
	f.calls++
	var names []string
	for p := range f.files {
		if rel, ok := cutPrefix(p, "_delta_log/"); ok {
			names = append(names, rel)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no commits")
	}
	return names, nil
}

func (f *fakeExternal) ExternalReadFile(sc *store.Shortcut, remainder string) ([]byte, error) {
	if b, ok := f.files[remainder]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("no such file %q", remainder)
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

func TestReflectorReadsAnExternalShortcutThroughExternal(t *testing.T) {
	st, _, itemID := seedLakehouse(t)
	if err := st.CreateShortcut(&store.Shortcut{ItemID: itemID, Path: "Tables", Name: "ext",
		TargetType: "ADLSGen2", TargetLocation: "https://example.blob.core.windows.net/c", TargetPath: "ext", ConnectionID: "conn"}); err != nil {
		t.Fatal(err)
	}
	external := &fakeExternal{files: map[string][]byte{
		"_delta_log/00000000000000000000.json": []byte(`{"add":{"path":"part-0.parquet"}}`),
		"part-0.parquet":                       writeParquet(t, []saleRow{{"us", 80}, {"eu", 60}}),
	}}
	db := testsupport.OpenMSSQL(t)
	r := &Reflector{External: external}
	done, err := r.Reflect(context.Background(), db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != "ext" {
		t.Fatalf("reflected %v, want [ext]", done)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM dbo.ext`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("dbo.ext: %d row(s), %v; want 2 rows", n, err)
	}

	// Unchanged: a second reflect touches external no further than the fingerprint
	// question needs, and does not re-read the table.
	callsAfterFirst := external.calls
	if _, err := r.Reflect(context.Background(), db, st, itemID); err != nil {
		t.Fatal(err)
	}
	if external.calls != callsAfterFirst+1 {
		t.Errorf("an unchanged reflect called ExternalDeltaCommits %d more time(s), want exactly 1 (the fingerprint question)", external.calls-callsAfterFirst)
	}
}

// Without an ExternalDelta, an external shortcut is not among the tables
// reflected — the same treatment as a folder with no _delta_log — rather than a
// failure that would block every OTHER table's reflection too.
func TestReflectorSkipsAnExternalShortcutWithNoExternalReaderWired(t *testing.T) {
	st, wsID, itemID := seedLakehouse(t)
	put(t, st, wsID, itemID, "Tables/own/part-0.parquet", writeParquet(t, []saleRow{{"us", 1}}))
	put(t, st, wsID, itemID, "Tables/own/_delta_log/00000000000000000000.json", []byte(`{"add":{"path":"part-0.parquet"}}`))
	if err := st.CreateShortcut(&store.Shortcut{ItemID: itemID, Path: "Tables", Name: "ext",
		TargetType: "ADLSGen2", TargetLocation: "https://example.blob.core.windows.net/c", TargetPath: "ext", ConnectionID: "conn"}); err != nil {
		t.Fatal(err)
	}
	db := testsupport.OpenMSSQL(t)
	r := &Reflector{} // External is nil
	done, err := r.Reflect(context.Background(), db, st, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0] != "own" {
		t.Fatalf("reflected %v, want only [own] — ext should be skipped, not fatal", done)
	}
}
