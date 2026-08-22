package onelake

// The engine-facing half of OneLake security: "Get authorized access for a
// principal".
//
//	GET {onelake}/v1.0/workspaces/{ws}/artifacts/{item}/securityPolicy/principalAccess
//
// WHAT IT IS FOR. This is the seam the AUTHORIZED ENGINE MODEL is built on. A
// third-party engine registers a privileged identity, reads the raw files with
// it, asks this endpoint what a given USER may see, and applies the returned
// row and column filters in its own query layer. That is why the response
// carries `rows` as SQL TEXT rather than rows: OneLake decides, the engine
// applies. Fabric's own Spark and SQL endpoints do the same thing internally.
//
// WHO MAY ASK. The caller is the engine's identity, not the subject: it asks
// about someone else by object id. So the CALLER needs ReadAll on the workspace
// — the same bar as reading the data it is about to filter — while the SUBJECT
// needs nothing at all, since the honest answer for a principal with no access
// is an empty list. Letting an unprivileged caller ask would turn this into an
// oracle for enumerating other people's access.
//
// A GET WITH A BODY, because that is what the reference documents. Unusual, and
// not ours to correct: an engine written against the published shape has to
// work here unmodified, which is the entire point of emulating it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/pkg/onelakesec"
)

type principalAccessRequest struct {
	AADObjectID       string `json:"aadObjectId"`
	InputPath         string `json:"inputPath"`
	ContinuationToken string `json:"continuationToken"`
	MaxResults        int    `json:"maxResults"`
}

type principalAccessEntry struct {
	Path    string   `json:"path"`
	Access  []string `json:"access"`
	Rows    string   `json:"rows,omitempty"`
	Columns []string `json:"columns,omitempty"`
	Effect  string   `json:"effect"`
}

type principalAccessResponse struct {
	IdentityETag      string                 `json:"identityETag"`
	MetadataETag      string                 `json:"metadataETag"`
	Value             []principalAccessEntry `json:"value"`
	ContinuationToken string                 `json:"continuationToken,omitempty"`
}

func (s *Service) principalAccess(w http.ResponseWriter, r *http.Request, p *auth.Principal, wsSeg, itemSeg string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeDFSErr(w, dfsError{"OperationNotAllowed", http.StatusMethodNotAllowed,
			"principalAccess is a GET."})
		return
	}
	ws, derr := s.resolveWorkspace(wsSeg)
	if derr != nil {
		writeDFSErr(w, *derr)
		return
	}
	// The CALLER is the engine. Reading policy is at least as sensitive as
	// reading the data it protects, so it takes the same ReadAll bar — and a
	// Viewer granted one table cannot use this to discover the rest.
	role, err := s.Store.RoleOf(ws.ID, p.ID)
	if err != nil {
		writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
		return
	}
	if store.RoleRank(role) < store.RoleRank(store.RoleContributor) {
		writeDFSErr(w, dfsError{"AuthorizationFailure", http.StatusForbidden,
			"Reading a security policy requires ReadAll (the Contributor role or above)."})
		return
	}
	it, derr := s.resolveItem(ws.ID, itemSeg)
	if derr != nil {
		writeDFSErr(w, dfsError{"ItemNotFound", http.StatusNotFound, "The item is not available."})
		return
	}

	var req principalAccessRequest
	body, _ := io.ReadAll(r.Body)
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeDFSErr(w, dfsError{"InvalidInput", http.StatusBadRequest,
				"Malformed request body: " + err.Error()})
			return
		}
	}
	if req.AADObjectID == "" {
		writeDFSErr(w, dfsError{"InvalidInput", http.StatusBadRequest,
			"aadObjectId is required: this API answers for a named principal."})
		return
	}
	// "Either Tables or Files"; the response covers one half only.
	input := onelakesec.InputTables
	switch strings.ToLower(req.InputPath) {
	case "", "tables":
	case "files":
		input = onelakesec.InputFiles
	default:
		writeDFSErr(w, dfsError{"InvalidInput", http.StatusBadRequest,
			`inputPath must be "Tables" or "Files".`})
		return
	}

	roles, err := s.Store.EvaluatableRoles(it.ID)
	if err != nil {
		writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
		return
	}
	entries, err := s.subjectAccess(ws.ID, req.AADObjectID, input, roles)
	if err != nil {
		writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
		return
	}

	out := principalAccessResponse{Value: []principalAccessEntry{}}
	for _, e := range entries {
		out.Value = append(out.Value, principalAccessEntry{
			Path: e.Path, Access: e.Access, Rows: e.Rows, Columns: e.Columns,
			Effect: string(e.Effect),
		})
	}
	out.IdentityETag = etagOf(req.AADObjectID, out.Value)
	out.MetadataETag = etagOf(it.ID, roles)

	w.Header().Set("ETag", `"`+out.MetadataETag+`"`)
	// A caller that already holds this answer can skip it, which is what an
	// engine caching per-user policy across a query needs.
	if match := r.Header.Get("If-None-Match"); match != "" &&
		strings.Trim(match, `"`) == out.MetadataETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// etagOf hashes a value into a stable tag. Deterministic across processes — a
// map-ordered or address-derived tag would change without the policy changing
// and defeat every conditional request made against it.
func etagOf(salt string, v any) string {
	// The error is discarded because every value passed here is a slice of
	// plain structs; json.Marshal cannot fail on one, and a branch that cannot
	// run is a branch nothing can check.
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(append([]byte(salt+"\x00"), b...))
	return hex.EncodeToString(sum[:])
}

// subjectAccess is what the subject may see, which is not the same question as
// what their item roles say.
//
//   - ReadAll (Contributor and above) sees the whole half. Those roles
//     "override any OneLake security Read permissions", so reporting whatever
//     roles happen to name them would tell an engine to filter rows away from
//     someone entitled to all of them.
//   - A Viewer sees exactly what the item's roles grant.
//   - No workspace role at all sees nothing, whatever an item role says:
//     workspace permissions are the first boundary. That is an empty list, not
//     an error — "no access" is a real answer, and an engine acts on it by
//     returning no rows.
func (s *Service) subjectAccess(wsID, subject, input string, roles []onelakesec.Role) ([]onelakesec.AccessEntry, error) {
	subjectRole, err := s.Store.RoleOf(wsID, subject)
	if err != nil {
		return nil, err
	}
	switch {
	case store.RoleRank(subjectRole) >= store.RoleRank(store.RoleContributor):
		return []onelakesec.AccessEntry{{
			Path: input, Access: []string{onelakesec.AccessRead}, Effect: onelakesec.EffectPermit,
		}}, nil
	case subjectRole == "":
		return nil, nil
	default:
		return onelakesec.Effective(roles, onelakesec.Principal{ObjectID: subject}, input), nil
	}
}

// isPrincipalAccessPath matches the security API's own URL shape, which both
// OneLake surfaces dispatch on. One predicate, so the two spellings cannot
// diverge into a policy readable through one and not the other.
func isPrincipalAccessPath(segs []string) bool {
	return len(segs) == 7 && segs[0] == "v1.0" && segs[1] == "workspaces" &&
		segs[3] == "artifacts" && segs[5] == "securityPolicy" && segs[6] == "principalAccess"
}
