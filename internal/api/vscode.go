package api

// This file terminates the private shared-backend/MWC protocol used by
// Microsoft's Fabric Data Engineering VS Code extension. The adapter keeps the
// generic Fabric item store authoritative; it only translates wire shapes.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/notebook"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) registerVSCodeCompatibility(mux *http.ServeMux) {
	// GLOBAL service discovery. DNS-pinning api.powerbi.com to the emulator is
	// enough to redirect the extension; it subsequently uses this returned host.
	mux.HandleFunc("PUT /spglobalservice/GetOrInsertClusterUrisByTenantLocation", a.withPBIOrControlAuth(a.vscodeCluster))
	mux.HandleFunc("GET /metadata/v201606/workspaces/", a.withPBIOrControlAuth(a.vscodeWorkspaces))
	mux.HandleFunc("GET /metadata/workspaces/{wid}/artifacts", a.withPBIOrControlAuth(a.vscodeArtifacts))
	mux.HandleFunc("POST /metadata/workspaces/{wid}/artifacts", a.withPBIOrControlAuth(a.vscodeCreateArtifact))
	mux.HandleFunc("GET /metadata/artifacts/{iid}", a.withPBIOrControlAuth(a.vscodeArtifact))
	mux.HandleFunc("PATCH /metadata/artifacts/{iid}", a.withPBIOrControlAuth(a.vscodeUpdateArtifact))
	mux.HandleFunc("DELETE /metadata/artifacts/{iid}", a.withPBIOrControlAuth(a.vscodeDeleteArtifact))
	mux.HandleFunc("POST /metadata/artifacts/{iid}/jobs/sparkjob", a.withPBIOrControlAuth(a.vscodeRunSparkJob))
	mux.HandleFunc("DELETE /metadata/artifacts/{iid}/jobs/{jid}", a.withPBIOrControlAuth(a.vscodeCancelSparkJob))
	mux.HandleFunc("POST /metadata/datahub/V2/artifacts", a.withPBIOrControlAuth(a.vscodeDatahubArtifacts))
	mux.HandleFunc("POST /metadata/v201606/generatemwctoken", a.withPBIOrControlAuth(a.vscodeMWCToken))
	mux.HandleFunc("POST /metadata/v201606/generatemwctokenv2", a.withPBIOrControlAuth(a.vscodeMWCToken))
	mux.HandleFunc("GET /metadata/b2b/userAssociatedTenants", a.withPBIOrControlAuth(func(w http.ResponseWriter, _ *http.Request, _ *auth.Principal) {
		writeJSON(w, http.StatusOK, []any{})
	}))

	content := "/webapi/capacities/{cid}/workloads/Notebook/Data/Direct/api/workspaces/{wid}/artifacts/{iid}/content"
	mux.HandleFunc("GET "+content, a.withMWCAuth(a.vscodeNotebookContent))
	mux.HandleFunc("HEAD "+content, a.withMWCAuth(a.vscodeNotebookContent))
	mux.HandleFunc("PUT "+content, a.withMWCAuth(a.vscodeUpdateNotebookContent))
	resourceBase := "/webapi/capacities/{cid}/workloads/Notebook/Data/Direct/api/workspaces/{wid}/artifacts/{iid}/filesystem"
	resources := resourceBase + "/workdir"
	mux.HandleFunc("GET "+resourceBase+"/workdirUsage", a.withMWCAuth(a.vscodeResourceUsage))
	mux.HandleFunc("GET "+resources+"/{path...}", a.withMWCAuth(a.vscodeResourceGet))
	mux.HandleFunc("PUT "+resources+"/{path...}", a.withMWCAuth(a.vscodeResourcePut))
	mux.HandleFunc("DELETE "+resources+"/{path...}", a.withMWCAuth(a.vscodeResourceDelete))
	mux.HandleFunc("GET /webapi/capacities/{cid}/workloads/SparkCore/SparkCoreService/direct/v1/monitoring/workspaces/{wid}/artifacts/{iid}/jobs", a.withMWCAuth(a.vscodeSparkJobs))
	mux.HandleFunc("GET /webapi/capacities/{cid}/workloads/Lakehouse/LakehouseService/direct/v1/workspaces/{wid}/artifacts/Lakehouse/{iid}/tables", a.withMWCAuth(a.vscodeLakehouseTables))
}

