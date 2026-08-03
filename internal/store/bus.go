package store

import (
	"sync"
	"sync/atomic"
)

// The flow event bus: a passive, read-only projection of work the emulator is
// already doing, streamed so a developer can watch data move instead of
// reconstructing it afterwards. See docs/31-flow-observability.md.
//
// It publishes nothing of its own and polls nothing — every event is caused by
// a caller that was going to do that work anyway.
//
// # Why this is safe to hang off the write path
//
// Events originate inside OneLake writes, and the store runs with a single
// database connection. A subscriber that blocked would stall every writer in
// the emulator. So delivery is deliberately lossy:
//
//   - the write path only appends to a buffered channel and returns;
//   - one dispatch goroutine does the (occasionally expensive) derivation and
//     fan-out, off the caller's thread;
//   - a subscriber whose buffer is full has events **dropped**, never blocks,
//     and can collect the count of what it missed (Subscription.TakeDropped).
//
// A slow consumer degrades itself and nothing else. That is the whole reason
// the bus can exist without endangering the emulator's determinism.

// Event kinds carried on the bus.
const (
	KindFile     = "file"     // a OneLake path was written, renamed, or deleted
	KindTable    = "table"    // a Delta commit landed: a table has a new version
	KindJob      = "job"      // an item job started or reached a terminal state
	KindActivity = "activity" // a pipeline activity reached its outcome
	KindLineage  = "lineage"  // a source→target movement was recorded
	KindQuery    = "query"    // a semantic model was queried (the Power BI hop)
	KindDropped  = "dropped"  // a subscriber fell behind; N events were lost
)

// Event is one thing that happened, in the flat envelope the SSE endpoint
// serialises directly. Fields are per-kind and omitted when they do not apply.
type Event struct {
	Seq  int64  `json:"seq"`
	At   int64  `json:"at"` // emulator time, like every other timestamp here
	Kind string `json:"kind"`

	WorkspaceID string `json:"workspaceId,omitempty"`
	ItemID      string `json:"itemId,omitempty"`

	// KindFile
	EventType string `json:"eventType,omitempty"`
	Path      string `json:"path,omitempty"`

	// KindTable. Version is a pointer so that a table's *first* commit —
	// version 0, the common case for a fresh medallion — is still reported,
	// while file events do not carry a meaningless "version": 0.
	Table        string `json:"table,omitempty"`
	Version      *int64 `json:"version,omitempty"`
	RowsAdded    int64  `json:"rowsAdded,omitempty"`
	FilesAdded   int    `json:"filesAdded,omitempty"`
	FilesRemoved int    `json:"filesRemoved,omitempty"`

	// KindJob. Field names match the job-instance wire shape, so a consumer
	// reading the stream and a consumer polling the API see the same words.
	JobID         string `json:"jobId,omitempty"`
	JobType       string `json:"jobType,omitempty"`
	InvokeType    string `json:"invokeType,omitempty"`
	Status        string `json:"status,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`

	// KindActivity — pipeline.ActivityRun's fields, under its own JSON names.
	ActivityName string  `json:"activityName,omitempty"`
	ActivityType string  `json:"activityType,omitempty"`
	Error        string  `json:"error,omitempty"`
	Duration     float64 `json:"durationInSeconds,omitempty"`
	RetryAttempt int     `json:"retryAttempt,omitempty"`

	// KindLineage — a movement was recorded, so a graph can redraw without
	// waiting for a job to end. The edge itself stays in lineage_edges; this
	// carries just enough to describe the hop in a log line.
	SourceItemID string `json:"sourceItemId,omitempty"`
	SourcePath   string `json:"sourcePath,omitempty"`
	// SourceKind is set on a lineage event whose source is not a Fabric item.
	// Without it a consumer renders an empty sourcePath — a source system has
	// none — and shows "undefined" where the vendor's name belongs.
	SourceKind string `json:"sourceKind,omitempty"`
	TargetPath   string `json:"targetPath,omitempty"`
	Producer     string `json:"producer,omitempty"`

	// KindQuery — a read, not a movement, so it is an event and never an edge.
	Dataset string `json:"dataset,omitempty"`
	Queries int    `json:"queries,omitempty"`

	// KindDropped
	Dropped int64 `json:"dropped,omitempty"`

	// Who caused this, when that is known — see Attribution.
	Attribution *Attribution `json:"attribution,omitempty"`
}

// Attribution says which unit of work moved some bytes.
//
// It is **never inferred**. The same rule the lineage design holds to applies
// here: an engine reports it (a notebook runtime sets headers, or carries the
// same values as bearer claims when it is built on Rust object_store and
// cannot set headers), or the emulator's own executor knows it because it is
// the thing doing the write. Anything else leaves the field empty rather than
// guessing.
//
// This is a live debugging aid. `lineage_edges` remains the durable,
// authoritative record of a source→target movement.
type Attribution struct {
	JobID        string `json:"jobId,omitempty"`
	ActivityName string `json:"activityName,omitempty"`
	// CellIndex is a pointer because cell 0 is a real cell: a plain int could
	// not tell "the first cell" from "no cell at all".
	CellIndex *int `json:"cellIndex,omitempty"`
}

// Empty reports whether nothing is known about who caused a write.
func (a Attribution) Empty() bool {
	return a.JobID == "" && a.ActivityName == "" && a.CellIndex == nil
}

// ActivityBy attributes a write to a pipeline activity.
func ActivityBy(jobID, activityName string) Attribution {
	return Attribution{JobID: jobID, ActivityName: activityName}
}

// CellBy attributes a write to a notebook cell.
func CellBy(jobID string, cellIndex int) Attribution {
	return Attribution{JobID: jobID, CellIndex: &cellIndex}
}

