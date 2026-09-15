package api

// Item permissions on the control plane: three surfaces over one store.
//
//   - Power BI's documented dataset-users API, for semantic models.
//   - An authenticated emulator-native surface, for every other item. Fabric's
//     Core REST has no item-permissions operation — the portal shares through
//     endpoints Microsoft does not document — so there is no contract to follow
//     and a path segment, `_emulator`, that says so.
//   - Fabric's documented admin list, for auditing any item.
//
// ONLY ENFORCED PERMISSIONS ARE ACCEPTED. Each surface refuses, by name, a
// permission nothing in the emulator acts on. Storing `Execute` and reporting it
// back would tell a caller they had shared an item for running when nothing
// would ever check. See docs/57-item-permissions.md.

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func (a *API) registerItemAccess(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workspaces/{wid}/items/{iid}/_emulator/access", a.withAuth(a.listItemAccess))
	mux.HandleFunc("PUT /v1/workspaces/{wid}/items/{iid}/_emulator/access/{principalId}", a.withAuth(a.putItemAccess))
	mux.HandleFunc("DELETE /v1/workspaces/{wid}/items/{iid}/_emulator/access/{principalId}", a.withAuth(a.deleteItemAccess))

	mux.HandleFunc("GET /v1.0/myorg/datasets/{datasetId}/users", a.withPBIAuth(a.getDatasetUsers))
	mux.HandleFunc("POST /v1.0/myorg/datasets/{datasetId}/users", a.withPBIAuth(a.postDatasetUser))
	mux.HandleFunc("PUT /v1.0/myorg/datasets/{datasetId}/users", a.withPBIAuth(a.putDatasetUser))
	mux.HandleFunc("GET /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/users", a.withPBIAuth(a.getDatasetUsers))
	mux.HandleFunc("POST /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/users", a.withPBIAuth(a.postDatasetUser))
	mux.HandleFunc("PUT /v1.0/myorg/groups/{groupId}/datasets/{datasetId}/users", a.withPBIAuth(a.putDatasetUser))

	mux.HandleFunc("GET /v1/admin/workspaces/{wid}/items/{iid}/users", a.withTenantRead(a.adminItemUsers))
}

// ---- shared rules -------------------------------------------------------------

// withinGrantorsAccess is the resharing rule: a grantor "can at most grant" the
// permissions they hold. Admin and Member inherit every permission these surfaces
// accept, so the one subset check covers them too — no separate role branch that
// could disagree with it.
func withinGrantorsAccess(grantor store.Access, requested ...string) (string, bool) {
	for _, p := range requested {
		if !grantor.Has(p) {
			return p, false
		}
	}
	return "", true
}

// refuseUPN names the one identifier the emulator cannot resolve. Power BI
// identifies a User by UPN; this emulator keys principals by Entra object id and
// has no directory to look a UPN up in, so guessing would grant somebody else.
func refuseUPN(w http.ResponseWriter, identifier string) bool {
	if strings.Contains(identifier, "@") {
		writeErr(w, http.StatusBadRequest, "PrincipalNotResolvable",
			"This emulator identifies principals by Entra object id and has no directory to resolve the UPN "+
				identifier+" against. Pass the principal's object id.")
		return true
	}
	return false
}

// ---- emulator-native surface ------------------------------------------------------

type itemAccessPrincipal struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type itemAccessEntry struct {
	Principal             itemAccessPrincipal `json:"principal"`
	Permissions           []string            `json:"permissions"`
	AdditionalPermissions []string            `json:"additionalPermissions"`
}

type putItemAccessBody struct {
	PrincipalType         string   `json:"principalType"`
	Permissions           []string `json:"permissions"`
	AdditionalPermissions []string `json:"additionalPermissions"`
}

