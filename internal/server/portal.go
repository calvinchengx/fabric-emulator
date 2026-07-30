package server

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
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
