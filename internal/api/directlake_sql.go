package api

import (
	"errors"
	"fmt"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// Direct Lake on SQL analytics endpoints (docs/59).

// errDirectLakeOnSQLNotServed names the flavour instead of failing to find a
// OneLake URL in it, which is what a Sql.Database expression used to read as.
var errDirectLakeOnSQLNotServed = errors.New("Direct Lake on SQL (Sql.Database) is recognised but not served " +
	"by this emulator yet; Direct Lake on OneLake (a onelake.dfs.fabric.microsoft.com URL) is")

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

// directLakeSQLEngine refuses by name when nothing serves the source's SQL.
func (a *API) directLakeSQLEngine(source *store.Item) error {
	if (source.Type == "Lakehouse" && a.LakehouseDB == nil) || (source.Type == "Warehouse" && a.SQLDB == nil) {
		return fmt.Errorf("this emulator serves no SQL: Direct Lake on SQL reads through the SQL analytics " +
			"endpoint, so it needs one attached")
	}
	return nil
}
