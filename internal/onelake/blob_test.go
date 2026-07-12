package onelake

// R0 tests: the Blob dialect (what delta-rs/object_store speaks), Range
// reads, rename, and the put-if-absent conditional that guards Delta's
// _delta_log commits.

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
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
		"/short":                                http.StatusBadRequest,
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
