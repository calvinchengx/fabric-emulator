// Package onelake serves the ADLS-Gen2-shaped data plane
// (onelake.dfs.fabric.microsoft.com): the filesystem is the workspace, the
// first path segment inside it is an item, and Fabric-managed folders are
// protected exactly as documented in onelake-api-parity.md — ADLS APIs can
// never create/rename/delete workspaces or items, an item's root and first
// level are read-only, disallowed query params reject the request, and
// banned headers are ignored but echoed via x-ms-rejected-headers.
package onelake

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/calvinchengx/fabric-emulator/internal/awssig"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/pkg/onelakesec"
)

// StorageAudience is the only token audience OneLake accepts.
var StorageAudience = []string{"https://storage.azure.com", "https://storage.azure.com/"}

// Service handles the DFS and Blob surfaces.
type Service struct {
	Store  *store.Store
	Auth   *auth.Validator // configured with the Storage audience
	stage  blockStage      // uncommitted Put Block staging (Blob dialect)
	Client *http.Client
}

// New builds the service; the validator must carry StorageAudience.
func New(st *store.Store, v *auth.Validator) *Service {
	return &Service{Store: st, Auth: v, Client: &http.Client{Timeout: 30 * time.Second}}
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

// pathHeaders stamps the per-path metadata storage clients depend on.
func pathHeaders(w http.ResponseWriter, p *store.OneLakePath, st *store.Store) {
	if p.ETag != "" {
		w.Header().Set("ETag", p.ETag)
	}
	mod := p.ModifiedAt
	if mod == 0 {
		mod = p.CreatedAt
	}
	w.Header().Set("Last-Modified", time.Unix(mod, 0).UTC().Format(http.TimeFormat))
}

// serveContent writes content honoring a single-range request; 206 with
// Content-Range for partial reads, 416 for unsatisfiable ranges. Both range
// header dialects are read: standard `Range` (DFS / Parquet seeks) and
// `x-ms-range` (the Azure Blob SDK always sends this for its chunked
// downloads and requires a 206 + Content-Range in reply).
func serveContent(w http.ResponseWriter, r *http.Request, content []byte) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	rng := r.Header.Get("Range")
	if rng == "" {
		rng = r.Header.Get("x-ms-range")
	}
	if rng == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
		return
	}
	start, end, ok := parseRange(rng, int64(len(content)))
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(content[start : end+1])
}

