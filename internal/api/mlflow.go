package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const mlflowMetadataPart = "mlflow-metadata.json"

func (a *API) SetMLflowBackend(raw string) error {
	if raw == "" {
		a.MLflowURL = nil
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("invalid MLflow URL %q", raw)
	}
	a.MLflowURL = u
	if a.MLflowHTTP == nil {
		a.MLflowHTTP = &http.Client{}
	}
	return nil
}

func (a *API) registerMLflow(mux *http.ServeMux) {
	mux.HandleFunc("/mlflow/workspaces/{wid}/{path...}", a.withAuth(a.mlflowProxy))
}

func (a *API) mlflowProxy(w http.ResponseWriter, r *http.Request, principal *auth.Principal) {
	if a.MLflowURL == nil {
		writeErr(w, http.StatusNotImplemented, "MLflowNotConfigured", "An MLflow tracking server is not attached.")
		return
	}
	wid := r.PathValue("wid")
	role := store.RoleContributor
	if mlflowReadOnly(r.Method, r.PathValue("path")) {
		role = store.RoleViewer
	}
	if _, _, ok := a.requireRole(w, wid, principal, role); !ok {
		return
	}
	upstreamPath := "/" + strings.TrimPrefix(r.PathValue("path"), "/")
	if upstreamPath != "/version" && !strings.HasPrefix(upstreamPath, "/api/2.0/mlflow/") && !strings.HasPrefix(upstreamPath, "/api/2.0/mlflow-artifacts/") {
		writeErr(w, http.StatusNotFound, "MLflowEndpointNotSupported", "The MLflow endpoint is not available.")
		return
	}
	// A relay, so a truncated body would be a request we do not own, silently
	// altered — MLflow would answer it as if the caller had sent it that way.
	raw, ok := httpx.ReadBounded(r.Body, httpx.MaxProxyBody)
	if !ok {
		writeErr(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge",
			"The request body is too large to relay.")
		return
	}
	original, transformed, query, err := transformMLflowRequest(wid, upstreamPath, raw, r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if err := a.authorizeMLflowReference(wid, upstreamPath, transformed, query); err != nil {
		writeErr(w, http.StatusForbidden, "MLflowWorkspaceMismatch", err.Error())
		return
	}
	status, header, response, err := a.callMLflow(r, upstreamPath, query, transformed)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "MLflowBackendError", err.Error())
		return
	}
	if status < 300 {
		a.syncMLflowItem(wid, upstreamPath, original, response)
		if r.Method == http.MethodPut && strings.HasPrefix(upstreamPath, "/api/2.0/mlflow-artifacts/artifacts/") {
			a.mirrorMLflowArtifact(wid, upstreamPath, raw)
		}
	}
	for key, values := range header {
		if strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Connection") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	response = filterAndStripMLflowResponse(wid, response)
	w.WriteHeader(status)
	_, _ = w.Write(response)
}

func mlflowReadOnly(method, endpoint string) bool {
	if method == http.MethodGet || method == http.MethodHead {
		return true
	}
	for _, suffix := range []string{"/search", "/get", "/get-by-name", "/get-download-uri", "/get-history"} {
		if strings.HasSuffix(endpoint, suffix) {
			return true
		}
	}
	return false
}

