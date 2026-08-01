package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
)

func newBusStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedItem creates the workspace + item a OneLake write needs.
func seedItem(t *testing.T, s *Store) *Item {
	t.Helper()
	ws := &Workspace{DisplayName: "flow-ws"}
	if err := s.CreateWorkspace(ws, Principal{ID: "p", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	it := &Item{WorkspaceID: ws.ID, DisplayName: "lake", Type: "Lakehouse"}
	if err := s.CreateItem(it, nil); err != nil {
		t.Fatal(err)
	}
	return it
}

// next waits for one event, failing rather than hanging if the bus is silent.
func next(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event within 3s")
		return Event{}
	}
}

func TestBusPublishesFileEvents(t *testing.T) {
	s := newBusStore(t)
	it := seedItem(t, s)
	events, cancel := s.Subscribe()
	defer cancel()

	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Files/landing/a.csv", Content: []byte("id\n1\n")}, false); err != nil {
		t.Fatal(err)
	}
	ev := next(t, events)
	if ev.Kind != KindFile || ev.EventType != EventFileCreated {
		t.Fatalf("kind/eventType = %s/%s", ev.Kind, ev.EventType)
	}
	if ev.Path != "Files/landing/a.csv" || ev.ItemID != it.ID || ev.WorkspaceID != it.WorkspaceID {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Seq != 1 || ev.At == 0 {
		t.Fatalf("seq/at = %d/%d", ev.Seq, ev.At)
	}

	// Deletes and renames are on the stream too, so a consumer sees the whole
	// life of a path rather than only its arrival.
	if err := s.RenameOneLakePath(it.ID, "Files/landing/a.csv", "Files/landing/b.csv"); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, events); ev.EventType != EventFileRenamed || ev.Path != "Files/landing/b.csv" {
		t.Fatalf("rename event = %+v", ev)
	}
	if err := s.DeleteOneLakePath(it.ID, "Files/landing/b.csv"); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, events); ev.EventType != EventFileDeleted {
		t.Fatalf("delete event = %+v", ev)
	}
}

func TestBusDoesNotDisturbDirectoryCreates(t *testing.T) {
	// A folder is not a file arriving; publishing one would put noise on every
	// consumer's stream.
	s := newBusStore(t)
	it := seedItem(t, s)
	events, cancel := s.Subscribe()
	defer cancel()

	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Files/folder", IsDir: true, Content: []byte{}}, false); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Files/folder/real.csv", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, events); ev.Path != "Files/folder/real.csv" {
		t.Fatalf("first event was %+v, want the file not the directory", ev)
	}
}

func TestBusDerivesTableEventsFromDeltaCommits(t *testing.T) {
	// The point of the whole design: a commit under _delta_log becomes a table
	// version, so a consumer can watch a medallion instead of part-file paths.
	s := newBusStore(t)
	it := seedItem(t, s)
	events, cancel := s.Subscribe()
	defer cancel()

	stats, _ := json.Marshal(map[string]any{"numRecords": 1203})
	commit := mustJSONL(
		map[string]any{"metaData": map[string]any{"schemaString": "{}"}},
		map[string]any{"remove": map[string]any{"path": "old.parquet"}},
		map[string]any{"add": map[string]any{"path": "part-0.parquet", "stats": string(stats)}},
	)
	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Tables/bronze_customers/_delta_log/00000000000000000004.json",
		Content: commit}, false); err != nil {
		t.Fatal(err)
	}
	// The raw file event still goes out — a consumer debugging a writer wants
	// it — and the derived table event follows it.
	if ev := next(t, events); ev.Kind != KindFile {
		t.Fatalf("first = %+v, want the file event", ev)
	}
	ev := next(t, events)
	if ev.Kind != KindTable {
		t.Fatalf("second = %+v, want the derived table event", ev)
	}
	if ev.Version == nil {
		t.Fatal("table event carries no version")
	}
	if ev.Table != "Tables/bronze_customers" || *ev.Version != 4 {
		t.Fatalf("table/version = %s/%d", ev.Table, *ev.Version)
	}
	if ev.RowsAdded != 1203 || ev.FilesAdded != 1 || ev.FilesRemoved != 1 {
		t.Fatalf("counts = rows %d, +%d files, -%d files", ev.RowsAdded, ev.FilesAdded, ev.FilesRemoved)
	}
	if ev.Seq <= 0 {
		t.Fatalf("derived event has no seq: %+v", ev)
	}
}

func TestDeltaLogCommitRecognition(t *testing.T) {
	cases := []struct {
		rel     string
		table   string
		version int64
		ok      bool
	}{
		{"Tables/t/_delta_log/00000000000000000000.json", "t", 0, true},
		{"Tables/t/_delta_log/00000000000000000012.json", "t", 12, true},
		// Not commits: data files, checkpoints, CRCs, nested tables, Files/.
		{"Tables/t/part-0.parquet", "", 0, false},
		{"Tables/t/_delta_log/00000000000000000010.checkpoint.parquet", "", 0, false},
		{"Tables/t/_delta_log/_last_checkpoint", "", 0, false},
		{"Tables/t/_delta_log/notanumber.json", "", 0, false},
		{"Files/landing/a.csv", "", 0, false},
		{"Tables/t/sub/_delta_log/00000000000000000001.json", "", 0, false},
		{"", "", 0, false},
	}
	for _, c := range cases {
		table, version, ok := deltaLogCommit(c.rel)
		if ok != c.ok || table != c.table || version != c.version {
			t.Errorf("deltaLogCommit(%q) = %q/%d/%v, want %q/%d/%v",
				c.rel, table, version, ok, c.table, c.version, c.ok)
		}
	}
}

