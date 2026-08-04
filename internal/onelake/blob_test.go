package onelake

// R0 tests: the Blob dialect (what delta-rs/object_store speaks), Range
// reads, rename, and the put-if-absent conditional that guards Delta's
// _delta_log commits.

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// doBlob drives ServeBlob directly.
func (f *fixture) doBlob(method, target, token string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd *strings.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, rd)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	f.svc.ServeBlob(w, r)
	return w
}

func TestBlobPutIfAbsent(t *testing.T) {
	f := newFixture(t)
	path := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/t/_delta_log/00000000000000000000.json"

	// First conditional create wins; the second loses with BlobAlreadyExists
	// — Delta's commit atomicity.
	if w := f.doBlob("PUT", path, f.token, []byte(`{"commit":1}`), map[string]string{"If-None-Match": "*"}); w.Code != http.StatusCreated {
		t.Fatalf("first conditional put = %d %s", w.Code, w.Body.Bytes())
	}
	w := f.doBlob("PUT", path, f.token, []byte(`{"commit":2}`), map[string]string{"If-None-Match": "*"})
	if w.Code != http.StatusConflict || w.Header().Get("x-ms-error-code") != "BlobAlreadyExists" {
		t.Fatalf("second conditional put = %d %q", w.Code, w.Header().Get("x-ms-error-code"))
	}
	// The loser did not overwrite the winner.
	g := f.doBlob("GET", path, f.token, nil, nil)
	if g.Code != http.StatusOK || !strings.Contains(g.Body.String(), `"commit":1`) {
		t.Fatalf("content after losing commit = %d %s", g.Code, g.Body.String())
	}
	// Unconditional PUT overwrites; ETag changes.
	e1 := g.Header().Get("ETag")
	if w := f.doBlob("PUT", path, f.token, []byte(`{"commit":3}`), nil); w.Code != http.StatusCreated {
		t.Fatalf("overwrite = %d", w.Code)
	}
	g2 := f.doBlob("GET", path, f.token, nil, nil)
	if g2.Header().Get("ETag") == e1 || g2.Header().Get("ETag") == "" {
		t.Fatalf("etag did not rotate: %q -> %q", e1, g2.Header().Get("ETag"))
	}
}

func TestBlobBlocksAndRange(t *testing.T) {
	f := newFixture(t)
	base := "/" + f.ws.ID + "/" + f.it.ID + "/Files/big.bin"

	// Stage two blocks, commit via blocklist, read with ranges.
	b1 := "QUFBQQ==" // base64 block ids
	b2 := "QkJCQg=="
	if w := f.doBlob("PUT", base+"?comp=block&blockid="+b1, f.token, []byte("hello "), nil); w.Code != http.StatusCreated {
		t.Fatalf("put block 1 = %d", w.Code)
	}
	if w := f.doBlob("PUT", base+"?comp=block&blockid="+b2, f.token, []byte("world"), nil); w.Code != http.StatusCreated {
		t.Fatalf("put block 2 = %d", w.Code)
	}
	bl := `<BlockList><Latest>` + b1 + `</Latest><Latest>` + b2 + `</Latest></BlockList>`
	if w := f.doBlob("PUT", base+"?comp=blocklist", f.token, []byte(bl), nil); w.Code != http.StatusCreated {
		t.Fatalf("blocklist = %d %s", w.Code, w.Body.Bytes())
	}
	// Unknown block id → 400; malformed XML → 400; bad blockid encoding → 400.
	if w := f.doBlob("PUT", base+"?comp=blocklist", f.token, []byte(`<BlockList><Latest>bm9wZQ==</Latest></BlockList>`), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown block = %d", w.Code)
	}
	if w := f.doBlob("PUT", base+"?comp=blocklist", f.token, []byte(`<nope`), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad xml = %d", w.Code)
	}
	if w := f.doBlob("PUT", base+"?comp=block&blockid=!!!", f.token, []byte("x"), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad blockid = %d", w.Code)
	}

	// Full read, then ranged reads (Parquet-style seeks).
	g := f.doBlob("GET", base, f.token, nil, nil)
	if g.Body.String() != "hello world" {
		t.Fatalf("committed content = %q", g.Body.String())
	}
	g = f.doBlob("GET", base, f.token, nil, map[string]string{"Range": "bytes=6-10"})
	if g.Code != http.StatusPartialContent || g.Body.String() != "world" ||
		g.Header().Get("Content-Range") != "bytes 6-10/11" {
		t.Fatalf("range = %d %q %q", g.Code, g.Body.String(), g.Header().Get("Content-Range"))
	}
	g = f.doBlob("GET", base, f.token, nil, map[string]string{"Range": "bytes=-5"})
	if g.Code != http.StatusPartialContent || g.Body.String() != "world" {
		t.Fatalf("suffix range = %d %q", g.Code, g.Body.String())
	}
	g = f.doBlob("GET", base, f.token, nil, map[string]string{"Range": "bytes=99-"})
	if g.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable range = %d", g.Code)
	}
	// HEAD carries type + length.
	h := f.doBlob("HEAD", base, f.token, nil, nil)
	if h.Header().Get("x-ms-blob-type") != "BlockBlob" || h.Header().Get("Content-Length") != "11" {
		t.Fatalf("head = %q %q", h.Header().Get("x-ms-blob-type"), h.Header().Get("Content-Length"))
	}
}

