package api

import (
	"context"
	"github.com/calvinchengx/fabric-emulator/internal/httpx"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// AirflowRuntime is the deliberately small upstream boundary. Production uses
// Apache Airflow's REST API and shared DAG volume; tests substitute a witness.
type AirflowRuntime interface {
	SyncDAGs(ctx context.Context, itemID string, files map[string][]byte) error
	TriggerAndWait(ctx context.Context, itemID, dagID, runID string, conf map[string]any) error
}

func (a *API) registerAirflow(mux *http.ServeMux) {
	base := "/v1/workspaces/{wid}/apacheAirflowJobs/{iid}/files"
	mux.HandleFunc("GET "+base, a.withAuth(a.listAirflowFiles))
	mux.HandleFunc("GET "+base+"/{path...}", a.withAuth(a.getAirflowFile))
	mux.HandleFunc("PUT "+base+"/{path...}", a.withAuth(a.putAirflowFile))
	mux.HandleFunc("DELETE "+base+"/{path...}", a.withAuth(a.deleteAirflowFile))
}

func (a *API) airflowItem(w http.ResponseWriter, r *http.Request, p *auth.Principal, role string) (*store.Item, bool) {
	wid, iid := r.PathValue("wid"), r.PathValue("iid")
	if _, _, ok := a.requireRole(w, wid, p, role); !ok {
		return nil, false
	}
	it, err := a.Store.GetItem(wid, iid)
	if err != nil || it.Type != "ApacheAirflowJob" {
		writeErr(w, 404, "ItemNotFound", "The Apache Airflow Job is not available.")
		return nil, false
	}
	return it, true
}

func cleanAirflowPath(raw string) (string, bool) {
	raw = strings.TrimPrefix(raw, "/")
	clean := path.Clean(raw)
	return clean, clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func airflowStorePath(raw string) (string, bool) {
	clean, ok := cleanAirflowPath(raw)
	return "Files/" + clean, ok
}

func (a *API) listAirflowFiles(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.airflowItem(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	root, valid := cleanAirflowPath(r.URL.Query().Get("rootPath"))
	if r.URL.Query().Get("rootPath") == "" {
		root, valid = "", true
	}
	if !valid {
		writeErr(w, 400, "InvalidPath", "rootPath must stay within the job.")
		return
	}
	prefix := "Files"
	if root != "" {
		prefix += "/" + root
	}
	paths, err := a.Store.ListOneLakePaths(it.ID, prefix, true)
	if err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	value := make([]map[string]any, 0, len(paths))
	for _, file := range paths {
		if !file.IsDir {
			value = append(value, map[string]any{"filePath": strings.TrimPrefix(file.RelPath, "Files/"), "sizeInBytes": len(file.Content)})
		}
	}
	writeJSON(w, 200, map[string]any{"value": value})
}

func (a *API) getAirflowFile(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.airflowItem(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	rel, valid := airflowStorePath(r.PathValue("path"))
	if !valid {
		writeErr(w, 400, "InvalidPath", "filePath must stay within the job.")
		return
	}
	file, err := a.Store.GetOneLakePath(it.ID, rel)
	if err != nil || file.IsDir {
		writeErr(w, 404, "FileNotFound", "The file is not available.")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(file.Content)
}

func (a *API) putAirflowFile(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.airflowItem(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	rel, valid := airflowStorePath(r.PathValue("path"))
	if !valid {
		writeErr(w, 400, "InvalidPath", "filePath must stay within the job.")
		return
	}
	// A file WRITE: the bytes are stored as the DAG. The UTF-8 check below
	// would catch some truncations of a .py file and none of any other kind,
	// which is not a bound.
	raw, ok := httpx.ReadBounded(r.Body, httpx.MaxItemContent)
	if !ok {
		writeErr(w, http.StatusRequestEntityTooLarge, "RequestBodyTooLarge",
			"The file is too large.")
		return
	}
	if strings.HasSuffix(strings.ToLower(rel), ".py") && !utf8Valid(raw) {
		writeErr(w, 400, "InvalidFile", "Python DAG files must be UTF-8.")
		return
	}
	if err := a.Store.CreateOneLakePath(&store.OneLakePath{WorkspaceID: it.WorkspaceID, ItemID: it.ID, RelPath: rel, Content: raw}, false); err != nil {
		writeErr(w, 500, "InternalError", err.Error())
		return
	}
	w.WriteHeader(200)
}

func utf8Valid(raw []byte) bool { return utf8.Valid(raw) }

func (a *API) deleteAirflowFile(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	it, ok := a.airflowItem(w, r, p, store.RoleContributor)
	if !ok {
		return
	}
	rel, valid := airflowStorePath(r.PathValue("path"))
	if !valid {
		writeErr(w, 400, "InvalidPath", "filePath must stay within the job.")
		return
	}
	if err := a.Store.DeleteOneLakePath(it.ID, rel); err != nil {
		writeErr(w, 404, "FileNotFound", "The file is not available.")
		return
	}
	w.WriteHeader(200)
}

func (a *API) runAirflow(ctx context.Context, it *store.Item, job *store.JobInstance, dagID string, conf map[string]any) {
	paths, err := a.Store.ListOneLakePaths(it.ID, "Files", true)
	files := map[string][]byte{}
	if err == nil {
		for _, file := range paths {
			if !file.IsDir && strings.HasSuffix(strings.ToLower(file.RelPath), ".py") {
				files[strings.TrimPrefix(file.RelPath, "Files/")] = file.Content
			}
		}
	}
	if err == nil && len(files) == 0 {
		err = &airflowError{"AirflowDAGFileRequired"}
	}
	if err == nil {
		// A SYNC FAILURE IS NOT A DAG FAILURE, and collapsing the two cost a
		// consumer an afternoon. The DAG directory is a volume shared with the
		// Airflow sidecar; if the emulator's uid cannot write there,
		// `os.WriteFile` returns `permission denied`, the job is finalized as
		// the generic `AirflowRunFailed`, and what the operator sees is "The
		// job failed." beside an empty dags folder -- with the real reason
		// discarded one line later. Its own code, so `jobFailureMessage` can
		// say what to check.
		if syncErr := a.Airflow.SyncDAGs(ctx, it.ID, files); syncErr != nil {
			err = &airflowError{"AirflowDAGSyncFailed"}
		}
	}
	if err == nil {
		err = a.Airflow.TriggerAndWait(ctx, it.ID, dagID, job.ID, conf)
	}
	code := ""
	if err != nil {
		code = "AirflowRunFailed"
		if e, ok := err.(*airflowError); ok {
			code = e.code
		}
	}
	_ = a.Store.FinalizeJob(it.ID, job.ID, code)
	a.publishJobOutcome(it.WorkspaceID, it.ID, job.ID, code)
}

type airflowError struct{ code string }

func (e *airflowError) Error() string { return e.code }