func (a *API) withPBIOrControlAuth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		validator := a.PBIAuth
		if validator == nil {
			validator = a.Auth
		}
		if validator == nil {
			writeErr(w, http.StatusNotImplemented, "VSCodeCompatibilityNotConfigured", "A token validator is required.")
			return
		}
		p, err := validator.ValidateRequest(r)
		if err != nil && validator != a.Auth && a.Auth != nil {
			p, err = a.Auth.ValidateRequest(r)
		}
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "TokenInvalid", err.Error())
			return
		}
		h(w, r, p)
	}
}

func (a *API) withMWCAuth(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(strings.ToLower(hdr), "mwctoken ") {
			writeErr(w, http.StatusUnauthorized, "TokenInvalid", "missing MwcToken authorization")
			return
		}
		token := strings.TrimSpace(hdr[len("MwcToken "):])
		validator := a.PBIAuth
		if validator == nil {
			validator = a.Auth
		}
		if validator == nil {
			writeErr(w, http.StatusNotImplemented, "VSCodeCompatibilityNotConfigured", "A token validator is required.")
			return
		}
		p, err := validator.Validate(token)
		if err != nil && validator != a.Auth && a.Auth != nil {
			p, err = a.Auth.Validate(token)
		}
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "TokenInvalid", err.Error())
			return
		}
		h(w, r, p)
	}
}

func (a *API) vscodeCluster(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	writeJSON(w, http.StatusOK, map[string]string{"FixedClusterUri": "https://" + r.Host})
}

func (a *API) vscodeWorkspaces(w http.ResponseWriter, _ *http.Request, p *auth.Principal) {
	workspaces, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(workspaces))
	for _, ws := range workspaces {
		out = append(out, map[string]any{"id": ws.ID, "name": ws.DisplayName, "displayName": ws.DisplayName, "description": ws.Description, "capacityObjectId": ws.CapacityID})
	}
	writeJSON(w, http.StatusOK, out)
}

func vscodeArtifactType(itemType string) string {
	if itemType == "Notebook" {
		return "SynapseNotebook"
	}
	return itemType
}

func vscodeItemType(artifactType string) string {
	if artifactType == "SynapseNotebook" {
		return "Notebook"
	}
	return artifactType
}

func (a *API) vscodeArtifactBody(it *store.Item) map[string]any {
	ws, _ := a.Store.GetWorkspace(it.WorkspaceID)
	capacityID := ""
	if ws != nil {
		capacityID = ws.CapacityID
	}
	return map[string]any{
		"objectId": it.ID, "artifactObjectId": it.ID, "artifactType": vscodeArtifactType(it.Type),
		"displayName": it.DisplayName, "description": it.Description,
		"folderObjectId": it.WorkspaceID, "workspaceObjectId": it.WorkspaceID,
		"capacityObjectId": capacityID, "lastUpdatedDate": time.Unix(it.CreatedAt, 0).UTC().Format(time.RFC3339),
	}
}

func (a *API) vscodeArtifacts(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleViewer); !ok {
		return
	}
	items, err := a.Store.ListItems(wid, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		out = append(out, a.vscodeArtifactBody(it))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) vscodeCreateArtifact(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleContributor); !ok {
		return
	}
	var body struct {
		ArtifactType, DisplayName, Description, WorkloadPayload string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.DisplayName) == "" || strings.TrimSpace(body.ArtifactType) == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "artifactType and displayName are required.")
		return
	}
	it := &store.Item{WorkspaceID: wid, Type: vscodeItemType(body.ArtifactType), DisplayName: body.DisplayName, Description: body.Description}
	var parts []store.DefinitionPart
	if body.WorkloadPayload != "" {
		if it.Type == "Notebook" {
			parts = vscodeNotebookParts([]byte(body.WorkloadPayload))
		} else {
			parts = []store.DefinitionPart{vscodeWorkloadPart(body.WorkloadPayload)}
		}
	}
	if err := a.Store.CreateItem(it, parts); err != nil {
		status := http.StatusInternalServerError
		code := "InternalError"
		if errors.Is(err, store.ErrNameConflict) {
			status, code = http.StatusConflict, "ArtifactDisplayNameAlreadyInUse"
		}
		writeErr(w, status, code, err.Error())
		return
	}
	w.Header().Set("ETag", vscodeETag([]byte(body.WorkloadPayload)))
	writeJSON(w, http.StatusCreated, a.vscodeArtifactBody(it))
}

