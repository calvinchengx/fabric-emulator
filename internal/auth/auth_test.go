package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testIssuer = "https://login.test/tenant-1/v2.0"

type mintOpts struct {
	iss   string
	aud   any // string or []string
	exp   int64
	nbf   int64
	oid   string
	sub   string
	appid string
	idtyp string
	kid   string
}

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func mint(t *testing.T, key *rsa.PrivateKey, o mintOpts) string {
	t.Helper()
	head := map[string]string{"alg": "RS256", "typ": "JWT", "kid": o.kid}
	claims := map[string]any{"iss": o.iss, "aud": o.aud, "exp": o.exp, "nbf": o.nbf}
	for k, v := range map[string]string{"oid": o.oid, "sub": o.sub, "appid": o.appid, "idtyp": o.idtyp} {
		if v != "" {
			claims[k] = v
		}
	}
	signing := b64(head) + "." + b64(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// newFixture returns a validator wired to a local JWKS server and the signing key.
func newFixture(t *testing.T, now int64) (*Validator, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "test-key",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)
	v := New(testIssuer, srv.URL, false, func() int64 { return now }, srv.Client())
	return v, key
}

func TestValidateHappyPathUserAndApp(t *testing.T) {
	v, key := newFixture(t, 1000)
	base := mintOpts{iss: testIssuer, aud: ControlPlaneAudiences[0], exp: 2000, kid: "test-key"}

	user := base
	user.oid, user.sub = "oid-1", "sub-1"
	p, err := v.Validate(mint(t, key, user))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "oid-1" || p.Type != "User" {
		t.Fatalf("user principal = %+v", p)
	}

	app := base
	app.oid, app.appid, app.idtyp = "sp-oid", "app-1", "app"
	p, err = v.Validate(mint(t, key, app))
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "sp-oid" || p.Type != "ServicePrincipal" || p.App != "app-1" {
		t.Fatalf("app principal = %+v", p)
	}

	// Legacy Power BI audience is also accepted, including aud-as-array.
	pbi := base
	pbi.aud = []string{ControlPlaneAudiences[1]}
	pbi.oid = "oid-2"
	if _, err := v.Validate(mint(t, key, pbi)); err != nil {
		t.Fatalf("power bi audience rejected: %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	v, key := newFixture(t, 1000)
	good := mintOpts{iss: testIssuer, aud: ControlPlaneAudiences[0], exp: 2000, oid: "o", kid: "test-key"}

	cases := map[string]func(mintOpts) mintOpts{
		"wrong issuer":   func(o mintOpts) mintOpts { o.iss = "https://evil/v2.0"; return o },
		"wrong audience": func(o mintOpts) mintOpts { o.aud = "https://graph.microsoft.com"; return o },
		"expired":        func(o mintOpts) mintOpts { o.exp = 900; return o }, // 1000 > 900+60
		"not yet valid":  func(o mintOpts) mintOpts { o.nbf = 2000; return o },
		"unknown kid":    func(o mintOpts) mintOpts { o.kid = "other"; return o },
		"no principal":   func(o mintOpts) mintOpts { o.oid, o.sub = "", ""; return o },
	}
	for name, mutate := range cases {
		if _, err := v.Validate(mint(t, key, mutate(good))); err == nil {
			t.Errorf("%s: accepted; want rejection", name)
		}
	}

	// Tampered signature: token signed by a different key.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := v.Validate(mint(t, other, good)); err == nil {
		t.Error("foreign-key signature accepted")
	}
	if _, err := v.Validate("not-a-jwt"); err == nil {
		t.Error("garbage accepted")
	}
}

func TestExpiryFollowsEmulatorClock(t *testing.T) {
	now := int64(1000)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	defer jwksSrv.Close()
	v := New(testIssuer, jwksSrv.URL, false, func() int64 { return now }, jwksSrv.Client())

	tok := mint(t, key, mintOpts{iss: testIssuer, aud: ControlPlaneAudiences[0], exp: 2000, oid: "o", kid: "k"})
	if _, err := v.Validate(tok); err != nil {
		t.Fatalf("valid before expiry: %v", err)
	}
	now = 3000 // advance the emulator clock past exp — same token now rejected
	if _, err := v.Validate(tok); err == nil {
		t.Fatal("token accepted after clock advanced past exp")
	}
}

func TestValidateRequest(t *testing.T) {
	v, key := newFixture(t, 1000)
	r := httptest.NewRequest("GET", "/v1/workspaces", nil)
	if _, err := v.ValidateRequest(r); err != ErrNoToken {
		t.Fatalf("missing header err = %v; want ErrNoToken", err)
	}
	tok := mint(t, key, mintOpts{iss: testIssuer, aud: ControlPlaneAudiences[0], exp: 2000, oid: "o", kid: "test-key"})
	r.Header.Set("Authorization", "Bearer "+tok)
	if _, err := v.ValidateRequest(r); err != nil {
		t.Fatal(err)
	}
}
