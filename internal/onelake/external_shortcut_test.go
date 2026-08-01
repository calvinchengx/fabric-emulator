package onelake

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/clock"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestResolveExternalShortcutCredentialsAndErrors(t *testing.T) {
	var gotPath, gotAuth, gotKey, gotQuery string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotKey, gotQuery = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.URL.RawQuery
		if r.URL.Path == "/fail" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("external-data"))
	}))
	defer target.Close()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil)

	// These four exercise the plain HTTP read-through, so the target type is
	// ADLSGen2: an AmazonS3 target with Basic credentials now means SigV4
	// (see TestResolveExternalShortcutSignsS3WithSigV4).
	tests := []struct {
		name, creds, wantAuth, wantKey, wantQuery string
	}{
		{"anonymous", `{"credentialType":"Anonymous"}`, "", "", ""},
		{"basic", `{"credentialType":"Basic","username":"u","password":"p"}`, "Basic dTpw", "", ""},
		{"key", `{"credentialType":"Key","key":"secret"}`, "", "secret", ""},
		{"sas", `{"credentialType":"SharedAccessSignature","token":"?sig=abc"}`, "", "", "sig=abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := &store.Connection{DisplayName: tc.name, CredentialsJSON: tc.creds}
			if err := st.CreateConnection(conn); err != nil {
				t.Fatal(err)
			}
			sc := &store.Shortcut{TargetType: "ADLSGen2", TargetLocation: target.URL, TargetPath: "bucket", ConnectionID: conn.ID}
			p, derr := svc.resolveExternal(sc, "folder/file.txt")
			if derr != nil || string(p.Content) != "external-data" {
				t.Fatalf("result = %v, error = %+v", p, derr)
			}
			if gotPath != "/bucket/folder/file.txt" || gotAuth != tc.wantAuth || gotKey != tc.wantKey || gotQuery != tc.wantQuery {
				t.Fatalf("request path=%q auth=%q key=%q query=%q", gotPath, gotAuth, gotKey, gotQuery)
			}
		})
	}

	missing := &store.Shortcut{TargetLocation: target.URL, ConnectionID: "missing"}
	if _, derr := svc.resolveExternal(missing, "x"); derr == nil || derr.code != "ExternalConnectionNotFound" {
		t.Fatalf("missing = %+v", derr)
	}
	unsupported := &store.Connection{DisplayName: "sp", CredentialsJSON: `{"credentialType":"ServicePrincipal"}`}
	if err := st.CreateConnection(unsupported); err != nil {
		t.Fatal(err)
	}
	if _, derr := svc.resolveExternal(&store.Shortcut{TargetLocation: target.URL, ConnectionID: unsupported.ID}, "x"); derr == nil || derr.code != "ExternalCredentialUnsupported" {
		t.Fatalf("unsupported = %+v", derr)
	}
	anon := &store.Connection{DisplayName: "failure", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(anon); err != nil {
		t.Fatal(err)
	}
	if _, derr := svc.resolveExternal(&store.Shortcut{TargetLocation: target.URL, ConnectionID: anon.ID}, "fail"); derr == nil || derr.code != "ExternalTargetError" {
		t.Fatalf("http failure = %+v", derr)
	}
}

func TestResolveReadExposesEmptyManagedFolders(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws := &store.Workspace{DisplayName: "w"}
	if err := st.CreateWorkspace(ws, store.Principal{ID: "owner", Type: "User"}); err != nil {
		t.Fatal(err)
	}
	lake := &store.Item{WorkspaceID: ws.ID, Type: "Lakehouse", DisplayName: "lake"}
	if err := st.CreateItem(lake, nil); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil)
	for _, name := range []string{"Files", "Tables"} {
		p, derr := svc.resolveRead(lake.ID, name, "owner")
		if derr != nil || !p.IsDir || p.RelPath != name {
			t.Fatalf("%s = %+v, error %+v", name, p, derr)
		}
	}
}

