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
)

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
		return
	}
	dirs, err := a.Store.ListOneLakePaths(lid, "Tables", false)
	if err != nil {
		return
	}
	tables := make([]map[string]string, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir {
			continue
		}
		name := strings.TrimPrefix(d.RelPath, "Tables/")
		if name == "" {
			continue
		}
		tables = append(tables, map[string]string{
			"name": name,
			// The account-prefixed OneLake form a Fabric notebook uses, so the
			// registered location is the same string a user would have written.
			"location": fmt.Sprintf(
				"abfs://%s@onelake.dfs.fabric.microsoft.com/%s/Tables/%s", wid, lid, name),
		})
	}
	if len(tables) == 0 {
		return
	}
	out, err := a.agentPost("/register", map[string]any{
		"session": session, "schema": it.DisplayName, "tables": tables})
	if err != nil {
		log.Printf("livy: registering %d table(s) of lakehouse %s with the Spark agent: %v",
			len(tables), it.DisplayName, err)
		return
	}
	if msg, ok := out["error"].(string); ok && msg != "" {
		log.Printf("livy: Spark agent could not register tables of lakehouse %s: %s",
			it.DisplayName, msg)
	}
}
