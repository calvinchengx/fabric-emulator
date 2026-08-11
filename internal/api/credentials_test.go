package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/akv"
	"github.com/calvinchengx/fabric-emulator/internal/entra"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// tokenEntra fakes entra's client-credentials endpoint: accepts only the
// given secret.
func tokenEntra(t *testing.T, wantSecret string) *entra.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostFormValue("client_secret") != wantSecret {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"x","token_type":"Bearer"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return entra.New(srv.URL, false, srv.Client())
}

func TestConnectionCredentialValidation(t *testing.T) {
	a, _ := newAPI(t)

	// Per-type required fields.
	bad := []string{
		`{"displayName":"c","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"Basic","username":"u"}}}`,
		`{"displayName":"c","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"ServicePrincipal","tenantId":"t"}}}`,
		`{"displayName":"c","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"WorkspaceIdentity"}}}`,
		`{"displayName":"c","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"Key"}}}`,
		`{"displayName":"c","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"SharedAccessSignature"}}}`,
		`{"displayName":"c","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"Kerberos"}}}`,
	}
	for _, body := range bad {
		if w := do(a.createConnection, admin, "POST", body, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400", body, w.Code)
		}
	}

	// Basic and Anonymous succeed; the response and every read expose the
	// credentialType but never the secret material.
	w := do(a.createConnection, admin, "POST",
		`{"displayName":"db","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"connectionEncryption":"NotEncrypted","credentials":{"credentialType":"Basic","username":"sa","password":"hunter2"}}}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("basic create = %d %s", w.Code, w.Body.Bytes())
	}
	for _, out := range []string{w.Body.String()} {
		if strings.Contains(out, "hunter2") || strings.Contains(out, `"password"`) {
			t.Fatalf("secret echoed in create response: %s", out)
		}
	}
	var created struct {
		ID                string
		CredentialDetails struct{ CredentialType, ConnectionEncryption string }
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.CredentialDetails.CredentialType != "Basic" || created.CredentialDetails.ConnectionEncryption != "NotEncrypted" {
		t.Fatalf("credentialDetails = %+v", created.CredentialDetails)
	}
	w = do(a.listConnections, admin, "GET", "", nil)
	if strings.Contains(w.Body.String(), "hunter2") {
		t.Fatalf("secret echoed in list: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"credentialType":"Basic"`) {
		t.Fatalf("list missing credentialType: %s", w.Body.String())
	}

	if w := do(a.createConnection, admin, "POST",
		`{"displayName":"open","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"Anonymous"}}}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("anonymous create = %d", w.Code)
	}
	// Connections without credentialDetails still work (git provider style).
	if w := do(a.createConnection, admin, "POST", `{"displayName":"plain","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]}}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("plain create = %d", w.Code)
	}
}

func TestServicePrincipalProbe(t *testing.T) {
	a, _ := newAPI(t)
	spBody := func(secret string) string {
		return `{"displayName":"sp","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"ServicePrincipal","tenantId":"tid","servicePrincipalClientId":"cid","servicePrincipalSecret":"` + secret + `"}}}`
	}

	// No entra configured → 503 unless skipTestConnection.
	if w := do(a.createConnection, admin, "POST", spBody("s"), nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("sp with nil entra = %d; want 503", w.Code)
	}
	skip := `{"displayName":"sp","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"skipTestConnection":true,"credentials":{"credentialType":"ServicePrincipal","tenantId":"t","servicePrincipalClientId":"c","servicePrincipalSecret":"s"}}}`
	if w := do(a.createConnection, admin, "POST", skip, nil); w.Code != http.StatusCreated {
		t.Fatalf("sp skipTestConnection = %d", w.Code)
	}

	// The probe: right secret passes, wrong secret is a 400 TestConnectionFailed.
	a.Entra = tokenEntra(t, "right-secret")
	if w := do(a.createConnection, admin, "POST", spBody("right-secret"), nil); w.Code != http.StatusCreated {
		t.Fatalf("valid sp = %d %s", w.Code, w.Body.Bytes())
	}
	w := do(a.createConnection, admin, "POST", spBody("wrong-secret"), nil)
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "TestConnectionFailed" {
		t.Fatalf("invalid sp = %d %s", w.Code, w.Body.Bytes())
	}
}

func TestWorkspaceIdentityCredential(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	body := `{"displayName":"wi","connectionDetails":{"type":"WebForPipeline","creationMethod":"WebForPipeline.Contents","parameters":[{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},"credentialDetails":{"credentials":{"credentialType":"WorkspaceIdentity","workspaceId":"` + ws.ID + `"}}}`

	// No provisioned identity → 400.
	if w := do(a.createConnection, admin, "POST", body, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("wi without identity = %d", w.Code)
	}
	if err := st.SetWorkspaceIdentity(&store.WorkspaceIdentity{WorkspaceID: ws.ID, IdentityID: "sp", AppID: "app"}); err != nil {
		t.Fatal(err)
	}
	if w := do(a.createConnection, admin, "POST", body, nil); w.Code != http.StatusCreated {
		t.Fatalf("wi with identity = %d %s", w.Code, w.Body.Bytes())
	}
}

// wiEntra fakes entra's workspace-identity token mint.
func wiEntra(t *testing.T, failMint bool) *entra.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fabric/workspaceidentities/{id}/token", func(w http.ResponseWriter, r *http.Request) {
		if failMint {
			http.Error(w, `{"error":"identity_not_ready"}`, http.StatusConflict)
			return
		}
		if r.URL.Query().Get("resource") != "https://vault.azure.net" {
			t.Errorf("mint resource = %q", r.URL.Query().Get("resource"))
		}
		_, _ = w.Write([]byte(`{"access_token":"wi-vault-token"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return entra.New(srv.URL, false, srv.Client())
}

// vaultFor returns a fake vault URL serving one secret, asserting the bearer.
func vaultFor(t *testing.T, name, wantBearer string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantBearer {
			http.Error(w, `{"error":{"code":"Unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if r.PathValue("name") != name {
			http.Error(w, `{"error":{"code":"SecretNotFound"}}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"value":"s3cret"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// A credential resolved from Key Vault, in the shape a tenant accepts.
//
// THIS TEST USED TO ENCODE AN INVENTED CONTRACT, and passing it was worthless.
// It sent `credentialType: "AzureKeyVaultReference"` with `vaultUri` and
// `workspaceId`. Measured against a real Fabric trial on 2026-08-11, that type
// does not exist: the enum is the ten values in credentialTypes, and a
// vault-backed credential is the OWNING type carrying a KeyVaultSecretReference
// — `credentialType: "Key"` with `keyReference {connectionId, secretName}`.
//
// The difference that matters is `connectionId`: the reference names a
// CONNECTION to the vault, so the vault has to exist as a connection before
// anything can point at it. `vaultUri` required nothing to exist, which is why
// the old shape could look valid while addressing nothing.
func TestKeyCredentialResolvedFromKeyVault(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	if err := st.SetWorkspaceIdentity(&store.WorkspaceIdentity{
		WorkspaceID: ws.ID, IdentityID: "sp-1", AppID: "app-1"}); err != nil {
		t.Fatal(err)
	}

	vaultConn := func(account string) string {
		return `{"displayName":"kv","connectionDetails":{"type":"AzureKeyVault",` +
			`"creationMethod":"AzureKeyVault.Actions","parameters":[` +
			`{"dataType":"Text","name":"accountName","value":"` + account + `"}]},` +
			`"credentialDetails":{"skipTestConnection":true,"credentials":` +
			`{"credentialType":"WorkspaceIdentity","workspaceId":"` + ws.ID + `"}}}`
	}
	keyRef := func(connID, secret string) string {
		return `{"displayName":"pos","connectionDetails":{"type":"WebForPipeline",` +
			`"creationMethod":"WebForPipeline.Contents","parameters":[` +
			`{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},` +
			`"credentialDetails":{"credentials":{"credentialType":"Key",` +
			`"keyReference":{"connectionId":"` + connID + `","secretName":"` + secret + `"}}}}`
	}

	// The invented type is refused BY NAME, and the message says what to write
	// instead — an "unknown credentialType" would leave the reader where the
	// tenant's own "invalid input" left us.
	w := do(a.createConnection, admin, "POST",
		`{"displayName":"akv","connectionDetails":{"type":"WebForPipeline",`+
			`"creationMethod":"WebForPipeline.Contents","parameters":[`+
			`{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},`+
			`"credentialDetails":{"credentials":{"credentialType":"AzureKeyVaultReference",`+
			`"workspaceId":"`+ws.ID+`","vaultUri":"https://v","secretName":"s"}}}`, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "keyReference") {
		t.Fatalf("AzureKeyVaultReference = %d %s; want 400 naming keyReference",
			w.Code, w.Body.Bytes())
	}

	// key and keyReference are alternatives, never both.
	both := `{"displayName":"pos","connectionDetails":{"type":"WebForPipeline",` +
		`"creationMethod":"WebForPipeline.Contents","parameters":[` +
		`{"dataType":"Text","name":"baseUrl","value":"https://x.example"}]},` +
		`"credentialDetails":{"credentials":{"credentialType":"Key","key":"literal",` +
		`"keyReference":{"connectionId":"c","secretName":"s"}}}}`
	if w := do(a.createConnection, admin, "POST", both, nil); w.Code != http.StatusBadRequest {
		t.Errorf("key AND keyReference = %d; want 400", w.Code)
	}

	// A reference to a connection that does not exist fails, and says so.
	if w := do(a.createConnection, admin, "POST", keyRef("no-such-conn", "s"), nil); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "AzureKeyVault") {
		t.Errorf("dangling connectionId = %d %s", w.Code, w.Body.Bytes())
	}

	// Now the real route. Stand up the vault connection first.
	a.Entra = wiEntra(t, false)
	vault := vaultFor(t, "db-password", "wi-vault-token")
	a.AKV = akv.New(false, nil, vault) // full URL: the stub is plain HTTP
	w = do(a.createConnection, admin, "POST", vaultConn("contoso-kv"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("vault connection = %d %s", w.Code, w.Body.Bytes())
	}
	var kv struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &kv)

	// A reference through it resolves, and the secret never comes back.
	w = do(a.createConnection, admin, "POST", keyRef(kv.ID, "db-password"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("keyReference = %d %s", w.Code, w.Body.Bytes())
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("resolved secret echoed: %s", w.Body.String())
	}

	// A reference pointing at a connection that is not a vault is refused —
	// the type is checked, not merely the id's existence.
	notAVault := do(a.createConnection, admin, "POST",
		`{"displayName":"web","connectionDetails":{"type":"WebForPipeline",`+
			`"creationMethod":"WebForPipeline.Contents","parameters":[`+
			`{"dataType":"Text","name":"baseUrl","value":"https://y.example"}]},`+
			`"credentialDetails":{"credentials":{"credentialType":"Anonymous"}}}`, nil)
	var web struct{ ID string }
	_ = json.Unmarshal(notAVault.Body.Bytes(), &web)
	if w := do(a.createConnection, admin, "POST", keyRef(web.ID, "db-password"), nil); w.Code != http.StatusBadRequest {
		t.Errorf("reference to a non-vault connection = %d; want 400", w.Code)
	}

	// Unknown secret fails the test connection; skipTestConnection bypasses it.
	if w := do(a.createConnection, admin, "POST", keyRef(kv.ID, "missing"), nil); w.Code != http.StatusBadRequest {
		t.Errorf("missing secret = %d; want 400", w.Code)
	}
	skip := strings.Replace(keyRef(kv.ID, "missing"),
		`"credentials":`, `"skipTestConnection":true,"credentials":`, 1)
	if w := do(a.createConnection, admin, "POST", skip, nil); w.Code != http.StatusCreated {
		t.Errorf("skipTestConnection = %d; want 201", w.Code)
	}
}

// mustHost is the host:port of a stub vault URL, for akv's vault allowlist.
func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}
