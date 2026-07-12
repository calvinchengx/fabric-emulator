// Package auth validates Entra bearer tokens the way real Fabric does:
// signature against the issuer's JWKS, issuer match, Fabric audience set, and
// expiry — with expiry checked against the emulator's controllable clock so
// token-lifetime scenarios are testable. No claims are trusted before the
// signature verifies.
package auth

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
)

// Accepted control-plane audiences (fabric-docs samples use both the Fabric
// and the legacy Power BI resource).
var ControlPlaneAudiences = []string{
	"https://api.fabric.microsoft.com",
	"https://analysis.windows.net/powerbi/api",
}

// Principal is the validated caller identity.
type Principal struct {
	ID   string // oid claim (falls back to sub)
	Type string // "User" | "ServicePrincipal"
	App  string // appid claim when present
}

// Validator verifies RS256 bearer tokens against a JWKS.
type Validator struct {
	Issuer    string
	Audiences []string
	Now       func() int64 // emulator clock

	jwksURL string
	client  *http.Client

	mu   sync.RWMutex
	keys map[string]*rsa.PublicKey // kid → key
}

// New builds a Validator fetching keys from jwksURL. insecure skips TLS
// verification (entra-emulator's self-signed cert on a compose network).
// client overrides the HTTP client when non-nil (in-process tests).
func New(issuer, jwksURL string, insecure bool, now func() int64, client *http.Client) *Validator {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if insecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		client = &http.Client{Transport: tr}
	}
	return &Validator{
		Issuer: issuer, Audiences: ControlPlaneAudiences, Now: now,
		jwksURL: jwksURL, client: client, keys: map[string]*rsa.PublicKey{},
	}
}

// Errors distinguished for the API's 401 bodies.
var (
	ErrNoToken  = errors.New("missing bearer token")
	ErrBadToken = errors.New("invalid token")
)

// ValidateRequest extracts and validates the Authorization header.
func (v *Validator) ValidateRequest(r *http.Request) (*Principal, error) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return nil, ErrNoToken
	}
	return v.Validate(strings.TrimSpace(h[len(prefix):]))
}

// Validate verifies the compact JWS and returns the principal.
func (v *Validator) Validate(token string) (*Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a compact JWS", ErrBadToken)
	}
	headB, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header encoding", ErrBadToken)
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headB, &head); err != nil || head.Alg != "RS256" {
		return nil, fmt.Errorf("%w: unsupported alg", ErrBadToken)
	}
	key, err := v.key(head.Kid)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadToken, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature encoding", ErrBadToken)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return nil, fmt.Errorf("%w: signature", ErrBadToken)
	}

	payloadB, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload encoding", ErrBadToken)
	}
	var claims struct {
		Iss   string          `json:"iss"`
		Aud   json.RawMessage `json:"aud"`
		Exp   int64           `json:"exp"`
		Nbf   int64           `json:"nbf"`
		Oid   string          `json:"oid"`
		Sub   string          `json:"sub"`
		AppID string          `json:"appid"`
		IdTyp string          `json:"idtyp"`
	}
	if err := json.Unmarshal(payloadB, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims", ErrBadToken)
	}
	if claims.Iss != v.Issuer {
		return nil, fmt.Errorf("%w: issuer %q not trusted", ErrBadToken, claims.Iss)
	}
	if !audMatch(claims.Aud, v.Audiences) {
		return nil, fmt.Errorf("%w: audience not accepted", ErrBadToken)
	}
	now := v.Now()
	const skew = 60
	if claims.Exp != 0 && now > claims.Exp+skew {
		return nil, fmt.Errorf("%w: expired", ErrBadToken)
	}
	if claims.Nbf != 0 && now < claims.Nbf-skew {
		return nil, fmt.Errorf("%w: not yet valid", ErrBadToken)
	}

	p := &Principal{ID: claims.Oid, App: claims.AppID, Type: "User"}
	if p.ID == "" {
		p.ID = claims.Sub
	}
	if claims.IdTyp == "app" {
		p.Type = "ServicePrincipal"
	}
	if p.ID == "" {
		return nil, fmt.Errorf("%w: no principal claim", ErrBadToken)
	}
	return p, nil
}

// aud may be a string or an array of strings.
func audMatch(raw json.RawMessage, accepted []string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		for _, a := range accepted {
			if one == a {
				return true
			}
		}
		return false
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		for _, got := range many {
			for _, a := range accepted {
				if got == a {
					return true
				}
			}
		}
	}
	return false
}

// key returns the RSA key for kid, refetching the JWKS once on a miss (key
// rotation, first use).
func (v *Validator) key(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	k := v.keys[kid]
	v.mu.RUnlock()
	if k != nil {
		return k, nil
	}
	if err := v.refresh(); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k := v.keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("no key %q in JWKS", kid)
}

func (v *Validator) refresh() error {
	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch JWKS: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}
	fresh := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nB, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eB, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		fresh[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(nB),
			E: int(new(big.Int).SetBytes(eB).Int64()),
		}
	}
	if len(fresh) == 0 {
		return errors.New("JWKS contained no RSA keys")
	}
	v.mu.Lock()
	v.keys = fresh
	v.mu.Unlock()
	return nil
}
