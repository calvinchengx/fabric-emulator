package store

import (
	"database/sql"
	"errors"
)

// OneLake event types, named as Fabric names them on the Real-Time hub.
const (
	EventFileCreated = "Microsoft.Fabric.OneLake.FileCreated"
	EventFileDeleted = "Microsoft.Fabric.OneLake.FileDeleted"
	EventFileRenamed = "Microsoft.Fabric.OneLake.FileRenamed"
	// EventEventstreamReceived is the stream-native Activator event: a Custom
	// HTTP produce landed, and a Reflex destination is bound. Not a file event.
	EventEventstreamReceived = "Microsoft.Fabric.Eventstream.EventReceived"
)

// FileEvent is one OneLake data-plane event.
//
// The emulator does not need a broker to produce these: every byte written to
// OneLake goes through this store, so a file event is observable **at the
// source**, whoever wrote it — an ADLS client, azcopy, delta-rs, a Copy
// activity, or the mirror writer. That is what makes the trigger a genuine
// emulation of Data Activator's OneLake source rather than a stub that only
// notices writes made through one API.
type FileEvent struct {
	Type        string
	WorkspaceID string
	ItemID      string
	RelPath     string
}

// emitFileEvent notifies the registered sink, if any. Called *after* the write
// has committed, so a subscriber never sees an event for a failed write.
//
// The call is synchronous and reentrant: a trigger may start a pipeline that
// writes more files, which emits more events. Cycles are broken by the
// dispatcher, not here (see internal/api/triggers.go).
func (s *Store) emitFileEvent(kind, workspaceID, itemID, relPath string, attr Attribution) {
	if relPath == "" {
		return
	}
	// Triggers first, synchronously: a Reflex must have fired by the time the
	// write returns.
	if s.FileEvents != nil {
		s.FileEvents(FileEvent{Type: kind, WorkspaceID: workspaceID, ItemID: itemID, RelPath: relPath})
	}
	// Observers second, asynchronously — queued and returned from immediately,
	// because a watching developer must never be able to slow a writer down.
	ev := Event{Kind: KindFile, EventType: kind,
		WorkspaceID: workspaceID, ItemID: itemID, Path: relPath}
	if !attr.Empty() {
		a := attr
		ev.Attribution = &a
	}
	s.publish(ev)
}

// EmitFileWritten reports that a staged write has completed — the flush step of
// the ADLS create/append/flush sequence, which is the point the DFS protocol
// considers a file written and the point Azure raises its own event.
//
// The store cannot infer this: mid-sequence the path exists and is simply
// empty, indistinguishable from an empty file. Only the protocol handler knows
// the write is finished, so it says so.
func (s *Store) EmitFileWritten(workspaceID, itemID, relPath string, attr Attribution) {
	s.emitFileEvent(EventFileCreated, workspaceID, itemID, relPath, attr)
}

// EventTrigger is a Reflex's subscription: an event shape to watch, and the
// item job to start when one matches.
//
// Real Fabric assembles this from an Eventstream plus a Reflex rule, bound in
// the portal — there is no public REST for the binding, so this flattened form
// is an **emulator-native** control surface (documented as such in
// docs/parity.md), not a claim to mirror an API that does not exist.
type EventTrigger struct {
	ID          string
	WorkspaceID string
	ReflexID    string
	DisplayName string
	Enabled     bool
	EventType   string
	// SourceItemID plus PathPrefix are the `subject` filter: which item's
	// OneLake storage to watch, and optionally which folder within it.
	SourceItemID string
	PathPrefix   string
	// The item job to start. TargetWorkspaceID defaults to the Reflex's own.
	TargetWorkspaceID string
	TargetItemID      string
	TargetJobType     string
	CreatedAt         int64
}

// Matches reports whether an event should start this trigger's job.
func (t *EventTrigger) Matches(ev FileEvent) bool {
	if !t.Enabled || t.EventType != ev.Type || t.SourceItemID != ev.ItemID {
		return false
	}
	if t.PathPrefix == "" {
		return true
	}
	return ev.RelPath == t.PathPrefix ||
		(len(ev.RelPath) > len(t.PathPrefix) && ev.RelPath[:len(t.PathPrefix)+1] == t.PathPrefix+"/")
}

const triggerCols = `id, workspace_id, reflex_id, display_name, enabled, event_type,
	source_item_id, path_prefix, target_workspace_id, target_item_id, target_job_type, created_at`

func scanTrigger(row interface{ Scan(...any) error }) (*EventTrigger, error) {
	t := &EventTrigger{}
	err := row.Scan(&t.ID, &t.WorkspaceID, &t.ReflexID, &t.DisplayName, &t.Enabled, &t.EventType,
		&t.SourceItemID, &t.PathPrefix, &t.TargetWorkspaceID, &t.TargetItemID, &t.TargetJobType, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// CreateEventTrigger stores a trigger.
func (s *Store) CreateEventTrigger(t *EventTrigger) error {
	if t.ID == "" {
		t.ID = NewID()
	}
	t.CreatedAt = s.Now()
	_, err := s.db.Exec(`
INSERT INTO event_triggers (`+triggerCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.WorkspaceID, t.ReflexID, t.DisplayName, t.Enabled, t.EventType,
		t.SourceItemID, t.PathPrefix, t.TargetWorkspaceID, t.TargetItemID, t.TargetJobType, t.CreatedAt)
	return err
}

// GetEventTrigger fetches one trigger scoped to its Reflex.
func (s *Store) GetEventTrigger(reflexID, id string) (*EventTrigger, error) {
	return scanTrigger(s.db.QueryRow(`SELECT `+triggerCols+`
FROM event_triggers WHERE reflex_id = ? AND id = ?`, reflexID, id))
}

// ListEventTriggers returns a Reflex's triggers, oldest first.
func (s *Store) ListEventTriggers(reflexID string) ([]*EventTrigger, error) {
	rows, err := s.db.Query(`SELECT `+triggerCols+`
FROM event_triggers WHERE reflex_id = ? ORDER BY created_at, id`, reflexID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectTriggers(rows)
}

// TriggersForItem returns every enabled trigger watching one item's storage —
// the candidate set for an event, narrowed in SQL so an idle tenant costs one
// indexed lookup per write rather than a full scan.
func (s *Store) TriggersForItem(itemID string) ([]*EventTrigger, error) {
	rows, err := s.db.Query(`SELECT `+triggerCols+`
FROM event_triggers WHERE source_item_id = ? AND enabled = 1 ORDER BY created_at, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectTriggers(rows)
}

func collectTriggers(rows *sql.Rows) ([]*EventTrigger, error) {
	var out []*EventTrigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateEventTrigger replaces a trigger's mutable fields.
func (s *Store) UpdateEventTrigger(t *EventTrigger) error {
	res, err := s.db.Exec(`
UPDATE event_triggers SET display_name = ?, enabled = ?, event_type = ?, source_item_id = ?,
	path_prefix = ?, target_workspace_id = ?, target_item_id = ?, target_job_type = ?
WHERE reflex_id = ? AND id = ?`,
		t.DisplayName, t.Enabled, t.EventType, t.SourceItemID, t.PathPrefix,
		t.TargetWorkspaceID, t.TargetItemID, t.TargetJobType, t.ReflexID, t.ID)
	if err != nil {
		return err
	}
	return oneRow(res)
}

// DeleteEventTrigger removes a trigger.
func (s *Store) DeleteEventTrigger(reflexID, id string) error {
	res, err := s.db.Exec(`DELETE FROM event_triggers WHERE reflex_id = ? AND id = ?`, reflexID, id)
	if err != nil {
		return err
	}
	return oneRow(res)
}
