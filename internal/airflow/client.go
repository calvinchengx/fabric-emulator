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

// newestWrite reports the most recent modification time under root, which is
// when the DAG files this item just synced actually hit the shared volume.
func newestWrite(root string) (time.Time, error) {
	var newest time.Time
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}

// waitForCurrent blocks until Airflow has parsed the files this item just
// synced, rather than an earlier version of them.
//
// THE DAG EXISTING IS NOT THE DAG BEING CURRENT, and conflating the two is a
// race that cannot fail loudly. TriggerAndWait already waits for the DAG to
// LOAD, which covers a brand-new file: until it parses, there is nothing to
// unpause. A CHANGED file has the opposite shape -- the DAG is already there,
// so every check passes instantly and the run is created from the structure
// currently serialised, which is the previous version. The result is not an
// error. It is a green run whose task instances belong to code that was
// replaced, and it reads as a DAG bug: a task the trigger rule referenced with
// no instance at all, or a newly added task returning in state `removed` while
// its downstream fails. Both were diagnosed as product defects first.
//
// `last_parsed_time` against the file's mtime is the exact question -- has the
// scheduler read what is on disk NOW -- and Airflow's own API answers it, so
// nothing here estimates a scan interval.
func (c *Client) waitForCurrent(ctx context.Context, itemID, dagID string) error {
	written, err := newestWrite(filepath.Join(c.DAGDir, itemID))
	if err != nil {
		// Not fatal. If the sync directory cannot be walked there is nothing
		// to compare against, and refusing to run would turn a missing
		// optimisation into an outage.
		return nil
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		var dag struct {
			LastParsedTime string `json:"last_parsed_time"`
		}
		status, err := c.call(ctx, "GET", "/api/v1/dags/"+url.PathEscape(dagID), nil, &dag)
		if err == nil && status < 300 && dag.LastParsedTime != "" {
			if parsed, perr := time.Parse(time.RFC3339Nano, dag.LastParsedTime); perr == nil {
				if parsed.After(written) {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"Airflow did not re-parse DAG %q within 2m of its files being "+
					"synced; triggering now would run the previous version", dagID)
		}
		if err := sleep(ctx, c.PollInterval); err != nil {
			return err
		}
	}
}

func (c *Client) TriggerAndWait(ctx context.Context, itemID, dagID, runID string, conf map[string]any) error {
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
	// BETWEEN LOADING AND TRIGGERING, which is the only place it works: after
	// the DAG is known to exist, and before a run is created from whatever is
	// serialised at that instant.
	if err := c.waitForCurrent(ctx, itemID, dagID); err != nil {
		return err
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