func (a *API) callMLflow(in *http.Request, upstreamPath string, query url.Values, body []byte) (int, http.Header, []byte, error) {
	target := *a.MLflowURL
	target.Path = strings.TrimSuffix(target.Path, "/") + upstreamPath
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(in.Context(), in.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for key, values := range in.Header {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	client := a.MLflowHTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Clone(), raw, err
}

func transformMLflowRequest(wid, endpoint string, raw []byte, query url.Values) (map[string]any, []byte, url.Values, error) {
	clonedQuery := make(url.Values, len(query))
	for key, values := range query {
		clonedQuery[key] = append([]string(nil), values...)
	}
	query = clonedQuery
	prefix := mlflowPrefix(wid)
	for _, key := range []string{"experiment_name", "name"} {
		if value := query.Get(key); value != "" && !strings.HasPrefix(value, prefix) {
			query.Set(key, prefix+value)
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 || !strings.HasPrefix(endpoint, "/api/2.0/mlflow/") {
		return nil, raw, query, nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, nil, nil, err
	}
	original := cloneJSONMap(body)
	switch {
	case strings.HasSuffix(endpoint, "/experiments/create"):
		prefixString(body, "name", prefix)
	case strings.HasSuffix(endpoint, "/experiments/get-by-name"):
		prefixString(body, "experiment_name", prefix)
	case strings.Contains(endpoint, "/registered-models/") || strings.Contains(endpoint, "/model-versions/"):
		prefixString(body, "name", prefix)
	}
	transformed, err := json.Marshal(body)
	return original, transformed, query, err
}

func cloneJSONMap(in map[string]any) map[string]any {
	raw, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func prefixString(body map[string]any, key, prefix string) {
	if value, ok := body[key].(string); ok && !strings.HasPrefix(value, prefix) {
		body[key] = prefix + value
	}
}

func mlflowPrefix(wid string) string { return "fabric__" + wid + "__" }

func filterAndStripMLflowResponse(wid string, raw []byte) []byte {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	prefix := mlflowPrefix(wid)
	value = filterMLflowValue(value, prefix)
	out, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return out
}

func filterMLflowValue(value any, prefix string) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if list, ok := child.([]any); ok && (key == "experiments" || key == "registered_models" || key == "model_versions") {
				filtered := make([]any, 0, len(list))
				for _, entry := range list {
					object, _ := entry.(map[string]any)
					name, _ := object["name"].(string)
					if strings.HasPrefix(name, prefix) {
						filtered = append(filtered, filterMLflowValue(entry, prefix))
					}
				}
				typed[key] = filtered
				continue
			}
			typed[key] = filterMLflowValue(child, prefix)
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = filterMLflowValue(typed[i], prefix)
		}
		return typed
	case string:
		return strings.TrimPrefix(typed, prefix)
	default:
		return value
	}
}

func (a *API) authorizeMLflowReference(wid, endpoint string, body []byte, query url.Values) error {
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	ids := []string{}
	if id, ok := payload["experiment_id"].(string); ok {
		ids = append(ids, id)
	}
	if list, ok := payload["experiment_ids"].([]any); ok {
		for _, value := range list {
			if id, ok := value.(string); ok {
				ids = append(ids, id)
			}
		}
	}
	if id := query.Get("experiment_id"); id != "" {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if !a.mlflowExperimentBelongsTo(wid, id) {
			return fmt.Errorf("experiment %q does not belong to this workspace", id)
		}
	}
	if strings.HasPrefix(endpoint, "/api/2.0/mlflow-artifacts/artifacts/") {
		experimentID, _, ok := mlflowArtifactReference(endpoint)
		if !ok {
			return fmt.Errorf("artifact path is invalid")
		}
		if !a.mlflowExperimentBelongsTo(wid, experimentID) {
			return fmt.Errorf("artifact experiment %q does not belong to this workspace", experimentID)
		}
	}
	runIDs := []string{}
	for _, key := range []string{"run_id", "run_uuid"} {
		if id, ok := payload[key].(string); ok && id != "" {
			runIDs = append(runIDs, id)
		}
		if id := query.Get(key); id != "" {
			runIDs = append(runIDs, id)
		}
	}
	if source, _ := payload["source"].(string); strings.HasPrefix(source, "runs:/") {
		runIDs = append(runIDs, strings.SplitN(strings.TrimPrefix(source, "runs:/"), "/", 2)[0])
	}
	for _, id := range runIDs {
		if !a.mlflowRunBelongsTo(wid, id) {
			return fmt.Errorf("run %q does not belong to this workspace", id)
		}
	}
	return nil
}

func (a *API) mlflowExperimentBelongsTo(wid, experimentID string) bool {
	items, err := a.Store.ListItems(wid, "MLExperiment")
	if err != nil {
		return false
	}
	for _, item := range items {
		metadata, err := a.mlflowItemMetadata(item.ID)
		if err == nil && metadata["experimentId"] == experimentID {
			return true
		}
	}
	return false
}

func (a *API) mlflowRunBelongsTo(wid, runID string) bool {
	items, err := a.Store.ListItems(wid, "MLExperiment")
	if err != nil {
		return false
	}
	for _, item := range items {
		metadata, err := a.mlflowItemMetadata(item.ID)
		if err == nil && metadata["runId:"+runID] == "true" {
			return true
		}
	}
	return false
}

func (a *API) syncMLflowItem(wid, endpoint string, request map[string]any, response []byte) {
	if request == nil {
		return
	}
	var wire map[string]any
	if json.Unmarshal(response, &wire) != nil {
		return
	}
	switch {
	case strings.HasSuffix(endpoint, "/experiments/create"):
		name, _ := request["name"].(string)
		id, _ := wire["experiment_id"].(string)
		if name != "" && id != "" {
			a.upsertMLflowItem(wid, "MLExperiment", name, map[string]string{"experimentId": id})
		}
	case strings.HasSuffix(endpoint, "/registered-models/create"):
		name, _ := request["name"].(string)
		if name != "" {
			a.upsertMLflowItem(wid, "MLModel", name, map[string]string{"registeredModelName": name})
		}
	case strings.HasSuffix(endpoint, "/runs/create"):
		experimentID, _ := request["experiment_id"].(string)
		runID := nestedMLflowString(wire, "run", "info", "run_id")
		if runID == "" {
			runID = nestedMLflowString(wire, "run", "info", "run_uuid")
		}
		if experimentID != "" && runID != "" {
			a.recordMLflowRun(wid, experimentID, runID)
		}
	}
}

func nestedMLflowString(value map[string]any, keys ...string) string {
	var current any = value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	result, _ := current.(string)
	return result
}

func (a *API) recordMLflowRun(wid, experimentID, runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	items, err := a.Store.ListItems(wid, "MLExperiment")
	if err != nil {
		return
	}
	for _, item := range items {
		metadata, err := a.mlflowItemMetadata(item.ID)
		if err != nil || metadata["experimentId"] != experimentID {
			continue
		}
		metadata["runId:"+runID] = "true"
		raw, _ := json.Marshal(metadata)
		_ = a.Store.SetDefinition(item.ID, []store.DefinitionPart{{Path: mlflowMetadataPart, PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString(raw)}})
		return
	}
}

func (a *API) upsertMLflowItem(wid, itemType, name string, metadata map[string]string) {
	item, err := a.Store.GetItemByName(wid, name, itemType)
	if err != nil {
		item = &store.Item{WorkspaceID: wid, Type: itemType, DisplayName: name}
		if a.Store.CreateItem(item, nil) != nil {
			return
		}
	}
	raw, _ := json.Marshal(metadata)
	_ = a.Store.SetDefinition(item.ID, []store.DefinitionPart{{Path: mlflowMetadataPart, PayloadType: "InlineBase64", Payload: base64.StdEncoding.EncodeToString(raw)}})
}

func (a *API) mlflowItemMetadata(itemID string) (map[string]string, error) {
	raw, err := a.definitionPart(itemID, mlflowMetadataPart)
	if err != nil {
		return nil, err
	}
	var metadata map[string]string
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

func (a *API) mirrorMLflowArtifact(wid, endpoint string, content []byte) {
	experimentID, artifactPath, ok := mlflowArtifactReference(endpoint)
	if !ok {
		return
	}
	items, err := a.Store.ListItems(wid, "MLExperiment")
	if err != nil {
		return
	}
	for _, item := range items {
		metadata, err := a.mlflowItemMetadata(item.ID)
		if err != nil || metadata["experimentId"] != experimentID {
			continue
		}
		relPath := path.Join("Files", "mlflow-artifacts", artifactPath)
		_ = a.Store.CreateOneLakePath(&store.OneLakePath{WorkspaceID: wid, ItemID: item.ID, RelPath: relPath, Content: content}, false)
		return
	}
}

func mlflowArtifactReference(endpoint string) (string, string, bool) {
	rel := strings.TrimPrefix(endpoint, "/api/2.0/mlflow-artifacts/artifacts/")
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	clean := strings.TrimPrefix(path.Clean("/"+parts[1]), "/")
	if clean != parts[1] || clean == "." || strings.HasPrefix(clean, "../") {
		return "", "", false
	}
	return parts[0], clean, true
}
