package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

// warehouseBackend is the slice of the SQL Server backend the router needs:
// prepare an item's isolated database and hand back its connection pool.
type warehouseBackend interface {
	EnsureDatabase(ctx context.Context, database string) error
	DB(database string) *sql.DB
}

// warehouseRouter builds the TDS OnConnect callback. The connection addresses a
// lakehouse/warehouse either by item id (GUID) or by display name — real
// Fabric's addressing, where the workspace is encoded in the server name. Each
// item gets its own isolated SQL Server database (named by the item id), which
// the router returns so the query loop routes there regardless of how the
// client named it.
//
// It enforces the same workspace RBAC as the rest of the emulator: the token's
// principal must have a role on the item's workspace (else the login is
// rejected), and the surface is read-only for a Lakehouse (the analytics
// endpoint) or a Viewer — read-write for a Warehouse with Contributor+.
// principalOf resolves the FedAuth token to its principal id.
func warehouseRouter(st *store.Store, be warehouseBackend, principalOf func(token string) (string, error)) func(context.Context, string, string, string) (string, bool, error) {
	return func(ctx context.Context, server, database, token string) (string, bool, error) {
		principal, err := principalOf(token)
		if err != nil {
			return "", false, fmt.Errorf("resolving principal: %w", err)
		}
		it, err := resolveSQLItem(st, server, database)
		if err != nil {
			return "", false, err
		}
		role, err := st.RoleOf(it.WorkspaceID, principal)
		if err != nil {
			return "", false, fmt.Errorf("checking access: %w", err)
		}
		if role == "" {
			return "", false, fmt.Errorf("access denied: the principal has no role on the workspace of %q", database)
		}
		if err := be.EnsureDatabase(ctx, it.ID); err != nil {
			return "", false, fmt.Errorf("preparing database: %w", err)
		}
		// A lakehouse endpoint is always read-only; a warehouse is read-write for
		// Contributor and above, read-only for a Viewer.
		readOnly := it.Type == "Lakehouse" || store.RoleRank(role) < store.RoleRank(store.RoleContributor)
		switch it.Type {
		case "Lakehouse":
			if _, err := warehouse.Reflect(ctx, be.DB(it.ID), st, it.ID); err != nil {
				return "", false, fmt.Errorf("reflecting lakehouse: %w", err)
			}
			return it.ID, readOnly, nil
		case "Warehouse", "SQLDatabase":
			// A Warehouse and a Fabric SQL Database are both read-write T-SQL over
			// their own SQL Server database (the SQL Database is OLTP and also mirrors
			// to OneLake Delta — see warehouse.Mirror).
			return it.ID, readOnly, nil
		default:
			return "", false, fmt.Errorf("item %q (type %s) has no SQL endpoint", database, it.Type)
		}
	}
}

// mirrorItem builds the control-plane mirror hook: ensure the item's SQL Server
// database exists, then snapshot its tables to OneLake Delta (warehouse.Mirror).
func mirrorItem(be warehouseBackend, st *store.Store) func(ctx context.Context, itemID string) error {
	return func(ctx context.Context, itemID string) error {
		if err := be.EnsureDatabase(ctx, itemID); err != nil {
			return fmt.Errorf("preparing database: %w", err)
		}
		return warehouse.Mirror(ctx, be.DB(itemID), st, itemID)
	}
}

// resolveSQLItem finds the lakehouse/warehouse a connection addresses. It first
// tries the database as an item id (GUID) — workspace-agnostic, and how the
// emulator's own tooling connects. Failing that, the database is a display name
// (real Fabric addressing) and the workspace is taken from the server name:
// Fabric SQL connection strings use "<workspace>.datawarehouse.fabric.microsoft.com",
// so the first DNS label identifies the workspace (by id or name).
func resolveSQLItem(st *store.Store, server, database string) (*store.Item, error) {
	if it, err := st.GetItemByID(database); err == nil {
		return it, nil
	}
	ref := workspaceRef(server)
	if ref == "" {
		return nil, fmt.Errorf("database %q not found by id; to address a warehouse or lakehouse by name, put the workspace in the server name", database)
	}
	ws, err := resolveWorkspace(st, ref)
	if err != nil {
		return nil, fmt.Errorf("workspace %q (from server name %q) not found", ref, server)
	}
	// A workspace can hold a lakehouse and a warehouse; both expose a SQL
	// endpoint keyed by the item name. Prefer a Warehouse, then a Lakehouse.
	for _, typ := range []string{"Warehouse", "Lakehouse"} {
		if it, err := st.GetItemByName(ws.ID, database, typ); err == nil {
			return it, nil
		}
	}
	return nil, fmt.Errorf("no warehouse or lakehouse named %q in workspace %q", database, ref)
}

// workspaceRef extracts the workspace identifier from a Fabric SQL server name —
// the first DNS label of "<workspace>.datawarehouse.fabric.microsoft.com". A
// bare label (no dots, e.g. a test alias) is taken verbatim; an empty or
// IP-like host yields "" (no workspace addressable).
func workspaceRef(server string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return ""
	}
	label := server
	if i := strings.IndexByte(server, '.'); i >= 0 {
		label = server[:i]
	}
	// An all-numeric first label (an IPv4 host like 127.0.0.1) is not a workspace.
	if label == "" || isAllDigits(label) {
		return ""
	}
	return label
}

func isAllDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// resolveWorkspace resolves a workspace by id first, then by display name.
func resolveWorkspace(st *store.Store, ref string) (*store.Workspace, error) {
	if ws, err := st.GetWorkspace(ref); err == nil {
		return ws, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return st.GetWorkspaceByName(ref)
}
