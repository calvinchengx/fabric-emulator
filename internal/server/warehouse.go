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
// is a Fabric item id; each item gets its own isolated SQL Server database.
//
// It enforces the same workspace RBAC as the rest of the emulator: the token's
// principal must have a role on the item's workspace (else the login is
// rejected), and the surface is read-only for a Lakehouse (the analytics
// endpoint) or a Viewer — read-write for a Warehouse with Contributor+.
// principalOf resolves the FedAuth token to its principal id.
func warehouseRouter(st *store.Store, be warehouseBackend, principalOf func(token string) (string, error)) func(context.Context, string, string) (bool, error) {
	return func(ctx context.Context, database, token string) (bool, error) {
		principal, err := principalOf(token)
		if err != nil {
			return false, fmt.Errorf("resolving principal: %w", err)
		}
		it, err := st.GetItemByID(database)
		if err != nil {
			return false, fmt.Errorf("database %q is not a lakehouse or warehouse in this workspace", database)
		}
		role, err := st.RoleOf(it.WorkspaceID, principal)
		if err != nil {
			return false, fmt.Errorf("checking access: %w", err)
		}
		if role == "" {
			return false, fmt.Errorf("access denied: the principal has no role on the workspace of %q", database)
		}
		if err := be.EnsureDatabase(ctx, it.ID); err != nil {
			return false, fmt.Errorf("preparing database: %w", err)
		}
		// A lakehouse endpoint is always read-only; a warehouse is read-write for
		// Contributor and above, read-only for a Viewer.
		readOnly := it.Type == "Lakehouse" || store.RoleRank(role) < store.RoleRank(store.RoleContributor)
		switch it.Type {
		case "Lakehouse":
			if _, err := warehouse.Reflect(ctx, be.DB(it.ID), st, it.ID); err != nil {
				return false, fmt.Errorf("reflecting lakehouse: %w", err)
			}
			return readOnly, nil
		case "Warehouse":
			return readOnly, nil
		default:
			return false, fmt.Errorf("item %q (type %s) has no SQL endpoint", database, it.Type)
		}
	}
}
