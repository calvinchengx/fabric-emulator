package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJWKSFailureModes(t *testing.T) {
	mint500 := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	}
	cases := map[string]http.HandlerFunc{
		"http 500":    func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"bad json":    func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{nope")) },
		"no rsa keys": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"keys":[{"kty":"EC","kid":"e"}]}`)) },
		"bad n encoding": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k","n":"!!!","e":"AQAB"}]}`))
		},
	}
	_ = mint500
	for name, h := range cases {
		srv := httptest.NewServer(h)
		v := New("iss", srv.URL, false, func() int64 { return 0 }, srv.Client())
		if _, err := v.key("k"); err == nil {
			t.Errorf("%s: key() succeeded; want error", name)
		}
		srv.Close()
	}

	// Unreachable JWKS host.
	v := New("iss", "http://127.0.0.1:1/keys", false, func() int64 { return 0 }, nil)
	if _, err := v.key("k"); err == nil {
		t.Error("unreachable JWKS: key() succeeded")
	}
}

func TestNewInsecureTransport(t *testing.T) {
	// A TLS JWKS server with a self-signed cert: insecure=false fails,
	// insecure=true (default client, no override) succeeds.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"keys":[{"kty":"RSA","kid":"k","n":"AQAB","e":"AQAB"}]}`))
	}))
	defer srv.Close()
	strict := New("iss", srv.URL, false, func() int64 { return 0 }, nil)
	if err := strict.refresh(); err == nil {
		t.Fatal("strict client accepted a self-signed JWKS cert")
	}
	insecure := New("iss", srv.URL, true, func() int64 { return 0 }, nil)
	if err := insecure.refresh(); err != nil {
		t.Fatalf("insecure client rejected: %v", err)
	}
}

func TestMalformedTokenPieces(t *testing.T) {
	v, key := newFixture(t, 1000)
	good := mint(t, key, mintOpts{iss: testIssuer, aud: ControlPlaneAudiences[0], exp: 2000, oid: "o", kid: "test-key"})
	parts := strings.Split(good, ".")

	bad64 := "!" // invalid base64url
	cases := map[string]string{
		"garbage header":    bad64 + "." + parts[1] + "." + parts[2],
		"garbage payload":   parts[0] + "." + bad64 + "." + parts[2],
		"garbage signature": parts[0] + "." + parts[1] + "." + bad64,
		"wrong alg": func() string {
			h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"test-key"}`))
			return h + "." + parts[1] + "." + parts[2]
		}(),
		"non-json claims": func() string {
			p := base64.RawURLEncoding.EncodeToString([]byte(`not json`))
			return parts[0] + "." + p + "." + parts[2]
		}(),
	}
	for name, tok := range cases {
		if _, err := v.Validate(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// aud as a number is not a match.
	num := mintOpts{iss: testIssuer, exp: 2000, oid: "o", kid: "test-key"}
	numTok := mint(t, key, num) // aud omitted → null
	if _, err := v.Validate(numTok); err == nil {
		t.Error("missing aud accepted")
	}
}

func TestValidateRequestHeaderShapes(t *testing.T) {
	v, _ := newFixture(t, 1000)
	for _, h := range []string{"", "Basic abc", "bearer lowercase-scheme"} {
		r := httptest.NewRequest("GET", "/", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		if _, err := v.ValidateRequest(r); err == nil {
			t.Errorf("header %q accepted", h)
		}
	}
}

func TestAudMatchShapes(t *testing.T) {
	acc := []string{"https://a"}
	if audMatch(json.RawMessage(`"https://a"`), acc) != true {
		t.Error("string aud")
	}
	if audMatch(json.RawMessage(`["x","https://a"]`), acc) != true {
		t.Error("array aud")
	}
	for _, raw := range []string{`"https://b"`, `["x"]`, `42`, `{"a":1}`} {
		if audMatch(json.RawMessage(raw), acc) {
			t.Errorf("aud %s matched", raw)
		}
	}
}