func (a *API) vscodeAuthorizedItem(w http.ResponseWriter, iid string, p *auth.Principal, role string) (*store.Item, bool) {
	it, err := a.Store.GetItemByID(iid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "ArtifactNotFound", "The artifact is not available.")
		return nil, false
	}
	if _, _, ok := a.requireRole(w, it.WorkspaceID, p, role); !ok {
		return nil, false
	}
	return it, true
}

func (a *API) vscodeArtifact(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleViewer)
	if !ok {
		return
	}
	body := a.vscodeArtifactBody(it)
	if raw, err := a.vscodeNotebookJSON(it.ID); err == nil {
		body["workloadPayload"] = string(raw)
	} else if payload, err := a.vscodeWorkloadPayload(it.ID); err == nil {
		body["workloadPayload"] = payload
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *API) vscodeUpdateArtifact(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleContributor)
	if !ok {
		return
	}
	var body struct{ DisplayName, Description, WorkloadPayload *string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed JSON body.")
		return
	}
	if body.DisplayName != nil {
		it.DisplayName = *body.DisplayName
	}
	if body.Description != nil {
		it.Description = *body.Description
	}
	if err := a.Store.UpdateItem(it); err != nil {
		writeErr(w, http.StatusConflict, "ArtifactUpdateFailed", err.Error())
		return
	}
	if body.WorkloadPayload != nil {
		parts := []store.DefinitionPart{vscodeWorkloadPart(*body.WorkloadPayload)}
		if it.Type == "Notebook" {
			parts = vscodeNotebookParts([]byte(*body.WorkloadPayload))
		}
		if err := a.Store.SetDefinition(it.ID, parts); err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, a.vscodeArtifactBody(it))
}

