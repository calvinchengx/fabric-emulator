// Package onelake serves the ADLS-Gen2-shaped data plane
// (onelake.dfs.fabric.microsoft.com): the filesystem is the workspace, the
// first path segment inside it is an item, and Fabric-managed folders are
// protected exactly as documented in onelake-api-parity.md — ADLS APIs can
// never create/rename/delete workspaces or items, an item's root and first
// level are read-only, disallowed query params reject the request, and
// banned headers are ignored but echoed via x-ms-rejected-headers.
package onelake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// StorageAudience is the only token audience OneLake accepts.
var StorageAudience = []string{"https://storage.azure.com", "https://storage.azure.com/"}

// Service handles the DFS surface.
type Service struct {
	Store *store.Store
	Auth  *auth.Validator // configured with the Storage audience
}

// New builds the service; the validator must carry StorageAudience.
func New(st *store.Store, v *auth.Validator) *Service {
	return &Service{Store: st, Auth: v}
}

// Headers OneLake ignores (unpermitted-action headers); echoed back in
// x-ms-rejected-headers rather than failing the call.
var ignoredHeaders = []string{
	"x-ms-owner", "x-ms-group", "x-ms-permissions", "x-ms-acls",
	"x-ms-encryption-key", "x-ms-encryption-algorithm", "x-ms-access-tier",
}

// Query params OneLake rejects outright (they change the whole call).
var rejectedActions = map[string]bool{"setaccesscontrol": true, "setaccesscontrolrecursive": true}

type dfsError struct {
	code   string
	status int
	msg    string
}

