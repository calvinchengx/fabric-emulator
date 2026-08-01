package onelake

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Headers a notebook runtime sets so the storage layer can attribute I/O to the
// cell that caused it. The runtime executes cells one at a time, so it always
// knows which one is running — no parsing of user code, and no guessing.
const (
	HeaderJobID     = "x-ms-fabric-job-id"
	HeaderCellIndex = "x-ms-fabric-cell-index"
)

// observe records one dataset touch when the caller identified itself as a
// notebook cell. Silent no-op otherwise, so ordinary clients are unaffected.
//
// Direction comes from the method, which is exactly what the storage layer
// knows: a GET read, a PUT/PATCH wrote. Paths under Tables/<name> collapse to
// the table root — a Delta write touches many files, but one table.
// attributionOf reads the caller's own statement of which notebook cell it is
// running, so a write can say who caused it. The same values observe() uses —
// computed once here rather than derived twice.
func (s *Service) attributionOf(r *http.Request) store.Attribution {
	jobID, cellRaw := s.cellContext(r)
	if jobID == "" {
		return store.Attribution{}
	}
	cell, err := strconv.Atoi(cellRaw)
	if err != nil {
		return store.Attribution{JobID: jobID}
	}
	return store.CellBy(jobID, cell)
}

// cellContext returns the job id and cell index the caller identified itself
// with, by header or by bearer claim.
func (s *Service) cellContext(r *http.Request) (jobID, cellRaw string) {
	jobID, cellRaw = r.Header.Get(HeaderJobID), r.Header.Get(HeaderCellIndex)
	if jobID == "" {
		return s.attributionFromToken(r)
	}
	return jobID, cellRaw
}

func (s *Service) observe(r *http.Request, itemID, rel string) {
	jobID, cellRaw := r.Header.Get(HeaderJobID), r.Header.Get(HeaderCellIndex)
	if jobID == "" {
		// Engines built on Rust object_store (delta-rs, Sail) cannot set request
		// headers — the storage client takes credentials, not HTTP options. They
		// carry the same attribution in the bearer instead, minted with the
		// claims below, which the validator surfaces on the principal.
		jobID, cellRaw = s.attributionFromToken(r)
	}
	if jobID == "" || itemID == "" || rel == "" {
		return
	}
	cell, err := strconv.Atoi(cellRaw)
	if err != nil {
		return
	}
	var direction string
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		direction = store.AccessRead
	case http.MethodPut, http.MethodPatch, http.MethodPost, http.MethodDelete:
		direction = store.AccessWrite
	default:
		return
	}
	_ = s.Store.RecordNotebookAccess(&store.NotebookAccess{
		JobID: jobID, CellIndex: cell, ItemID: itemID,
		Path: TableRoot(rel), Direction: direction,
	})
}

// TableRoot collapses a path inside a managed table to the table root, so every
// Parquet part and _delta_log entry attributes to Tables/<name>. Other paths
// pass through unchanged.
func TableRoot(rel string) string {
	p := strings.Trim(rel, "/")
	if !strings.HasPrefix(p, "Tables/") {
		return p
	}
	seg := strings.Split(p, "/")
	if len(seg) < 2 {
		return p
	}
	return seg[0] + "/" + seg[1]
}

// attributionFromToken reads notebook attribution out of the request's bearer.
// The token is validated on the request path already; this re-validates rather
// than threading the principal through every handler, which keeps the hook a
// single call at the dispatch point.
func (s *Service) attributionFromToken(r *http.Request) (jobID, cell string) {
	if s.Auth == nil {
		return "", ""
	}
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if raw == "" || raw == r.Header.Get("Authorization") {
		return "", ""
	}
	p, err := s.Auth.Validate(raw)
	if err != nil || p == nil {
		return "", ""
	}
	return p.JobID, p.CellIndex
}
