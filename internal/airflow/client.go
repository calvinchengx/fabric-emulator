package airflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client attaches fabric-emulator to an unmodified Apache Airflow 2.10 API.
type Client struct {
	BaseURL, DAGDir, Username, Password string
	HTTP                                *http.Client
	PollInterval                        time.Duration
}

func New(baseURL, dagDir, username, password string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid Airflow URL %q", baseURL)
	}
	if dagDir == "" {
		return nil, fmt.Errorf("Airflow DAG directory is required")
	}
	return &Client{BaseURL: strings.TrimSuffix(baseURL, "/"), DAGDir: dagDir, Username: username, Password: password, HTTP: &http.Client{Timeout: 30 * time.Second}, PollInterval: time.Second}, nil
}

func (c *Client) SyncDAGs(ctx context.Context, itemID string, files map[string][]byte) error {
	root := filepath.Join(c.DAGDir, itemID)
	cleanFiles := make(map[string][]byte, len(files))
	for name, raw := range files {
		clean := filepath.Clean(name)
		// A leading separator must be rejected explicitly: on Windows
		// filepath.IsAbs("/x") is FALSE (an absolute path there needs a
		// volume, "C:\\x"), so a rooted name would otherwise pass validation
		// — and since validation gates the RemoveAll below, that silently
		// wiped the item's existing DAGs and reported success.
		rooted := clean != "" && os.IsPathSeparator(clean[0])
		if clean == "." || clean == ".." || rooted || filepath.IsAbs(clean) ||
			strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid DAG path %q", name)
		}
		cleanFiles[clean] = raw
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	for clean, raw := range cleanFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := filepath.Join(root, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) TriggerAndWait(ctx context.Context, dagID, runID string, conf map[string]any) error {
	// Uploaded DAGs may take one scheduler parse interval to appear.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, err := c.call(ctx, "PATCH", "/api/v1/dags/"+url.PathEscape(dagID), map[string]any{"is_paused": false}, nil)
		if err == nil && status < 300 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Airflow DAG %q was not loaded: %w", dagID, err)
		}
		if err := sleep(ctx, c.PollInterval); err != nil {
			return err
		}
	}
	var created map[string]any
	status, err := c.call(ctx, "POST", "/api/v1/dags/"+url.PathEscape(dagID)+"/dagRuns", map[string]any{"dag_run_id": runID, "conf": conf}, &created)
	if err != nil || status >= 300 {
		return fmt.Errorf("trigger Airflow DAG: status %d: %w", status, err)
	}
	for {
		var run struct {
			State string `json:"state"`
		}
		status, err = c.call(ctx, "GET", "/api/v1/dags/"+url.PathEscape(dagID)+"/dagRuns/"+url.PathEscape(runID), nil, &run)
		if err != nil || status >= 300 {
			return fmt.Errorf("poll Airflow DAG: status %d: %w", status, err)
		}
		switch run.State {
		case "success":
			return nil
		case "failed":
			return fmt.Errorf("Airflow DAG run failed")
		}
		if err := sleep(ctx, c.PollInterval); err != nil {
			return err
		}
	}
}

func (c *Client) call(ctx context.Context, method, p string, body any, out any) (int, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+p, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Username != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s", resp.Status)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
