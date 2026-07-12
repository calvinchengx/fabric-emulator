// Package akv is fabric-emulator's outbound client to an Azure Key Vault
// data plane — azure-keyvault-emulator in the family composition, or any
// vault-shaped endpoint. Used to resolve AKV-reference connections: the
// workspace identity's vault-audience token fetches the secret at
// create/use; the secret value itself is never stored here.
package akv

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// APIVersion is the Key Vault data-plane api-version we speak.
const APIVersion = "7.4"

// Client fetches secrets from a vault.
type Client struct {
	http *http.Client
}

// New builds a client. insecure skips TLS verification (the emulator's
// self-signed cert); client overrides when non-nil (tests).
func New(insecure bool, client *http.Client) *Client {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if insecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		client = &http.Client{Transport: tr}
	}
	return &Client{http: client}
}

// ResolveSecret GETs {vaultURI}/secrets/{name}?api-version=… with the bearer
// token and returns the secret value.
func (c *Client) ResolveSecret(vaultURI, name, bearer string) (string, error) {
	u := strings.TrimSuffix(vaultURI, "/") + "/secrets/" + url.PathEscape(name) + "?api-version=" + APIVersion
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
