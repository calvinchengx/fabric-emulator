package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// Direct Lake on SQL analytics endpoints (docs/59).

// errNoOneLakeLocation is a Direct Lake on SQL expression asked for a OneLake
// location, which it does not have: it names a SQL analytics endpoint.
var errNoOneLakeLocation = errors.New("Direct Lake on SQL (Sql.Database) names a SQL analytics endpoint, " +
	"not a OneLake location")

// errCannotReadSQLSource is every way a caller fails to reach the source, alike:
// no such item, an item they hold no Read on. Somebody who cannot read it learns
// that, not whether a GUID or a name exists.
var errCannotReadSQLSource = errors.New("Direct Lake on SQL: caller cannot read the source: it needs Read on " +
	"the lakehouse or warehouse behind the SQL analytics endpoint, and SELECT on its tables")

// resolveDirectLakeSQLSource finds the lakehouse or warehouse a Sql.Database
// expression's database argument names, for a caller who may read it.
//
// The argument is the SQL analytics endpoint's GUID — a lakehouse's endpoint is
// its own item, with its own id — or a warehouse's; or either by display name,
// looked up in the model's own workspace. Fabric requires the GUID only "to use
// Edit tables and refresh", so a name must still answer a query; which workspace
// a name is looked up in is not documented, and the model's is the inference.
//
// The server argument is not consulted (docs/59): the emulator's SQL address
// depends on the host a caller used, so no tenant's host could match it.
func (a *API) resolveDirectLakeSQLSource(modelItemID string, src *semanticmodel.DirectLakeSource, p *auth.Principal) (*store.Item, error) {
	named, err := a.directLakeSQLItem(modelItemID, src.Database)
	if err != nil {
		return nil, err
	}
	data := named
	if named.Type == "SQLEndpoint" {
		props, err := a.Store.ItemProperties(named.ID)
		if err != nil {
			return nil, err
		}
		if data, err = a.Store.GetItemByID(props[propParentLakehouse]); err != nil {
			return nil, fmt.Errorf("Direct Lake on SQL: SQL analytics endpoint %q serves no lakehouse", src.Database)
		}
	}
	// Read decides before the type is described, so the refusals below tell only
	// a reader what the item is.
	access, err := a.Store.EffectiveItemAccess(data, p.ID)
	if err != nil {
		return nil, err
	}
	if !access.Has(store.PermRead) {
		return nil, errCannotReadSQLSource
	}
	switch data.Type {
	case "Lakehouse":
		if named.Type == "Lakehouse" {
			return nil, fmt.Errorf("Direct Lake on SQL: %q is a lakehouse; Sql.Database names its SQL analytics "+
				"endpoint (sqlEndpointProperties.id), which is a different item", src.Database)
		}
		return data, nil
	case "Warehouse":
		return data, nil
	}
	return nil, fmt.Errorf("Direct Lake on SQL: %q is a %s; the source must be a lakehouse's SQL analytics "+
		"endpoint or a warehouse", src.Database, data.Type)
}

