package onelake

// The Blob-endpoint alias (onelake.blob.fabric.microsoft.com). OneLake
// serves the same data over Blob and DFS APIs (onelake-api-parity.md); the
// Blob dialect is what Rust object_store — and therefore delta-rs — speaks:
// Put Blob with If-None-Match:* (Delta's _delta_log put-if-absent), block
// uploads, Range reads, XML container listing, and Copy Blob.
//
// Two addressings reach this surface:
//   Host onelake.blob.…       →  /{workspace}/{blob…}
//   any host (endpoint override, azurite-style) → /onelake/{workspace}/{blob…}
// The account segment is always the literal "onelake", as documented.

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// maxStagedBytes bounds the total uncommitted Put Block data held in memory.
//
// Staged blocks are freed on commit, and a client that never commits used to
// hold its bytes for the life of the process. That is not hypothetical: an
// aborted upload leaks, and delta-rs retries a lost `_delta_log` commit under a
// NEW blob key, so the abandoned staging of every losing attempt accumulated.
//
// Real Azure expires uncommitted blocks after 7 days, which a process that
// rarely lives that long cannot meaningfully emulate — and tying eviction to the
// emulator's clock would make it stop entirely whenever a test freezes time. A
// size bound is the honest substitute: it targets the actual failure (unbounded
// growth) with behaviour that does not depend on how long anything ran.
//
// 256 MiB is far above any real staging set — the Blob path caps a single block
// at 256 MiB and Delta commits are kilobytes — so eviction only ever reaches
// abandoned uploads.
const maxStagedBytes = 256 << 20

// blockStage holds uncommitted Put Block data per blob (transient, like the
// real service's uncommitted block list).
type blockStage struct {
	mu     sync.Mutex
	blocks map[string]map[string][]byte // blobKey → blockID → data
	bytes  int64                        // total staged, for the eviction bound
	seq    int64                        // monotonic, to evict oldest-first
	staged map[string]int64             // blobKey → seq of its first block
	// max overrides maxStagedBytes when non-zero. Only tests set it: proving the
	// bound at its real value would mean allocating a quarter of a gigabyte to
	// watch one branch, which is a slow test rather than a better one.
	max int64
}

func (b *blockStage) limit() int64 {
	if b.max > 0 {
		return b.max
	}
	return maxStagedBytes
}

func (b *blockStage) put(key, id string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.blocks == nil {
		b.blocks = map[string]map[string][]byte{}
		b.staged = map[string]int64{}
	}
	if b.blocks[key] == nil {
		b.blocks[key] = map[string][]byte{}
		b.seq++
		b.staged[key] = b.seq
	}
	// Replacing a block id replaces its bytes, as Azure does.
	b.bytes -= int64(len(b.blocks[key][id]))
	b.blocks[key][id] = data
	b.bytes += int64(len(data))
	b.evictLocked(key)
}

// evictLocked drops whole abandoned blobs, oldest first, until the stage is back
// under its bound. `keep` is the blob being written right now, which is never
// evicted — dropping it would fail the upload that is actively making progress.
//
// A dropped blob's later commit fails with InvalidBlockList, which is what Azure
// answers for expired blocks: a loud, correct error rather than a silent
// truncation.
func (b *blockStage) evictLocked(keep string) {
	for b.bytes > b.limit() {
		oldest, oldestSeq := "", int64(0)
		for k, s := range b.staged {
			if k == keep {
				continue
			}
			if oldestSeq == 0 || s < oldestSeq {
				oldest, oldestSeq = k, s
			}
		}
		if oldest == "" {
			return // only the in-progress blob is left; it is not ours to drop
		}
		b.dropLocked(oldest)
	}
}

func (b *blockStage) dropLocked(key string) {
	for _, d := range b.blocks[key] {
		b.bytes -= int64(len(d))
	}
	delete(b.blocks, key)
	delete(b.staged, key)
}