func TestBlobCopyAndDelete(t *testing.T) {
	f := newFixture(t)
	src := "/" + f.ws.ID + "/" + f.it.ID + "/Files/src.txt"
	dst := "/" + f.ws.ID + "/" + f.it.ID + "/Files/dst.txt"
	if w := f.doBlob("PUT", src, f.token, []byte("payload"), nil); w.Code != http.StatusCreated {
		t.Fatal(w.Code)
	}
	// Copy (object_store rename = copy + delete), with the account-prefixed
	// source form.
	w := f.doBlob("PUT", dst, f.token, nil, map[string]string{
		"x-ms-copy-source": "http://onelake.blob.fabric.microsoft.com/onelake/" + f.ws.ID + "/" + f.it.ID + "/Files/src.txt",
	})
	if w.Code != http.StatusAccepted || w.Header().Get("x-ms-copy-status") != "success" {
		t.Fatalf("copy = %d %s", w.Code, w.Body.Bytes())
	}
	if g := f.doBlob("GET", dst, f.token, nil, nil); g.Body.String() != "payload" {
		t.Fatalf("copied content = %q", g.Body.String())
	}
	// copy_if_not_exists loses against an existing destination.
	w = f.doBlob("PUT", dst, f.token, nil, map[string]string{
		"x-ms-copy-source": "http://x/" + f.ws.ID + "/" + f.it.ID + "/Files/src.txt",
		"If-None-Match":    "*",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("conditional copy over existing = %d", w.Code)
	}
	// Missing source → 404; garbage source → 400.
	w = f.doBlob("PUT", dst, f.token, nil, map[string]string{
		"x-ms-copy-source": "http://x/" + f.ws.ID + "/" + f.it.ID + "/Files/nope.txt"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing copy source = %d", w.Code)
	}
	if w := f.doBlob("PUT", dst, f.token, nil, map[string]string{"x-ms-copy-source": "http://x/short"}); w.Code != http.StatusBadRequest {
		t.Fatalf("short copy source = %d", w.Code)
	}
	// Delete → 202; second delete → 404.
	if w := f.doBlob("DELETE", src, f.token, nil, nil); w.Code != http.StatusAccepted {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := f.doBlob("DELETE", src, f.token, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("double delete = %d", w.Code)
	}
}

func TestBlobListAndWalls(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"Files/a.txt", "Files/raw/b.txt", "Tables/t/part-0.parquet"} {
		if w := f.doBlob("PUT", "/"+f.ws.ID+"/"+f.it.ID+"/"+p, f.token, []byte("x"), nil); w.Code != http.StatusCreated {
			t.Fatalf("seed %s = %d", p, w.Code)
		}
	}
	type listing struct {
		Blobs struct {
			Blob []struct {
				Name  string `xml:"Name"`
				Props struct {
					ContentLength int    `xml:"Content-Length"`
					ContentType   string `xml:"Content-Type"`
					ETag          string `xml:"Etag"`
				} `xml:"Properties"`
			} `xml:"Blob"`
			BlobPrefix []struct {
				Name string `xml:"Name"`
			} `xml:"BlobPrefix"`
		} `xml:"Blobs"`
		NextMarker string `xml:"NextMarker"`
	}
	// Flat list.
	w := f.doBlob("GET", "/"+f.ws.ID+"?restype=container&comp=list", f.token, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	}
	var l listing
	if err := xml.Unmarshal(w.Body.Bytes(), &l); err != nil {
		t.Fatalf("list xml: %v\n%s", err, w.Body.String())
	}
	if len(l.Blobs.Blob) != 3 || l.Blobs.Blob[0].Props.ETag == "" || l.Blobs.Blob[0].Props.ContentLength != 1 {
		t.Fatalf("flat list = %+v", l.Blobs.Blob)
	}
	// Delimited list under a prefix collapses directories.
	w = f.doBlob("GET", "/"+f.ws.ID+"?comp=list&prefix=lake.Lakehouse/Files/&delimiter=/", f.token, nil, nil)
	l = listing{}
	_ = xml.Unmarshal(w.Body.Bytes(), &l)
	if len(l.Blobs.Blob) != 1 || len(l.Blobs.BlobPrefix) != 1 ||
		l.Blobs.BlobPrefix[0].Name != "lake.Lakehouse/Files/raw/" {
		t.Fatalf("delimited list = %+v / %+v", l.Blobs.Blob, l.Blobs.BlobPrefix)
	}
	// Paging: maxresults=1 yields a NextMarker; marker resumes after it.
	w = f.doBlob("GET", "/"+f.ws.ID+"?comp=list&maxresults=1", f.token, nil, nil)
	l = listing{}
	_ = xml.Unmarshal(w.Body.Bytes(), &l)
	if len(l.Blobs.Blob) != 1 || l.NextMarker == "" {
		t.Fatalf("paged list = %+v marker=%q", l.Blobs.Blob, l.NextMarker)
	}
	w = f.doBlob("GET", "/"+f.ws.ID+"?comp=list&marker="+l.NextMarker, f.token, nil, nil)
	l2 := listing{}
	_ = xml.Unmarshal(w.Body.Bytes(), &l2)
	if len(l2.Blobs.Blob) != 2 {
		t.Fatalf("resumed list = %+v", l2.Blobs.Blob)
	}

	// Walls: no token 401; managed folders immune; container mutations blocked.
	if w := f.doBlob("GET", "/"+f.ws.ID+"?comp=list", "", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", w.Code)
	}
	if w := f.doBlob("PUT", "/"+f.ws.ID+"/"+f.it.ID+"/Files", f.token, []byte("x"), nil); w.Code != http.StatusConflict {
		t.Fatalf("write managed folder = %d", w.Code)
	}
	if w := f.doBlob("DELETE", "/"+f.ws.ID+"/nope.Lakehouse/Files/x", f.token, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown item = %d", w.Code)
	}
	if w := f.doBlob("PUT", "/"+f.ws.ID, f.token, nil, nil); w.Code != http.StatusConflict {
		t.Fatalf("create container = %d", w.Code)
	}
	if w := f.doBlob("HEAD", "/"+f.ws.ID, f.token, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("head container = %d", w.Code)
	}
	if w := f.doBlob("GET", "/nope?comp=list", f.token, nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown container = %d", w.Code)
	}
	if w := f.doBlob("GET", "/", f.token, nil, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("no container = %d", w.Code)
	}
	if w := f.doBlob("PATCH", "/"+f.ws.ID+"/"+f.it.ID+"/Files/a.txt", f.token, nil, nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported verb = %d", w.Code)
	}
}

// TestBlobListGUIDAddressing: docs/08-onelake and the parity map promise that
// GUID and name addressing resolve to the same item on every surface. The
// Blob listing used to emit only name.Type-form names, so delta-rs opening
// az://{workspaceGuid}/{itemGuid}/Tables/t listed _delta_log by GUID prefix,
// matched nothing, and failed with "No files in log segment". The listing
// must echo the addressing the client used.
func TestBlobListGUIDAddressing(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{
		"Tables/t/_delta_log/00000000000000000000.json",
		"Tables/t/part-0.parquet",
	} {
		if w := f.doBlob("PUT", "/"+f.ws.ID+"/"+f.it.ID+"/"+p, f.token, []byte("x"), nil); w.Code != http.StatusCreated {
			t.Fatalf("seed %s = %d", p, w.Code)
		}
	}
	type listing struct {
		Blobs struct {
			Blob []struct {
				Name string `xml:"Name"`
			} `xml:"Blob"`
			BlobPrefix []struct {
				Name string `xml:"Name"`
			} `xml:"BlobPrefix"`
		} `xml:"Blobs"`
	}
	list := func(query string) listing {
		t.Helper()
		w := f.doBlob("GET", "/"+f.ws.ID+"?comp=list&"+query, f.token, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("list %s = %d %s", query, w.Code, w.Body.Bytes())
		}
		var l listing
		if err := xml.Unmarshal(w.Body.Bytes(), &l); err != nil {
			t.Fatalf("list xml: %v\n%s", err, w.Body.String())
		}
		return l
	}

	// The exact listing delta-kernel issues when the table is GUID-addressed.
	l := list("prefix=" + f.it.ID + "/Tables/t/_delta_log/")
	if len(l.Blobs.Blob) != 1 ||
		l.Blobs.Blob[0].Name != f.it.ID+"/Tables/t/_delta_log/00000000000000000000.json" {
		t.Fatalf("GUID-prefixed _delta_log list = %+v", l.Blobs.Blob)
	}
	// Delimited GUID listing (object_store list_with_delimiter) echoes GUID
	// form in both Blob and BlobPrefix entries.
	l = list("prefix=" + f.it.ID + "/Tables/t/&delimiter=/")
	if len(l.Blobs.Blob) != 1 || l.Blobs.Blob[0].Name != f.it.ID+"/Tables/t/part-0.parquet" ||
		len(l.Blobs.BlobPrefix) != 1 || l.Blobs.BlobPrefix[0].Name != f.it.ID+"/Tables/t/_delta_log/" {
		t.Fatalf("GUID delimited list = %+v / %+v", l.Blobs.Blob, l.Blobs.BlobPrefix)
	}
	// Name addressing keeps returning name-form entries.
	l = list("prefix=lake.Lakehouse/Tables/t/_delta_log/")
	if len(l.Blobs.Blob) != 1 ||
		l.Blobs.Blob[0].Name != "lake.Lakehouse/Tables/t/_delta_log/00000000000000000000.json" {
		t.Fatalf("name-prefixed list = %+v", l.Blobs.Blob)
	}
}

func TestDFSRenameAndConditionals(t *testing.T) {
	f := newFixture(t)
	src := "/" + f.ws.ID + "/" + f.it.ID + "/Files/staging/part-0"
	dst := "/" + f.ws.ID + "/" + f.it.ID + "/Files/final/part-0"

	if w := f.do("PUT", src+"?resource=file", f.token, []byte("data")); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	// DFS conditional create: If-None-Match:* on an existing path → 409.
	r := httptest.NewRequest("PUT", src+"?resource=file", strings.NewReader("clobber"))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("If-None-Match", "*")
	w := httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("dfs conditional create = %d", w.Code)
	}

	// Rename (the Hadoop committer move).
	r = httptest.NewRequest("PUT", dst, strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("x-ms-rename-source", "/"+f.ws.ID+"/"+f.it.ID+"/Files/staging/part-0")
	w = httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("rename = %d %s", w.Code, w.Body.Bytes())
	}
	if g := f.do("GET", src, f.token, nil); g.Code != http.StatusNotFound {
		t.Fatalf("source survived rename = %d", g.Code)
	}
	g := f.do("GET", dst, f.token, nil)
	if g.Code != http.StatusOK || g.Body.String() != "data" || g.Header().Get("ETag") == "" {
		t.Fatalf("dest after rename = %d %q etag=%q", g.Code, g.Body.String(), g.Header().Get("ETag"))
	}

	// Bad rename sources: cross-item, too short, unknown.
	for src, want := range map[string]int{
		"/short": http.StatusBadRequest,
		"/" + f.ws.ID + "/nope.Lakehouse/Files/x": http.StatusNotFound,
	} {
		r = httptest.NewRequest("PUT", dst, strings.NewReader(""))
		r.Header.Set("Authorization", "Bearer "+f.token)
		r.Header.Set("x-ms-rename-source", src)
		w = httptest.NewRecorder()
		f.svc.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("rename source %q = %d; want %d", src, w.Code, want)
		}
	}

	// DFS Range read.
	r = httptest.NewRequest("GET", dst, nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Range", "bytes=0-1")
	w = httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	if w.Code != http.StatusPartialContent || w.Body.String() != "da" {
		t.Fatalf("dfs range = %d %q", w.Code, w.Body.String())
	}
}

// TestConcurrentDeltaCommitRace is the mechanism-level oracle for _delta_log
// atomicity: N writers race to create the SAME commit file with
// If-None-Match: * (put-if-absent). Exactly one must win (201) and the rest
// must lose (409) — the property that keeps concurrent Delta commits from
// silently clobbering each other. delta-rs assumes a single writer by
// default, so this direct race is a stronger signal than any delta-rs test.
func TestConcurrentDeltaCommitRace(t *testing.T) {
	f := newFixture(t)
	commit := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/t/_delta_log/00000000000000000001.json"

	const writers = 24
	var wg sync.WaitGroup
	codes := make([]int, writers)
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once to maximize contention
			body := []byte(`{"writer":` + strconv.Itoa(i) + `}`)
			w := f.doBlob("PUT", commit, f.token, body, map[string]string{"If-None-Match": "*"})
			codes[i] = w.Code
		}(i)
	}
	close(start)
	wg.Wait()

	created, conflict, winner := 0, 0, -1
	for i, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
			winner = i
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("writer %d got unexpected status %d", i, c)
		}
	}
	if created != 1 || conflict != writers-1 {
		t.Fatalf("race outcome: %d created, %d conflict; want 1 + %d", created, conflict, writers-1)
	}
	// The committed content is the winner's — no torn or overwritten write.
	g := f.doBlob("GET", commit, f.token, nil, nil)
	if g.Code != http.StatusOK || g.Body.String() != `{"writer":`+strconv.Itoa(winner)+`}` {
		t.Fatalf("committed content = %q; want the winner's (writer %d)", g.Body.String(), winner)
	}
}