// directLakeSQLItem looks the database argument up: by id anywhere, else by
// display name in the model's workspace among the items a SQL endpoint is.
func (a *API) directLakeSQLItem(modelItemID, database string) (*store.Item, error) {
	if it, err := a.Store.GetItemByID(database); err == nil {
		return it, nil
	}
	model, err := a.Store.GetItemByID(modelItemID)
	if err != nil {
		return nil, err
	}
	var found []*store.Item
	for _, typ := range []string{"SQLEndpoint", "Warehouse"} {
		if it, err := a.Store.GetItemByName(model.WorkspaceID, database, typ); err == nil {
			found = append(found, it)
		}
	}
	switch len(found) {
	case 0:
		return nil, errCannotReadSQLSource
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("Direct Lake on SQL: %q names both a SQL analytics endpoint and a warehouse in this "+
		"workspace; name the source by its GUID", database)
}

// loadDirectLakeSQLData serves every table of a Direct Lake on SQL model by
// reading through its SQL analytics endpoint AS THE CALLER.
//
// Fabric's own read is Delta through OneLake, under the model's permission, once
// the endpoint has vouched for the caller — or, when the endpoint enforces RLS,
// masking or object security, a DirectQuery fallback that queries it as the
// caller. Reading as the caller gives the second answer always, and it equals
// the first wherever the endpoint secures nothing: the rows are the same Delta
// either way. What it buys is that every permission decision stays SQL Server's
// — SELECT, column denials, predicates, masks — rather than a re-implementation
// of them here.
func (a *API) loadDirectLakeSQLData(ctx context.Context, modelItemID string, model *semanticmodel.Model,
	binding directLakeSources, data semanticmodel.Data, p *auth.Principal) error {
	source, err := a.resolveDirectLakeSQLSource(modelItemID, binding.sql, p)
	if err != nil {
		return err
	}
	if a.SQLDBAs == nil {
		return fmt.Errorf("this emulator serves no SQL: Direct Lake on SQL reads through the SQL analytics " +
			"endpoint, so it needs one attached")
	}
	db, err := a.SQLDBAs(ctx, source.ID, p.ID)
	if err != nil {
		return fmt.Errorf("Direct Lake on SQL: %w", err)
	}
	defer db.Close()
	for i := range model.Tables {
		table := &model.Tables[i]
		if table.DirectLake == nil {
			continue
		}
		object, err := sqlObjectName(table)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: %w", table.Name, err)
		}
		read, err := readDirectLakeSQLTable(ctx, db, table, object)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: the SQL analytics endpoint refused the read: %w", table.Name, err)
		}
		if model.DirectLakeBehavior == semanticmodel.DirectLakeOnly {
			if err := a.refuseDirectQueryFallback(ctx, source, table, object); err != nil {
				return err
			}
		}
		// Positional: the SELECT named the model's columns in order.
		rows := make([]semanticmodel.Row, 0, len(read.Rows))
		for _, cells := range read.Rows {
			row := semanticmodel.Row{}
			for j, c := range table.Columns {
				row[c.Name] = cells[j]
			}
			rows = append(rows, row)
		}
		data[table.Name] = rows
	}
	return nil
}

// readDirectLakeSQLTable selects a model table's source columns BY NAME, so a
// column the endpoint denies the caller fails the read the way it fails a query
// in Fabric, rather than being skipped by a SELECT * that never asked for it.
func readDirectLakeSQLTable(ctx context.Context, db *sql.DB, table *semanticmodel.Table, object string) (*warehouse.Table, error) {
	var sources []string
	for _, c := range table.Columns {
		source := c.SourceColumn
		if source == "" {
			source = c.Name
		}
		sources = append(sources, source)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("the model table has no columns to read")
	}
	cols, err := quoteSQLNames(sources...)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(cols, ", ")+" FROM "+object)
	if err != nil {
		return nil, err
	}
	return scanSQLTable(rows)
}

// sqlObjectName is a Direct Lake table's entity as a quoted two-part name,
// schema dbo when the partition names none.
func sqlObjectName(table *semanticmodel.Table) (string, error) {
	schema := table.DirectLake.SchemaName
	if schema == "" {
		schema = "dbo"
	}
	names, err := quoteSQLNames(schema, table.DirectLake.EntityName)
	if err != nil {
		return "", err
	}
	return strings.Join(names, "."), nil
}

// quoteSQLNames bracket-quotes identifiers. SQL has no placeholder for a name;
// doubling the one character that closes a bracket makes any printable name
// safe, and a name that could not exist in Fabric — empty, over 128 characters,
// or carrying a control character — is refused rather than quoted.
func quoteSQLNames(names ...string) ([]string, error) {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" || len(n) > 128 || strings.ContainsFunc(n, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, fmt.Errorf("unusable SQL name %q", n)
		}
		out = append(out, "["+strings.ReplaceAll(n, "]", "]]")+"]")
	}
	return out, nil
}