func (b *blockStage) commit(key string, ids []string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	staged := b.blocks[key]
	var out []byte
	for _, id := range ids {
		data, ok := staged[id]
		if !ok {
			return nil, false
		}
		out = append(out, data...)
	}
	b.dropLocked(key)
	return out, true
}

func writeBlobErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("x-ms-error-code", code)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg)
}

// ServeBlob handles the Blob dialect. Paths arrive workspace-first (the
// /onelake account prefix, when present, is stripped by the router).
func (s *Service) ServeBlob(w http.ResponseWriter, r *http.Request) {
	// Same env-gated tracing as the DFS surface (ServeHTTP): the Blob dialect
	// is what delta-rs speaks, so a Delta commit is only visible here.
	if os.Getenv("ONELAKE_TRACE") != "" {
		tw := &traceWriter{ResponseWriter: w, status: 200}
		w = tw
		defer func() {
			log.Printf("[onelake-blob] %s %s?%s inm=%q copy=%q -> %d (%dB)",
				r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("If-None-Match"),
				r.Header.Get("x-ms-copy-source"), tw.status, tw.n)
		}()
	}
	p, err := s.Auth.ValidateRequest(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer authorization_uri="`+s.Auth.Issuer+`"`)
		writeBlobErr(w, http.StatusUnauthorized, "NoAuthenticationInformation", err.Error())
		return
	}

	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		writeBlobErr(w, http.StatusBadRequest, "InvalidUri", "Container (workspace) required.")
		return
	}
	ws, derr := s.resolveWorkspace(segs[0])
	if derr != nil {
		writeBlobErr(w, http.StatusNotFound, "ContainerNotFound", derr.msg)
		return
	}
	role, err := s.Store.RoleOf(ws.ID, p.ID)
	if err != nil {
		writeBlobErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	if store.RoleRank(role) < store.RoleRank(store.RoleContributor) {
		writeBlobErr(w, http.StatusForbidden, "AuthorizationFailure",
			"OneLake API access requires ReadAll (Contributor or above).")
		return
	}

	// Container level: HEAD (exists) and List Blobs.
	if len(segs) == 1 {
		switch {
		case r.Method == http.MethodHead || (r.Method == http.MethodGet && r.URL.Query().Get("comp") == ""):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Query().Get("comp") == "list":
			s.listBlobs(w, r, ws)
		default:
			writeBlobErr(w, http.StatusConflict, "OperationNotAllowedOnContainer",
				"Workspaces are managed through Fabric experiences, not Blob APIs.")
		}
		return
	}

	it, derr := s.resolveItem(ws.ID, segs[1])
	if derr != nil {
		writeBlobErr(w, http.StatusNotFound, "BlobNotFound", derr.msg)
		return
	}
	rel := strings.Join(segs[2:], "/")
	// Managed folders: blobs live only under the item's managed first level.
	if len(segs) <= 3 && (r.Method == http.MethodPut || r.Method == http.MethodDelete) {
		writeBlobErr(w, http.StatusConflict, "OperationNotAllowedOnManagedFolder",
			"Fabric-managed folders (the item root and its first level) cannot be modified via Blob APIs.")
		return
	}
	blobKey := it.ID + "|" + rel
	s.observe(r, it.ID, rel) // notebook-cell attribution; see observe.go

	switch r.Method {
	case http.MethodPut:
		q := r.URL.Query()
		switch q.Get("comp") {
		case "block": // stage an uncommitted block
			id := q.Get("blockid")
			if _, err := base64.StdEncoding.DecodeString(id); err != nil || id == "" {
				writeBlobErr(w, http.StatusBadRequest, "InvalidQueryParameterValue", "blockid must be base64.")
				return
			}
			data, ok := httpx.ReadBounded(r.Body, httpx.MaxBlobWrite)
			if !ok {
				writeBlobErr(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge",
					"The request body is too large.")
				return
			}
			s.stage.put(blobKey, id, data)
			w.WriteHeader(http.StatusCreated)
		case "blocklist": // commit staged blocks in the given order
			var bl struct {
				Latest      []string `xml:"Latest"`
				Committed   []string `xml:"Committed"`
				Uncommitted []string `xml:"Uncommitted"`
			}
			// Bounded explicitly even though the XML parse would reject a
			// truncated document anyway: "InvalidXmlDocument" tells the caller
			// their XML is malformed, which is a lie when what actually
			// happened is that their block list was too big — and the day this
			// stops parsing the body it would inherit the silent bug.
			raw, ok := httpx.ReadBounded(r.Body, httpx.MaxBlobMetadata)
			if !ok {
				writeBlobErr(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge",
					"The block list is too large.")
				return
			}
			if err := xml.Unmarshal(raw, &bl); err != nil {
				writeBlobErr(w, http.StatusBadRequest, "InvalidXmlDocument", err.Error())
				return
			}
			ids := append(append(bl.Latest, bl.Committed...), bl.Uncommitted...)
			content, ok := s.stage.commit(blobKey, ids)
			if !ok {
				writeBlobErr(w, http.StatusBadRequest, "InvalidBlockList", "Unknown block id in the block list.")
				return
			}
			s.putBlob(w, r, ws, it, rel, content)
		default:
			if src := r.Header.Get("x-ms-copy-source"); src != "" {
				s.copyBlob(w, r, ws, it, rel, src)
				return
			}
			data, ok := httpx.ReadBounded(r.Body, httpx.MaxBlobWrite)
			if !ok {
				writeBlobErr(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge",
					"The request body is too large.")
				return
			}
			s.putBlob(w, r, ws, it, rel, data)
		}

	case http.MethodHead:
		pth, err := s.Store.GetOneLakePath(it.ID, rel)
		if err != nil || pth.IsDir {
			writeBlobErr(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
			return
		}
		pathHeaders(w, pth, s.Store)
		w.Header().Set("Content-Length", strconv.Itoa(len(pth.Content)))
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		pth, err := s.Store.GetOneLakePath(it.ID, rel)
		if err != nil || pth.IsDir {
			writeBlobErr(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
			return
		}
		pathHeaders(w, pth, s.Store)
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		serveContent(w, r, pth.Content)

	case http.MethodDelete:
		if err := s.Store.DeleteOneLakePath(it.ID, rel); err != nil {
			writeBlobErr(w, http.StatusNotFound, "BlobNotFound", "The specified blob does not exist.")
			return
		}
		w.WriteHeader(http.StatusAccepted)

	default:
		writeBlobErr(w, http.StatusMethodNotAllowed, "UnsupportedHttpVerb", "Unsupported method.")
	}
}

// putBlob writes a block blob, honoring If-None-Match:* (put-if-absent —
// Delta's _delta_log commit primitive).
func (s *Service) putBlob(w http.ResponseWriter, r *http.Request, ws *store.Workspace, it *store.Item, rel string, data []byte) {
	ifNoneMatch := r.Header.Get("If-None-Match") == "*"
	pth := &store.OneLakePath{WorkspaceID: ws.ID, ItemID: it.ID, RelPath: rel, Content: data}
	err := s.Store.CreateOneLakePathAs(s.attributionOf(r), pth, ifNoneMatch)
	if err == store.ErrPathExists {
		writeBlobErr(w, http.StatusConflict, "BlobAlreadyExists", "The specified blob already exists.")
		return
	}
	if err != nil {
		writeBlobErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("ETag", pth.ETag)
	w.Header().Set("Last-Modified", time.Unix(pth.ModifiedAt, 0).UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusCreated)
}

// copyBlob implements Copy Blob within OneLake (object_store's rename is
// copy + delete; copy_if_not_exists carries If-None-Match:*).
func (s *Service) copyBlob(w http.ResponseWriter, r *http.Request, ws *store.Workspace, it *store.Item, rel, src string) {
	u, err := url.Parse(src)
	if err != nil {
		writeBlobErr(w, http.StatusBadRequest, "InvalidHeaderValue", "x-ms-copy-source is not a URL.")
		return
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) > 0 && segs[0] == "onelake" { // account-prefixed form
		segs = segs[1:]
	}
	if len(segs) < 3 {
		writeBlobErr(w, http.StatusBadRequest, "InvalidHeaderValue", "x-ms-copy-source must address /{workspace}/{item}/{path}.")
		return
	}
	srcWS, derr := s.resolveWorkspace(segs[0])
	if derr != nil {
		writeBlobErr(w, http.StatusNotFound, "CannotVerifyCopySource", derr.msg)
		return
	}
	srcIt, derr := s.resolveItem(srcWS.ID, segs[1])
	if derr != nil {
		writeBlobErr(w, http.StatusNotFound, "CannotVerifyCopySource", derr.msg)
		return
	}
	srcPath, err := s.Store.GetOneLakePath(srcIt.ID, strings.Join(segs[2:], "/"))
	if err != nil {
		writeBlobErr(w, http.StatusNotFound, "CannotVerifyCopySource", "The copy source does not exist.")
		return
	}
	ifNoneMatch := r.Header.Get("If-None-Match") == "*"
	dst := &store.OneLakePath{WorkspaceID: ws.ID, ItemID: it.ID, RelPath: rel, Content: srcPath.Content}
	err = s.Store.CreateOneLakePathAs(s.attributionOf(r), dst, ifNoneMatch)
	if err == store.ErrPathExists {
		writeBlobErr(w, http.StatusConflict, "BlobAlreadyExists", "The specified blob already exists.")
		return
	}
	if err != nil {
		writeBlobErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.Header().Set("ETag", dst.ETag)
	w.Header().Set("x-ms-copy-status", "success")
	w.WriteHeader(http.StatusAccepted)
}

// guidAddressed reports whether the list prefix addresses the item by GUID
// rather than name.Type. Emitted names must echo the client's addressing:
// delta-rs opens az://{workspaceGuid}/{itemGuid}/Tables/t and lists
// _delta_log under the GUID prefix — name-form results would match nothing
// and delta-kernel fails with "No files in log segment".
func guidAddressed(prefix, itemID string) bool {
	seg, _, hasSlash := strings.Cut(prefix, "/")
	if hasSlash {
		return seg == itemID
	}
	return seg != "" && strings.HasPrefix(itemID, seg)
}

// listBlobs implements List Blobs (?comp=list) with prefix, delimiter,
// marker paging, and startFrom — the XML dialect object_store's list() parses.
//
// startFrom is how object_store expresses list_with_offset on Azure, and its
// semantics are the opposite of the S3/GCP equivalent: **inclusive**, where
// S3's start-after is exclusive. object_store compensates by dropping the
// first entry when it equals the offset, so returning a *half-open* range here
// would silently skip a blob (arrow-rs-object-store src/azure/client.rs:
// "startFrom is inclusive (unlike S3/GCP's start-after which is exclusive)").
//
// Ignoring it entirely — as this did — is worse than skipping: the listing
// starts from the beginning, so delta-rs's get_latest_version() gets a log
// segment beginning at version 0 when it asked for one beginning at N, and
// the kernel rejects it as "Invalid table version: N". That makes every
// delta-rs commit-conflict path fail (OPTIMIZE, VACUUM, MERGE) while plain
// writes still pass, because only the former re-reads the log from an offset.
func (s *Service) listBlobs(w http.ResponseWriter, r *http.Request, ws *store.Workspace) {
	q := r.URL.Query()
	prefix := strings.TrimPrefix(q.Get("prefix"), "/")
	delimiter := q.Get("delimiter")
	marker := q.Get("marker")
	startFrom := q.Get("startFrom")
	maxResults := 5000
	if mr := q.Get("maxresults"); mr != "" {
		if n, err := strconv.Atoi(mr); err == nil && n > 0 && n < maxResults {
			maxResults = n
		}
	}

	// Gather every blob in the workspace as {item}.{Type}/{rel} names.
	items, err := s.Store.ListItems(ws.ID, "")
	if err != nil {
		writeBlobErr(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	type blob struct {
		name string
		p    *store.OneLakePath
	}
	var all []blob
	for _, it := range items {
		base := it.DisplayName + "." + it.Type
		if guidAddressed(prefix, it.ID) {
			base = it.ID
		}
		paths, err := s.Store.ListOneLakePaths(it.ID, "", true)
		if err != nil {
			writeBlobErr(w, http.StatusInternalServerError, "InternalError", err.Error())
			return
		}
		for _, p := range paths {
			if p.IsDir {
				continue // Blob namespaces are flat; directories are virtual
			}
			all = append(all, blob{name: base + "/" + p.RelPath, p: p})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })

	type xmlBlob struct {
		Name  string `xml:"Name"`
		Props struct {
			LastModified  string `xml:"Last-Modified"`
			ETag          string `xml:"Etag"`
			ContentLength int    `xml:"Content-Length"`
			ContentType   string `xml:"Content-Type"`
			BlobType      string `xml:"BlobType"`
		} `xml:"Properties"`
	}
	var blobs []xmlBlob
	prefixSet := map[string]bool{}
	var prefixes []string
	next := ""
	for _, b := range all {
		if prefix != "" && !strings.HasPrefix(b.name, prefix) {
			continue
		}
		if marker != "" && b.name <= marker {
			continue
		}
		// Inclusive, per the note above — `<`, not `<=`. marker wins when both
		// are present: object_store sends startFrom only on the first request
		// and marker on every page after it.
		if marker == "" && startFrom != "" && b.name < startFrom {
			continue
		}
		if len(blobs)+len(prefixes) >= maxResults {
			next = blobs[len(blobs)-1].Name
			if len(prefixes) > 0 && prefixes[len(prefixes)-1] > next {
				next = prefixes[len(prefixes)-1]
			}
			break
		}
		if delimiter != "" {
			rest := strings.TrimPrefix(b.name, prefix)
			if i := strings.Index(rest, delimiter); i >= 0 {
				dir := prefix + rest[:i+len(delimiter)]
				if !prefixSet[dir] {
					prefixSet[dir] = true
					prefixes = append(prefixes, dir)
				}
				continue
			}
		}
		xb := xmlBlob{Name: b.name}
		mod := b.p.ModifiedAt
		if mod == 0 {
			mod = b.p.CreatedAt
		}
		xb.Props.LastModified = time.Unix(mod, 0).UTC().Format(http.TimeFormat)
		xb.Props.ETag = b.p.ETag
		xb.Props.ContentLength = len(b.p.Content)
		xb.Props.ContentType = "application/octet-stream"
		xb.Props.BlobType = "BlockBlob"
		blobs = append(blobs, xb)
	}

	type blobPrefix struct {
		Name string `xml:"Name"`
	}
	type blobsWrap struct {
		Blob       []xmlBlob    `xml:"Blob"`
		BlobPrefix []blobPrefix `xml:"BlobPrefix"`
	}
	type enumeration struct {
		XMLName    xml.Name  `xml:"EnumerationResults"`
		Container  string    `xml:"ContainerName,attr"`
		Prefix     string    `xml:"Prefix"`
		Blobs      blobsWrap `xml:"Blobs"`
		NextMarker string    `xml:"NextMarker"`
	}
	out := enumeration{Container: ws.DisplayName, Prefix: prefix, NextMarker: next}
	out.Blobs.Blob = blobs
	for _, p := range prefixes {
		out.Blobs.BlobPrefix = append(out.Blobs.BlobPrefix, blobPrefix{Name: p})
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(out)
}