// TestXMSRange: the Azure Blob SDK sends its range as x-ms-range (not the
// standard Range header) and requires a 206 + Content-Range in reply — found
// by driving the real azure-storage-blob client against the emulator.
func TestXMSRange(t *testing.T) {
	f := newFixture(t)
	path := "/" + f.ws.ID + "/" + f.it.ID + "/Files/blob.bin"
	if w := f.doBlob("PUT", path, f.token, []byte("hello world"), nil); w.Code != http.StatusCreated {
		t.Fatal(w.Code)
	}
	// x-ms-range yields 206 + Content-Range just like Range.
	g := f.doBlob("GET", path, f.token, nil, map[string]string{"x-ms-range": "bytes=0-4"})
	if g.Code != http.StatusPartialContent || g.Body.String() != "hello" ||
		g.Header().Get("Content-Range") != "bytes 0-4/11" {
		t.Fatalf("x-ms-range = %d %q %q", g.Code, g.Body.String(), g.Header().Get("Content-Range"))
	}
	// The SDK's whole-blob fetch (range covers past the end) → clamped 206.
	g = f.doBlob("GET", path, f.token, nil, map[string]string{"x-ms-range": "bytes=0-33554431"})
	if g.Code != http.StatusPartialContent || g.Body.String() != "hello world" ||
		g.Header().Get("Content-Range") != "bytes 0-10/11" {
		t.Fatalf("x-ms-range whole = %d %q %q", g.Code, g.Body.String(), g.Header().Get("Content-Range"))
	}
	// Standard Range still wins when both are present (defensive).
	g = f.doBlob("GET", path, f.token, nil, map[string]string{"Range": "bytes=6-10", "x-ms-range": "bytes=0-0"})
	if g.Body.String() != "world" {
		t.Fatalf("Range precedence = %q", g.Body.String())
	}
}