// refuseDirectQueryFallback is directLakeBehavior directLakeOnly: "if conditions
// aren't met, the query fails with an error". The conditions checked are the
// ones a table carries at the endpoint — row-level security, dynamic data
// masking, or being a view — each of which makes Fabric fall back to
// DirectQuery under automatic. The column- and table-level denials that fail a
// query under every behaviour have already failed the caller's read, which runs
// first, in the order Fabric evaluates them. Guardrails and framing are not
// modelled (docs/59), so neither can trigger a fallback here.
//
// Asked through the service connection, not the caller's: a caller the policy
// restricts is exactly the one who may not be allowed to see the policy, and
// reading "no policy" from their view of the catalog would serve a table the
// author asked to fail. It reads the catalog only, never a row.
func (a *API) refuseDirectQueryFallback(ctx context.Context, source *store.Item, table *semanticmodel.Table, object string) error {
	open := a.SQLDB
	if source.Type == "Lakehouse" {
		open = a.LakehouseDB
		// "The SQL analytics endpoint can be changed to SSO. When this happens,
		// OneLake security roles are added as SQL granular access control rules …
		// At this point, Direct Lake on SQL falls back to DirectQuery 100% of the
		// time." Every table, whatever its catalog says (docs/60).
		mode, err := a.Store.DataAccessMode(source)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: reading the endpoint's access mode: %w", table.Name, err)
		}
		if mode == store.AccessModeUserIdentity {
			return fmt.Errorf("Direct Lake table %q cannot stay in Direct Lake mode and directLakeBehavior is "+
				"directLakeOnly, which disables DirectQuery fallback: the SQL analytics endpoint is in user identity "+
				"access mode, where Direct Lake on SQL always falls back", table.Name)
		}
	}
	if open == nil {
		return fmt.Errorf("Direct Lake table %q: directLakeOnly needs the endpoint's catalog, and this emulator "+
			"serves no service connection to read it", table.Name)
	}
	db, err := open(ctx, source.ID)
	if err != nil {
		return fmt.Errorf("Direct Lake table %q: reading the endpoint's catalog: %w", table.Name, err)
	}
	causes, err := directQueryFallbackCauses(ctx, db, object)
	if err != nil {
		return fmt.Errorf("Direct Lake table %q: reading the endpoint's catalog: %w", table.Name, err)
	}
	if len(causes) > 0 {
		return fmt.Errorf("Direct Lake table %q cannot stay in Direct Lake mode and directLakeBehavior is "+
			"directLakeOnly, which disables DirectQuery fallback: %s", table.Name, strings.Join(causes, "; "))
	}
	return nil
}

// directQueryFallbackCauses names what at the endpoint would send a table to
// DirectQuery: "tables that have SQL row-level security (RLS) defined", "tables
// that have SQL dynamic data masking (DDM) defined", and "tables based on
// unmaterialized SQL views". A variable so the API tests, which have no SQL
// Server, can stand in for the catalog; the real query is witnessed in
// internal/server/directlake_sql_test.go.
var directQueryFallbackCauses = func(ctx context.Context, db *sql.DB, object string) ([]string, error) {
	var rls, ddm, view int
	err := db.QueryRowContext(ctx, `
SELECT
  (SELECT COUNT(*) FROM sys.security_predicates sp
     JOIN sys.security_policies p ON p.object_id = sp.object_id
    WHERE sp.target_object_id = OBJECT_ID(@object) AND p.is_enabled = 1),
  (SELECT COUNT(*) FROM sys.masked_columns WHERE object_id = OBJECT_ID(@object)),
  (SELECT COUNT(*) FROM sys.views WHERE object_id = OBJECT_ID(@object))`,
		sql.Named("object", object)).Scan(&rls, &ddm, &view)
	if err != nil {
		return nil, err
	}
	var causes []string
	if rls > 0 {
		causes = append(causes, "the SQL analytics endpoint enforces row-level security on "+object)
	}
	if ddm > 0 {
		causes = append(causes, "the SQL analytics endpoint masks columns of "+object)
	}
	if view > 0 {
		causes = append(causes, object+" is a SQL view")
	}
	return causes, nil
}
