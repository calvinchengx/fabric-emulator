package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
	"github.com/calvinchengx/fabric-emulator/portal"
)

// registerPortal mounts the operator portal: the embedded SPA at "/" and its
// read-only data endpoints under /_emulator/portal/. Like entra-emulator's
// admin portal this surface is unauthenticated — the /v1 contract requires
// bearer tokens, so the portal reads emulator state through the local-tooling
// escape hatch instead of impersonating a principal.
func (s *Server) registerPortal() {
	s.mux.HandleFunc("GET /_emulator/portal/workspaces", s.portalWorkspaces)
	s.mux.HandleFunc("GET /_emulator/portal/workspaces/{id}", s.portalWorkspaceDetail)
	s.mux.HandleFunc("GET /_emulator/portal/operations", s.portalOperations)
	s.mux.HandleFunc("GET /_emulator/portal/connections", s.portalConnections)
	s.mux.HandleFunc("GET /_emulator/portal/shortcuts", s.portalShortcuts)
	s.mux.HandleFunc("GET /_emulator/portal/capacities", s.portalCapacities)
	s.mux.HandleFunc("GET /_emulator/portal/jobs", s.portalJobs)
	s.mux.HandleFunc("GET /_emulator/portal/warehouse", s.portalWarehouse)
	s.mux.HandleFunc("GET /_emulator/portal/models", s.portalModels)
	s.mux.HandleFunc("GET /_emulator/portal/lineage", s.portalLineage)
	s.mux.HandleFunc("GET /_emulator/portal/table", s.portalTable)

	assets, err := portal.Dist()
	if err != nil {
		return // no embedded portal (should not happen with committed dist)
	}
	files := http.FileServerFS(assets)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		// An unrouted path under an API prefix is a 404, never the SPA. A
		// strict API client that hits an unimplemented or mistyped endpoint
		// must get a Fabric-shaped JSON error it can parse, not a web page:
		// Azure PowerShell dies on HTML with "Unexpected character
		// encountered while parsing value: <", which says nothing about the
		// real problem. Found by the Az e2e (docs/23).
		if isAPIPath(r.URL.Path) {
			writeJSONError(w, http.StatusNotFound, "UnknownEndpoint",
				"No such endpoint on this emulator.")
			return
		}
		// Serve real assets as-is; anything else falls back to the SPA shell
		// (hash routing means only "/" is ever navigated to, but deep links
		// should not 404).
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(assets, p); err == nil {
				files.ServeHTTP(w, r)
				return
			}
		}
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
}

// apiPrefixes are the path roots that belong to an API surface rather than to
// the operator portal. Anything under them must answer as an API — a JSON
// error — even when no route matches.
var apiPrefixes = []string{"/v1/", "/_emulator/", "/metadata/", "/subscriptions/"}

// isAPIPath reports whether a path belongs to an API surface. The bare prefix
// without its trailing slash counts too ("/v1" as well as "/v1/…").
func isAPIPath(p string) bool {
	for _, pre := range apiPrefixes {
		if strings.HasPrefix(p, pre) || p == strings.TrimSuffix(pre, "/") {
			return true
		}
	}
	return false
}

// writeJSONError emits the Fabric-shaped error envelope used across /v1.
func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("x-ms-public-api-error-code", code)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// portalWorkspace is the enriched list row the portal renders.
type portalWorkspace struct {
	*store.Workspace
	ItemCount         int                      `json:"itemCount"`
	RoleCount         int                      `json:"roleCount"`
	Git               *store.GitConnection     `json:"git"`
	WorkspaceIdentity *store.WorkspaceIdentity `json:"workspaceIdentity"`
}

func (s *Server) enrich(w *store.Workspace) (*portalWorkspace, error) {
	items, err := s.Store.ListItems(w.ID, "")
	if err != nil {
		return nil, err
	}
	roles, err := s.Store.ListRoleAssignments(w.ID)
	if err != nil {
		return nil, err
	}
	git, err := s.Store.GetGitConnection(w.ID)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	wi, err := s.Store.GetWorkspaceIdentity(w.ID)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	return &portalWorkspace{Workspace: w, ItemCount: len(items), RoleCount: len(roles), Git: git, WorkspaceIdentity: wi}, nil
}

