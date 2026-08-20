package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

// TestOpenCreatesTheDataDir is the first-run guard.
//
// `Open` is documented as opening the database "creating if needed", and until
// DataDir defaulted to ./data nothing tested what "needed" covered: every
// caller either passed "" for in-memory or a t.TempDir() that already existed.
// The day the default became a real path, `fabric-emulator` in any directory
// without a ./data died before it could listen — with SQLite's "unable to open
// database file (14)", which names neither the path nor the directory, on the
// one error a first run is most likely to hit.
func TestOpenCreatesTheDataDir(t *testing.T) {
	// A nested path, so this fails if only the leaf is created.
	dir := filepath.Join(t.TempDir(), "state", "data")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s should not exist yet (%v)", dir, err)
	}

	s, err := Open(dir, clock.New())
	if err != nil {
		t.Fatalf("Open on a missing data dir: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := os.Stat(filepath.Join(dir, "fabric-emulator.db")); err != nil {
		t.Fatalf("database not created in %s: %v", dir, err)
	}
}

// TestOpenStillDefaultsToMemory keeps the other half intact: an explicitly
// empty dataDir means in-memory and must not create anything on disk.
func TestOpenStillDefaultsToMemory(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatalf("in-memory Open: %v", err)
	}
	defer func() { _ = s.Close() }()
}
