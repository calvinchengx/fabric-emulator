package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/pipeline"
)

// databricksRemote submits a notebook or Python task to the workspace at
// DatabricksURL (databricks-emulator, or any host that speaks Jobs 2.2 +
// workspace import). dbfs: / /Workspace paths stay as written — they resolve
// there. A lakehouse <itemId>/<path> is read from OneLake and imported first,
// so the remote job still names a workspace path.
func (e *pipelineExecutor) databricksRemote(
	act pipeline.Activity,
	spec databricksSpec,
	rawPath string,
	params map[string]any,
	argv []string,
) (map[string]any, error) {
	remote := strings.TrimRight(e.a.DatabricksURL, "/")
	jobPath := rawPath
	if !isDatabricksNativePath(rawPath) {
		itemID, base, ok := splitRootPath(rawPath)
		if !ok || base == "" {
			return nil, fmt.Errorf("databricks activity %q: %s %q must be "+
				"<lakehouseItemId>/<path> or a Databricks path (dbfs: / /Workspace / /Shared / /Repos)",
				act.Name, spec.pathKey, rawPath)
		}
		p, gerr := e.a.Store.GetOneLakePath(itemID, base)
		if gerr != nil || p.IsDir {
			return nil, fmt.Errorf("databricks activity %q: no file at %q in item %q",
				act.Name, base, itemID)
		}
		jobPath = "/Shared/fabric-emulator/" + e.jobID + "/" + act.Name + "/" + path.Base(base)
		if err := e.databricksImport(jobPath, p.Content); err != nil {
			return nil, fmt.Errorf("databricks activity %q: import %q: %w", act.Name, jobPath, err)
		}
	}

	task := map[string]any{"task_key": "main"}
	if spec.kind == "notebook" {
		task["notebook_task"] = map[string]any{
			"notebook_path":   jobPath,
			"base_parameters": params,
		}
	} else {
		task["spark_python_task"] = map[string]any{
			"python_file": jobPath,
			"parameters":  argv,
		}
	}
	var created struct {
		JobID int64 `json:"job_id"`
	}
	if err := e.databricksJSON("POST", "/api/2.2/jobs/create", map[string]any{
		"name":  "fabric-" + e.jobID + "-" + act.Name,
		"tasks": []any{task},
	}, &created); err != nil {
		return nil, fmt.Errorf("databricks activity %q: jobs/create: %w", act.Name, err)
	}

	var started struct {
		RunID int64 `json:"run_id"`
	}
	if err := e.databricksJSON("POST", "/api/2.2/jobs/run-now", map[string]any{
		"job_id": created.JobID,
	}, &started); err != nil {
		return nil, fmt.Errorf("databricks activity %q: jobs/run-now: %w", act.Name, err)
	}

	run, err := e.databricksPoll(started.RunID)
	if err != nil {
		return nil, fmt.Errorf("databricks activity %q: %w", act.Name, err)
	}
	state, _ := run["state"].(map[string]any)
	if result, _ := state["result_state"].(string); result != "SUCCESS" {
		msg, _ := state["state_message"].(string)
		if msg == "" {
			msg = fmt.Sprint(state["result_state"])
		}
		if logs := e.databricksRunError(started.RunID); logs != "" {
			msg = logs
		}
		return nil, fmt.Errorf("databricks activity %q: remote run %d %s", act.Name, started.RunID, msg)
	}

	executedBy, _ := run["executedBy"].(string)
	if executedBy == "" {
		executedBy = "the workspace at " + remote + ", not a Fabric Spark agent"
	}
	result := map[string]any{
		"status":     "Succeeded",
		"executedBy": executedBy,
		"run_id":     started.RunID,
		"job_id":     created.JobID,
	}
	result[spec.pathKey] = rawPath
	if spec.kind == "notebook" {
		result["baseParameters"] = params
	} else {
		result["parameters"] = argv
	}
	return result, nil
}

func isDatabricksNativePath(p string) bool {
	for _, prefix := range []string{"dbfs:", "/Workspace/", "/Shared/", "/Repos/", "/Users/"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func (e *pipelineExecutor) databricksImport(workspacePath string, content []byte) error {
	return e.databricksJSON("POST", "/api/2.0/workspace/import", map[string]any{
		"path":      workspacePath,
		"format":    "SOURCE",
		"language":  "PYTHON",
		"content":   base64.StdEncoding.EncodeToString(content),
		"overwrite": true,
	}, nil)
}

func (e *pipelineExecutor) databricksPoll(runID int64) (map[string]any, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		var run map[string]any
		if err := e.databricksJSON("GET",
			fmt.Sprintf("/api/2.2/jobs/runs/get?run_id=%d", runID), nil, &run); err != nil {
			return nil, err
		}
		state, _ := run["state"].(map[string]any)
		life, _ := state["life_cycle_state"].(string)
		if life == "TERMINATED" || life == "SKIPPED" || life == "INTERNAL_ERROR" {
			return run, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("run %d did not terminate", runID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *pipelineExecutor) databricksRunError(runID int64) string {
	var out map[string]any
	if err := e.databricksJSON("GET",
		fmt.Sprintf("/api/2.2/jobs/runs/get-output?run_id=%d", runID), nil, &out); err != nil {
		return ""
	}
	if s, _ := out["error"].(string); s != "" {
		return s
	}
	return ""
}

func (e *pipelineExecutor) databricksJSON(method, path string, body any, dest any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(e.a.DatabricksURL, "/")+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := e.a.DatabricksToken; tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := e.a.DatabricksHTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, path, strings.TrimSpace(string(raw)))
	}
	if dest == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dest)
}
