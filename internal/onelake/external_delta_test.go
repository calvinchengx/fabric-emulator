package onelake

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func newDeltaTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

const s3ListXML = `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>bucket</Name>
  <IsTruncated>%s</IsTruncated>
  <Contents><Key>orders/_delta_log/00000000000000000000.json</Key></Contents>
  <Contents><Key>orders/_delta_log/00000000000000000001.json</Key></Contents>
  <Contents><Key>orders/_delta_log/</Key></Contents>
  <Contents><Key>orders/_delta_log/_commit.crc</Key></Contents>
</ListBucketResult>`

func TestExternalDeltaCommitsListsAndSignsS3(t *testing.T) {
	var gotAuth, gotQuery string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		_, _ = fmt.Fprintf(w, s3ListXML, "false")
	}))
	defer target.Close()
	st := newDeltaTestStore(t)
	conn := &store.Connection{DisplayName: "s3", CredentialsJSON: `{"credentialType":"Basic","username":"AKIA","password":"secret"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil)
	sc := &store.Shortcut{Name: "orders", TargetType: "AmazonS3", TargetLocation: target.URL, TargetPath: "orders", ConnectionID: conn.ID}

	got, err := svc.ExternalDeltaCommits(sc)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"00000000000000000000.json", "00000000000000000001.json"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("commits = %v, want %v — the folder marker and the .crc file must not appear", got, want)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIA/") {
		t.Errorf("not signed with the shortcut's access key: %q", gotAuth)
	}
	if !strings.Contains(gotQuery, "prefix=orders%2F_delta_log%2F") || !strings.Contains(gotQuery, "list-type=2") {
		t.Errorf("query = %q, want list-type=2 and the _delta_log prefix", gotQuery)
	}
}

func TestExternalDeltaCommitsRefusesATruncatedS3Listing(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, s3ListXML, "true")
	}))
	defer target.Close()
	st := newDeltaTestStore(t)
	conn := &store.Connection{DisplayName: "s3", CredentialsJSON: `{"credentialType":"Basic","username":"AKIA","password":"secret"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil)
	sc := &store.Shortcut{Name: "orders", TargetType: "AmazonS3", TargetLocation: target.URL, TargetPath: "orders", ConnectionID: conn.ID}
	if _, err := svc.ExternalDeltaCommits(sc); err == nil || !strings.Contains(err.Error(), "pagination is not supported") {
		t.Fatalf("truncated listing: %v, want a refusal naming pagination", err)
	}
}

func TestExternalDeltaCommitsRequiresAnAccessKeyForS3(t *testing.T) {
	st := newDeltaTestStore(t)
	conn := &store.Connection{DisplayName: "sas", CredentialsJSON: `{"credentialType":"SharedAccessSignature","token":"?sig=x"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil)
	sc := &store.Shortcut{Name: "orders", TargetType: "AmazonS3", TargetLocation: "https://bucket.s3.example", TargetPath: "orders", ConnectionID: conn.ID}
	if _, err := svc.ExternalDeltaCommits(sc); err == nil || !strings.Contains(err.Error(), "Basic") {
		t.Fatalf("wrong credential type: %v, want a refusal naming Basic", err)
	}
}

func blobListXML(nextMarker string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<EnumerationResults>
  <Blobs>
    <Blob><Name>orders/_delta_log/00000000000000000000.json</Name></Blob>
    <Blob><Name>orders/_delta_log/00000000000000000001.json</Name></Blob>
    <Blob><Name>orders/_delta_log/_commit.crc</Name></Blob>
  </Blobs>
  <NextMarker>%s</NextMarker>
</EnumerationResults>`, nextMarker)
}

func TestExternalDeltaCommitsListsBlobWithTheSASQuery(t *testing.T) {
	var gotQuery string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(blobListXML("")))
	}))
	defer target.Close()
	st := newDeltaTestStore(t)
	conn := &store.Connection{DisplayName: "sas", CredentialsJSON: `{"credentialType":"SharedAccessSignature","token":"?sig=abc123&sv=2024"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil)
	sc := &store.Shortcut{Name: "orders", TargetType: "ADLSGen2", TargetLocation: target.URL, TargetPath: "orders", ConnectionID: conn.ID}

	got, err := svc.ExternalDeltaCommits(sc)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"00000000000000000000.json", "00000000000000000001.json"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("commits = %v, want %v", got, want)
	}
	if !strings.Contains(gotQuery, "sig=abc123") || !strings.Contains(gotQuery, "restype=container") || !strings.Contains(gotQuery, "comp=list") {
		t.Errorf("query = %q, want the SAS signature and the List Blobs verbs", gotQuery)
	}
}

func TestExternalDeltaCommitsRefusesATruncatedBlobListing(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(blobListXML("marker-1")))
	}))
	defer target.Close()
	st := newDeltaTestStore(t)
	conn := &store.Connection{DisplayName: "anon", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil)
	sc := &store.Shortcut{Name: "orders", TargetType: "Dataverse", TargetLocation: target.URL, TargetPath: "orders", ConnectionID: conn.ID}
	if _, err := svc.ExternalDeltaCommits(sc); err == nil || !strings.Contains(err.Error(), "pagination is not supported") {
		t.Fatalf("truncated listing: %v, want a refusal naming pagination", err)
	}
}

func TestExternalDeltaCommitsSurfacesAListingFailure(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	defer target.Close()
	st := newDeltaTestStore(t)
	conn := &store.Connection{DisplayName: "anon", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil)
	sc := &store.Shortcut{Name: "orders", TargetType: "ADLSGen2", TargetLocation: target.URL, TargetPath: "orders", ConnectionID: conn.ID}
	if _, err := svc.ExternalDeltaCommits(sc); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("listing refused by the target: %v, want the status surfaced", err)
	}
}

func TestExternalReadFileReadsThroughAndWrapsAFailure(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/orders/part-0.parquet" {
			_, _ = w.Write([]byte("parquet-bytes"))
			return
		}
		http.NotFound(w, r)
	}))
	defer target.Close()
	st := newDeltaTestStore(t)
	conn := &store.Connection{DisplayName: "anon", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil)
	sc := &store.Shortcut{Name: "orders", TargetType: "ADLSGen2", TargetLocation: target.URL, TargetPath: "orders", ConnectionID: conn.ID}

	got, err := svc.ExternalReadFile(sc, "part-0.parquet")
	if err != nil || string(got) != "parquet-bytes" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := svc.ExternalReadFile(sc, "missing.parquet"); err == nil || !strings.Contains(err.Error(), "ExternalTargetError") {
		t.Fatalf("a missing file: %v, want the wrapped dfsError code", err)
	}
}
