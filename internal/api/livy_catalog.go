package api

// Lakehouse tables -> the Spark session catalog.
//
// On real Fabric a Lakehouse's `Tables/` ARE catalog tables: attach a notebook
// and `SELECT * FROM silver_customers` resolves, because Fabric keeps a
// metastore in step with the folder. Nothing in this stack holds a metastore —
// Sail is handed object storage and nothing else — so until this existed, every
// Spark client had to address data by path (`abfs://…/Tables/x`) and a client
// that resolved a table by NAME simply failed.
//
// That had gone unnoticed because every earlier consumer used paths: the
// notebook step writes an explicit abfs URI, and the Livy e2e execs Python.
// Microsoft's dbt-fabricspark is the first client here that compiles a model
// referencing a source by name, which is what surfaced it.
//
// So when a Livy session opens against a lakehouse, enumerate its Delta tables
// and register them in the agent's Spark catalog. The enumeration is the same
// one warehouse.Reflect does to build the SQL analytics endpoint — the two
// surfaces now expose the same set of tables, which is the property that was
// missing.

import (
	"fmt"
	"log"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// hasDeltaLog reports whether a folder's immediate children mark it as a Delta
// table. IsDir is deliberately not required of the match: which shape a store
// lists _delta_log as is an implementation detail, and missing it here
// reclassifies a real table as a schema.
func hasDeltaLog(children []*store.OneLakePath) bool {
	for _, c := range children {
		if strings.HasSuffix(c.RelPath, "/_delta_log") {
			return true
		}
	}
	return false
}

// isSchemaDir is the discriminator between a table folder and a schema folder
// under Tables/. Fabric's schema-enabled lakehouses nest tables one level down
// (Tables/<schema>/<table>), and only the table level carries _delta_log.
//
// The default is TABLE, because that was the contract before schemas were
// modelled at all and the registration path is tolerant of a folder that turns
// out not to be one (skipped, logged). A folder is a SCHEMA only when nothing
// about it says table: no _delta_log, and no loose files either — data files
// sit inside tables, never directly inside a schema. That leaves two schema
// shapes: a folder of sub-folders, and an EMPTY folder, which is what a
// provisioned but not-yet-written schema looks like at the start of a first
// run.
func isSchemaDir(children []*store.OneLakePath) bool {
	if hasDeltaLog(children) {
		return false
	}
	for _, c := range children {
		if !c.IsDir {
			return false
		}
	}
	return true
}

// registerLakehouseTables declares the lakehouse's Delta tables to the Spark
// agent under a schema named after the lakehouse.
//
// Best-effort by design: a lakehouse with no Tables/ yet is the normal case at
// session-open time, and failing session creation over it would break every
// notebook that creates its tables later. Failures are logged rather than
// swallowed — an unregistered table shows up as "table not found" much later,
// and a silent cause is the worst version of that.
func (a *API) registerLakehouseTables(session, wid, lid string) {
	it, err := a.Store.GetItem(wid, lid)
	if err != nil {
		// A binding naming a lakehouse this workspace does not have is how a
		// STALE deployment presents (an emulator recreate minted new ids and
		// the notebook still carries the old ones). Skipping silently here
		// cost real diagnosis time: no catalog, no Files mount, and the first
		// visible symptom was a FileNotFoundError deep inside a notebook.
		log.Printf("livy: session %s binds lakehouse %s which does not exist in "+
			"workspace %s; no catalog or Files mount for this session "+
			"(stale deployment? redeploy after re-provisioning)", session, lid, wid)
		return
	}
	// Ask the agent to mirror this lakehouse's Files/ at /lakehouse/default —
	// the mount a real Fabric runtime provides and notebook code reads relative
	// paths against (contract files, config). A separate call from /register on
	// purpose: the catalog is about Tables/, the mount is about Files/, and a
	// lakehouse holding only files still deserves its mount (the register below
	// deliberately says nothing when there are no tables). Best-effort like the
	// rest of this function; an agent predating /mount answers 404 and the
	// notebook keeps whatever was staged by hand.
	if err := a.mountLakehouseFiles(session, wid, lid); err != nil {
		log.Printf("livy: lakehouse %s Files did NOT mount for session %s: %v",
			lid, session, err)
	}
	dirs, err := a.Store.ListOneLakePaths(lid, "Tables", false)
	if err != nil {
		return
	}
	// The account-prefixed OneLake form a Fabric notebook uses, so a
	// registered location is the same string a user would have written.
	loc := func(rel string) string {
		return fmt.Sprintf("abfs://%s@onelake.dfs.fabric.microsoft.com/%s/%s", wid, lid, rel)
	}
	tables := make([]map[string]string, 0, len(dirs))
	schemas := make([]map[string]string, 0)
	for _, d := range dirs {
		if !d.IsDir {
			continue
		}
		name := strings.TrimPrefix(d.RelPath, "Tables/")
		if name == "" {
			continue
		}
		children, err := a.Store.ListOneLakePaths(lid, d.RelPath, false)
		if err != nil {
			continue
		}
		if !isSchemaDir(children) {
			tables = append(tables, map[string]string{
				"name": name, "location": loc(d.RelPath)})
			continue
		}
		// A schema folder (Fabric's schema-enabled lakehouse layout,
		// Tables/<schema>/<table>) is registered WITH its location so a
		// schema-qualified write lands in the lakehouse: a schema created
		// bare lives in the engine's own warehouse, and
		// `saveAsTable("bronze.x")` then goes green while writing nothing
		// anybody can find, the exact failure bindDefaultLakehouse exists to
		// prevent for unqualified names.
		schemas = append(schemas, map[string]string{
			"name": name, "location": loc(d.RelPath)})
		for _, c := range children {
			tname := strings.TrimPrefix(c.RelPath, d.RelPath+"/")
			if tname == "" {
				continue
			}
			grand, err := a.Store.ListOneLakePaths(lid, c.RelPath, false)
			if err != nil || !hasDeltaLog(grand) {
				continue
			}
			tables = append(tables, map[string]string{
				"name": tname, "schema": name, "location": loc(c.RelPath)})
		}
	}
	if len(tables) == 0 && len(schemas) == 0 {
		return
	}
	out, err := a.agentPost("/register", map[string]any{
		"session": session, "schema": it.DisplayName,
		"schemas": schemas, "tables": tables})
	if err != nil {
		log.Printf("livy: registering %d table(s) of lakehouse %s with the Spark agent: %v",
			len(tables), it.DisplayName, err)
		return
	}
	// Report a PARTIAL failure too, not just a total one. The agent answers
	// {"registered": N, "skipped": [...]} when some tables would not register,
	// and checking only for an "error" key made that invisible — the table then
	// surfaces as "table not found" much later, with nothing pointing here.
	if msg, ok := out["error"].(string); ok && msg != "" {
		log.Printf("livy: Spark agent could not register tables of lakehouse %s: %s",
			it.DisplayName, msg)
		return
	}
	if skipped, ok := out["skipped"].([]any); ok && len(skipped) > 0 {
		log.Printf("livy: %d of %d table(s) of lakehouse %s did not register: %v",
			len(skipped), len(tables), it.DisplayName, skipped)
	}
}

func (a *API) mountLakehouseFiles(session, wid, lid string) error {
	out, err := a.agentPost("/mount", map[string]any{
		"session": session, "workspace": wid, "lakehouse": lid})
	if err != nil {
		return fmt.Errorf("mounting lakehouse Files: %w", err)
	}
	if mounted, _ := out["mounted"].(bool); !mounted {
		// The agent answers 200 with mounted:false when it could not mirror
		// (its error rides in the body). Treating that as success is how a
		// missing mount stayed invisible until the code using /lakehouse hit
		// FileNotFoundError; the cause belongs at bind time.
		return fmt.Errorf("%v", out["error"])
	}
	return nil
}

// applyEnvironment hands a session's Environment to the agent: the packages to
// install and the Spark config to apply, before the first statement runs.
//
// This closes the gap docs/37 §1 names — the parse existed and nothing read its
// answer, so a run REPORTED an environment while the session never RECEIVED
// one. `/opt/wheels` demotes from *the* mechanism to the fallback it should be:
// an Environment item drives installs when one is bound, the bind-mount still
// serves consumers who do not model one.
//
// Best-effort on the same contract as /mount: an agent predating /environment
// answers 404 and the session keeps whatever the image shipped. Failures are
// LOGGED rather than swallowed — a missing package resurfaces much later as a
// ModuleNotFoundError inside a notebook, and a silent cause is the worst version
// of that.
// envOutcome says whether a session actually got the environment it binds.
//
// The reason it exists: applying used to be fire-and-forget, so a run whose
// environment could not be read, or which the agent declined, still finished
// Completed with the run detail reporting the Environment as honoured. The
// caller then hit ModuleNotFoundError inside a cell, with nothing anywhere
// saying the packages had never been installed. A resolved, reported, ignored
// dependency is the worst of the three, and reporting it as honoured is what
// made it so.
type envOutcome struct {
	OK     bool
	Reason string
}

func envApplied() envOutcome { return envOutcome{OK: true} }
func envFailed(f string, a ...any) envOutcome {
	return envOutcome{Reason: fmt.Sprintf(f, a...)}
}

func (a *API) applyEnvironment(session, wid, envID string) envOutcome {
	if envID == "" {
		return envApplied()
	}
	env, err := a.resolveEnvironment(wid, envID)
	if err != nil {
		log.Printf("livy: session %s binds environment %s which cannot be read: %v",
			session, envID, err)
		return envFailed("environment %s cannot be read: %v", envID, err)
	}
	if len(env.PythonPackages) == 0 && len(env.SparkConfig) == 0 && len(env.JARs) == 0 {
		// Nothing to install is not a failure: the binding is honoured by
		// having nothing to do.
		return envApplied()
	}
	out, err := a.agentPost("/environment", map[string]any{
		"session": session, "environment": envID,
		"packages": env.PythonPackages, "sparkConfig": env.SparkConfig, "jars": env.JARs})
	if err != nil {
		log.Printf("livy: applying environment %s to session %s: %v", envID, session, err)
		return envFailed("environment %s could not be applied: %v", envID, err)
	}
	// The agent answers applied:false with a reason when it declines — most
	// importantly when another session already bound a DIFFERENT environment.
	// Fabric isolates those per container and this emulator cannot, so refusing
	// is the honest answer; letting the last bind win would corrupt a dependency
	// tree for a session that never asked (docs/37 §1).
	if applied, _ := out["applied"].(bool); !applied {
		reason, _ := out["reason"].(string)
		log.Printf("livy: environment %s NOT applied to session %s: %s",
			envID, session, reason)
		return envFailed("environment %s was not applied: %s", envID, reason)
	}
	return envApplied()
}
