package store

import (
	"encoding/json"
	"path"
	"strconv"
	"strings"
)

// Turning Delta commits into table events.
//
// Raw file events are a firehose: one table write is dozens of Parquet parts
// plus a log entry. But a write to `Tables/<name>/_delta_log/…0004.json` *is* a
// table-version event — the commit says what changed. Deriving that is what
// makes the stream legible enough to visualise a medallion, instead of a wall
// of part-file paths.
//
// The commit parse below is deliberately minimal and local. internal/warehouse
// has a fuller Delta reader, but it imports this package, so this one cannot
// import it back. Only the fields an event needs are read here; the warehouse
// reader remains the authority for actually reading a table.

// deltaLogCommit matches `Tables/<name>/_delta_log/<version>.json` and returns
// the table name and version. ok is false for anything else — including
// checkpoints and CRC files, which are not commits.
func deltaLogCommit(rel string) (table string, version int64, ok bool) {
	p := strings.Trim(rel, "/")
	seg := strings.Split(p, "/")
	if len(seg) != 4 || seg[0] != "Tables" || seg[2] != "_delta_log" {
		return "", 0, false
	}
	base := seg[3]
	if !strings.HasSuffix(base, ".json") {
		return "", 0, false
	}
	// Delta pads versions to 20 digits; ParseInt handles the leading zeros.
	v, err := strconv.ParseInt(strings.TrimSuffix(base, ".json"), 10, 64)
	if err != nil {
		return "", 0, false
	}
	return seg[1], v, true
}

// commitLine is the subset of a Delta action this file cares about.
type commitLine struct {
	Add *struct {
		Path string `json:"path"`
		// Stats is a JSON *string* in the Delta spec, not an object. Writers
		// that emit it (delta-rs, Spark, and ours) put numRecords in it, which
		// is the only honest source of a row count — counting would mean
		// reading every Parquet part on the write path.
		Stats string `json:"stats"`
	} `json:"add"`
	Remove *struct {
		Path string `json:"path"`
	} `json:"remove"`
}

// deriveTableEvent turns a `file` event for a Delta commit into a `table`
// event. Returns nil for any path that is not a commit, which is almost all of
// them — the check is a cheap string test before any database read.
func (s *Store) deriveTableEvent(ev Event) *Event {
	if ev.Kind != KindFile || ev.EventType != EventFileCreated {
		return nil
	}
	table, version, ok := deltaLogCommit(ev.Path)
	if !ok {
		return nil
	}
	p, err := s.GetOneLakePath(ev.ItemID, ev.Path)
	if err != nil {
		return nil // the commit was removed again before we looked
	}
	out := Event{
		At: ev.At, Kind: KindTable, WorkspaceID: ev.WorkspaceID, ItemID: ev.ItemID,
		Table: path.Join("Tables", table), Version: &version,
	}
	for _, line := range strings.Split(string(p.Content), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var a commitLine
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			continue // an action shape we do not model; the rest still counts
		}
		switch {
		case a.Add != nil:
			out.FilesAdded++
			out.RowsAdded += numRecords(a.Add.Stats)
		case a.Remove != nil:
			out.FilesRemoved++
		}
	}
	return &out
}

// numRecords pulls numRecords out of a Delta add action's stats string. Absent
// or unparseable stats yield 0 — the event then reports files without rows,
// which is honest, rather than a guessed count.
func numRecords(stats string) int64 {
	if stats == "" {
		return 0
	}
	var s struct {
		NumRecords int64 `json:"numRecords"`
	}
	if err := json.Unmarshal([]byte(stats), &s); err != nil {
		return 0
	}
	return s.NumRecords
}
