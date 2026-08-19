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
	"sort"
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

type syncedFile struct {
	content []byte
	modTime time.Time
}

// readExisting snapshots what an item's DAG directory already holds, keyed the
// same way the incoming files are, so the two can be compared byte for byte.
func readExisting(root string) map[string]syncedFile {
	out := map[string]syncedFile{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		out[rel] = syncedFile{content: content, modTime: info.ModTime()}
		return nil
	})
	return out
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
	// READ BEFORE WIPING, so an unchanged file can keep its timestamp. Best
	// effort: anything unreadable simply counts as changed, which is the safe
	// direction -- it waits when it need not have, rather than triggering
	// against a serialisation it should have waited for.
	previous := readExisting(root)
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
		// KEEP THE TIMESTAMP OF AN UNCHANGED FILE. The wipe-and-rewrite that
		// makes this sync simple to reason about also stamps every file as new
		// on every run, which needlessly invites the scheduler to re-parse
		// work it has already done.
		if prior, ok := previous[clean]; ok && bytes.Equal(prior.content, raw) {
			_ = os.Chtimes(target, prior.modTime, prior.modTime)
		}
	}
	return nil
}

// DAGFingerprint is the serialised task structure Airflow currently holds for
// a DAG, as an ordered task-id list. Empty when the DAG is unknown or
// unreachable, which callers treat as "nothing to compare against".
//
// THE TASK LIST IS THE ONLY AUTHORITATIVE SIGNAL, and three cheaper ones were
// measured and rejected before settling here:
//
//   - `last_parsed_time` against the file's mtime. WRONG, and wrong in the
//     direction that matters: observed 20ms AHEAD of a write whose content the
//     parse had not read, because the cycle began before the write landed. It
//     reports that A parse finished, not that THIS file was ingested.
//   - the same, requiring the parse to be newer by some margin. That is the
//     45-second sleep this replaces, wearing a different hat.
//   - `/dagSources/{file_token}` compared against the bytes on disk. Closer,
//     and still wrong: DagCode was observed matching disk a full THIRTEEN
//     SECONDS before the task structure changed, so a trigger gated on it
//     still runs the previous topology.
//
// The gap is Airflow's own `min_serialized_dag_update_interval` (30s by
// default): the processor may read a file and skip rewriting the serialised
// DAG. Task instances come from that serialisation, so it is the thing to
// wait for, and asking for the task list asks exactly that question.
func (c *Client) DAGFingerprint(ctx context.Context, dagID string) string {
	var payload struct {
		Tasks []struct {
			TaskID string `json:"task_id"`
		} `json:"tasks"`
	}
	status, err := c.call(ctx, "GET", "/api/v1/dags/"+url.PathEscape(dagID)+"/tasks", nil, &payload)
	if err != nil || status >= 300 {
		return ""
	}
	ids := make([]string, 0, len(payload.Tasks))
	for _, t := range payload.Tasks {
		ids = append(ids, t.TaskID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// waitForCurrent blocks until Airflow's serialised DAG reflects the files just
// synced, rather than the version it held a moment ago.
//
// A DAG THAT EXISTS IS NOT A DAG THAT IS CURRENT. TriggerAndWait already waits
// for the DAG to LOAD, which covers a brand-new file: until it parses there is
// nothing to unpause. A CHANGED file has the opposite shape -- the DAG is
// already registered, every check passes instantly, and the run is created
// from the structure currently serialised. The result is not an error. It is a
// green run whose task instances belong to replaced code, and it surfaces as a
// task the trigger rule references having no instance, or a new task returning
// `removed` while its downstream fails. Both were diagnosed as DAG bugs first.
func (c *Client) waitForCurrent(ctx context.Context, dagID, before string) error {
	if before == "" {
		// Nothing to compare against: a DAG Airflow has never served cannot go
		// stale, and the load wait above already covered its arrival.
		return nil
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if now := c.DAGFingerprint(ctx, dagID); now != "" && now != before {
			return nil
		}
		if time.Now().After(deadline) {
			// DELIBERATELY NOT AN ERROR. A change that does not alter the task
			// set -- a callable's body, a default arg -- produces no
			// observable difference here and would otherwise fail every such
			// run. Two minutes is far past the serialisation interval that
			// causes the staleness, so proceeding is the right call; the
			// topology changes that actually bit are caught above.
			return nil
		}
		if err := sleep(ctx, c.PollInterval); err != nil {
			return err
		}
	}
}

func (c *Client) TriggerAndWait(ctx context.Context, dagID, runID, before string, conf map[string]any) error {
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
	if err := c.waitForCurrent(ctx, dagID, before); err != nil {
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