func TestTableEventWithoutStatsReportsFilesNotRows(t *testing.T) {
	// A writer that omits stats gets an honest event: files changed, row count
	// unknown — never a guessed number.
	s := newBusStore(t)
	it := seedItem(t, s)
	events, cancel := s.Subscribe()
	defer cancel()

	commit := mustJSONL(map[string]any{"add": map[string]any{"path": "part-0.parquet"}})
	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Tables/t/_delta_log/00000000000000000000.json", Content: commit}, false); err != nil {
		t.Fatal(err)
	}
	next(t, events) // the file event
	ev := next(t, events)
	if ev.Kind != KindTable || ev.FilesAdded != 1 || ev.RowsAdded != 0 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	// The safety property the whole design rests on: the emit happens inside a
	// OneLake write on a single-connection store, so a subscriber that never
	// reads must not be able to stall a writer.
	s := newBusStore(t)
	it := seedItem(t, s)
	_, cancel := s.Subscribe() // deliberately never read from
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subBuffer*2; i++ {
			_ = s.CreateOneLakePath(&OneLakePath{
				WorkspaceID: it.WorkspaceID, ItemID: it.ID,
				RelPath: fmt.Sprintf("Files/x/%d.csv", i), Content: []byte("x")}, false)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writes stalled behind a subscriber that never reads")
	}

	// And the writes really landed — dropping is a delivery choice, never a
	// reason to lose data.
	paths, err := s.ListOneLakePaths(it.ID, "Files/x", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != subBuffer*2 {
		t.Fatalf("stored %d paths, want %d", len(paths), subBuffer*2)
	}
}

func TestSubscriberIsToldWhatItMissed(t *testing.T) {
	s := newBusStore(t)
	it := seedItem(t, s)
	events, cancel := s.Subscribe()
	defer cancel()

	// Overflow the buffer while not reading, then start reading: a gap the
	// consumer cannot see would be worse than one it is told about.
	for i := 0; i < subBuffer+50; i++ {
		if err := s.CreateOneLakePath(&OneLakePath{
			WorkspaceID: it.WorkspaceID, ItemID: it.ID,
			RelPath: fmt.Sprintf("Files/y/%d.csv", i), Content: []byte("x")}, false); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Kind == KindDropped {
				if ev.Dropped <= 0 {
					t.Fatalf("dropped notice with no count: %+v", ev)
				}
				return
			}
		case <-deadline:
			t.Fatal("never told about the dropped events")
		}
	}
}

func TestReplayServesLateJoinersAndSince(t *testing.T) {
	s := newBusStore(t)
	it := seedItem(t, s)

	// Nobody is subscribed yet — the ring still fills, so a developer who
	// starts watching after the run began sees what happened.
	for i := 0; i < 3; i++ {
		if err := s.CreateOneLakePath(&OneLakePath{
			WorkspaceID: it.WorkspaceID, ItemID: it.ID,
			RelPath: fmt.Sprintf("Files/z/%d.csv", i), Content: []byte("x")}, false); err != nil {
			t.Fatal(err)
		}
	}
	var all []Event
	deadline := time.After(3 * time.Second)
	for len(all) < 3 {
		select {
		case <-deadline:
			t.Fatalf("ring holds %d events, want 3", len(all))
		default:
		}
		all = s.Replay(0)
		time.Sleep(10 * time.Millisecond)
	}
	if all[0].Seq != 1 || all[2].Seq != 3 {
		t.Fatalf("replay seqs = %d…%d", all[0].Seq, all[2].Seq)
	}
	// since= skips what the client already has.
	if got := s.Replay(2); len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("Replay(2) = %+v", got)
	}
	if got := s.Replay(99); len(got) != 0 {
		t.Fatalf("Replay past the end = %+v", got)
	}
}

func TestCancelStopsDelivery(t *testing.T) {
	s := newBusStore(t)
	it := seedItem(t, s)
	events, cancel := s.Subscribe()

	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Files/a.csv", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	next(t, events)
	cancel()
	cancel() // idempotent

	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Files/b.csv", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	// Give the dispatcher time to have delivered, had it still been subscribed.
	time.Sleep(200 * time.Millisecond)
	select {
	case ev := <-events:
		t.Fatalf("received %+v after cancel", ev)
	default:
	}
}

func TestPublishAfterCloseIsSafe(t *testing.T) {
	// Closed-store paths are exercised across this package; the bus must not
	// panic on a send to a stopped dispatcher.
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.publish(Event{Kind: KindFile, Path: "Files/after-close.csv"})
	s.emitFileEvent(EventFileCreated, "w", "i", "Files/after-close.csv", Attribution{})
}

// mustJSONL renders Delta actions as the newline-delimited JSON a commit is.
func mustJSONL(actions ...map[string]any) []byte {
	var out []byte
	for _, a := range actions {
		b, err := json.Marshal(a)
		if err != nil {
			panic(err)
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	return out
}