// An S3 shortcut configured with an Access Key ID and Secret Access Key —
// what Fabric's own shortcut dialog collects — is signed with SigV4. Header
// credentials are not enough for a real S3 endpoint.
func TestResolveExternalShortcutSignsS3WithSigV4(t *testing.T) {
	var gotAuth, gotSHA, gotDate, gotToken string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSHA = r.Header.Get("x-amz-content-sha256")
		gotDate = r.Header.Get("X-Amz-Date")
		gotToken = r.Header.Get("x-amz-security-token")
		_, _ = w.Write([]byte("s3-object-bytes"))
	}))
	defer target.Close()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil)

	// Fabric's S3 connector collects an Access Key Id and a Secret Access Key;
	// the REST reference has no AccessKey credential type, so they travel as
	// Basic's username/password.
	conn := &store.Connection{DisplayName: "s3", CredentialsJSON: `{
		"credentialType":"Basic",
		"username":"AKIAIOSFODNN7EXAMPLE",
		"password":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	sc := &store.Shortcut{TargetType: "AmazonS3", TargetLocation: target.URL,
		TargetPath: "bucket", ConnectionID: conn.ID}
	p, derr := svc.resolveExternal(sc, "folder/file.txt")
	if derr != nil || string(p.Content) != "s3-object-bytes" {
		t.Fatalf("result = %v, error = %+v", p, derr)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/") {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date") {
		t.Fatalf("unexpected signed headers: %q", gotAuth)
	}
	if gotSHA == "" || gotDate == "" {
		t.Fatalf("sha=%q date=%q", gotSHA, gotDate)
	}
	// No STS session token is sent: Fabric's S3 connector does not collect one.
	if gotToken != "" {
		t.Fatalf("unexpected session token %q", gotToken)
	}
	// The Basic credential must NOT also be sent as an Authorization header —
	// it is key material for the signature, not a header credential.
	if strings.Contains(gotAuth, "Basic ") {
		t.Fatalf("basic credential leaked: %q", gotAuth)
	}
}

// Writing through an ADLS Gen2 shortcut must reach the target, and writing
// through an S3 shortcut must not be attempted at all: Fabric documents S3
// shortcuts as read-only "regardless of the user's permissions".
func TestExternalShortcutWriteAndDelete(t *testing.T) {
	var gotMethod, gotPath, gotBlobType string
	var gotBody []byte
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBlobType = r.Header.Get("x-ms-blob-type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer target.Close()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil)

	conn := &store.Connection{DisplayName: "adls", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	adls := &store.Shortcut{TargetType: "ADLSGen2", TargetLocation: target.URL,
		TargetPath: "container", ConnectionID: conn.ID}

	// Write reaches the target, as a whole-file block-blob PUT.
	if derr := svc.writeExternal(adls, "dir/out.csv", []byte("a,b\n1,2\n")); derr != nil {
		t.Fatalf("write failed: %+v", derr)
	}
	if gotMethod != http.MethodPut || gotPath != "/container/dir/out.csv" {
		t.Fatalf("upstream got %s %s", gotMethod, gotPath)
	}
	if string(gotBody) != "a,b\n1,2\n" {
		t.Fatalf("upstream body = %q", gotBody)
	}
	if gotBlobType != "BlockBlob" {
		t.Fatalf("x-ms-blob-type = %q", gotBlobType)
	}

	// Delete reaches the target too.
	if derr := svc.deleteExternal(adls, "dir/out.csv"); derr != nil {
		t.Fatalf("delete failed: %+v", derr)
	}
	if gotMethod != http.MethodDelete {
		t.Fatalf("upstream method = %s, want DELETE", gotMethod)
	}

	// S3 is read-only by specification — neither write nor delete may be
	// forwarded, and the refusal must not depend on the target's permissions.
	s3 := &store.Shortcut{TargetType: "AmazonS3", TargetLocation: target.URL,
		TargetPath: "bucket", ConnectionID: conn.ID}
	gotMethod = ""
	if derr := svc.writeExternal(s3, "x.csv", []byte("nope")); derr == nil {
		t.Fatal("write through an S3 shortcut was allowed; Fabric documents them as read-only")
	}
	if derr := svc.deleteExternal(s3, "x.csv"); derr == nil {
		t.Fatal("delete through an S3 shortcut was allowed")
	}
	if gotMethod != "" {
		t.Fatalf("an S3 write reached the target (%s) — it must be refused locally", gotMethod)
	}
}

// An upstream refusal must surface as an error, not a silent success.
func TestExternalShortcutWriteSurfacesUpstreamFailure(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer target.Close()
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil)
	conn := &store.Connection{DisplayName: "adls", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	sc := &store.Shortcut{TargetType: "ADLSGen2", TargetLocation: target.URL,
		TargetPath: "container", ConnectionID: conn.ID}
	derr := svc.writeExternal(sc, "x.csv", []byte("data"))
	if derr == nil || derr.code != "ExternalTargetError" {
		t.Fatalf("a 403 from the target produced %+v, want ExternalTargetError", derr)
	}
}

// The signer's region is overridable: a bucket outside us-east-1 rejects a
// signature scoped to the wrong region with SignatureDoesNotMatch.
func TestS3RegionOverride(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil)
	if got := svc.S3Region(); got != "us-east-1" {
		t.Fatalf("default region = %q", got)
	}
	t.Setenv("FABRIC_S3_REGION", "eu-west-2")
	if got := svc.S3Region(); got != "eu-west-2" {
		t.Fatalf("override region = %q", got)
	}
}

// Every failure mode of an external write must surface as an error. A write
// that cannot be delivered must never look like a success to the caller.
func TestExternalWriteAndDeleteFailurePaths(t *testing.T) {
	st, err := store.Open("", clock.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(st, nil)

	// A shortcut whose connection no longer exists.
	orphan := &store.Shortcut{TargetType: "ADLSGen2", TargetLocation: "http://127.0.0.1:1",
		TargetPath: "c", ConnectionID: "missing"}
	if derr := svc.writeExternal(orphan, "x", []byte("d")); derr == nil ||
		derr.code != "ExternalConnectionNotFound" {
		t.Fatalf("missing connection produced %+v", derr)
	}
	if derr := svc.deleteExternal(orphan, "x"); derr == nil ||
		derr.code != "ExternalConnectionNotFound" {
		t.Fatalf("missing connection on delete produced %+v", derr)
	}

	conn := &store.Connection{DisplayName: "anon", CredentialsJSON: `{"credentialType":"Anonymous"}`}
	if err := st.CreateConnection(conn); err != nil {
		t.Fatal(err)
	}
	// An unreachable target is a transport failure, not a silent success.
	dead := &store.Shortcut{TargetType: "ADLSGen2", TargetLocation: "http://127.0.0.1:1",
		TargetPath: "c", ConnectionID: conn.ID}
	if derr := svc.writeExternal(dead, "x", []byte("d")); derr == nil ||
		derr.code != "ExternalTargetUnavailable" {
		t.Fatalf("unreachable target produced %+v", derr)
	}
	if derr := svc.deleteExternal(dead, "x"); derr == nil ||
		derr.code != "ExternalTargetUnavailable" {
		t.Fatalf("unreachable target on delete produced %+v", derr)
	}
	// An unparseable target location.
	bad := &store.Shortcut{TargetType: "ADLSGen2", TargetLocation: "://nope",
		TargetPath: "c", ConnectionID: conn.ID}
	if derr := svc.writeExternal(bad, "x", []byte("d")); derr == nil ||
		derr.code != "ExternalTargetInvalid" {
		t.Fatalf("invalid location produced %+v", derr)
	}
	// A credential type with no HTTP mapping.
	sp := &store.Connection{DisplayName: "sp", CredentialsJSON: `{"credentialType":"ServicePrincipal"}`}
	if err := st.CreateConnection(sp); err != nil {
		t.Fatal(err)
	}
	unsupported := &store.Shortcut{TargetType: "ADLSGen2", TargetLocation: "http://127.0.0.1:1",
		TargetPath: "c", ConnectionID: sp.ID}
	if derr := svc.writeExternal(unsupported, "x", []byte("d")); derr == nil ||
		derr.code != "ExternalCredentialUnsupported" {
		t.Fatalf("unsupported credential produced %+v", derr)
	}
}
