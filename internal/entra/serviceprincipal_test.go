package entra

// The service-principal token exchange, which is the credential real Fabric
// accepts for an AzureKeyVault connection.
//
// Each case asserts WHY it failed, not merely that it did. A client-credentials
// exchange has four distinct ways to disappoint — the server refuses, the
// server lies about the content type, the body is well-formed but empty, and
// the host is unreachable — and three of them produce a Go error that reads the
// same at the call site unless the message distinguishes them.

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMintServicePrincipalTokenSendsTheGrantAndReturnsTheToken(t *testing.T) {
	var gotForm url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm, gotPath = r.PostForm, r.URL.Path
		_, _ = w.Write([]byte(`{"access_token":"vault-token"}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, false, srv.Client()).
		MintServicePrincipalToken("tid", "cid", "shh", "https://vault.azure.net/.default")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if got != "vault-token" {
		t.Errorf("token = %q, want vault-token", got)
	}
	// The scope is the caller's, not a hardcoded Fabric one — that hardcoding is
	// exactly why this function had to exist beside ValidateClientCredentials.
	if s := gotForm.Get("scope"); s != "https://vault.azure.net/.default" {
		t.Errorf("scope = %q", s)
	}
	if g := gotForm.Get("grant_type"); g != "client_credentials" {
		t.Errorf("grant_type = %q", g)
	}
	if gotForm.Get("client_id") != "cid" || gotForm.Get("client_secret") != "shh" {
		t.Errorf("credentials not sent: %v", gotForm)
	}
	if want := "/tid/oauth2/v2.0/token"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestMintServicePrincipalTokenSaysWhyItFailed(t *testing.T) {
	cases := []struct {
		name, body, want string
		status           int
	}{
		{"refused", `{"error":"invalid_client"}`, "rejected", http.StatusUnauthorized},
		{"not json", `<html>nope</html>`, "bad JSON", http.StatusOK},
		{"no token", `{"token_type":"Bearer"}`, "no access_token", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := New(srv.URL, false, srv.Client()).
				MintServicePrincipalToken("t", "c", "s", "scope")
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// An unreachable authority is a different failure from a rejection, and a
// caller that cannot tell them apart retries the wrong one.
func TestMintServicePrincipalTokenDistinguishesUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	_, err := New(url, false, nil).MintServicePrincipalToken("t", "c", "s", "scope")
	if err == nil || !strings.Contains(err.Error(), "entra unreachable") {
		t.Fatalf("error = %v, want 'entra unreachable'", err)
	}
}
