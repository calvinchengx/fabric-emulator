// Package akv is fabric-emulator's outbound client to an Azure Key Vault
// data plane — azure-keyvault-emulator in the family composition, or any
// vault-shaped endpoint. Used to resolve AKV-reference connections: the
// workspace identity's vault-audience token fetches the secret at
// create/use; the secret value itself is never stored here.
package akv

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"net/http"
	"net/url"
	"strings"
)

// APIVersion is the Key Vault data-plane api-version we speak.
const APIVersion = "7.4"

// Client fetches secrets from a vault.
type Client struct {
	http *http.Client
	// extraHost is one additional host:port accepted besides Azure's vault
	// suffixes — the family's own keyvault-emulator. Empty accepts none.
	extraHost string
	// extraScheme is how to DIAL extraHost when composing a URI from an
	// accountName. The allowlist deliberately accepts that host on either
	// scheme; this records which one it was configured with.
	extraScheme string
}

// AzureVaultSuffixes are the Key Vault data-plane domains across Azure's
// clouds. Real Fabric will not resolve an AKV reference to anything else, so
// neither will this.
var AzureVaultSuffixes = []string{
	".vault.azure.net",         // public
	".vault.azure.cn",          // China
	".vault.usgovcloudapi.net", // US Gov
	".vault.microsoftazure.de", // Germany (legacy)
	".managedhsm.azure.net",    // Managed HSM
}

// ErrVaultNotAllowed rejects a vaultURI that is not a Key Vault.
//
// WHY THIS EXISTS. ResolveSecret sends a workspace-identity bearer token for
// https://vault.azure.net to whatever host the caller names. Without this
// check, a connection body with vaultURI pointing at an attacker's host
// exfiltrates that token, and the emulator doubles as an SSRF probe into
// whatever its process can reach. That is not merely theoretical here: this
// project supports pointing --entra-issuer at a REAL tenant, in which case the
// leaked token is a real Azure one. Constraining the host is also what Azure
// itself does, so the permissive version was a parity bug too.
var ErrVaultNotAllowed = errors.New("vaultURI must be an Azure Key Vault (https://<name>.vault.azure.net) or the configured emulator vault")

// checkVaultURI returns the parsed URI if it is one we may send a token to.
func (c *Client) checkVaultURI(vaultURI string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(vaultURI))
	if err != nil {
		return nil, fmt.Errorf("%w: unparseable", ErrVaultNotAllowed)
	}
	// No embedded credentials, ever: userinfo is a classic way to make a
	// hostile host read as a familiar one (https://name.vault.azure.net@evil).
	if u.User != nil || u.Host == "" {
		return nil, ErrVaultNotAllowed
	}
	host := strings.ToLower(u.Hostname())
	// The configured emulator vault may be plain HTTP — it is an explicit
	// local choice by whoever ran the emulator, and the family composes it
	// both ways. Azure's real domains must be https: a token for
	// vault.azure.net does not go out over cleartext.
	if c.extraHost != "" && strings.EqualFold(u.Host, c.extraHost) {
		return u, nil
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, ErrVaultNotAllowed
	}
	for _, suffix := range AzureVaultSuffixes {
		// A suffix match alone would accept "evil-vault.azure.net.attacker.com"
		// were it not anchored, and bare "vault.azure.net" with no label is not
		// a vault either — require a non-empty label before the suffix.
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return u, nil
		}
	}
	return nil, ErrVaultNotAllowed
}

// New builds a client. insecure skips TLS verification (the emulator's
// self-signed cert); client overrides when non-nil (tests). extraHost is the
// one non-Azure host:port to accept — the family's keyvault-emulator.
func New(insecure bool, client *http.Client, extraHost string) *Client {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if insecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		client = &http.Client{Transport: tr}
	}
	// extraHost is host:port, but callers may pass a full URL (a stub vault's
	// address, or a deployment naming its scheme explicitly). Split it: the
	// allowlist compares hosts, VaultURI needs the scheme.
	scheme := "https"
	if i := strings.Index(extraHost, "://"); i >= 0 {
		scheme, extraHost = extraHost[:i], extraHost[i+3:]
	}
	return &Client{http: client, extraHost: extraHost, extraScheme: scheme}
}

// VaultURI turns an AzureKeyVault connection's `accountName` parameter into the
// address to dial.
//
// Real Fabric composes `https://{accountName}.vault.azure.net`, and that is what
// this returns unless the emulator was started with its own vault host — in
// which case every account resolves there, because a local stack has exactly one
// vault and no DNS to give it an Azure name. Substituting the HOST while keeping
// the request shape is the same move the SQL endpoint and OneLake addresses
// make: what a client must SEND stays identical, only where it lands differs.
func (c *Client) VaultURI(accountName string) string {
	if c.extraHost != "" {
		// checkVaultURI lets the configured host through on EITHER scheme,
		// because a local vault may legitimately be plain HTTP. So the scheme
		// cannot be assumed here either: it is whatever the host was configured
		// with, defaulting to https like every real vault.
		return c.extraScheme + "://" + c.extraHost
	}
	return "https://" + accountName + ".vault.azure.net"
}

// ResolveSecret GETs {vaultURI}/secrets/{name}?api-version=… with the bearer
// token and returns the secret value.
func (c *Client) ResolveSecret(vaultURI, name, bearer string) (string, error) {
	// Build the request from the URL checkVaultURI RETURNED, never from the raw
	// argument. Validating one representation and then requesting another is how
	// an allowlist gets bypassed: any input `url.Parse` normalises away (a stray
	// control character, a second scheme, an alternate host encoding) is checked
	// in the parsed form and re-introduced by the raw string. They agree today;
	// nothing enforced that they keep agreeing.
	base, err := c.checkVaultURI(vaultURI)
	if err != nil {
		return "", err
	}
	// The name is ONE path segment, so it is escaped into one. Setting only
	// `Path` would not do it: a `/` is legal in a decoded path and `String()`
	// keeps it, so a name of `../../certificates/evil` would leave /secrets/
	// entirely and send the vault-audience token to another endpoint on the
	// same host. Setting `RawPath` alongside is how net/url is told the exact
	// encoding to emit, and building the URL directly rather than through
	// `ResolveReference` keeps its dot-segment removal out of it — that
	// normalisation is what turns `..` into traversal rather than a 404.
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + "/secrets/" + name
	u.RawPath = strings.TrimSuffix(base.EscapedPath(), "/") + "/secrets/" + url.PathEscape(name)
	u.RawQuery = "api-version=" + APIVersion
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, ok := httpx.ReadBounded(resp.Body, httpx.MaxControlBody)
	if !ok {
		return "", fmt.Errorf("vault returned more than %d bytes, or the read failed", int64(httpx.MaxControlBody))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vault rejected the reference (status %d): %s", resp.StatusCode, raw)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("vault returned bad JSON: %w", err)
	}
	return out.Value, nil
}
