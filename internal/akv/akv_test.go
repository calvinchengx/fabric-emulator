package akv

import (
	"errors"
	"fmt"
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

// TestTLSVerificationIsScopedToTheConfiguredVaultHost is the regression test
// for a transport-wide InsecureSkipVerify on the one client that may reach a
// real Azure Key Vault.
//
// `insecure` arrives from FABRIC_ENTRA_TLS_INSECURE, which docker-compose.yml
// sets to "true" and docs/04-configuration.md documents as being for
// entra-emulator's self-signed cert. Before the fix it disabled certificate
// verification for EVERY host this client dials, and checkVaultURI
// deliberately admits `*.vault.azure.net` — so a connection body naming a real
// vault sent a workspace-identity bearer token, and received a secret, over a
// connection nobody authenticated. The https requirement that comment leans on
// ("a token for vault.azure.net does not go out over cleartext") was enforced
// while the trust behind it was not.
//
// ONE untrusted certificate, ONE request URL, TWO clients differing only in
// which host was configured as the emulator vault. That isolation is the whole
// point: it cannot pass because of a cert detail or a URL detail, only because
// the skip is keyed on the host. On the old code the second half succeeds.
func TestTLSVerificationIsScopedToTheConfiguredVaultHost(t *testing.T) {
	// An untrusted cert, exactly like the emulator's own: httptest signs with
	// its own CA, which the system pool does not carry. srv.Client() is NOT
	// used anywhere here — the point is what New builds for itself.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"value":"hunter2"}`))
	}))
	defer srv.Close()
	host := hostOf(t, srv.URL)

	// The transport New BUILT, exercised directly. Going through ResolveSecret
	// cannot ask this question: the allowlist refuses any host that is neither
	// an Azure vault nor the configured one, so the refusal would come from
	// checkVaultURI before a single byte was dialed and the TLS decision would
	// never be reached. What is under test is the trust decision, so the
	// request has to get as far as making one.
	get := func(c *Client) error {
		resp, err := c.http.Get(srv.URL)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	// Configured AS the emulator vault: the skip is what that flag was for, so
	// its self-signed cert is accepted.
	if err := get(New(true, nil, host)); err != nil {
		t.Fatalf("the configured emulator vault was refused its own cert: %v", err)
	}

	// The SAME host and the SAME cert, with some OTHER host configured as the
	// emulator vault. Verification must now apply, because this host is not the
	// one the flag was justified for. A real deployment has keyvault-emulator
	// configured here and an Azure vault as the target; this is that case with
	// the two swapped, so it needs no network and names no real vault.
	err := get(New(true, nil, "keyvault-emulator:8444"))
	if err == nil {
		t.Fatal("a host that is NOT the configured emulator vault was served an " +
			"untrusted certificate and accepted it: the skip is transport-wide, " +
			"so a real *.vault.azure.net gets no verification either")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("refused, but not for the certificate: %v", err)
	}

	// And with NO emulator vault configured there is nothing to exempt, so the
	// flag must weaken nothing: every host reachable then is an Azure one.
	if err := get(New(true, nil, "")); err == nil {
		t.Fatal("insecure with no configured vault host still skipped verification")
	}

	// The flag off is the same decision for the emulator host too.
	if err := get(New(false, nil, host)); err == nil {
		t.Fatal("insecure=false accepted an untrusted certificate")
	}
}

// TestAnAzureVaultHostAlwaysVerifies names the routing decision per host,
// including the sovereign-cloud domains a single end-to-end case cannot reach.
//
// AzureVaultSuffixes is what the allowlist admits; every one of them must land
// on the VERIFYING transport however `insecure` is set. Asserted on the
// RoundTripper rather than over the network so each domain is actually covered
// instead of standing in for the others.
func TestAnAzureVaultHostAlwaysVerifies(t *testing.T) {
	var used string
	mark := func(name string) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			used = name
			return nil, fmt.Errorf("stub %s", name)
		})
	}
	rt := &hostScopedTLS{
		verify:   mark("verify"),
		skip:     mark("skip"),
		skipHost: "keyvault-emulator:8444",
	}
	send := func(rawurl string) string {
		used = ""
		req, err := http.NewRequest(http.MethodGet, rawurl, nil)
		if err != nil {
			t.Fatalf("new request %q: %v", rawurl, err)
		}
		_, _ = rt.RoundTrip(req)
		return used
	}

	for _, suffix := range AzureVaultSuffixes {
		for _, uri := range []string{
			"https://contoso" + suffix,
			"https://CONTOSO" + strings.ToUpper(suffix), // case must not exempt
			"https://contoso" + suffix + ":443",         // an explicit port either
		} {
			if got := send(uri); got != "verify" {
				t.Errorf("%s went to the %s transport; an Azure vault must always verify", uri, got)
			}
		}
	}

	// The configured emulator vault, and only on an exact host:port match.
	if got := send("https://keyvault-emulator:8444/secrets/s"); got != "skip" {
		t.Errorf("the configured emulator vault went to the %s transport", got)
	}
	if got := send("https://KeyVault-Emulator:8444"); got != "skip" {
		t.Errorf("the configured host was case-sensitive: went to %s", got)
	}
	for _, near := range []string{
		"https://keyvault-emulator:9999",          // right host, wrong port
		"https://keyvault-emulator",               // no port at all
		"https://keyvault-emulator.evil.com:8444", // the host as a prefix
		"https://evil.com",
	} {
		if got := send(near); got != "verify" {
			t.Errorf("%s went to the %s transport; only the configured host:port is exempt", near, got)
		}
	}
}

// roundTripFunc adapts a function to http.RoundTripper, so the test above can
// tell which of the two transports a request was handed to.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
