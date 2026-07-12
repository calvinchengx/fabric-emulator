// Package entra is fabric-emulator's outbound HTTP client to entra-emulator's
// workspace-identity surface: the control plane asks the STS to create,
// rename, and delete the auto-managed app registration + service principal
// that backs a Fabric workspace identity. Coupled over HTTP only — the same
// admin API a human can curl.
package entra

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Identity is entra's workspace-identity DTO (the fields fabric needs).
type Identity struct {
	ID    string `json:"id"`    // service principal object id
	AppID string `json:"appId"` // client/app id — the sub/appid in minted tokens
	State string `json:"state"`
}

// Client calls entra-emulator's admin API.
type Client struct {
	base string // entra origin, e.g. https://entra-emulator:8443
	http *http.Client
}

// New builds a client for the entra origin. client overrides the HTTP client
// when non-nil (in-process tests); insecure skips TLS verification.
func New(origin string, insecure bool, client *http.Client) *Client {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if insecure {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		client = &http.Client{Transport: tr}
	}
	return &Client{base: strings.TrimSuffix(origin, "/"), http: client}
}

// OriginFromIssuer derives the entra origin from a v2.0 issuer URL
// ({origin}/{tenant}/v2.0 → {origin}).
func OriginFromIssuer(issuer string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("cannot derive entra origin from issuer %q", issuer)
	}
	return u.Scheme + "://" + u.Host, nil
}

func (c *Client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("entra unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("entra %s %s: status %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("entra %s %s: bad JSON: %w", method, path, err)
		}
	}
	return nil
}

// CreateWorkspaceIdentity provisions the identity for a workspace
// (name-follows-workspace starts here).
func (c *Client) CreateWorkspaceIdentity(workspaceID, workspaceName string) (*Identity, error) {
	var wi Identity
	err := c.do("POST", "/admin/api/workspace-identities",
		map[string]string{"workspaceId": workspaceID, "workspaceName": workspaceName}, &wi)
	if err != nil {
		return nil, err
	}
	return &wi, nil
}

// RenameWorkspaceIdentity keeps the identity's name following the workspace.
func (c *Client) RenameWorkspaceIdentity(id, workspaceName string) error {
	return c.do("PATCH", "/admin/api/workspace-identities/"+url.PathEscape(id),
		map[string]string{"workspaceName": workspaceName}, nil)
}

// DeleteWorkspaceIdentity deprovisions (cascade from workspace delete or an
// explicit deprovision).
func (c *Client) DeleteWorkspaceIdentity(id string) error {
	return c.do("DELETE", "/admin/api/workspace-identities/"+url.PathEscape(id), nil, nil)
}

// ValidateClientCredentials performs a real client-credentials grant — the
// "test connection" probe for ServicePrincipal connection credentials. A
// wrong id/secret fails here exactly as it would in production.
func (c *Client) ValidateClientCredentials(tenantID, clientID, secret string) error {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {secret},
		"scope":         {"https://api.fabric.microsoft.com/.default"},
	}
	resp, err := c.http.PostForm(c.base+"/"+url.PathEscape(tenantID)+"/oauth2/v2.0/token", form)
	if err != nil {
		return fmt.Errorf("entra unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("service principal credentials rejected (status %d): %s", resp.StatusCode, raw)
	}
	return nil
}
