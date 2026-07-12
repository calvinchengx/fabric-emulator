package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		`{"displayName":"c","credentialDetails":{"credentials":{"credentialType":"Basic","username":"u"}}}`,
		`{"displayName":"c","credentialDetails":{"credentials":{"credentialType":"ServicePrincipal","tenantId":"t"}}}`,
		`{"displayName":"c","credentialDetails":{"credentials":{"credentialType":"WorkspaceIdentity"}}}`,
		`{"displayName":"c","credentialDetails":{"credentials":{"credentialType":"Key"}}}`,
		`{"displayName":"c","credentialDetails":{"credentials":{"credentialType":"SharedAccessSignature"}}}`,
		`{"displayName":"c","credentialDetails":{"credentials":{"credentialType":"Kerberos"}}}`,
	}
	for _, body := range bad {
		if w := do(a.createConnection, admin, "POST", body, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d; want 400", body, w.Code)
		}
	}

	// Basic and Anonymous succeed; the response and every read expose the
	// credentialType but never the secret material.
	w := do(a.createConnection, admin, "POST",
		`{"displayName":"db","credentialDetails":{"connectionEncryption":"NotEncrypted","credentials":{"credentialType":"Basic","username":"sa","password":"hunter2"}}}`, nil)
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
		`{"displayName":"open","credentialDetails":{"credentials":{"credentialType":"Anonymous"}}}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("anonymous create = %d", w.Code)
	}
	// Connections without credentialDetails still work (git provider style).
	if w := do(a.createConnection, admin, "POST", `{"displayName":"plain"}`, nil); w.Code != http.StatusCreated {
		t.Fatalf("plain create = %d", w.Code)
	}
}

func TestServicePrincipalProbe(t *testing.T) {
	a, _ := newAPI(t)
	spBody := func(secret string) string {
		return `{"displayName":"sp","credentialDetails":{"credentials":{"credentialType":"ServicePrincipal","tenantId":"tid","servicePrincipalClientId":"cid","servicePrincipalSecret":"` + secret + `"}}}`
	}

	// No entra configured → 503 unless skipTestConnection.
	if w := do(a.createConnection, admin, "POST", spBody("s"), nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("sp with nil entra = %d; want 503", w.Code)
	}
	skip := `{"displayName":"sp","credentialDetails":{"skipTestConnection":true,"credentials":{"credentialType":"ServicePrincipal","tenantId":"t","servicePrincipalClientId":"c","servicePrincipalSecret":"s"}}}`
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
	body := `{"displayName":"wi","credentialDetails":{"credentials":{"credentialType":"WorkspaceIdentity","workspaceId":"` + ws.ID + `"}}}`

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

func TestAzureKeyVaultReferenceCredential(t *testing.T) {
	a, st := newAPI(t)
	ws := seedWorkspace(t, st)
	body := func(vault, secret string) string {
		return `{"displayName":"akv","credentialDetails":{"credentials":{"credentialType":"AzureKeyVaultReference","workspaceId":"` + ws.ID + `","vaultUri":"` + vault + `","secretName":"` + secret + `"}}}`
	}

	// Missing fields → 400.
	if w := do(a.createConnection, admin, "POST",
		`{"displayName":"akv","credentialDetails":{"credentials":{"credentialType":"AzureKeyVaultReference","vaultUri":"v"}}}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("incomplete akv ref = %d", w.Code)
	}
	// No provisioned identity → 400.
	if w := do(a.createConnection, admin, "POST", body("https://v", "s"), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("akv ref without identity = %d", w.Code)
	}
	if err := st.SetWorkspaceIdentity(&store.WorkspaceIdentity{WorkspaceID: ws.ID, IdentityID: "sp-1", AppID: "app-1"}); err != nil {
		t.Fatal(err)
	}
	// No entra/vault clients → 503; skipTestConnection bypasses.
	if w := do(a.createConnection, admin, "POST", body("https://v", "s"), nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("akv ref nil clients = %d", w.Code)
	}
	skip := `{"displayName":"akv","credentialDetails":{"skipTestConnection":true,"credentials":{"credentialType":"AzureKeyVaultReference","workspaceId":"` + ws.ID + `","vaultUri":"https://v","secretName":"s"}}}`
	if w := do(a.createConnection, admin, "POST", skip, nil); w.Code != http.StatusCreated {
		t.Fatalf("akv ref skip = %d", w.Code)
	}

	// Resolution: WI token minted from entra, secret fetched from the vault.
	a.Entra = wiEntra(t, false)
	a.AKV = akv.New(false, nil)
	vault := vaultFor(t, "db-password", "wi-vault-token")
	w := do(a.createConnection, admin, "POST", body(vault, "db-password"), nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("akv ref = %d %s", w.Code, w.Body.Bytes())
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatalf("resolved secret echoed: %s", w.Body.String())
	}
	// Unknown secret → TestConnectionFailed; mint failure → 502.
	if w := do(a.createConnection, admin, "POST", body(vault, "missing"), nil); w.Code != http.StatusBadRequest {
		t.Fatalf("missing secret = %d", w.Code)
	}
	a.Entra = wiEntra(t, true)
	if w := do(a.createConnection, admin, "POST", body(vault, "db-password"), nil); w.Code != http.StatusBadGateway {
		t.Fatalf("mint failure = %d", w.Code)
	}
}
