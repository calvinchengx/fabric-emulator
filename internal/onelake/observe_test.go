package onelake

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// TestTableRootCollapsesManagedTablePaths: a Delta write touches many files
// under one table; lineage is about the table.
func TestTableRootCollapsesManagedTablePaths(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Tables/orders/part-0.parquet", "Tables/orders"},
		{"Tables/orders/_delta_log/00000000000000000000.json", "Tables/orders"},
		{"Tables/orders", "Tables/orders"},
		{"/Tables/orders/", "Tables/orders"},
		{"Files/landing/day1.csv", "Files/landing/day1.csv"},
		{"Tables", "Tables"},
	} {
		if got := TableRoot(tc.in); got != tc.want {
			t.Errorf("TableRoot(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// doTagged drives ServeHTTP with the notebook cell headers a runtime sets.
func (f *fixture) doTagged(method, target, jobID string, cell int, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	var rd *strings.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, rd)
	r.Header.Set("Authorization", "Bearer "+f.token)
	if jobID != "" {
		r.Header.Set(HeaderJobID, jobID)
		r.Header.Set(HeaderCellIndex, fmt.Sprint(cell))
	}
	w := httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	return w
}

// TestObserveRecordsTaggedIO: the storage layer attributes real requests to the
// cell that made them, and leaves untagged traffic alone.
func TestObserveRecordsTaggedIO(t *testing.T) {
	f := newFixture(t)
	job := &store.JobInstance{ItemID: f.it.ID, JobType: "RunNotebook"}
	if err := f.st.CreateJobInstance(job); err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("https://onelake.dfs.fabric.microsoft.com/%s/%s", f.ws.ID, f.it.ID)

	// A tagged write, then a tagged read of a different table.
	if w := f.doTagged("PUT", base+"/Tables/silver/part-0.parquet?resource=file", job.ID, 2, nil); w.Code >= 400 {
		t.Fatalf("tagged write = %d %s", w.Code, w.Body.Bytes())
	}
	if w := f.doTagged("GET", base+"/Tables/bronze/part-0.parquet", job.ID, 2, nil); w.Code >= 500 {
		t.Fatalf("tagged read = %d", w.Code)
	}
	// Untagged traffic must not be attributed to anything.
	if w := f.doTagged("PUT", base+"/Files/untagged.txt?resource=file", "", 0, nil); w.Code >= 400 {
		t.Fatalf("untagged write = %d", w.Code)
	}

	got, err := f.st.ListNotebookAccesses(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 observed accesses (untagged excluded), got %d: %+v", len(got), got)
	}
	byPath := map[string]*store.NotebookAccess{}
	for _, a := range got {
		byPath[a.Path] = a
		if a.CellIndex != 2 {
			t.Fatalf("cell attribution lost: %+v", a)
		}
	}
	// Both collapse to the table root, and direction comes from the method.
	if w := byPath["Tables/silver"]; w == nil || w.Direction != store.AccessWrite {
		t.Fatalf("write not observed at table root: %+v", got)
	}
	if rd := byPath["Tables/bronze"]; rd == nil || rd.Direction != store.AccessRead {
		t.Fatalf("read not observed at table root: %+v", got)
	}
}

// mintAttributed signs a Storage token carrying notebook attribution as extra
// claims — what entra's forge produces for the Spark agent, whose Rust
// object_store client cannot set request headers.
func mintAttributed(t *testing.T, f *fixture, oid, jobID string, cell int) string {
	t.Helper()
	head := map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test-key"}
	claims := map[string]any{
		"iss": testIssuer, "aud": StorageAudience[0], "exp": int64(2000), "oid": oid,
		"fabric_job_id": jobID, "fabric_cell_index": fmt.Sprint(cell),
	}
	signing := b64(t, head) + "." + b64(t, claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// TestObserveAttributesFromTokenClaims: engines that cannot set headers carry
// the same attribution inside the bearer. This is the Spark/delta-rs path —
// Rust object_store takes credentials, not HTTP options.
func TestObserveAttributesFromTokenClaims(t *testing.T) {
	f := newFixture(t)
	job := &store.JobInstance{ItemID: f.it.ID, JobType: "RunNotebook"}
	if err := f.st.CreateJobInstance(job); err != nil {
		t.Fatal(err)
	}
	tok := mintAttributed(t, f, "admin-1", job.ID, 4)
	target := fmt.Sprintf("https://onelake.dfs.fabric.microsoft.com/%s/%s/Tables/gold/part-0.parquet?resource=file",
		f.ws.ID, f.it.ID)

	r := httptest.NewRequest("PUT", target, strings.NewReader(""))
	r.Header.Set("Authorization", "Bearer "+tok) // no x-ms-fabric-* headers at all
	w := httptest.NewRecorder()
	f.svc.ServeHTTP(w, r)
	if w.Code >= 400 {
		t.Fatalf("write = %d %s", w.Code, w.Body.Bytes())
	}

	got, err := f.st.ListNotebookAccesses(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("token-carried attribution not observed: %+v", got)
	}
	if got[0].CellIndex != 4 || got[0].Path != "Tables/gold" || got[0].Direction != store.AccessWrite {
		t.Fatalf("observed access = %+v", got[0])
	}
}

// TestAttributionFromTokenIgnoresBadBearers: attribution must never be taken
// from an unvalidated or unattributed token — it is evidence, so a forged or
// plain bearer must contribute nothing.
func TestAttributionFromTokenIgnoresBadBearers(t *testing.T) {
	f := newFixture(t)
	job := &store.JobInstance{ItemID: f.it.ID, JobType: "RunNotebook"}
	if err := f.st.CreateJobInstance(job); err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("https://onelake.dfs.fabric.microsoft.com/%s/%s/Files/x.txt?resource=file", f.ws.ID, f.it.ID)
	for _, tc := range []struct{ name, authz string }{
		{"plain token, no attribution claims", "Bearer " + f.token},
		{"garbage bearer", "Bearer not-a-jwt"},
		{"no scheme", f.token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("PUT", target, strings.NewReader(""))
			r.Header.Set("Authorization", tc.authz)
			f.svc.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
	got, err := f.st.ListNotebookAccesses(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("nothing should be attributed: %+v", got)
	}
}
