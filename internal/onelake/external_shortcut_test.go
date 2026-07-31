package onelake

import (
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
			sc := &store.Shortcut{TargetType: "AmazonS3", TargetLocation: target.URL, TargetPath: "bucket", ConnectionID: conn.ID}
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

	conn := &store.Connection{DisplayName: "s3", CredentialsJSON: `{
		"accessKeyID":"AKIAIOSFODNN7EXAMPLE",
		"secretAccessKey":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"sessionToken":"SESSION"}`}
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
	if !strings.Contains(gotAuth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token") {
		t.Fatalf("session token not signed: %q", gotAuth)
	}
	if gotSHA == "" || gotDate == "" || gotToken != "SESSION" {
		t.Fatalf("sha=%q date=%q token=%q", gotSHA, gotDate, gotToken)
	}
	// No basic/api-key credential leaks alongside the signature.
	if strings.Contains(gotAuth, "Basic") {
		t.Fatalf("basic credential leaked: %q", gotAuth)
	}
}