// TestABFSPutAppendFlush: the Hadoop ABFS driver writes files as PUT create
// → PUT ?action=append → PUT ?action=flush (not the PATCH the ADLS REST spec
// documents), and commits by writing a .tmp then renaming it. The flush PUT
// carries no body, so if it were treated as a create it would truncate the
// file to zero — which silently corrupted every Delta commit until the DFS
// PUT handler learned to route append/flush. Found by driving real Spark.
func TestABFSPutAppendFlush(t *testing.T) {
	f := newFixture(t)
	tmp := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/t/_delta_log/.0.json.tmp"
	final := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/t/_delta_log/00000000000000000000.json"
	commit := []byte(`{"protocol":{"minReaderVersion":1}}` + "\n" + `{"metaData":{"id":"x"}}`)

	// ABFS write sequence via PUT: create empty, append body, flush.
	if w := f.do("PUT", tmp+"?resource=file", f.token, nil); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	if w := f.do("PUT", tmp+"?action=append&position=0", f.token, commit); w.Code != http.StatusAccepted {
		t.Fatalf("append (PUT) = %d %s", w.Code, w.Body.Bytes())
	}
	flushURL := tmp + "?action=flush&position=" + strconv.Itoa(len(commit)) + "&close=true"
	if w := f.do("PUT", flushURL, f.token, nil); w.Code != http.StatusOK {
		t.Fatalf("flush (PUT, no body) = %d %s", w.Code, w.Body.Bytes())
	}
	// The flush must NOT have truncated the appended data.
	g := f.do("GET", tmp, f.token, nil)
	if g.Code != http.StatusOK || !bytes.Equal(g.Body.Bytes(), commit) {
		t.Fatalf("after flush: %d %q; want the appended commit", g.Code, g.Body.Bytes())
	}

	// Commit: rename .tmp → 0.json (the atomic Delta commit), content intact.
	r := httptest.NewRequest("PUT", final, nil)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("x-ms-rename-source", "/"+f.ws.ID+"/"+f.it.ID+"/Tables/t/_delta_log/.0.json.tmp")
	w := httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("commit rename = %d %s", w.Code, w.Body.Bytes())
	}
	g = f.do("GET", final, f.token, nil)
	if g.Code != http.StatusOK || !bytes.Equal(g.Body.Bytes(), commit) {
		t.Fatalf("committed 0.json = %d %q; want the full commit (this is the actionNotFound bug)", g.Code, g.Body.Bytes())
	}
	// HEAD reports the right length (Delta reads this to size the file).
	h := f.do("HEAD", final, f.token, nil)
	if h.Header().Get("Content-Length") != strconv.Itoa(len(commit)) {
		t.Fatalf("HEAD Content-Length = %q; want %d", h.Header().Get("Content-Length"), len(commit))
	}
}