// nativeShareable resolves the item and its caller's access, and refuses anyone
// who may not share it. Semantic models are sent to their documented API: one
// contract per item type, so the two cannot drift into disagreeing.
func (a *API) nativeShareable(w http.ResponseWriter, r *http.Request, p *auth.Principal) (*store.Item, store.Access, bool) {
	it, err := a.Store.GetItem(r.PathValue("wid"), r.PathValue("iid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return nil, store.Access{}, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, store.Access{}, false
	}
	if it.Type == "SemanticModel" {
		writeErr(w, http.StatusBadRequest, "UseDatasetUsersAPI",
			"Semantic model permissions are managed through Power BI's documented "+
				"/v1.0/myorg/datasets/{datasetId}/users API.")
		return nil, store.Access{}, false
	}
	access, err := a.Store.EffectiveItemAccess(it, p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, store.Access{}, false
	}
	if !access.Has(store.PermReshare) {
		writeErr(w, http.StatusForbidden, "InsufficientPrivileges",
			"Sharing this item requires the Reshare permission: the workspace Admin or Member role, or a grant that includes it.")
		return nil, store.Access{}, false
	}
	return it, access, true
}

func (a *API) listItemAccess(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, _, ok := a.nativeShareable(w, r, p)
	if !ok {
		return
	}
	grants, err := a.Store.ListItemAccess(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]itemAccessEntry, 0, len(grants))
	for _, g := range grants {
		out = append(out, itemAccessEntry{
			Principal:   itemAccessPrincipal{ID: g.PrincipalID, Type: g.PrincipalType},
			Permissions: g.Permissions, AdditionalPermissions: g.Additional,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

// nativePermissions and nativeAdditional are what this surface accepts, and each
// is enforced somewhere: Read and ReadData at the SQL endpoint, ReadAll on
// OneLake and Direct Lake, Reshare right here.
var (
	nativePermissions = map[string]bool{store.PermRead: true, store.PermReshare: true}
	nativeAdditional  = map[string]bool{store.PermReadAll: true, store.PermReadData: true}
)

func (a *API) putItemAccess(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, grantor, ok := a.nativeShareable(w, r, p)
	if !ok {
		return
	}
	principalID := r.PathValue("principalId")
	if refuseUPN(w, principalID) {
		return
	}
	var body putItemAccessBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidInput", "Malformed request body: "+err.Error())
		return
	}
	switch body.PrincipalType {
	case "User", "Group", "ServicePrincipal":
	default:
		writeErr(w, http.StatusBadRequest, "InvalidInput",
			"principalType must be User, Group or ServicePrincipal.")
		return
	}
	for _, perm := range body.Permissions {
		if perm == store.PermWrite {
			writeErr(w, http.StatusBadRequest, "PermissionNotGrantable",
				"Sharing grants no write permission: \"Lakehouse sharing does not provide write permissions\".")
			return
		}
		if !nativePermissions[perm] {
			writeErr(w, http.StatusBadRequest, "PermissionNotModelled",
				"Permission "+perm+" is not modelled by this emulator; accepted are Read and Reshare.")
			return
		}
	}
	for _, perm := range body.AdditionalPermissions {
		if !nativeAdditional[perm] {
			writeErr(w, http.StatusBadRequest, "PermissionNotModelled",
				"Additional permission "+perm+" is not modelled by this emulator; accepted are ReadAll and ReadData.")
			return
		}
	}
	// "Read permission is always granted during sharing."
	perms := append([]string{store.PermRead}, body.Permissions...)
	if missing, ok := withinGrantorsAccess(grantor, append(perms, body.AdditionalPermissions...)...); !ok {
		writeErr(w, http.StatusForbidden, "InsufficientPrivileges",
			"A grantor can share at most the permissions they hold, and the caller does not hold "+missing+".")
		return
	}
	g := store.ItemAccess{ItemID: it.ID, PrincipalID: principalID, PrincipalType: body.PrincipalType,
		Permissions: perms, Additional: body.AdditionalPermissions}
	if err := a.Store.PutItemAccess(g); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	stored, err := a.Store.GetItemAccess(it.ID, principalID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, itemAccessEntry{
		Principal:   itemAccessPrincipal{ID: stored.PrincipalID, Type: stored.PrincipalType},
		Permissions: stored.Permissions, AdditionalPermissions: stored.Additional,
	})
}

// deleteItemAccess revokes a DIRECT grant. What a workspace role implies is
// untouched, and that is the product: removing an item permission "isn't enough"
// to take away what the workspace gives.
func (a *API) deleteItemAccess(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, _, ok := a.nativeShareable(w, r, p)
	if !ok {
		return
	}
	err := a.Store.DeleteItemAccess(it.ID, r.PathValue("principalId"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "ItemAccessNotFound",
			"The principal holds no direct grant on this item; access from a workspace role is not revocable here.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Power BI dataset users ----------------------------------------------------

type datasetUser struct {
	Identifier             string `json:"identifier"`
	PrincipalType          string `json:"principalType"`
	DatasetUserAccessRight string `json:"datasetUserAccessRight"`
}

// datasetRight composes the documented enum from a permission set, in its own
// spelling order: Read, then Write, Reshare, Explore.
func datasetRight(a store.Access) string {
	right := "Read"
	for _, p := range []string{store.PermWrite, store.PermReshare, store.PermExplore} {
		if a.Has(p) {
			right += p
		}
	}
	return right
}

// parseDatasetRight is the inverse. "None" is reported as ok with no permissions.
func parseDatasetRight(right string) ([]string, bool) {
	if right == "None" {
		return nil, true
	}
	if !strings.HasPrefix(right, "Read") {
		return nil, false
	}
	perms := []string{store.PermRead}
	rest := right[len("Read"):]
	for _, p := range []string{store.PermWrite, store.PermReshare, store.PermExplore} {
		if strings.HasPrefix(rest, p) {
			perms = append(perms, p)
			rest = rest[len(p):]
		}
	}
	return perms, rest == ""
}

// pbiPrincipalType maps the store's principal types onto Power BI's enum, where a
// service principal is an App.
func pbiPrincipalType(t string) string {
	if t == "ServicePrincipal" {
		return "App"
	}
	return t
}

// dataset resolves a semantic model, honouring the group in the path when one is
// given: a dataset addressed through the wrong workspace is not found there.
func (a *API) datasetForUsers(w http.ResponseWriter, r *http.Request) (*store.Item, bool) {
	it, err := a.Store.GetItemByID(r.PathValue("datasetId"))
	if err != nil || it.Type != "SemanticModel" {
		writeErr(w, http.StatusNotFound, "DatasetNotFound", "The dataset was not found.")
		return nil, false
	}
	if g := r.PathValue("groupId"); g != "" && !strings.EqualFold(g, it.WorkspaceID) {
		writeErr(w, http.StatusNotFound, "DatasetNotFound", "The dataset was not found.")
		return nil, false
	}
	return it, true
}

// datasetCaller resolves the caller's access and requires the permissions the
// operation documents.
func (a *API) datasetCaller(w http.ResponseWriter, it *store.Item, p *auth.Principal, need ...string) (store.Access, bool) {
	access, err := a.Store.EffectiveItemAccess(it, p.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return store.Access{}, false
	}
	if missing, ok := withinGrantorsAccess(access, need...); !ok {
		writeErr(w, http.StatusForbidden, "InsufficientPrivileges",
			"This operation requires the "+strings.Join(need, "")+" permission on the dataset; the caller does not hold "+missing+".")
		return store.Access{}, false
	}
	return access, true
}

// getDatasetUsers lists every principal with access: workspace-role holders with
// what their role implies, unioned with direct grants. The documented sample
// includes an App at ReadWriteReshareExplore — the shape an owner's inherited
// access takes. "Caller must have ReadWriteReshare permissions on the dataset."
func (a *API) getDatasetUsers(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.datasetForUsers(w, r)
	if !ok {
		return
	}
	if _, ok := a.datasetCaller(w, it, p, store.PermRead, store.PermWrite, store.PermReshare); !ok {
		return
	}
	principals, err := a.itemPrincipals(it)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]datasetUser, 0, len(principals))
	for _, pr := range principals {
		out = append(out, datasetUser{Identifier: pr.id, PrincipalType: pbiPrincipalType(pr.typ),
			DatasetUserAccessRight: datasetRight(pr.access)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"value": out})
}

func (a *API) postDatasetUser(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	a.writeDatasetUser(w, r, p, false)
}

func (a *API) putDatasetUser(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	a.writeDatasetUser(w, r, p, true)
}

// writeDatasetUser is Post (grant, adding to an existing direct grant) and Put
// (update to exactly the given right; None removes the direct grant).
func (a *API) writeDatasetUser(w http.ResponseWriter, r *http.Request, p *auth.Principal, replace bool) {
	it, ok := a.datasetForUsers(w, r)
	if !ok {
		return
	}
	// Post: "Caller must have ReadReshare permissions." Put: "ReadWriteReshare".
	need := []string{store.PermRead, store.PermReshare}
	if replace {
		need = []string{store.PermRead, store.PermWrite, store.PermReshare}
	}
	grantor, ok := a.datasetCaller(w, it, p, need...)
	if !ok {
		return
	}
	var body datasetUser
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "Malformed request body: "+err.Error())
		return
	}
	if body.Identifier == "" {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", "identifier is required.")
		return
	}
	if refuseUPN(w, body.Identifier) {
		return
	}
	var storeType string
	switch body.PrincipalType {
	case "User", "Group":
		storeType = body.PrincipalType
	case "App":
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"Adding or updating permissions for service principals (App principalType) isn't supported.")
		return
	default:
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"principalType must be User or Group; organization-wide access (None) is not modelled.")
		return
	}
	perms, ok := parseDatasetRight(body.DatasetUserAccessRight)
	if !ok || (perms == nil && !replace) {
		writeErr(w, http.StatusBadRequest, "InvalidRequest",
			"datasetUserAccessRight "+body.DatasetUserAccessRight+" is not a valid access right here.")
		return
	}
	for _, perm := range perms {
		if perm == store.PermWrite {
			writeErr(w, http.StatusBadRequest, "InvalidRequest",
				"This API can't be used to add or remove write permission.")
			return
		}
	}

	if perms == nil { // Put with None
		if err := a.Store.DeleteItemAccess(it.ID, body.Identifier); err != nil && !errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if missing, ok := withinGrantorsAccess(grantor, perms...); !ok {
		writeErr(w, http.StatusForbidden, "InsufficientPrivileges",
			"A grantor can share at most the permissions they hold, and the caller does not hold "+missing+".")
		return
	}
	if !replace {
		existing, err := a.Store.GetItemAccess(it.ID, body.Identifier)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		if existing != nil {
			perms = append(perms, existing.Permissions...)
		}
	}
	if err := a.Store.PutItemAccess(store.ItemAccess{ItemID: it.ID, PrincipalID: body.Identifier,
		PrincipalType: storeType, Permissions: perms}); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---- admin list ------------------------------------------------------------------

type principalAccess struct {
	id, typ string
	access  store.Access
}

// itemPrincipals is every principal with access to an item — role holders on its
// workspace and direct grantees — each with its effective access, ordered by id.
func (a *API) itemPrincipals(it *store.Item) ([]principalAccess, error) {
	assignments, err := a.Store.ListRoleAssignments(it.WorkspaceID)
	if err != nil {
		return nil, err
	}
	grants, err := a.Store.ListItemAccess(it.ID)
	if err != nil {
		return nil, err
	}
	roles, types := map[string]string{}, map[string]string{}
	for _, ra := range assignments {
		roles[ra.Principal.ID] = ra.Role
		types[ra.Principal.ID] = ra.Principal.Type
	}
	direct := map[string]*store.ItemAccess{}
	for i := range grants {
		g := &grants[i]
		direct[g.PrincipalID] = g
		if _, seen := types[g.PrincipalID]; !seen {
			types[g.PrincipalID] = g.PrincipalType
		}
	}
	ids := make([]string, 0, len(types))
	for id := range types {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]principalAccess, 0, len(ids))
	for _, id := range ids {
		out = append(out, principalAccess{id: id, typ: types[id],
			access: store.MergeAccess(roles[id], it.Type, direct[id])})
	}
	return out, nil
}

type adminItemAccess struct {
	Principal         itemAccessPrincipal `json:"principal"`
	ItemAccessDetails struct {
		Type                  string   `json:"type"`
		Permissions           []string `json:"permissions"`
		AdditionalPermissions []string `json:"additionalPermissions"`
	} `json:"itemAccessDetails"`
}

// adminTypeRequired are the types the reference says need `type` in the query.
var adminTypeRequired = map[string]bool{
	"Report": true, "Dashboard": true, "SemanticModel": true, "App": true, "Dataflow": true,
}

// adminItemUsers is List Item Access Details. It reports EFFECTIVE access —
// inherited from workspace roles and granted directly — which is an inference
// the parity map grades as such: no documented sample shows an inherited row,
// but the operation is described as listing users "and their workspace roles".
func (a *API) adminItemUsers(w http.ResponseWriter, r *http.Request, _ *auth.Principal) {
	it, err := a.Store.GetItem(r.PathValue("wid"), r.PathValue("iid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "Item ID doesn't exist.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if raw := r.URL.Query().Get("type"); raw != "" {
		canonical, ok := store.CanonicalItemType(raw)
		if !ok {
			writeErr(w, http.StatusBadRequest, "InvalidItemType", "Item type isn't valid.")
			return
		}
		if canonical != it.Type {
			writeErr(w, http.StatusNotFound, "ItemNotFound", "Item ID doesn't exist for that type.")
			return
		}
	} else if adminTypeRequired[it.Type] {
		writeErr(w, http.StatusBadRequest, "InvalidItemType",
			"The type parameter is required when querying a "+it.Type+".")
		return
	}
	principals, err := a.itemPrincipals(it)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	out := make([]adminItemAccess, 0, len(principals))
	for _, pr := range principals {
		var entry adminItemAccess
		entry.Principal = itemAccessPrincipal{ID: pr.id, Type: pr.typ}
		entry.ItemAccessDetails.Type = it.Type
		entry.ItemAccessDetails.Permissions = pr.access.Permissions
		entry.ItemAccessDetails.AdditionalPermissions = pr.access.Additional
		if entry.ItemAccessDetails.AdditionalPermissions == nil {
			entry.ItemAccessDetails.AdditionalPermissions = []string{}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accessDetails": out})
}