func (a *API) vscodeDeleteArtifact(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleContributor)
	if !ok {
		return
	}
	if err := a.Store.DeleteItem(it.WorkspaceID, it.ID); err != nil {
		writeErr(w, http.StatusNotFound, "ArtifactNotFound", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *API) vscodeDatahubArtifacts(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body struct {
		SupportedTypes []string `json:"supportedTypes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.SupportedTypes) == 0 {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "supportedTypes is required.")
		return
	}
	workspaces, err := a.Store.ListWorkspacesFor(p.ID)
	if err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	wanted := map[string]bool{}
	for _, typ := range body.SupportedTypes {
		wanted[vscodeItemType(typ)] = true
	}
	var out []map[string]any
	for _, ws := range workspaces {
		items, err := a.Store.ListItems(ws.ID, "")
		if err != nil {
			writeErr(w, 500, "InternalError", err.Error())
			return
		}
		for _, it := range items {
			if wanted[it.Type] {
				b := a.vscodeArtifactBody(it)
				b["workspaceName"] = ws.DisplayName
				out = append(out, b)
			}
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) vscodeMWCToken(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	hdr := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(hdr, "Bearer "))
	if token == "" {
		writeErr(w, 401, "TokenInvalid", "missing bearer token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"Token": token, "TargetUriHost": r.Host})
}

func (a *API) vscodeNotebookContent(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleContributor)
	if !ok {
		return
	}
	if it.WorkspaceID != r.PathValue("wid") || it.Type != "Notebook" {
		writeErr(w, 404, "ArtifactNotFound", "The notebook is not available.")
		return
	}
	raw, err := a.vscodeNotebookJSON(it.ID)
	if err != nil {
		writeErr(w, 500, "InvalidNotebookDefinition", err.Error())
		return
	}
	w.Header().Set("ETag", vscodeETag(raw))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

func (a *API) vscodeUpdateNotebookContent(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleContributor)
	if !ok {
		return
	}
	if it.WorkspaceID != r.PathValue("wid") || it.Type != "Notebook" {
		writeErr(w, 404, "ArtifactNotFound", "The notebook is not available.")
		return
	}
	current, err := a.vscodeNotebookJSON(it.ID)
	if err != nil {
		writeErr(w, 500, "InvalidNotebookDefinition", err.Error())
		return
	}
	if match := r.Header.Get("If-Match"); match != "" && match != "*" && match != vscodeETag(current) {
		writeErr(w, http.StatusPreconditionFailed, "ArtifactModified", "The notebook changed since it was downloaded.")
		return
	}
	// json.Valid would reject most truncations, but it would report them as
	// malformed content — telling the caller their notebook is broken when
	// what happened is that it was too big.
	raw, ok := httpx.ReadBounded(r.Body, httpx.MaxItemContent)
	if !ok {
		writeErr(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge",
			"The notebook is too large.")
		return
	}
	if !json.Valid(raw) {
		writeErr(w, 400, "InvalidNotebookContent", "Notebook content must be valid JSON.")
		return
	}
	if err := a.Store.SetDefinition(it.ID, vscodeNotebookParts(raw)); err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	w.Header().Set("ETag", vscodeETag(raw))
	w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
}

func vscodeETag(raw []byte) string {
	sum := sha256.Sum256(raw)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func vscodeWorkloadPart(payload string) store.DefinitionPart {
	return store.DefinitionPart{Path: "vscode-workload-payload.json", Payload: base64.StdEncoding.EncodeToString([]byte(payload)), PayloadType: "InlineBase64"}
}

func (a *API) vscodeWorkloadPayload(itemID string) (string, error) {
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return "", err
	}
	for _, part := range parts {
		if part.Path == "vscode-workload-payload.json" {
			raw, err := base64.StdEncoding.DecodeString(part.Payload)
			return string(raw), err
		}
	}
	return "", store.ErrNotFound
}

func vscodeNotebookParts(raw []byte) []store.DefinitionPart {
	py := vscodeIPYNBToFabric(raw)
	return []store.DefinitionPart{
		{Path: "notebook-content.ipynb", Payload: base64.StdEncoding.EncodeToString(raw), PayloadType: "InlineBase64"},
		{Path: "notebook-content.py", Payload: base64.StdEncoding.EncodeToString(py), PayloadType: "InlineBase64"},
	}
}

func (a *API) vscodeNotebookJSON(itemID string) ([]byte, error) {
	parts, err := a.Store.GetDefinition(itemID)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		if part.Path == "notebook-content.ipynb" {
			raw, err := base64.StdEncoding.DecodeString(part.Payload)
			if err != nil {
				return nil, err
			}
			return raw, nil
		}
	}
	for _, part := range parts {
		if part.Path == "notebook-content.py" {
			raw, err := base64.StdEncoding.DecodeString(part.Payload)
			if err != nil {
				return nil, err
			}
			return vscodeFabricToIPYNB(raw), nil
		}
	}
	return nil, fmt.Errorf("notebook-content.py is missing")
}

func vscodeFabricToIPYNB(src []byte) []byte {
	cells := notebook.Parse(src)
	out := map[string]any{"nbformat": 4, "nbformat_minor": 5, "metadata": map[string]any{"kernelspec": map[string]any{"display_name": "Synapse PySpark", "language": "python", "name": "synapse_pyspark"}}}
	wireCells := make([]map[string]any, 0, len(cells))
	for _, cell := range cells {
		kind := "code"
		c := map[string]any{"cell_type": kind, "metadata": map[string]any{}, "source": strings.SplitAfter(cell.Source, "\n")}
		if cell.Kind == notebook.Markdown {
			c["cell_type"] = "markdown"
		} else {
			c["execution_count"] = nil
			c["outputs"] = []any{}
		}
		wireCells = append(wireCells, c)
	}
	out["cells"] = wireCells
	raw, _ := json.Marshal(out)
	return raw
}

func vscodeIPYNBToFabric(raw []byte) []byte {
	var nb struct {
		Cells []struct {
			CellType string `json:"cell_type"`
			Source   any    `json:"source"`
		} `json:"cells"`
	}
	if json.Unmarshal(raw, &nb) != nil {
		return raw
	}
	var b strings.Builder
	b.WriteString("# Fabric notebook source\n")
	for _, cell := range nb.Cells {
		if cell.CellType == "markdown" {
			b.WriteString("\n# MARKDOWN ************\n")
		} else {
			b.WriteString("\n# CELL ************\n")
		}
		var source string
		switch v := cell.Source.(type) {
		case string:
			source = v
		case []any:
			for _, line := range v {
				source += fmt.Sprint(line)
			}
		}
		if cell.CellType == "markdown" {
			for _, line := range strings.Split(source, "\n") {
				b.WriteString("# MAGIC " + line + "\n")
			}
		} else {
			b.WriteString(source)
			if !strings.HasSuffix(source, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	return []byte(b.String())
}

const vscodeResourcePrefix = "Files/.notebook-resources"

func (a *API) vscodeResourceItem(w http.ResponseWriter, r *http.Request, p *auth.Principal) (*store.Item, bool) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleContributor)
	if !ok {
		return nil, false
	}
	if it.Type != "Notebook" || it.WorkspaceID != r.PathValue("wid") {
		writeErr(w, 404, "ArtifactNotFound", "The notebook is not available.")
		return nil, false
	}
	return it, true
}

func vscodeResourcePath(r *http.Request) (string, bool) {
	raw := strings.Trim(r.PathValue("path"), "/")
	clean := path.Clean(raw)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	if clean == "." {
		return vscodeResourcePrefix, true
	}
	return vscodeResourcePrefix + "/" + clean, true
}

func (a *API) vscodeResourceUsage(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeResourceItem(w, r, p)
	if !ok {
		return
	}
	paths, err := a.Store.ListOneLakePaths(it.ID, vscodeResourcePrefix, true)
	if err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	var used int64
	for _, path := range paths {
		used += int64(len(path.Content))
	}
	writeJSON(w, 200, map[string]int64{"usedBytes": used, "quotaBytes": 100 << 20})
}

func (a *API) vscodeResourceGet(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeResourceItem(w, r, p)
	if !ok {
		return
	}
	rel, valid := vscodeResourcePath(r)
	if !valid {
		writeErr(w, 400, "InvalidResourcePath", "Resource path must stay within the notebook work directory.")
		return
	}
	if r.URL.Query().Has("recursive") || strings.HasSuffix(r.URL.Path, "/") || r.PathValue("path") == "" {
		paths, err := a.Store.ListOneLakePaths(it.ID, rel, r.URL.Query().Get("recursive") == "true")
		if err != nil {
			writeErr(w, 500, "InternalError", err.Error())
			return
		}
		children := make([]map[string]any, 0, len(paths))
		for _, path := range paths {
			name := strings.TrimPrefix(path.RelPath, rel+"/")
			if strings.Contains(name, "/") {
				name = strings.SplitN(name, "/", 2)[0]
			}
			kind := "file"
			if path.IsDir {
				kind = "folder"
			}
			children = append(children, map[string]any{"name": name, "fileSystemEntryType": kind, "size": len(path.Content), "lastModified": time.Unix(path.ModifiedAt, 0).UTC().Format(time.RFC3339)})
		}
		writeJSON(w, 200, map[string]any{"children": children})
		return
	}
	path, err := a.Store.GetOneLakePath(it.ID, rel)
	if err != nil || path.IsDir {
		writeErr(w, 404, "ResourceNotFound", "The resource is not available.")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(path.Content)
}

func (a *API) vscodeResourcePut(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeResourceItem(w, r, p)
	if !ok {
		return
	}
	isDir := strings.EqualFold(r.Header.Get("ms-filesystem-entry-type"), "folder")
	// THE ONE THAT WAS STILL LIVE. Nothing parses this — `content` goes
	// straight into the store as the file — so an oversized resource was
	// stored short and answered 200, exactly the OneLake defect on the VS Code
	// extension's write path.
	content, ok := httpx.ReadBounded(r.Body, httpx.MaxItemContent)
	if !ok {
		writeErr(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge",
			"The resource file is too large.")
		return
	}
	rel, valid := vscodeResourcePath(r)
	if !valid {
		writeErr(w, 400, "InvalidResourcePath", "Resource path must stay within the notebook work directory.")
		return
	}
	path := &store.OneLakePath{WorkspaceID: it.WorkspaceID, ItemID: it.ID, RelPath: rel, IsDir: isDir, Content: content}
	if err := a.Store.CreateOneLakePath(path, false); err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	w.WriteHeader(200)
}

func (a *API) vscodeResourceDelete(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeResourceItem(w, r, p)
	if !ok {
		return
	}
	rel, valid := vscodeResourcePath(r)
	if !valid {
		writeErr(w, 400, "InvalidResourcePath", "Resource path must stay within the notebook work directory.")
		return
	}
	if err := a.Store.DeleteOneLakePath(it.ID, rel); err != nil {
		writeErr(w, 404, "ResourceNotFound", "The resource is not available.")
		return
	}
	w.WriteHeader(200)
}

func (a *API) vscodeRunSparkJob(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleContributor)
	if !ok {
		return
	}
	if it.Type != "SparkJobDefinition" {
		writeErr(w, 400, "InvalidArtifactType", "Only Spark Job Definition artifacts can run sparkjob.")
		return
	}
	delay, fail := a.nextOpFate()
	job := &store.JobInstance{ItemID: it.ID, JobType: "sparkjob", FailWith: fail, CompleteAt: a.Store.Now() + delay}
	if err := a.Store.CreateJobInstance(job); err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": job.ID, "artifactId": it.ID, "retriableBatchId": job.ID, "state": job.StatusAt(a.Store.Now()), "name": it.DisplayName})
}

func (a *API) vscodeCancelSparkJob(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleContributor)
	if !ok {
		return
	}
	if err := a.Store.CancelJobInstance(it.ID, r.PathValue("jid")); err != nil {
		writeErr(w, 404, "JobNotFound", "The Spark job is not available.")
		return
	}
	w.WriteHeader(200)
}

func (a *API) vscodeSparkJobs(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleViewer)
	if !ok {
		return
	}
	if it.WorkspaceID != r.PathValue("wid") {
		writeErr(w, 404, "ArtifactNotFound", "The artifact is not available.")
		return
	}
	jobs, err := a.Store.ListJobInstances(1000)
	if err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	items := []map[string]any{}
	for _, job := range jobs {
		if job.ItemID == it.ID && job.JobType == "sparkjob" {
			items = append(items, map[string]any{"id": job.ID, "artifactId": it.ID, "retriableBatchId": job.ID, "name": it.DisplayName, "state": job.StatusAt(a.Store.Now())})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *API) vscodeLakehouseTables(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.vscodeAuthorizedItem(w, r.PathValue("iid"), p, store.RoleViewer)
	if !ok {
		return
	}
	if it.Type != "Lakehouse" || it.WorkspaceID != r.PathValue("wid") {
		writeErr(w, 404, "ArtifactNotFound", "The lakehouse is not available.")
		return
	}
	paths, err := a.Store.ListOneLakePaths(it.ID, "Tables", false)
	if err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	data := []map[string]any{}
	for _, entry := range paths {
		name := strings.TrimPrefix(entry.RelPath, "Tables/")
		if name != "" && !strings.Contains(name, "/") {
			data = append(data, map[string]any{"name": name})
		}
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