func TestRequestTrace(t *testing.T) {
	t.Setenv("ONELAKE_TRACE", "1")
	f := newFixture(t)
	// A HEAD at the account level exercises the traced path end to end.
	if w := f.do("HEAD", "/", f.token, nil); w.Code != http.StatusOK {
		t.Fatalf("traced HEAD = %d", w.Code)
	}
	path := "/" + f.ws.ID + "/" + f.it.ID + "/Files/t.txt"
	f.do("PUT", path+"?resource=file", f.token, []byte("hi"))
	if w := f.do("GET", path, f.token, nil); w.Body.String() != "hi" {
		t.Fatalf("traced GET = %q", w.Body.String())
	}
}

// TestShortcutResolution: a read through a shortcut resolves into the target
// item, authorized against the TARGET workspace's RBAC (trusted-workspace-
// access). A deleted target dangles → 404.
func TestShortcutResolution(t *testing.T) {
	f := newFixture(t)
	// A second (target) workspace + lakehouse with a file, in a different
	// workspace than the source.
	tgtWS := &store.Workspace{DisplayName: "target-ws"}
	if err := f.st.CreateWorkspace(tgtWS, store.Principal{ID: "owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	tgt := &store.Item{WorkspaceID: tgtWS.ID, Type: "Lakehouse", DisplayName: "shared"}
	if err := f.st.CreateItem(tgt, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: tgtWS.ID, ItemID: tgt.ID, RelPath: "Files/data/x.txt", Content: []byte("shared-bytes"),
	}, false); err != nil {
		t.Fatal(err)
	}
	// A shortcut in the source item (f.it) → the target item's Files/data.
	sc := &store.Shortcut{
		ItemID: f.it.ID, Path: "Files", Name: "linked",
		TargetWorkspace: tgtWS.ID, TargetItem: tgt.ID, TargetPath: "Files/data",
	}
	if err := f.st.CreateShortcut(sc); err != nil {
		t.Fatal(err)
	}

	read := "/" + f.ws.ID + "/" + f.it.ID + "/Files/linked/x.txt"

	// admin-1 has no role on the target workspace → 403 (trusted-workspace
	// access is enforced against the TARGET).
	if w := f.do("GET", read, f.token, nil); w.Code != http.StatusForbidden {
		t.Fatalf("read without target access = %d; want 403", w.Code)
	}
	// Grant admin-1 Contributor on the target → the read resolves through.
	if err := f.st.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: tgtWS.ID, Principal: store.Principal{ID: "admin-1", Type: "ServicePrincipal"}, Role: store.RoleContributor,
	}); err != nil {
		t.Fatal(err)
	}
	g := f.do("GET", read, f.token, nil)
	if g.Code != http.StatusOK || g.Body.String() != "shared-bytes" {
		t.Fatalf("shortcut read = %d %q", g.Code, g.Body.String())
	}
	// HEAD through the shortcut works too.
	if w := f.do("HEAD", read, f.token, nil); w.Code != http.StatusOK || w.Header().Get("Content-Length") != "12" {
		t.Fatalf("shortcut HEAD = %d len=%q", w.Code, w.Header().Get("Content-Length"))
	}
	// A path under the shortcut that doesn't exist in the target → 404.
	if w := f.do("GET", "/"+f.ws.ID+"/"+f.it.ID+"/Files/linked/missing", f.token, nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing through shortcut = %d", w.Code)
	}
	// Delete the target item → the shortcut dangles → 404.
	if err := f.st.DeleteItem(tgtWS.ID, tgt.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.do("GET", read, f.token, nil); w.Code != http.StatusNotFound {
		t.Fatalf("dangling shortcut read = %d; want 404", w.Code)
	}
}