// parseRange handles the single-range forms storage clients emit:
// bytes=a-b, bytes=a-, bytes=-n (suffix).
func parseRange(h string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	if a == "" { // suffix: last n bytes
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, size > 0
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if b == "" {
		return start, size - 1, true
	}
	end, err = strconv.ParseInt(b, 10, 64)
	if err != nil || end < start {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end, true
}

// resolveRead returns the OneLakePath for rel within the item, following a
// shortcut when the direct path is absent. Reads through a shortcut are
// authorized against the TARGET workspace's RBAC — the trusted-workspace-
// access model (source workspace role already checked by the caller). A
// target that was deleted after the shortcut was made dangles → 404.
func (s *Service) resolveRead(itemID, rel, principalID string) (*store.OneLakePath, *dfsError) {
	if p, err := s.Store.GetOneLakePath(itemID, rel); err == nil {
		return p, nil
	}
	// Lakehouse managed folders exist even before they contain user data. They
	// are virtual service-owned directories: readable/stat-able, but mutation
	// remains blocked by the managed-folder guards in ServeHTTP.
	if rel == "Files" || rel == "Tables" {
		return &store.OneLakePath{ItemID: itemID, RelPath: rel, IsDir: true, CreatedAt: s.Store.Now()}, nil
	}
	sc, remainder, err := s.Store.ShortcutFor(itemID, rel)
	if err != nil {
		return nil, &dfsError{"InternalError", http.StatusInternalServerError, err.Error()}
	}
	if sc == nil {
		return nil, &dfsError{"PathNotFound", http.StatusNotFound, "The path does not exist."}
	}
	if sc.IsExternalTarget() {
		return s.resolveExternal(sc, remainder)
	}
	role, err := s.Store.RoleOf(sc.TargetWorkspace, principalID)
	if err != nil {
		return nil, &dfsError{"InternalError", http.StatusInternalServerError, err.Error()}
	}
	if store.RoleRank(role) < store.RoleRank(store.RoleContributor) {
		return nil, &dfsError{"AuthorizationFailure", http.StatusForbidden,
			"Reading through the shortcut requires ReadAll on the target workspace."}
	}
	target := joinPath(sc.TargetPath, remainder)
	p, err := s.Store.GetOneLakePath(sc.TargetItem, target)
	if err != nil {
		return nil, &dfsError{"PathNotFound", http.StatusNotFound, "The shortcut target path does not exist (dangling shortcut)."}
	}
	return p, nil
}

// externalRequest builds an authenticated upstream request for a shortcut
// target. Shared by the read, write and delete paths so a credential is
// applied identically however the shortcut is being used — the read path
// having its own copy is how the S3 signing bug stayed invisible to writes.
func (s *Service) externalRequest(sc *store.Shortcut, method, remainder string, body []byte) (*http.Request, *dfsError) {
	// TargetTable sits between the folder and the remainder rather than being
	// folded into TargetPath at create time, so the DTO can echo Dataverse's
	// `deltaLakeFolder` and `tableName` back as the two separate fields the
	// reference documents. It is empty for every other target type, and
	// joinPath drops empty segments, so this composes identically for them.
	root := joinPath(sc.TargetPath, sc.TargetTable)
	target, err := url.Parse(sc.TargetLocation + "/" + joinPath(root, remainder))
	if err != nil {
		return nil, &dfsError{"ExternalTargetInvalid", http.StatusBadGateway, err.Error()}
	}
	conn, err := s.Store.GetConnection(sc.ConnectionID)
	if err != nil {
		return nil, &dfsError{"ExternalConnectionNotFound", http.StatusBadGateway, "The shortcut connection is unavailable."}
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, target.String(), reader)
	var creds struct {
		CredentialType, Username, Password, Key, Token string
	}
	_ = json.Unmarshal([]byte(conn.CredentialsJSON), &creds)

	// An Amazon S3 target is signed with SigV4 — a header credential is not
	// something a real S3 endpoint accepts.
	//
	// Which credential carries the keys is a documented-shape decision worth
	// spelling out. Fabric's S3 connector uses authentication kind "Access
	// Key" with an **Access Key Id** and a **Secret Access Key**
	// (fabric-docs data-factory/connector-amazon-s3.md), but the REST
	// reference's CredentialType enumeration has no `AccessKey` member, and
	// `Basic` is the only documented type carrying two secrets. So the pair
	// travels as Basic's username/password rather than on invented fields.
	if sc.TargetType == "AmazonS3" && creds.CredentialType == "Basic" {
		payload := awssig.EmptyPayloadHash
		if body != nil {
			payload = awssig.HashPayload(body)
		}
		awssig.Sign(req, awssig.Credentials{
			AccessKeyID:     creds.Username,
			SecretAccessKey: creds.Password,
		}, s.S3Region(), "s3", payload, time.Unix(s.Store.Now(), 0).UTC())
		return req, nil
	}
	switch creds.CredentialType {
	case "Basic":
		req.SetBasicAuth(creds.Username, creds.Password)
	case "SharedAccessSignature":
		target.RawQuery = strings.TrimPrefix(creds.Token, "?")
		req.URL = target
	case "Key":
		req.Header.Set("x-api-key", creds.Key)
	case "", "Anonymous":
	default:
		return nil, &dfsError{"ExternalCredentialUnsupported", http.StatusBadGateway, "The shortcut credential type is not supported for HTTP read-through."}
	}
	return req, nil
}

// resolveExternal reads a file from the shortcut target.
func (s *Service) resolveExternal(sc *store.Shortcut, remainder string) (*store.OneLakePath, *dfsError) {
	req, derr := s.externalRequest(sc, http.MethodGet, remainder, nil)
	if derr != nil {
		return nil, derr
	}
	resp, err := s.Client.Do(req)
	return s.readExternalBody(resp, err, remainder)
}

// externalWritable reports whether writes may be forwarded to this target.
//
// Fabric documents S3 shortcuts as read-only — "They don't support write
// operations regardless of the user's permissions"
// (onelake/create-s3-shortcut.md) — so a write there must fail rather than be
// forwarded. Dataverse carries the identical sentence, word for word
// (onelake/create-dataverse-shortcut.md, stated twice: in the intro and again
// under Limitations), and is read-only for the same reason. ADLS Gen2 carries
// no such restriction, and the shortcuts doc describes deleting through one
// deleting in the target account.
//
// Note this is NOT the negation of Shortcut.IsExternalTarget: a target can be external
// and still refuse writes, which is exactly the case the two predicates exist
// to keep apart. Writable is the narrow allow-list; external is the broad one,
// and it lives in store because the API layer asks it too.
func externalWritable(sc *store.Shortcut) bool { return sc.TargetType == "ADLSGen2" }

// writeExternal pushes the assembled file to the shortcut target. Called at
// flush, which is the point the DFS protocol considers a file written.
func (s *Service) writeExternal(sc *store.Shortcut, remainder string, content []byte) *dfsError {
	if !externalWritable(sc) {
		return &dfsError{"UnsupportedOperation", http.StatusBadRequest,
			"Writes are not supported through a shortcut to this target type."}
	}
	req, derr := s.externalRequest(sc, http.MethodPut, remainder, content)
	if derr != nil {
		return derr
	}
	// Blob PUT semantics: real ADLS Gen2 accounts expose a Blob endpoint
	// alongside DFS, and a single authenticated PUT is what both accept for a
	// whole-file write.
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	req.ContentLength = int64(len(content))
	return s.discardExternalBody(s.Client.Do(req))
}

// deleteExternal removes a file in the shortcut target. Deleting a file
// *within* a shortcut deletes it at the target, which the shortcuts doc states
// explicitly; deleting the shortcut object itself is a control-plane
// operation and leaves the target untouched.
func (s *Service) deleteExternal(sc *store.Shortcut, remainder string) *dfsError {
	if !externalWritable(sc) {
		return &dfsError{"UnsupportedOperation", http.StatusBadRequest,
			"Deletes are not supported through a shortcut to this target type."}
	}
	req, derr := s.externalRequest(sc, http.MethodDelete, remainder, nil)
	if derr != nil {
		return derr
	}
	return s.discardExternalBody(s.Client.Do(req))
}

// discardExternalBody turns an upstream response into a dfsError or nil. The
// upstream status is surfaced rather than swallowed: a write that the target
// refused must not look like a success to the caller.
func (s *Service) discardExternalBody(resp *http.Response, err error) *dfsError {
	if err != nil {
		return &dfsError{"ExternalTargetUnavailable", http.StatusBadGateway, err.Error()}
	}
	defer resp.Body.Close()
	// bounded-read-exempt: this DISCARDS the body to let the connection be
	// reused. Nothing is stored or served, so there is nothing to truncate.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &dfsError{"ExternalTargetError", http.StatusBadGateway,
			fmt.Sprintf("external target returned HTTP %d", resp.StatusCode)}
	}
	return nil
}

