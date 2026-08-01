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
//     and is told how many it missed.
//
// A slow consumer degrades itself and nothing else. That is the whole reason
// the bus can exist without endangering the emulator's determinism.

// Event kinds carried on the bus.
const (
	KindFile     = "file"     // a OneLake path was written, renamed, or deleted
	KindTable    = "table"    // a Delta commit landed: a table has a new version
	KindJob      = "job"      // an item job started or reached a terminal state
	KindActivity = "activity" // a pipeline activity reached its outcome
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

	// KindDropped
	Dropped int64 `json:"dropped,omitempty"`
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
		// Tell a recovering subscriber what it missed before giving it more.
		if n := sub.dropped.Load(); n > 0 {
			select {
			case sub.ch <- Event{Seq: ev.Seq, At: ev.At, Kind: KindDropped, Dropped: n}:
				sub.dropped.Add(-n)
			default:
				sub.dropped.Add(1)
				continue
			}
		}
		select {
		case sub.ch <- ev:
		default:
			sub.dropped.Add(1) // never block a writer
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

// Subscribe returns a channel of events and a cancel func. The channel is
// buffered; a reader that falls behind gets KindDropped events rather than
// backpressure onto the emulator. Cancel is idempotent.
func (s *Store) Subscribe() (<-chan Event, func()) {
	b := s.bus
	sub := &subscriber{ch: make(chan Event, subBuffer)}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subs[id] = sub
	b.mu.Unlock()

	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			// Not closed: dispatch may be mid-send. Dropping the reference is
			// enough — the buffered channel is garbage once nobody holds it.
		})
	}
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