// TestBlobListStartFrom pins the offset parameter object_store uses for
// list_with_offset — and pins it as INCLUSIVE, which is where it differs from
// S3/GCP's exclusive start-after. object_store drops the first entry itself
// when it equals the offset, so a half-open range here loses a blob.
//
// The regression this guards is not cosmetic: ignoring startFrom made
// delta-rs's get_latest_version() see a log segment starting at version 0 when
// it asked for one starting at N, which the kernel rejects with "Invalid table
// version: N" — breaking every OPTIMIZE/VACUUM against OneLake while plain
// writes kept passing.
func TestBlobListStartFrom(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"Tables/t/_delta_log/00000000000000000000.json",
		"Tables/t/_delta_log/00000000000000000001.json",
		"Tables/t/_delta_log/00000000000000000002.json"} {
		if w := f.doBlob("PUT", "/"+f.ws.ID+"/"+f.it.ID+"/"+p, f.token, []byte("x"), nil); w.Code != http.StatusCreated {
			t.Fatalf("seed %s = %d", p, w.Code)
		}
	}
	type listing struct {
		Blobs struct {
			Blob []struct {
				Name string `xml:"Name"`
			} `xml:"Blob"`
		} `xml:"Blobs"`
	}
	base := "lake.Lakehouse/Tables/t/_delta_log/"
	get := func(query string) []string {
		w := f.doBlob("GET", "/"+f.ws.ID+"?comp=list&"+query, f.token, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("list %s = %d", query, w.Code)
		}
		var l listing
		if err := xml.Unmarshal(w.Body.Bytes(), &l); err != nil {
			t.Fatalf("list xml: %v", err)
		}
		var names []string
		for _, b := range l.Blobs.Blob {
			names = append(names, b.Name)
		}
		return names
	}

	// Inclusive: the offset itself must come back.
	got := get("prefix=" + base + "&startFrom=" + base + "00000000000000000001.json")
	want := []string{base + "00000000000000000001.json", base + "00000000000000000002.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startFrom inclusive: got %v want %v", got, want)
	}

	// An offset that matches no blob still bounds the range.
	got = get("prefix=" + base + "&startFrom=" + base + "00000000000000000001a")
	want = []string{base + "00000000000000000002.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("startFrom between keys: got %v want %v", got, want)
	}

	// Negative control: without the offset all three come back, so the
	// assertions above are testing the parameter and not the seed.
	if got = get("prefix=" + base); len(got) != 3 {
		t.Fatalf("no startFrom: got %v, want all 3", got)
	}

	// marker wins over startFrom: object_store sends startFrom only on the
	// first request and marker on every page after it. Exclusive, as marker is.
	got = get("prefix=" + base + "&marker=" + base + "00000000000000000001.json" +
		"&startFrom=" + base + "00000000000000000000.json")
	want = []string{base + "00000000000000000002.json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("marker beats startFrom: got %v want %v", got, want)
	}
}

