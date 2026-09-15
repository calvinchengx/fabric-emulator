package api

// OneLake security roles on the control plane: `dataAccessRoles`.
//
// AUTHORING IS CONTROL-PLANE, ALWAYS. OneLake is ADLS-compatible for reading,
// and deliberately not for this: "some operations, such as managing permissions
// or updating items, must be done through Fabric experiences, and can't be done
// via ADLS APIs" (onelake/onelake-access-api.md). So roles are written here, on
// api.fabric.microsoft.com, and read back by engines from the data plane.
//
// PUT REPLACES. The reference is explicit that this "updates role definitions by
// creating, updating, and deleting roles to match the payload you send" — a
// merge would leave behind a role the caller believes it deleted, which is the
// direction that keeps access alive after someone revoked it.
//
// WHO MAY WRITE. "Can edit OneLake security roles" is Admin and Member only;
// Contributor cannot. That is a narrower gate than the rest of the item surface
// uses, and it is the product's, not ours.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// dataAccessRolesBody is the documented envelope: a `value` array of roles.
type dataAccessRolesBody struct {
	Value []json.RawMessage `json:"value"`
}

// roleName reads just the name out of a role body, which is the only field this
// layer needs in order to key it. Everything else is the evaluator's business
// and is stored verbatim.
type roleName struct {
	Name string `json:"name"`
}

// onelakeSecItems is the set of item types that carry OneLake security roles.
//
// The product's supported-items table names three, and a Warehouse is
// conspicuously not among them: warehouse data is secured by T-SQL and nothing
// else ([55-tsql-security.md]). Storing a role on an item whose engines never
// consult it is the expensive kind of divergence — a consumer authors a policy
// here, watches the emulator honour it, and ships it to a tenant that ignores
// it. The refusal has to happen at the door, because by the time anything reads
// the role the item it belongs to is no longer in the frame.
//
// Two neighbouring enum members are deliberately absent. MirroredWarehouse and
// MirroredCatalog are plausible on the docs' broader "lakehouses and mirrored
// items" phrasing, but the supported-items table lists neither by name, and a
// type we admit on inference is one nothing would ever correct. A wrong refusal
// arrives as a bug report naming the type; a wrong acceptance arrives as a
// policy that did not hold in production.
//
// [55-tsql-security.md]: ../../docs/55-tsql-security.md
var onelakeSecItems = map[string]bool{
	"Lakehouse":                      true,
	"MirroredDatabase":               true,
	"MirroredAzureDatabricksCatalog": true,
}

// item resolves the item and enforces the workspace role this surface needs.
func (a *API) onelakeSecItem(w http.ResponseWriter, r *http.Request, p *auth.Principal, write bool) (*store.Item, bool) {
	wid := r.PathValue("wid")
	need := store.RoleViewer
	if write {
		// Admin and Member edit roles; Contributor does not.
		need = store.RoleMember
	}
	if _, _, ok := a.requireRole(w, wid, p, need); !ok {
		return nil, false
	}
	it, err := a.Store.GetItem(wid, r.PathValue("iid"))
	// A missing item and an unreachable store are DIFFERENT answers here, and
	// the surrounding surface collapses them into 404. On a security surface
	// that collapse is harmful: "no such item" invites a caller to conclude
	// there is no policy, when what actually happened is that we could not read
	// it. Fail loudly instead.
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "ItemNotFound", "The item is not available.")
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return nil, false
	}
	// The item-type rule gates the WRITE only. Authoring a role on an item type
	// the product does not secure this way is the harm; what Fabric answers to a
	// GET against such an item is not something the vendored docs say, and this
	// repo does not invent a refusal it cannot source. A read therefore keeps
	// answering, which for any item outside the supported set means an empty
	// set, since nothing can put a role there any more.
	if write {
		canonical, _ := store.CanonicalItemType(it.Type)
		if !onelakeSecItems[canonical] {
			writeErr(w, http.StatusBadRequest, "DataAccessRolesNotSupported",
				"OneLake security roles are not supported on item type "+it.Type+
					". Supported types are Lakehouse, MirroredDatabase and "+
					"MirroredAzureDatabricksCatalog; a Warehouse is secured by T-SQL "+
					"(GRANT, CREATE SECURITY POLICY, MASKED WITH) instead.")
			return nil, false
		}
	}
	return it, true
}

func (a *API) putDataAccessRoles(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.onelakeSecItem(w, r, p, true)
	if !ok {
		return
	}
	var body dataAccessRolesBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidInput", "Malformed request body: "+err.Error())
		return
	}
	roles := make([]store.OneLakeRole, 0, len(body.Value))
	seen := map[string]bool{}
	for _, raw := range body.Value {
		var n roleName
		if err := json.Unmarshal(raw, &n); err != nil || n.Name == "" {
			writeErr(w, http.StatusBadRequest, "InvalidInput", "Each role requires a name.")
			return
		}
		// The name is the identity of a role. Two rules under one name is a
		// payload whose author disagrees with itself about what the role is;
		// storing the last would silently discard the first.
		if seen[n.Name] {
			writeErr(w, http.StatusBadRequest, "InvalidInput",
				"Role "+n.Name+" appears more than once; role names are unique within an item.")
			return
		}
		seen[n.Name] = true
		roles = append(roles, store.OneLakeRole{ItemID: it.ID, Name: n.Name, Body: raw})
	}
	if err := a.Store.PutOneLakeRoles(it.ID, roles); err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	a.listDataAccessRoles(w, r, p)
}

func (a *API) listDataAccessRoles(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.onelakeSecItem(w, r, p, false)
	if !ok {
		return
	}
	stored, err := a.Store.ListOneLakeRoles(it.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	// Verbatim bodies: a field this emulator does not read is still a field the
	// client sent and expects back.
	out := make([]json.RawMessage, 0, len(stored))
	for _, s := range stored {
		out = append(out, s.Body)
	}
	writeJSON(w, http.StatusOK, dataAccessRolesBody{Value: out})
}
