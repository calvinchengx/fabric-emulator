package akv

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func fakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api-version") == "" {
			http.Error(w, `{"error":{"code":"BadParameter"}}`, http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, `{"error":{"code":"Unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		if r.PathValue("name") != "db-password" {
			http.Error(w, `{"error":{"code":"SecretNotFound"}}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"value":"hunter2","id":"https://v/secrets/db-password/1"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveSecret(t *testing.T) {
	srv := fakeVault(t)
	c := New(false, srv.Client(), hostOf(t, srv.URL))

	v, err := c.ResolveSecret(srv.URL+"/", "db-password", "tok")
	if err != nil || v != "hunter2" {
		t.Fatalf("resolve = %q, %v", v, err)
	}
	if _, err := c.ResolveSecret(srv.URL, "missing", "tok"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing secret err = %v", err)
	}
	// Unreachable vault; default client construction.
	dead := New(false, nil, "127.0.0.1:1")
	if _, err := dead.ResolveSecret("http://127.0.0.1:1", "s", "t"); err == nil {
		t.Fatal("unreachable vault accepted")
	}
	// Non-JSON success body.
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer junk.Close()
	cj := New(false, junk.Client(), hostOf(t, junk.URL))
	if _, err := cj.ResolveSecret(junk.URL, "s", "t"); err == nil {
		t.Fatal("garbage vault JSON accepted")
	}
}

// TestAnOversizedVaultResponseIsRefused covers the response side of the
// truncation defect on this client.
//
// Before internal/httpx, a vault response past the ceiling was cut and the
// error the caller got was "vault returned bad JSON" — which is a lie about
// somebody else's service and sends whoever is debugging it in the wrong
// direction entirely. The parse failing is not a bound; it is a coincidence.
func TestAnOversizedVaultResponseIsRefused(t *testing.T) {
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 64<<10)
		for sent := 0; sent <= 1<<20; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer vault.Close()

	_, err := New(true, vault.Client(), hostOf(t, vault.URL)).ResolveSecret(vault.URL, "s", "bearer")
	if err == nil {
		t.Fatal("an oversized vault response was accepted")
	}
	if strings.Contains(err.Error(), "bad JSON") {
		t.Fatalf("reported as malformed JSON rather than oversized: %v — the "+
			"caller is told the wrong thing about the vault", err)
	}
}

// hostOf is a test server's host:port, which the allowlist must be told to
// accept — real code accepts only Azure's vault domains plus one such host.
func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}

// TestVaultURIAllowlist: ResolveSecret sends a vault-audience bearer token to
// whatever host it is given, so the host is the security boundary. These are
// the cases that must never reach the network.
func TestVaultURIAllowlist(t *testing.T) {
	c := New(false, nil, "keyvault-emulator:8444")

	allowed := []string{
		"https://contoso.vault.azure.net",
		"https://contoso.vault.azure.net/", // trailing slash
		"https://CONTOSO.VAULT.AZURE.NET",  // case
		"https://contoso.vault.azure.cn",   // sovereign clouds
		"https://contoso.vault.usgovcloudapi.net",
		"https://contoso.managedhsm.azure.net",
		"https://keyvault-emulator:8444", // the configured host
		"http://keyvault-emulator:8444",  // ...may be plain HTTP
	}
	for _, uri := range allowed {
		if _, err := c.checkVaultURI(uri); err != nil {
			t.Errorf("checkVaultURI(%q) refused a real vault: %v", uri, err)
		}
	}

	refused := map[string]string{
		"http://contoso.vault.azure.net":           "cleartext to a real vault",
		"https://evil.example.com":                 "a foreign host",
		"https://169.254.169.254/metadata":         "cloud instance metadata (SSRF)",
		"http://127.0.0.1:9443/v1/workspaces":      "the emulator's own API",
		"https://contoso.vault.azure.net.evil.com": "suffix smuggled into a longer host",
		"https://vault.azure.net":                  "the bare suffix, no vault label",
		"https://contoso.vault.azure.net@evil.com": "userinfo disguising the real host",
		"https://keyvault-emulator:9999":           "right host, wrong port",
		"file:///etc/passwd":                       "not even http",
		"":                                         "empty",
		"://nonsense":                              "unparseable",
	}
	for uri, why := range refused {
		if _, err := c.checkVaultURI(uri); err == nil {
			t.Errorf("checkVaultURI(%q) allowed %s", uri, why)
		}
	}

	// With no configured host, only Azure's domains are reachable.
	bare := New(false, nil, "")
	if _, err := bare.checkVaultURI("http://keyvault-emulator:8444"); err == nil {
		t.Error("an unconfigured client accepted the emulator vault")
	}

	// And the check runs before any request: a refused URI never dials.
	if _, err := c.ResolveSecret("https://evil.example.com", "s", "token"); !errors.Is(err, ErrVaultNotAllowed) {
		t.Errorf("ResolveSecret sent a token to a foreign host: %v", err)
	}
}

// TestASecretNameCannotLeaveTheSecretsPath is the traversal guard.
//
// The name is caller-supplied: it arrives in an AKV-reference connection body,
// alongside the vaultURI the allowlist already constrains. Constraining the
// HOST is only half the job, because the request carries a vault-audience
// bearer token and the vault serves more than /secrets — /certificates and
// /keys sit on the same host behind the same token. A name of
// `../../certificates/evil` that resolves out of /secrets/ hands that token's
// reach to whoever wrote the connection body.
//
// This is a regression test in the strict sense: escaping the name into ONE
// segment was already the behaviour, and rebuilding the URL through
// ResolveReference quietly dropped it, because a `/` inside a decoded Path is
// a separator and ResolveReference removes dot segments on top.
func TestASecretNameCannotLeaveTheSecretsPath(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.EscapedPath())
		_, _ = w.Write([]byte(`{"value":"v"}`))
	}))
	defer srv.Close()
	c := New(false, srv.Client(), hostOf(t, srv.URL))

	for _, name := range []string{
		"../../certificates/evil",
		"a/b",
		"..%2f..%2fkeys/x",
		"./../keys/x",
	} {
		got = nil
		if _, err := c.ResolveSecret(srv.URL, name, "tok"); err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%q: %d requests", name, len(got))
		}
		// Whatever the name contained, the request must still be for a single
		// segment under /secrets/.
		rest, ok := strings.CutPrefix(got[0], "/secrets/")
		if !ok {
			t.Fatalf("%q escaped the secrets path: %s", name, got[0])
		}
		if strings.Contains(rest, "/") {
			t.Fatalf("%q became more than one segment: %s", name, got[0])
		}
	}
}

// TestTheRequestedURLComesFromTheValidatedVault keeps the other half of the
// same line honest: the host actually requested is the one the allowlist
// approved, not whatever the raw argument said.
func TestTheRequestedURLComesFromTheValidatedVault(t *testing.T) {
	srv := fakeVault(t)
	c := New(false, srv.Client(), hostOf(t, srv.URL))
	// A trailing slash, which the join must not double.
	if _, err := c.ResolveSecret(srv.URL+"/", "db-password", "tok"); err != nil {
		t.Fatalf("trailing slash: %v", err)
	}
	// A vault the allowlist rejects is never requested at all.
	if _, err := c.ResolveSecret("https://evil.example.com", "db-password", "tok"); !errors.Is(err, ErrVaultNotAllowed) {
		t.Fatalf("allowlist bypass: %v", err)
	}
}