// TestBlockStageIsBounded pins the memory bound on uncommitted blocks.
//
// Staged bytes were freed only on commit, so an upload that never committed held
// them for the life of the process. delta-rs makes that routine rather than
// exotic: a `_delta_log` commit that loses the put-if-absent race retries under a
// NEW blob key, so every losing attempt's staging was abandoned in place.
//
// The bound evicts whole abandoned blobs oldest-first. The blob being written is
// never evicted, and an evicted one fails its later commit with InvalidBlockList
// — the same answer Azure gives for expired blocks, and a loud one rather than a
// silently truncated file.
//
// `max` is set so the bound is exercised in kilobytes; proving it at 256 MiB
// would allocate a quarter of a gigabyte to watch the same branches.
func TestBlockStageIsBounded(t *testing.T) {
	st := &blockStage{max: 10 << 10} // 10 KiB
	chunk := make([]byte, 1<<10)     // 1 KiB per block

	const blobs = 24 // well past the bound
	for i := range blobs {
		st.put(fmt.Sprintf("item|Files/abandoned-%d", i), "AAAA", chunk)
	}

	st.mu.Lock()
	total, held := st.bytes, len(st.blocks)
	st.mu.Unlock()
	if total > st.limit() {
		t.Errorf("staged %d bytes, above the %d bound: abandoned uploads are not evicted",
			total, st.limit())
	}
	if held == 0 || held >= blobs {
		t.Errorf("holding %d of %d blobs; want some evicted and some kept", held, blobs)
	}

	// The blob written most recently survives, and still commits.
	last := fmt.Sprintf("item|Files/abandoned-%d", blobs-1)
	if _, ok := st.commit(last, []string{"AAAA"}); !ok {
		t.Error("the most recently staged blob was evicted; an in-progress upload must not be")
	}

	// An evicted blob fails its commit rather than committing a partial file.
	if _, ok := st.commit("item|Files/abandoned-0", []string{"AAAA"}); ok {
		t.Error("an evicted blob committed; it must fail with an unknown block id")
	}
}

// TestBlockStageKeepsTheBlobBeingWritten: when a SINGLE blob is itself over the
// bound there is nothing else to evict, and the upload in progress must not be
// destroyed to satisfy the limit. It stays, over budget, rather than being
// silently truncated mid-upload — the bound exists to stop abandoned uploads
// accumulating, not to break the one making progress.
func TestBlockStageKeepsTheBlobBeingWritten(t *testing.T) {
	st := &blockStage{max: 4 << 10} // 4 KiB
	big := make([]byte, 16<<10)     // one blob, four times the bound
	st.put("item|Files/one-big", "AAAA", big)

	st.mu.Lock()
	held, total := len(st.blocks), st.bytes
	st.mu.Unlock()
	if held != 1 || total != int64(len(big)) {
		t.Fatalf("holding %d blob(s) / %d bytes; the in-progress upload must survive",
			held, total)
	}
	if _, ok := st.commit("item|Files/one-big", []string{"AAAA"}); !ok {
		t.Error("the only staged blob was evicted; it had nothing to yield to")
	}
}

