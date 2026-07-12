package api

// Git integration: the emulator's "remote" is a local per-branch store of
// item definitions, so commitToGit/updateFromGit round-trip with no real
// GitHub/AzDO. Wire shapes follow fabric-docs' git-automation walkthrough.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// gitProviderDetails is the documented connect payload (AzureDevOps adds
// projectName; GitHub uses ownerName — both captured, stored verbatim).
type gitProviderDetails struct {
	GitProviderType  string `json:"gitProviderType"`
	OrganizationName string `json:"organizationName,omitempty"`
	OwnerName        string `json:"ownerName,omitempty"`
	ProjectName      string `json:"projectName,omitempty"`
	RepositoryName   string `json:"repositoryName"`
	BranchName       string `json:"branchName"`
	DirectoryName    string `json:"directoryName"`
}

func (g gitProviderDetails) remoteKey() string {
	org := g.OrganizationName
	if org == "" {
		org = g.OwnerName
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s", g.GitProviderType, org, g.ProjectName, g.RepositoryName, g.DirectoryName)
}

// gitConnect binds the workspace to a provider branch. Admin-only, like real
// Fabric. Service principals must use a ConfiguredConnection (documented SP
// constraint); the connectionId must exist.
func (a *API) gitConnect(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, _, ok := a.requireRole(w, wid, p, store.RoleAdmin); !ok {
		return
	}
	var body struct {
		GitProviderDetails gitProviderDetails `json:"gitProviderDetails"`
		MyGitCredentials   struct {
			Source       string `json:"source"`
			ConnectionID string `json:"connectionId"`
		} `json:"myGitCredentials"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		body.GitProviderDetails.RepositoryName == "" || body.GitProviderDetails.BranchName == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "gitProviderDetails with repositoryName and branchName is required.")
		return
	}
	cred := body.MyGitCredentials
	if cred.Source == "" {
		cred.Source = "Automatic"
	}
	if p.Type == "ServicePrincipal" && cred.Source != "ConfiguredConnection" {
		writeErr(w, http.StatusBadRequest, "InvalidCredentialSource",
			"Service principals must use a ConfiguredConnection for git credentials.")
		return
	}
	if cred.Source == "ConfiguredConnection" {
		if _, err := a.Store.GetConnection(cred.ConnectionID); err != nil {
			writeErr(w, http.StatusBadRequest, "ConnectionNotFound", "myGitCredentials.connectionId does not resolve to a connection.")
			return
		}
	}
	provider, _ := json.Marshal(body.GitProviderDetails)
	g := &store.GitConnection{
		WorkspaceID: wid, ProviderJSON: string(provider),
		RemoteKey: body.GitProviderDetails.remoteKey(), Branch: body.GitProviderDetails.BranchName,
		CredSource: cred.Source, ConnectionID: cred.ConnectionID,
	}
	if err := a.Store.SetGitConnection(g); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// requireGit loads the workspace's git binding after an RBAC check.
func (a *API) requireGit(w http.ResponseWriter, wid string, p *auth.Principal, min string) (*store.GitConnection, bool) {
	if _, _, ok := a.requireRole(w, wid, p, min); !ok {
		return nil, false
	}
	g, err := a.Store.GetGitConnection(wid)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "WorkspaceNotConnectedToGit", "The workspace is not connected to git.")
		return nil, false
	}
	return g, true
}

// gitInitializeConnection reports which direction the first sync must go.
func (a *API) gitInitializeConnection(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	g, ok := a.requireGit(w, wid, p, store.RoleAdmin)
	if !ok {
		return
	}
	head, err := a.Store.GetRemoteHead(g.RemoteKey, g.Branch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	items, err := a.Store.ListItems(wid, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	action := "None"
	switch {
	case head != "":
		action = "UpdateFromGit"
	case len(items) > 0:
		action = "CommitToGit"
	}
	g.Initialized = true
	if err := a.Store.SetGitConnection(g); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requiredAction":   action,
		"workspaceHead":    g.SyncedCommit,
		"remoteCommitHash": head,
	})
}

// gitStatus diffs the workspace against the remote branch by type+name.
func (a *API) gitStatus(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	g, ok := a.requireGit(w, wid, p, store.RoleContributor)
	if !ok {
		return
	}
	head, err := a.Store.GetRemoteHead(g.RemoteKey, g.Branch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	items, err := a.Store.ListItems(wid, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	remote, err := a.Store.ListRemoteItems(g.RemoteKey, g.Branch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}

	type change struct {
		ItemMetadata struct {
			ItemIdentifier struct {
				ObjectID  string `json:"objectId,omitempty"`
				LogicalID string `json:"logicalId,omitempty"`
			} `json:"itemIdentifier"`
			ItemType    string `json:"itemType"`
			DisplayName string `json:"displayName"`
		} `json:"itemMetadata"`
		WorkspaceChange string `json:"workspaceChange,omitempty"`
		RemoteChange    string `json:"remoteChange,omitempty"`
		ConflictType    string `json:"conflictType,omitempty"`
	}
	key := func(t, n string) string { return t + "\x00" + n }
	remoteBy := map[string]*store.RemoteItem{}
	for _, ri := range remote {
		remoteBy[key(ri.Type, ri.DisplayName)] = ri
	}
	var changes []change
	seen := map[string]bool{}
	for _, it := range items {
		k := key(it.Type, it.DisplayName)
		seen[k] = true
		c := change{}
		c.ItemMetadata.ItemType, c.ItemMetadata.DisplayName = it.Type, it.DisplayName
		c.ItemMetadata.ItemIdentifier.ObjectID = it.ID
		ri := remoteBy[k]
		if ri == nil {
			c.WorkspaceChange = "Added"
			changes = append(changes, c)
			continue
		}
		c.ItemMetadata.ItemIdentifier.LogicalID = ri.LogicalID
		parts, err := a.Store.GetDefinition(it.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		if !partsEqual(parts, ri.Parts) {
			c.WorkspaceChange = "Modified"
			changes = append(changes, c)
		}
	}
	for _, ri := range remote {
		if seen[key(ri.Type, ri.DisplayName)] {
			continue
		}
		c := change{RemoteChange: "Added"}
		c.ItemMetadata.ItemType, c.ItemMetadata.DisplayName = ri.Type, ri.DisplayName
		c.ItemMetadata.ItemIdentifier.LogicalID = ri.LogicalID
		changes = append(changes, c)
	}
	if changes == nil {
		changes = []change{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspaceHead":    g.SyncedCommit,
		"remoteCommitHash": head,
		"changes":          changes,
	})
}

func partsEqual(a, b []store.DefinitionPart) bool {
	if len(a) != len(b) {
		return false
	}
	byPath := map[string]store.DefinitionPart{}
	for _, p := range a {
		byPath[p.Path] = p
	}
	for _, p := range b {
		if byPath[p.Path] != p {
			return false
		}
	}
	return true
}

// gitCommitToGit pushes workspace items (definitions) to the remote branch.
// Side effects apply at request time; completion reports via the LRO engine.
func (a *API) gitCommitToGit(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	g, ok := a.requireGit(w, wid, p, store.RoleContributor)
	if !ok {
		return
	}
	var body struct {
		Mode    string `json:"mode"`
		Comment string `json:"comment"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	items, err := a.Store.ListItems(wid, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	var remote []*store.RemoteItem
	for _, it := range items {
		parts, err := a.Store.GetDefinition(it.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		remote = append(remote, &store.RemoteItem{Type: it.Type, DisplayName: it.DisplayName, Parts: parts})
	}
	hash, err := a.Store.CommitRemoteItems(g.RemoteKey, g.Branch, remote)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	g.SyncedCommit = hash
	if err := a.Store.SetGitConnection(g); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.startOperation(w, r, "CommitToGit", wid)
}

// gitUpdateFromGit mirrors the remote branch into the workspace: create
// missing items, replace definitions of existing ones, delete items absent
// from the remote.
func (a *API) gitUpdateFromGit(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	g, ok := a.requireGit(w, wid, p, store.RoleContributor)
	if !ok {
		return
	}
	var body struct {
		RemoteCommitHash string `json:"remoteCommitHash"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	remote, err := a.Store.ListRemoteItems(g.RemoteKey, g.Branch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	items, err := a.Store.ListItems(wid, "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	key := func(t, n string) string { return t + "\x00" + n }
	existing := map[string]*store.Item{}
	for _, it := range items {
		existing[key(it.Type, it.DisplayName)] = it
	}
	seen := map[string]bool{}
	for _, ri := range remote {
		k := key(ri.Type, ri.DisplayName)
		seen[k] = true
		if it := existing[k]; it != nil {
			if err := a.Store.SetDefinition(it.ID, ri.Parts); err != nil {
				writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
				return
			}
			continue
		}
		it := &store.Item{WorkspaceID: wid, Type: ri.Type, DisplayName: ri.DisplayName}
		if err := a.Store.CreateItem(it, ri.Parts); err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
	}
	for k, it := range existing {
		if seen[k] {
			continue
		}
		if err := a.Store.DeleteItem(wid, it.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
	}
	head, err := a.Store.GetRemoteHead(g.RemoteKey, g.Branch)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	g.SyncedCommit = head
	if err := a.Store.SetGitConnection(g); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.startOperation(w, r, "UpdateFromGit", wid)
}

// gitDisconnect detaches the workspace from git.
func (a *API) gitDisconnect(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	if _, ok := a.requireGit(w, wid, p, store.RoleAdmin); !ok {
		return
	}
	if err := a.Store.DeleteGitConnection(wid); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// gitMyCredentials reports the caller's credential configuration.
func (a *API) gitMyCredentials(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	wid := r.PathValue("wid")
	g, ok := a.requireGit(w, wid, p, store.RoleContributor)
	if !ok {
		return
	}
	resp := map[string]any{"source": g.CredSource}
	if g.ConnectionID != "" {
		resp["connectionId"] = g.ConnectionID
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- connections ----

// listConnections returns stored connections.
func (a *API) listConnections(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	cs, err := a.Store.ListConnections()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if cs == nil {
		cs = []*store.Connection{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": cs})
}

// createConnection stores a connection; details are kept verbatim.
func (a *API) createConnection(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body struct {
		DisplayName       string          `json:"displayName"`
		ConnectivityType  string          `json:"connectivityType"`
		ConnectionDetails json.RawMessage `json:"connectionDetails"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DisplayName == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "displayName is required.")
		return
	}
	c := &store.Connection{
		DisplayName: body.DisplayName, ConnectivityType: body.ConnectivityType, Details: body.ConnectionDetails,
	}
	if err := a.Store.CreateConnection(c); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, c)
}