func (s *Server) portalWorkspaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListAllWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	out := make([]*portalWorkspace, 0, len(list))
	for _, ws := range list {
		row, err := s.enrich(ws)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
			return
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

func (s *Server) portalWorkspaceDetail(w http.ResponseWriter, r *http.Request) {
	ws, err := s.Store.GetWorkspace(r.PathValue("id"))
	if err == store.ErrNotFound {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"message": "workspace not found"}})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	row, err := s.enrich(ws)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	items, err := s.Store.ListItems(ws.ID, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	roles, err := s.Store.ListRoleAssignments(ws.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace":         row.Workspace,
		"items":             items,
		"roleAssignments":   roles,
		"git":               row.Git,
		"workspaceIdentity": row.WorkspaceIdentity,
	})
}

// portalConnections lists connections as an explicit metadata-only shape:
// credential secret material is write-only in the store, and the explicit row
// keeps any future Connection field from leaking here by accident.
func (s *Server) portalConnections(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Store.ListConnections()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	type connRow struct {
		ID                   string `json:"id"`
		DisplayName          string `json:"displayName"`
		ConnectivityType     string `json:"connectivityType"`
		CredentialType       string `json:"credentialType"`
		SingleSignOnType     string `json:"singleSignOnType,omitempty"`
		ConnectionEncryption string `json:"connectionEncryption,omitempty"`
	}
	out := make([]connRow, 0, len(cs))
	for _, c := range cs {
		row := connRow{ID: c.ID, DisplayName: c.DisplayName, ConnectivityType: c.ConnectivityType}
		if c.CredentialDetails != nil {
			row.CredentialType = c.CredentialDetails.CredentialType
			row.SingleSignOnType = c.CredentialDetails.SingleSignOnType
			row.ConnectionEncryption = c.CredentialDetails.ConnectionEncryption
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

// portalShortcut is one OneLake shortcut with its owning workspace/item and a
// dangling flag — the target existed at create time but may have been deleted
// since (resolution fails at read time, matching real OneLake).
type portalShortcut struct {
	WorkspaceID       string `json:"workspaceId"`
	WorkspaceName     string `json:"workspaceName"`
	ItemID            string `json:"itemId"`
	ItemName          string `json:"itemName"`
	Path              string `json:"path"`
	Name              string `json:"name"`
	TargetWorkspaceID string `json:"targetWorkspaceId"`
	TargetItemID      string `json:"targetItemId"`
	TargetPath        string `json:"targetPath"`
	Dangling          bool   `json:"dangling"`
}

func (s *Server) portalShortcuts(w http.ResponseWriter, r *http.Request) {
	workspaces, err := s.Store.ListAllWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	out := make([]portalShortcut, 0)
	for _, ws := range workspaces {
		items, err := s.Store.ListItems(ws.ID, "")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
			return
		}
		for _, it := range items {
			shortcuts, err := s.Store.ListShortcuts(it.ID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
				return
			}
			for _, sc := range shortcuts {
				_, err := s.Store.GetItem(sc.TargetWorkspace, sc.TargetItem)
				if err != nil && err != store.ErrNotFound {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
					return
				}
				out = append(out, portalShortcut{
					WorkspaceID: ws.ID, WorkspaceName: ws.DisplayName,
					ItemID: it.ID, ItemName: it.DisplayName,
					Path: sc.Path, Name: sc.Name,
					TargetWorkspaceID: sc.TargetWorkspace, TargetItemID: sc.TargetItem, TargetPath: sc.TargetPath,
					Dangling: err == store.ErrNotFound,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

func (s *Server) portalCapacities(w http.ResponseWriter, r *http.Request) {
	caps, err := s.Store.ListCapacities()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	workspaces, err := s.Store.ListAllWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	type wsRef struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	}
	type capRow struct {
		*store.Capacity
		Workspaces []wsRef `json:"workspaces"`
	}
	out := make([]capRow, 0, len(caps))
	for _, c := range caps {
		row := capRow{Capacity: c, Workspaces: []wsRef{}}
		for _, ws := range workspaces {
			if ws.CapacityID == c.ID {
				row.Workspaces = append(row.Workspaces, wsRef{ID: ws.ID, DisplayName: ws.DisplayName})
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

// portalTablePreviewRows bounds the sample a node inspection returns. Enough to
// see the shape of the data; not enough to turn clicking a node into a table
// scan.
const portalTablePreviewRows = 20

// portalTable inspects one Delta table for the Data flow view: what it holds
// right now, read through the same warehouse reader the SQL endpoint uses.
//
// The flow stream says a table *changed*; this says what it changed *into*,
// which is the question a developer asks next.
func (s *Server) portalTable(w http.ResponseWriter, r *http.Request) {
	itemID := r.URL.Query().Get("itemId")
	name := strings.TrimPrefix(r.URL.Query().Get("table"), "Tables/")
	if itemID == "" || name == "" {
		writeJSON(w, http.StatusBadRequest,
			map[string]any{"error": map[string]string{"message": "itemId and table are required"}})
		return
	}
	out := map[string]any{"itemId": itemID, "table": "Tables/" + name, "version": latestDeltaVersion(s.Store, itemID, name)}
	tbl, err := warehouse.ReadDeltaTable(s.Store, itemID, name)
	if err != nil {
		// A table that cannot be read is not a server error: it may be a Files
		// path, or a table whose first commit has not landed. Say so plainly
		// and let the view render the reason.
		out["readable"] = false
		out["message"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	rows := tbl.Rows
	if len(rows) > portalTablePreviewRows {
		rows = rows[:portalTablePreviewRows]
	}
	preview := make([][]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = ""
				continue
			}
			cells[i] = fmt.Sprint(v)
		}
		preview = append(preview, cells)
	}
	out["readable"] = true
	out["columns"] = tbl.Columns
	out["rowCount"] = len(tbl.Rows)
	out["preview"] = preview
	out["truncated"] = len(tbl.Rows) > len(preview)
	writeJSON(w, http.StatusOK, out)
}

// latestDeltaVersion is the highest committed version in the table's log, or -1
// when there is no log to read. The warehouse reader replays the log but does
// not surface the version, and the flow stream reports versions — so the
// inspector must speak the same units.
func latestDeltaVersion(st *store.Store, itemID, name string) int64 {
	entries, err := st.ListOneLakePaths(itemID, path.Join("Tables", name, "_delta_log"), false)
	if err != nil {
		return -1
	}
	latest := int64(-1)
	for _, e := range entries {
		base := path.Base(e.RelPath)
		if !strings.HasSuffix(base, ".json") {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSuffix(base, ".json"), 10, 64)
		if err == nil && v > latest {
			latest = v
		}
	}
	return latest
}

// portalLineage serves the data-flow graph's edges: every recorded
// source→target movement, with item display names resolved so a node can be
// labelled by something a human recognises rather than a GUID.
func (s *Server) portalLineage(w http.ResponseWriter, r *http.Request) {
	edges, err := s.Store.ListAllLineageEdges(500)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	type edgeRow struct {
		JobID        string `json:"jobId"`
		ActivityName string `json:"activityName"`
		Producer     string `json:"producer"`
		SourceItemID string `json:"sourceItemId"`
		SourceItem   string `json:"sourceItem"`
		SourcePath   string `json:"sourcePath"`
		TargetItemID string `json:"targetItemId"`
		TargetItem   string `json:"targetItem"`
		TargetPath   string `json:"targetPath"`
		// SourceKind tells the graph whether the source is a Fabric item or a
		// source system reached through a connection. Without it the view would
		// have to infer "outside Fabric" from an empty path, which is the kind
		// of guess this codebase keeps out of its data.
		SourceKind string `json:"sourceKind,omitempty"`
		CreatedAt  int64  `json:"createdAt"`
	}
	names := map[string]string{}
	nameOf := func(itemID string) string {
		if n, ok := names[itemID]; ok {
			return n
		}
		n := ""
		if it, err := s.Store.GetItemByID(itemID); err == nil {
			n = it.DisplayName
		}
		names[itemID] = n
		return n
	}
	// A connection source resolves through a different table, so it needs its
	// own lookup — GetItemByID would return nothing and the node would be
	// labelled with a bare GUID, which is exactly what the resolution above
	// exists to avoid.
	connNames := map[string]string{}
	connNameOf := func(id string) string {
		if n, ok := connNames[id]; ok {
			return n
		}
		n := ""
		if c, err := s.Store.GetConnection(id); err == nil {
			n = c.DisplayName
		}
		connNames[id] = n
		return n
	}
	out := make([]edgeRow, 0, len(edges))
	for _, e := range edges {
		srcName := nameOf(e.SourceItemID)
		if e.SourceIsConnection() {
			srcName = connNameOf(e.SourceItemID)
		}
		out = append(out, edgeRow{
			JobID: e.JobID, ActivityName: e.ActivityName, Producer: e.Producer,
			SourceItemID: e.SourceItemID, SourceItem: srcName, SourcePath: e.SourcePath,
			TargetItemID: e.TargetItemID, TargetItem: nameOf(e.TargetItemID), TargetPath: e.TargetPath,
			SourceKind: e.SourceKind,
			CreatedAt:  e.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

func (s *Server) portalJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.ListJobInstances(100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	now := s.Clock.Now()
	type jobRow struct {
		ID          string `json:"id"`
		ItemID      string `json:"itemId"`
		ItemName    string `json:"itemName"`
		ItemType    string `json:"itemType"`
		WorkspaceID string `json:"workspaceId"`
		JobType     string `json:"jobType"`
		InvokeType  string `json:"invokeType"`
		Status      string `json:"status"`
		CreatedAt   int64  `json:"createdAt"`
	}
	out := make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		row := jobRow{
			ID: j.ID, ItemID: j.ItemID, JobType: j.JobType, InvokeType: j.InvokeType,
			Status: j.StatusAt(now), CreatedAt: j.CreatedAt,
		}
		// The owning item may have been deleted since the job ran; the row
		// still lists, just without item context.
		if it, err := s.Store.GetItemByID(j.ItemID); err == nil {
			row.ItemName, row.ItemType, row.WorkspaceID = it.DisplayName, it.Type, it.WorkspaceID
		} else if err != store.ErrNotFound {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
			return
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

// portalWarehouse reports whether the warehouse SQL surface is wired: config
// presence only — the DSN in FABRIC_WAREHOUSE_SQL_URL carries credentials and
// is never echoed.
func (s *Server) portalWarehouse(w http.ResponseWriter, r *http.Request) {
	state := "off"
	if s.TDS != nil {
		if s.TDS.Backend != nil {
			state = "relay"
		} else {
			state = "stub"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sqlTdsConfigured":       s.Cfg.SQLTDSAddr != "",
		"warehouseSqlConfigured": s.Cfg.WarehouseSQLURL != "",
		"tdsListener":            state,
	})
}

func (s *Server) portalOperations(w http.ResponseWriter, r *http.Request) {
	ops, err := s.Store.ListOperations(100)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
		return
	}
	now := s.Clock.Now()
	type opRow struct {
		ID        string `json:"id"`
		Kind      string `json:"kind"`
		Status    string `json:"status"`
		CreatedAt int64  `json:"createdAt"`
		ResultRef string `json:"resultRef"`
	}
	out := make([]opRow, 0, len(ops))
	for _, op := range ops {
		out = append(out, opRow{ID: op.ID, Kind: op.Kind, Status: op.StatusAt(now), CreatedAt: op.CreatedAt, ResultRef: op.ResultRef})
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}