// Job statuses a flow event reports at the moment they are known. Generic
// items derive status from the clock and have no such moment, so they get a
// Started event and nothing more — the stream reports what actually happened,
// and says nothing where there is nothing to say.
const (
	JobStarted = "Started"
)

// PublishJobEvent announces a job starting or reaching a terminal state.
func (s *Store) PublishJobEvent(workspaceID, itemID, jobID, jobType, invokeType, status, failureReason string) {
	s.publish(Event{
		Kind: KindJob, WorkspaceID: workspaceID, ItemID: itemID, JobID: jobID,
		JobType: jobType, InvokeType: invokeType, Status: status, FailureReason: failureReason,
	})
}

// PublishActivityEvent announces one pipeline activity's settled outcome.
func (s *Store) PublishActivityEvent(workspaceID, itemID, jobID, name, actType, status, errMsg string,
	duration float64, retry int) {
	s.publish(Event{
		Kind: KindActivity, WorkspaceID: workspaceID, ItemID: itemID, JobID: jobID,
		ActivityName: name, ActivityType: actType, Status: status, Error: errMsg,
		Duration: duration, RetryAttempt: retry,
	})
}

// ringSize is how many recent events are replayable via Replay, so a client
// that connects mid-run — or a portal that reloads — still sees what happened.
const ringSize = 1000

// subBuffer is one subscriber's queue depth before events start dropping.
const subBuffer = 256

type subscriber struct {
	ch      chan Event
	dropped atomic.Int64
}

type bus struct {
	mu      sync.Mutex
	seq     int64
	subs    map[int64]*subscriber
	nextID  int64
	ring    []Event
	raw     chan Event
	stopped bool
	done    chan struct{}
}

func newBus() *bus {
	b := &bus{
		subs: map[int64]*subscriber{},
		ring: make([]Event, 0, ringSize),
		// Generous: the raw queue absorbs a burst of Delta part-file writes
		// without the writer noticing.
		raw:  make(chan Event, 1024),
		done: make(chan struct{}),
	}
	return b
}

// run is the single dispatch goroutine. Being the only writer of seq and ring
// is what makes ordering and gap detection meaningful.
func (s *Store) runBus() {
	b := s.bus
	defer close(b.done)
	for ev := range b.raw {
		// A Delta commit is a *table* event wearing a file event's clothes.
		// Deriving it here rather than in the write path keeps the extra read
		// off the caller's thread.
		derived := s.deriveTableEvent(ev)
		b.dispatch(ev)
		if derived != nil {
			b.dispatch(*derived)
		}
	}
}

// dispatch stamps and fans out one event.
func (b *bus) dispatch(ev Event) {
	b.mu.Lock()
	b.seq++
	ev.Seq = b.seq
	if len(b.ring) == ringSize {
		copy(b.ring, b.ring[1:])
		b.ring = b.ring[:ringSize-1]
	}
	b.ring = append(b.ring, ev)
	subs := make([]*subscriber, 0, len(b.subs))
	for _, sub := range b.subs {
		subs = append(subs, sub)
	}
	b.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- ev:
		default:
			// Never block a writer. The count is the subscriber's to collect
			// and report — see Subscription.TakeDropped for why announcing it
			// from here cannot work.
			sub.dropped.Add(1)
		}
	}
}

// publish queues an event from a caller's thread. Non-blocking by contract: if
// even the raw queue is full the event is dropped, because the alternative is
// stalling a OneLake write.
func (s *Store) publish(ev Event) {
	b := s.bus
	if b == nil {
		return
	}
	ev.At = s.Now()
	b.mu.Lock()
	stopped := b.stopped
	b.mu.Unlock()
	if stopped {
		return
	}
	select {
	case b.raw <- ev:
	default:
	}
}

// Subscription is one consumer's view of the bus: a buffered channel of events
// and the count of what it was too slow to receive.
type Subscription struct {
	// C delivers events. Buffered, and never blocked on — a reader that falls
	// behind loses events rather than applying backpressure to a OneLake write.
	C <-chan Event

	sub  *subscriber
	once sync.Once
	stop func()
}

// TakeDropped returns how many events this subscriber has missed since the
// last call, and resets the count.
//
// Reporting drops is the *consumer's* job, not the bus's, and that division is
// forced by the problem rather than chosen. The bus can only announce a drop
// while dispatching some later event — so a subscriber that falls behind and
// then goes quiet would never be told, which is precisely when it most needs
// to know. A consumer, by contrast, can ask whenever it likes; the SSE handler
// asks on every loop iteration and on its keepalive tick, so a gap surfaces
// within one interval whether or not traffic continues.
func (s *Subscription) TakeDropped() int64 {
	return s.sub.dropped.Swap(0)
}

// Close unsubscribes. Idempotent.
func (s *Subscription) Close() {
	s.once.Do(s.stop)
}

// Subscribe registers a consumer.
func (s *Store) Subscribe() *Subscription {
	b := s.bus
	sub := &subscriber{ch: make(chan Event, subBuffer)}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subs[id] = sub
	b.mu.Unlock()

	return &Subscription{C: sub.ch, sub: sub, stop: func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
		// Not closed: dispatch may be mid-send. Dropping the reference is
		// enough — the buffered channel is garbage once nobody holds it.
	}}
}

// Replay returns buffered events with Seq greater than since, oldest first.
// A since of 0 returns everything still buffered.
func (s *Store) Replay(since int64) []Event {
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, 0, len(b.ring))
	for _, ev := range b.ring {
		if ev.Seq > since {
			out = append(out, ev)
		}
	}
	return out
}

// stopBus shuts the dispatch goroutine down and waits for it, so a closed store
// has no goroutine still holding a database handle.
func (b *bus) stop() {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true
	b.mu.Unlock()
	close(b.raw)
	<-b.done
}