// TestBlockStageFreesOnCommit: the ordinary path still releases its bytes, so
// the bound is not doing the work a normal commit should.
func TestBlockStageFreesOnCommit(t *testing.T) {
	var st blockStage
	st.put("item|Files/a", "AAAA", make([]byte, 1024))
	st.put("item|Files/a", "BBBB", make([]byte, 2048))
	if _, ok := st.commit("item|Files/a", []string{"AAAA", "BBBB"}); !ok {
		t.Fatal("commit failed")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.bytes != 0 || len(st.blocks) != 0 {
		t.Fatalf("after commit: %d bytes across %d blobs; want nothing held",
			st.bytes, len(st.blocks))
	}
}

// TestBlobRefusesBelowContributor.
//
// The Blob dialect is what delta-rs speaks, so this gate is what stops a
// principal with read-only rights from writing Delta into a workspace. It had
// never executed: every blob test used the admin token, so the whole
// `RoleRank(role) < Contributor` branch was dark, and so was the case of a
// principal with no assignment at all.
func TestBlobRefusesBelowContributor(t *testing.T) {
	f := newFixture(t)
	path := "/" + f.ws.ID + "/" + f.it.ID + "/Tables/t/part-0.parquet"

	// Seed a blob as admin so the refusals below cannot pass merely because
	// there is nothing there to read.
	if w := f.doBlob("PUT", path, f.token, []byte("PAR1"), nil); w.Code != http.StatusCreated {
		t.Fatalf("seed put = %d %s", w.Code, w.Body.Bytes())
	}

	// A Viewer is below Contributor: OneLake API access requires ReadAll.
	if err := f.st.CreateRoleAssignment(&store.RoleAssignment{
		WorkspaceID: f.ws.ID,
		Principal:   store.Principal{ID: "viewer-1", Type: "User"},
		Role:        store.RoleViewer,
	}); err != nil {
		t.Fatal(err)
	}
	viewer := f.storageToken("viewer-1")

	for _, tc := range []struct{ method string }{{"GET"}, {"HEAD"}, {"PUT"}, {"DELETE"}} {
		var body []byte
		if tc.method == "PUT" {
			body = []byte("overwritten")
		}
		w := f.doBlob(tc.method, path, viewer, body, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s as Viewer = %d %s; want 403", tc.method, w.Code, w.Body.Bytes())
			continue
		}
		if code := w.Header().Get("x-ms-error-code"); code != "AuthorizationFailure" &&
			tc.method != "HEAD" {
			t.Errorf("%s as Viewer error code = %q; want AuthorizationFailure", tc.method, code)
		}
	}

	// A principal with NO assignment in the workspace is refused the same way.
	stranger := f.storageToken("stranger-1")
	if w := f.doBlob("GET", path, stranger, nil, nil); w.Code != http.StatusForbidden {
		t.Errorf("GET as an ungranted principal = %d; want 403", w.Code)
	}

	// The refusals changed nothing.
	if w := f.doBlob("GET", path, f.token, nil, nil); w.Code != http.StatusOK ||
		w.Body.String() != "PAR1" {
		t.Fatalf("content after refused writes = %d %q", w.Code, w.Body.String())
	}
}

// TestBlobHeadAndGetOnSomethingThatIsNotABlob: a directory is not a blob, and
// neither is a path that was never written. Answering 200 for a directory would
// hand object_store an empty body for a prefix it is about to list.
func TestBlobHeadAndGetOnSomethingThatIsNotABlob(t *testing.T) {
	f := newFixture(t)
	base := "/" + f.ws.ID + "/" + f.it.ID
	if w := f.doBlob("PUT", base+"/Tables/t/part-0.parquet", f.token, []byte("PAR1"), nil); w.Code != http.StatusCreated {
		t.Fatalf("seed put = %d", w.Code)
	}

	// An EXPLICIT directory row, which is what a mkdir through the DFS surface
	// leaves behind. The implied directory below is a different case: it has no
	// row at all, so it 404s on the lookup rather than on the IsDir check, and
	// a test that used only that one would never reach the IsDir branch.
	if err := f.st.CreateOneLakePath(&store.OneLakePath{
		WorkspaceID: f.ws.ID, ItemID: f.it.ID, RelPath: "Files/adir",
		IsDir: true, Content: []byte{},
	}, false); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		base + "/Files/adir",             // an explicit directory row
		base + "/Tables/t",               // a directory implied by the blob above
		base + "/Tables/t/never-written", // nothing there
	} {
		for _, method := range []string{"HEAD", "GET"} {
			w := f.doBlob(method, target, f.token, nil, nil)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d %s; want 404", method, target, w.Code, w.Body.Bytes())
			}
		}
	}
}