// S3Region is the region the shortcut signer signs for. S3-compatible servers
// (SeaweedFS, RustFS, and AWS itself for us-east-1) accept this default;
// FABRIC_S3_REGION overrides it for a bucket in another region.
func (s *Service) S3Region() string {
	if r := os.Getenv("FABRIC_S3_REGION"); r != "" {
		return r
	}
	return "us-east-1"
}

// readExternalBody turns an external response into a synthesized OneLake path.
func (s *Service) readExternalBody(resp *http.Response, err error, remainder string) (*store.OneLakePath, *dfsError) {
	if err != nil {
		return nil, &dfsError{"ExternalTargetUnavailable", http.StatusBadGateway, err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &dfsError{"ExternalTargetError", http.StatusBadGateway, fmt.Sprintf("external target returned HTTP %d", resp.StatusCode)}
	}
	// Bounded like a write, and for the same reason: whatever comes back is
	// handed to the client AS the file. A silent truncation here would be this
	// emulator lying about somebody else's storage account.
	body, ok := httpx.ReadBounded(resp.Body, httpx.MaxExternalRead)
	if !ok {
		return nil, &dfsError{"ExternalTargetError", http.StatusBadGateway,
			fmt.Sprintf("The shortcut target returned more than %d bytes, or the read failed; refusing to serve a partial file.", int64(httpx.MaxExternalRead))}
	}
	return &store.OneLakePath{RelPath: remainder, Content: body, CreatedAt: s.Store.Now(), ModifiedAt: s.Store.Now()}, nil
}

func joinPath(base, remainder string) string {
	switch {
	case base == "":
		return remainder
	case remainder == "":
		return base
	default:
		return base + "/" + remainder
	}
}

// renameSource resolves the x-ms-rename-source path (which is
// /{filesystem}/{item}/{rel…} on the wire) to a rel path within the same
// item — cross-item renames are rejected, matching managed-folder rules.
func (s *Service) renameSource(wsID, itemID, src string) (string, *dfsError) {
	src, _, _ = strings.Cut(src, "?") // may carry a sas/query suffix
	segs := strings.Split(strings.Trim(src, "/"), "/")
	if len(segs) < 4 {
		return "", &dfsError{"InvalidRenameSource", http.StatusBadRequest,
			"x-ms-rename-source must be /{workspace}/{item}/{path} within the same item."}
	}
	ws, derr := s.resolveWorkspace(segs[0])
	if derr != nil {
		return "", derr
	}
	it, derr := s.resolveItem(ws.ID, segs[1])
	if derr != nil {
		return "", derr
	}
	if ws.ID != wsID || it.ID != itemID {
		return "", &dfsError{"InvalidRenameSource", http.StatusBadRequest,
			"Renames must stay within one item (Fabric-managed folders cannot move)."}
	}
	return strings.Join(segs[2:], "/"), nil
}

// ServeHTTP implements the DFS endpoint.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Env-gated request tracing (diagnostics only; off in prod). Read per
	// request so it can be toggled without a restart (and in tests).
	if os.Getenv("ONELAKE_TRACE") != "" {
		tw := &traceWriter{ResponseWriter: w, status: 200}
		w = tw
		defer func() {
			log.Printf("[onelake-dfs] %s %s?%s range=%q x-ms-range=%q rename=%q -> %d (%dB)",
				r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Range"),
				r.Header.Get("x-ms-range"), r.Header.Get("x-ms-rename-source"), tw.status, tw.n)
		}()
	}
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
	// only (roles-workspaces.md). A Viewer has no ReadAll — but OneLake
	// security can GRANT one specific paths: "No by default. Use OneLake
	// security to grant the access" (data-access-control-model.md).
	//
	// So a Viewer is not refused here. The grant is per ITEM and per PATH, and
	// neither is known yet, so the decision moves to authorizeViewer below.
	// Anyone with no workspace role at all is refused now regardless: workspace
	// permissions are "the first security boundary", and OneLake security
	// narrows within an item rather than admitting a stranger to the tenant.
	viewerOnly := store.RoleRank(role) < store.RoleRank(store.RoleContributor)
	if viewerOnly && (role == "" || len(segs) == 1) {
		writeDFSErr(w, dfsError{"AuthorizationFailure", http.StatusForbidden,
			"OneLake API access requires ReadAll (the Contributor role or above), or a OneLake security role granting the path."})
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

	// A Viewer reaches only what a OneLake security role grants, and only for
	// reading. Admin/Member/Contributor never arrive here: their Write
	// permission "overrides any OneLake security Read permissions", so a role
	// cannot take away what the workspace already gave.
	if viewerOnly {
		if derr := s.authorizeViewer(it.ID, rel, p.ID, r.Method); derr != nil {
			writeDFSErr(w, *derr)
			return
		}
	}

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
			// Hadoop ABFS implements mkdirs as an unconditional create request.
			// For an already-existing managed directory, make that operation
			// idempotent; no user-owned path is created or changed. Other managed
			// folder mutations retain the public OneLake rejection below.
			if r.Method == http.MethodPut && r.URL.Query().Get("resource") == "directory" &&
				strings.Contains(r.UserAgent(), "Azure Blob FS") {
				w.WriteHeader(http.StatusOK)
				return
			}
			writeDFSErr(w, dfsError{"OperationNotAllowedOnManagedFolder", http.StatusConflict,
				"Fabric-managed folders (the item root and its first level) cannot be created, renamed, or deleted via ADLS APIs."})
		}
		return
	}

	// Attribute this touch to the notebook cell that made it, when the caller
	// says which one (see observe.go). Recorded before dispatch so both reads
	// and writes are seen at one place.
	s.observe(r, it.ID, rel)

	switch r.Method {
	case http.MethodPut: // create file/directory, rename, or append/flush
		// The Hadoop ABFS driver sends append/flush as PUT with an action
		// query param (the ADLS REST spec uses PATCH, but ABFS uses PUT).
		// Without this, a flush PUT — which carries no body — would fall
		// through to "create file" and truncate the file to zero bytes.
		if a := strings.ToLower(r.URL.Query().Get("action")); a == "append" || a == "flush" {
			s.patch(w, r, it.ID, rel)
			return
		}
		// DFS rename: PUT dst with x-ms-rename-source (Hadoop committers).
		if src := r.Header.Get("x-ms-rename-source"); src != "" {
			srcRel, derr := s.renameSource(ws.ID, it.ID, src)
			if derr != nil {
				writeDFSErr(w, *derr)
				return
			}
			if err := s.Store.RenameOneLakePath(it.ID, srcRel, rel); err != nil {
				writeDFSErr(w, dfsError{"PathNotFound", http.StatusNotFound, err.Error()})
				return
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		isDir := r.URL.Query().Get("resource") == "directory"
		body, ok := httpx.ReadBounded(r.Body, httpx.MaxDFSPut)
		if !ok {
			writeDFSErr(w, dfsError{"RequestBodyTooLarge", http.StatusRequestEntityTooLarge,
				fmt.Sprintf("The request body is too large. The maximum size is %d bytes.", int64(httpx.MaxDFSPut))})
			return
		}
		ifNoneMatch := r.Header.Get("If-None-Match") == "*"
		err := s.Store.CreateOneLakePathAs(s.attributionOf(r), &store.OneLakePath{
			WorkspaceID: ws.ID, ItemID: it.ID, RelPath: rel, IsDir: isDir, Content: body,
		}, ifNoneMatch)
		if errors.Is(err, store.ErrPathExists) {
			writeDFSErr(w, dfsError{"PathAlreadyExists", http.StatusConflict, "The specified path already exists."})
			return
		}
		if err != nil {
			writeDFSErr(w, dfsError{"InternalError", http.StatusInternalServerError, err.Error()})
			return
		}
		w.WriteHeader(http.StatusCreated)

	case http.MethodPatch: // append | flush
		s.patch(w, r, it.ID, rel)

	case http.MethodHead:
		pth, derr := s.resolveRead(it.ID, rel, p.ID)
		if derr != nil {
			writeDFSErr(w, *derr)
			return
		}
		permHeaders(w)
		pathHeaders(w, pth, s.Store)
		w.Header().Set("Content-Length", strconv.Itoa(len(pth.Content)))
		if pth.IsDir {
			w.Header().Set("x-ms-resource-type", "directory")
		} else {
			w.Header().Set("x-ms-resource-type", "file")
		}
		w.WriteHeader(http.StatusOK)

	case http.MethodGet: // read file (Range-aware: Parquet readers seek)
		pth, derr := s.resolveRead(it.ID, rel, p.ID)
		if derr != nil {
			writeDFSErr(w, *derr)
			return
		}
		if pth.IsDir {
			writeDFSErr(w, dfsError{"PathIsDirectory", http.StatusBadRequest, "The path is a directory."})
			return
		}
		permHeaders(w)
		pathHeaders(w, pth, s.Store)
		serveContent(w, r, pth.Content)

	case http.MethodDelete:
		// Deleting a file *within* a shortcut deletes it at the target
		// (onelake/onelake-shortcuts.md, "How do shortcuts handle
		// deletions?"). Deleting the shortcut object itself is a
		// control-plane operation and does not reach here.
		if sc, remainder, err := s.Store.ShortcutFor(it.ID, rel); err == nil && sc != nil && sc.IsExternalTarget() {
			if derr := s.deleteExternal(sc, remainder); derr != nil {
				writeDFSErr(w, *derr)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
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
		data, ok := httpx.ReadBounded(r.Body, httpx.MaxDFSAppend)
		if !ok {
			writeDFSErr(w, dfsError{"RequestBodyTooLarge", http.StatusRequestEntityTooLarge,
				fmt.Sprintf("The request body is too large. The maximum size for one append is %d bytes.", int64(httpx.MaxDFSAppend))})
			return
		}
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
		// A path under an external shortcut belongs to the target, not to us.
		// Flush is the point the DFS protocol considers the file written, so
		// that is where the bytes go upstream. Without this the write would
		// succeed locally and silently never reach the storage account.
		if sc, remainder, err := s.Store.ShortcutFor(itemID, rel); err == nil && sc != nil && sc.IsExternalTarget() {
			if derr := s.writeExternal(sc, remainder, pth.Content); derr != nil {
				// Drop the local buffer: it must not masquerade as target data.
				_ = s.Store.DeleteOneLakePath(itemID, rel)
				writeDFSErr(w, *derr)
				return
			}
			// Reads go through to the target, so the local copy is redundant
			// and would shadow later changes made at the target.
			_ = s.Store.DeleteOneLakePath(itemID, rel)
			// The bytes went to the external account, not to OneLake, so no
			// OneLake file event is raised — nothing landed here.
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		// Flush is where the file becomes real, so it is where triggers fire
		// and the flow stream reports an arrival — not the empty create above.
		s.Store.EmitFileWritten(pth.WorkspaceID, itemID, rel, s.attributionOf(r))
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

// traceWriter captures the status and byte count for the trace log.
type traceWriter struct {
	http.ResponseWriter
	status int
	n      int
}

func (t *traceWriter) WriteHeader(code int) { t.status = code; t.ResponseWriter.WriteHeader(code) }
func (t *traceWriter) Write(b []byte) (int, error) {
	n, err := t.ResponseWriter.Write(b)
	t.n += n
	return n, err
}

// authorizeViewer decides whether a Viewer — who has no ReadAll — may touch one
// path, on the strength of the item's OneLake security roles.
//
// READ ONLY, DELIBERATELY. The model defines a ReadWrite permission, and a
// Viewer holding one could legitimately write. This increment implements Read
// and refuses the rest rather than accepting a ReadWrite grant it would then
// half-honour: a write allowed here but not scoped elsewhere is worse than a
// write refused. docs/54-onelake-security.md records the boundary.
func (s *Service) authorizeViewer(itemID, rel, principalID, method string) *dfsError {
	if method != http.MethodGet && method != http.MethodHead {
		return &dfsError{"AuthorizationFailure", http.StatusForbidden,
			"A OneLake security role grants Read; writing requires the Contributor role or above."}
	}
	roles, err := s.Store.EvaluatableRoles(itemID)
	if err != nil {
		return &dfsError{"InternalError", http.StatusInternalServerError, err.Error()}
	}
	// Deny by default: no roles is a decision, not a reason to fall through to
	// some looser check.
	entries := onelakesec.Effective(roles,
		onelakesec.Principal{ObjectID: principalID}, onelakesec.InputFor(rel))
	if !onelakesec.Allows(entries, rel) {
		return &dfsError{"AuthorizationFailure", http.StatusForbidden,
			"No OneLake security role grants this principal access to the path."}
	}
	return nil
}
