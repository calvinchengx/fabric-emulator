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
		read, err := readDirectLakeSQLTable(ctx, db, table)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: the SQL analytics endpoint refused the read: %w", table.Name, err)
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
func readDirectLakeSQLTable(ctx context.Context, db *sql.DB, table *semanticmodel.Table) (*warehouse.Table, error) {
	schema := table.DirectLake.SchemaName
	if schema == "" {
		schema = "dbo"
	}
	from, err := quoteSQLNames(schema, table.DirectLake.EntityName)
	if err != nil {
		return nil, err
	}
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
	rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(cols, ", ")+" FROM "+strings.Join(from, "."))
	if err != nil {
		return nil, err
	}
	return scanSQLTable(rows)
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
