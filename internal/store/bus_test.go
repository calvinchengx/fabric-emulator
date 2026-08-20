package store

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
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
	t.Cleanup(func() { _ = s.Close() })
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
	sub := s.Subscribe()
	defer sub.Close()

	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Files/landing/a.csv", Content: []byte("id\n1\n")}, false); err != nil {
		t.Fatal(err)
	}
	ev := next(t, sub.C)
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
	if ev := next(t, sub.C); ev.EventType != EventFileRenamed || ev.Path != "Files/landing/b.csv" {
		t.Fatalf("rename event = %+v", ev)
	}
	if err := s.DeleteOneLakePath(it.ID, "Files/landing/b.csv"); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, sub.C); ev.EventType != EventFileDeleted {
		t.Fatalf("delete event = %+v", ev)
	}
}

func TestBusDoesNotDisturbDirectoryCreates(t *testing.T) {
	// A folder is not a file arriving; publishing one would put noise on every
	// consumer's stream.
	s := newBusStore(t)
	it := seedItem(t, s)
	sub := s.Subscribe()
	defer sub.Close()

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
	if ev := next(t, sub.C); ev.Path != "Files/folder/real.csv" {
		t.Fatalf("first event was %+v, want the file not the directory", ev)
	}
}

func TestBusDerivesTableEventsFromDeltaCommits(t *testing.T) {
	// The point of the whole design: a commit under _delta_log becomes a table
	// version, so a consumer can watch a medallion instead of part-file paths.
	s := newBusStore(t)
	it := seedItem(t, s)
	sub := s.Subscribe()
	defer sub.Close()

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
	if ev := next(t, sub.C); ev.Kind != KindFile {
		t.Fatalf("first = %+v, want the file event", ev)
	}
	ev := next(t, sub.C)
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
	sub := s.Subscribe()
	defer sub.Close()

	commit := mustJSONL(map[string]any{"add": map[string]any{"path": "part-0.parquet"}})
	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Tables/t/_delta_log/00000000000000000000.json", Content: commit}, false); err != nil {
		t.Fatal(err)
	}
	next(t, sub.C) // the file event
	ev := next(t, sub.C)
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
	sub := s.Subscribe() // deliberately never read from
	defer sub.Close()

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

func TestSubscriberCanSeeWhatItMissed(t *testing.T) {
	// A gap the consumer cannot see would be worse than one it is told about.
	//
	// Note what this asserts and what it does not. The *count* is available the
	// moment a drop happens — that is a real guarantee and is what this test
	// checks. Delivering a notice is the consumer's job (see TakeDropped): the
	// bus could only announce one while dispatching a later event, so a
	// subscriber that fell behind and then went quiet would never hear about
	// it. An earlier version of this test waited for such an announcement and
	// was therefore racy — it passed only when a late event happened to arrive
	// after the reader had freed a slot.
	s := newBusStore(t)
	it := seedItem(t, s)
	sub := s.Subscribe()
	defer sub.Close()

	const overflow = 50
	for i := 0; i < subBuffer+overflow; i++ {
		if err := s.CreateOneLakePath(&OneLakePath{
			WorkspaceID: it.WorkspaceID, ItemID: it.ID,
			RelPath: fmt.Sprintf("Files/y/%d.csv", i), Content: []byte("x")}, false); err != nil {
			t.Fatal(err)
		}
	}

	// Dispatch is asynchronous, so accumulate until the whole series is
	// accounted for rather than sampling once. The total is exact, not
	// approximate: subBuffer events fit and the rest cannot, so waiting for
	// equality is what makes this deterministic.
	//
	// Sampling once and then asserting the count had been cleared — the first
	// version of this — is a race, because dispatch is still draining and
	// still dropping while the assertion runs.
	var total int64
	deadline := time.After(10 * time.Second)
	for total < overflow {
		total += sub.TakeDropped()
		if total >= overflow {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("credited with %d of the %d events that overflowed", total, overflow)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if total != overflow {
		t.Fatalf("dropped %d, want exactly the %d that did not fit", total, overflow)
	}

	// Nothing is published after this, so the count stays cleared: a drop is
	// reported once, not on every poll.
	if again := sub.TakeDropped(); again != 0 {
		t.Fatalf("TakeDropped reported a further %d with nothing left to drop", again)
	}

	// And the events that did fit are still there to read.
	if ev := next(t, sub.C); ev.Kind != KindFile {
		t.Fatalf("buffered event = %+v", ev)
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

func TestCloseStopsDelivery(t *testing.T) {
	s := newBusStore(t)
	it := seedItem(t, s)
	sub := s.Subscribe()

	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Files/a.csv", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	next(t, sub.C)
	sub.Close()
	sub.Close() // idempotent

	if err := s.CreateOneLakePath(&OneLakePath{
		WorkspaceID: it.WorkspaceID, ItemID: it.ID,
		RelPath: "Files/b.csv", Content: []byte("x")}, false); err != nil {
		t.Fatal(err)
	}
	// Give the dispatcher time to have delivered, had it still been subscribed.
	time.Sleep(200 * time.Millisecond)
	select {
	case ev := <-sub.C:
		t.Fatalf("received %+v after Close", ev)
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
	_ = s.Close()
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

// TestPublishDuringCloseDoesNotPanic pins a shutdown crash.
//
// publish used to read `stopped` under the lock, RELEASE it, and only then send
// on b.raw. stop() sets the flag and closes the channel, so a publisher could
// pass the check and reach the send after the close. A send on a closed channel
// is a *ready* select case — the `default:` does not save it — so it panicked on
// the writer's own goroutine. In the server that goroutine is serving a OneLake
// write that had already committed: the event is best-effort, but the crash
// was not.
//
// The store's own writes emit events (emitFileEvent -> publish), so this races
// real publishers against Close the way a shutdown mid-request does. Without
// the fix this fails as `panic: send on closed channel`, and under -race also
// as a write/read data race on `stopped`.
func TestPublishDuringCloseDoesNotPanic(t *testing.T) {
	for range 50 { // the window is a few instructions wide; repeat to hit it
		s, err := Open("", clock.New())
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var stop atomic.Bool
		start := make(chan struct{})
		for i := range 8 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				// Publish continuously so writers are in flight BEFORE, DURING
				// and AFTER the close — a fixed burst tends to finish first and
				// never straddle it.
				for j := 0; !stop.Load(); j++ {
					s.publish(Event{Kind: KindFile, Path: fmt.Sprintf("f/%d/%d", i, j)})
				}
			}(i)
		}
		close(start)
		runtime.Gosched() // let the publishers get going before shutting down
		_ = s.Close()     // concurrent with every publisher above
		stop.Store(true)
		wg.Wait()
	}
}

// TestPublishNeverBlocksWhenTheQueueIsFull pins the bus's central contract: a
// writer is never stalled by the observability path. The raw queue is bounded,
// and when it fills, publish must DROP rather than wait — the alternative is a
// OneLake write blocking on a slow or absent consumer.
//
// The dispatch goroutine is stopped first so nothing drains the queue, then far
// more events than it can hold are published. If publish ever blocked, this
// would deadlock rather than fail, so it runs under a deadline.
func TestPublishNeverBlocksWhenTheQueueIsFull(t *testing.T) {
	s, err := Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	// Wedge the dispatcher: a subscriber that never reads, plus enough events to
	// overflow the 1024-deep raw queue several times over.
	sub := s.Subscribe()
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 5000 {
			s.publish(Event{Kind: KindFile, Path: fmt.Sprintf("f/%d", i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publish blocked when the queue was full; it must drop instead")
	}
}
