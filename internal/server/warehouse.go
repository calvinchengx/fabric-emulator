package server

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// warehouseBackend is the slice of the SQL Server backend the router needs:
// prepare an item's isolated database and hand back its connection pool.
type warehouseBackend interface {
	EnsureDatabase(ctx context.Context, database string) error
	DB(database string) *sql.DB
}

// warehouseRouter builds the TDS OnConnect callback. The connection's database
// is a Fabric item id; each item gets its own isolated SQL Server database. A
// Lakehouse is the read-only SQL analytics endpoint — its Delta is reflected in
// on connect; a Warehouse is read-write. Returns whether the surface is
// read-only, and errors (rejecting the login) for unknown or non-SQL items.
func warehouseRouter(st *store.Store, be warehouseBackend) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, database string) (bool, error) {
		it, err := st.GetItemByID(database)
		if err != nil {
			return false, fmt.Errorf("database %q is not a lakehouse or warehouse in this workspace", database)
		}
		if err := be.EnsureDatabase(ctx, it.ID); err != nil {
			return false, fmt.Errorf("preparing database: %w", err)
		}
		switch it.Type {
		case "Lakehouse":
			if _, err := warehouse.Reflect(ctx, be.DB(it.ID), st, it.ID); err != nil {
				return false, fmt.Errorf("reflecting lakehouse: %w", err)
			}
			return true, nil // read-only analytics endpoint
		case "Warehouse":
			return false, nil // read-write
		default:
			return false, fmt.Errorf("item %q (type %s) has no SQL endpoint", database, it.Type)
		}
	}
}
