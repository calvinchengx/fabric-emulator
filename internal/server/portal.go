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