func writeDFSErr(w http.ResponseWriter, e dfsError) {
	w.Header().Set("x-ms-error-code", e.code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	fmt.Fprintf(w, `{"error":{"code":%q,"message":%q}}`, e.code, e.msg)
}

// permHeaders sets OneLake's canned permission response headers.
func permHeaders(w http.ResponseWriter) {
	w.Header().Set("x-ms-owner", "$superuser")
	w.Header().Set("x-ms-group", "$superuser")
	w.Header().Set("x-ms-permissions", "---------")
}

// ServeHTTP implements the DFS endpoint.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Banned headers: ignore + echo.
	var rejected []string
	for _, h := range ignoredHeaders {
		if r.Header.Get(h) != "" {
			rejected = append(rejected, h)
		}
	}
	if len(rejected) > 0 {
		w.Header().Set("x-ms-rejected-headers", strings.Join(rejected, ","))
	}
	// Rejected query params fail the whole request.
	if rejectedActions[strings.ToLower(r.URL.Query().Get("action"))] {
		writeDFSErr(w, dfsError{"UnsupportedQueryParameter", http.StatusBadRequest,
			"OneLake does not support setting access control via Azure Storage APIs."})
		return
	}

	p, err := s.Auth.ValidateRequest(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer authorization_uri="`+s.Auth.Issuer+`"`)
		writeDFSErr(w, dfsError{"AuthenticationFailed", http.StatusUnauthorized, err.Error()})
		return
	}

	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		// Account level: HEAD only.
		if r.Method == http.MethodHead {
			permHeaders(w)
			w.WriteHeader(http.StatusOK)
			return
		}
		writeDFSErr(w, dfsError{"OperationNotAllowedOnAccount", http.StatusBadRequest,
			"Only HEAD is supported at the account level."})
		return
	}

	ws, derr := s.resolveWorkspace(segs[0])
	if derr != nil {
		writeDFSErr(w, *derr)
		return
	}
	role, err := s.Store.RoleOf(ws.ID, p.ID)
	if err != nil {
		writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
		return
	}
	// OneLake API access is the ReadAll permission: Admin/Member/Contributor
	// only (roles-workspaces.md). Viewers read through the SQL endpoint
	// (ReadData), which the emulator does not model — so they are denied
	// here, exactly as in real Fabric.
	if store.RoleRank(role) < store.RoleRank(store.RoleContributor) {
		writeDFSErr(w, dfsError{"AuthorizationFailure", http.StatusForbidden,
			"OneLake API access requires ReadAll (the Contributor role or above); Viewers read via the SQL endpoint."})
		return
	}

	// Workspace (container) level.
	if len(segs) == 1 {
		switch {
		case r.Method == http.MethodHead:
			permHeaders(w)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("resource") == "filesystem":
			s.list(w, r, ws)
		default:
			// Managing workspaces is a Fabric-experience operation.
			writeDFSErr(w, dfsError{"OperationNotAllowedOnFilesystem", http.StatusConflict,
				"Workspaces are managed through Fabric experiences, not ADLS APIs."})
		}
		return
	}

	it, derr := s.resolveItem(ws.ID, segs[1])
	if derr != nil {
		writeDFSErr(w, *derr)
		return
	}
	rel := strings.Join(segs[2:], "/")

	// The item root (/{item}) and its first level (/{item}/Files, /Tables)
	// are Fabric-managed: readable, never created/renamed/deleted via ADLS.
	// CRUD is allowed only on paths *within* the managed folders.
	if len(segs) <= 3 {
		switch r.Method {
		case http.MethodHead:
			permHeaders(w)
			w.Header().Set("x-ms-resource-type", "directory")
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			writeDFSErr(w, dfsError{"PathIsDirectory", http.StatusBadRequest,
				"The path is a Fabric-managed folder."})
		default:
			writeDFSErr(w, dfsError{"OperationNotAllowedOnManagedFolder", http.StatusConflict,
				"Fabric-managed folders (the item root and its first level) cannot be created, renamed, or deleted via ADLS APIs."})
		}
		return
	}

	switch r.Method {
	case http.MethodPut: // create file or directory
		isDir := r.URL.Query().Get("resource") == "directory"
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		err := s.Store.CreateOneLakePath(&store.OneLakePath{
			WorkspaceID: ws.ID, ItemID: it.ID, RelPath: rel, IsDir: isDir, Content: body,
		})
		if err != nil {
			writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
			return
		}
		w.WriteHeader(http.StatusCreated)

	case http.MethodPatch: // append | flush
		s.patch(w, r, it.ID, rel)

	case http.MethodHead:
		pth, err := s.Store.GetOneLakePath(it.ID, rel)
		if err != nil {
			writeDFSErr(w, dfsError{"PathNotFound", http.StatusNotFound, "The path does not exist."})
			return
		}
		permHeaders(w)
		w.Header().Set("Content-Length", strconv.Itoa(len(pth.Content)))
		if pth.IsDir {
			w.Header().Set("x-ms-resource-type", "directory")
		} else {
			w.Header().Set("x-ms-resource-type", "file")
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodGet: // read file
		pth, err := s.Store.GetOneLakePath(it.ID, rel)
		if err != nil {
			writeDFSErr(w, dfsError{"PathNotFound", http.StatusNotFound, "The path does not exist."})
			return
		}
		if pth.IsDir {
			writeDFSErr(w, dfsError{"PathIsDirectory", http.StatusBadRequest, "The path is a directory."})
			return
		}
		permHeaders(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(pth.Content)))
		_, _ = w.Write(pth.Content)

	case http.MethodDelete:
		if err := s.Store.DeleteOneLakePath(it.ID, rel); err != nil {
			writeDFSErr(w, dfsError{"PathNotFound", http.StatusNotFound, "The path does not exist."})
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		writeDFSErr(w, dfsError{"UnsupportedHttpVerb", http.StatusMethodNotAllowed, "Unsupported method."})
	}
}

// patch handles ?action=append (body at position) and ?action=flush.
func (s *Service) patch(w http.ResponseWriter, r *http.Request, itemID, rel string) {
	action := strings.ToLower(r.URL.Query().Get("action"))
	pos, _ := strconv.ParseInt(r.URL.Query().Get("position"), 10, 64)
	switch action {
	case "append":
		data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if _, err := s.Store.AppendOneLakePath(itemID, rel, pos, data); err != nil {
			writeDFSErr(w, dfsError{"InvalidFlushPosition", http.StatusBadRequest, err.Error()})
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case "flush":
		pth, err := s.Store.GetOneLakePath(itemID, rel)
		if err != nil {
			writeDFSErr(w, dfsError{"PathNotFound", http.StatusNotFound, "The path does not exist."})
			return
		}
		if pos != int64(len(pth.Content)) {
			writeDFSErr(w, dfsError{"InvalidFlushPosition", http.StatusBadRequest, "Flush position does not match data length."})
			return
		}
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	default:
		writeDFSErr(w, dfsError{"UnsupportedQueryParameter", http.StatusBadRequest, "Unsupported action."})
	}
}

// list implements GET /{workspace}?resource=filesystem[&directory=][&recursive=].
func (s *Service) list(w http.ResponseWriter, r *http.Request, ws *store.Workspace) {
	recursive := strings.EqualFold(r.URL.Query().Get("recursive"), "true")
	directory := strings.Trim(r.URL.Query().Get("directory"), "/")

	type entry struct {
		Name          string `json:"name"`
		IsDirectory   string `json:"isDirectory,omitempty"`
		ContentLength string `json:"contentLength,omitempty"`
	}
	var out []entry

	if directory == "" {
		// Top level: items appear as directories named name.Type.
		items, err := s.Store.ListItems(ws.ID, "")
		if err != nil {
			writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
			return
		}
		for _, it := range items {
			name := it.DisplayName + "." + it.Type
			out = append(out, entry{Name: name, IsDirectory: "true"})
			if recursive {
				paths, err := s.Store.ListOneLakePaths(it.ID, "", true)
				if err != nil {
					writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
					return
				}
				for _, p := range paths {
					e := entry{Name: name + "/" + p.RelPath}
					if p.IsDir {
						e.IsDirectory = "true"
					} else {
						e.ContentLength = strconv.Itoa(len(p.Content))
					}
					out = append(out, e)
				}
			}
		}
	} else {
		segs := strings.SplitN(directory, "/", 2)
		it, derr := s.resolveItem(ws.ID, segs[0])
		if derr != nil {
			writeDFSErr(w, *derr)
			return
		}
		prefix := ""
		if len(segs) == 2 {
			prefix = segs[1]
		}
		paths, err := s.Store.ListOneLakePaths(it.ID, prefix, recursive)
		if err != nil {
			writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
			return
		}
		for _, p := range paths {
			e := entry{Name: segs[0] + "/" + p.RelPath}
			if p.IsDir {
				e.IsDirectory = "true"
			} else {
				e.ContentLength = strconv.Itoa(len(p.Content))
			}
			out = append(out, e)
		}
	}
	if out == nil {
		out = []entry{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"paths": out})
}

// resolveWorkspace accepts a GUID or a display name.
func (s *Service) resolveWorkspace(seg string) (*store.Workspace, *dfsError) {
	if ws, err := s.Store.GetWorkspace(seg); err == nil {
		return ws, nil
	}
	if ws, err := s.Store.GetWorkspaceByName(seg); err == nil {
		return ws, nil
	}
	return nil, &dfsError{"FilesystemNotFound", http.StatusNotFound, "No workspace matches " + seg + "."}
}

// resolveItem accepts a GUID or name.Type addressing.
func (s *Service) resolveItem(workspaceID, seg string) (*store.Item, *dfsError) {
	if it, err := s.Store.GetItem(workspaceID, seg); err == nil {
		return it, nil
	}
	if i := strings.LastIndexByte(seg, '.'); i > 0 {
		if it, err := s.Store.GetItemByName(workspaceID, seg[:i], seg[i+1:]); err == nil {
			return it, nil
		}
	}
	return nil, &dfsError{"PathNotFound", http.StatusNotFound, "No item matches " + seg + " (use name.ItemType or GUIDs)."}
}
