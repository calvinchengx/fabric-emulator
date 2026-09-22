package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
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
func warehouseRouter(st *store.Store, be warehouseBackend, principalOf func(token string) (string, error), external warehouse.ExternalDelta) func(context.Context, string, string, string) (tds.Connection, error) {
	// One Reflector for the life of the server, captured here rather than made
	// per connection — its whole value is remembering across logins. See its
	// doc: without that memory a retrying client restarts the entire reflection
	// every attempt and can only finish if one attempt fits inside the login
	// timeout.
	reflector := &warehouse.Reflector{External: external}
	return func(ctx context.Context, server, database, token string) (tds.Connection, error) {
		principal, err := principalOf(token)
		if err != nil {
			return tds.Connection{}, fmt.Errorf("resolving principal: %w", err)
		}
		it, err := resolveSQLItem(st, server, database)
		if err != nil {
			return tds.Connection{}, err
		}
		access, grants, err := sqlAccess(st, it, principal)
		if err != nil {
			return tds.Connection{}, err
		}
		role := access.Role
		if err := be.EnsureDatabase(ctx, it.ID); err != nil {
			return tds.Connection{}, fmt.Errorf("preparing database: %w", err)
		}
		// A lakehouse endpoint is always read-only; a warehouse is read-write for
		// Contributor and above, read-only for a Viewer.
		readOnly := it.Type == "Lakehouse" || store.RoleRank(role) < store.RoleRank(store.RoleContributor)
		switch it.Type {
		case "Lakehouse":
			if _, err := reflector.Reflect(ctx, be.DB(it.ID), st, it.ID); err != nil {
				return tds.Connection{}, fmt.Errorf("reflecting lakehouse: %w", err)
			}
			if err := syncIfUserIdentity(ctx, be, st, it); err != nil {
				return tds.Connection{}, err
			}
			// The database rung, which is NOT the same question as read-only: a
			// Contributor may write but must not be able to rewrite the security
			// policy that constrains them. Admin and Member own the item in Fabric's
			// model — "can edit OneLake security roles" is exactly those two — so they
			// are the ones who can author here too. In user identity mode the rung
			// below Contributor is CONNECT: the workspace sweep decides it.
			dbRole := targetGrant(grants, it.ID).Role
			return tds.Connection{
				TargetDB: it.ID, ReadOnly: readOnly, AnalyticsEndpoint: true, Principal: principal, Role: dbRole,
				Grants: grants,
			}, nil
		case "Warehouse", "SQLDatabase":
			// A Warehouse and a Fabric SQL Database are both read-write T-SQL over
			// their own SQL Server database (the SQL Database is OLTP and also mirrors
			// to OneLake Delta — see warehouse.Mirror).
			// The database rung, which is NOT the same question as read-only: a
			// Contributor may write but must not be able to rewrite the security
			// policy that constrains them. Admin and Member own the item in Fabric's
			// model — "can edit OneLake security roles" is exactly those two — so they
			// are the ones who can author here too.
			dbRole := dbRung(role, access, readOnly)
			return tds.Connection{
				TargetDB: it.ID, ReadOnly: readOnly, Principal: principal, Role: dbRole,
				Grants: grants,
			}, nil
		default:
			return tds.Connection{}, fmt.Errorf("item %q (type %s) has no SQL endpoint", database, it.Type)
		}
	}
}

// sqlAccess is a principal's access to one SQL item and the rung it gets in every
// database its workspace reaches. It is the one decision behind both a relayed
// connection and a server-side read on the caller's behalf (sqlDBAsFor).
//
// Access to the endpoint is Read on the item, from a workspace role or a grant:
// sharing a lakehouse "also grants access to the SQL analytics endpoint", and
// Read is what lets a principal "connect to the Warehouse or SQL analytics
// endpoint".
func sqlAccess(st *store.Store, it *store.Item, principal string) (store.Access, []tds.Grant, error) {
	access, err := st.EffectiveItemAccess(it, principal)
	if err != nil {
		return store.Access{}, nil, fmt.Errorf("checking access: %w", err)
	}
	if !access.Has(store.PermRead) {
		return store.Access{}, nil, fmt.Errorf("access denied: the principal has no role on the workspace of %q "+
			"and no Read permission on it", it.DisplayName)
	}
	grants, err := workspaceGrants(st, it.WorkspaceID, principal, access.Role)
	if err != nil {
		return store.Access{}, nil, fmt.Errorf("checking access: %w", err)
	}
	return access, grants, nil
}

// targetGrant is the workspace sweep's grant for one database. The sweep lists
// every SQL item in the connected item's workspace, so the target is always
// there.
func targetGrant(grants []tds.Grant, database string) tds.Grant {
	for _, g := range grants {
		if g.Database == database {
			return g
		}
	}
	return tds.Grant{Database: database, Role: tds.RoleNone}
}

// syncIfUserIdentity syncs a lakehouse's OneLake security into its endpoint
// when the endpoint is in user identity mode. It runs on every connect and
// every server-side read, after reflection, so the synced grants follow both
// the roles and the tables: Fabric syncs "up to 5 minutes" after a change, the
// emulator on next use.
func syncIfUserIdentity(ctx context.Context, be warehouseBackend, st *store.Store, lake *store.Item) error {
	mode, err := st.DataAccessMode(lake)
	if err != nil {
		return fmt.Errorf("checking access: %w", err)
	}
	if mode != store.AccessModeUserIdentity {
		return nil
	}
	return syncOneLakeRoles(ctx, be.DB(lake.ID), st, lake)
}

// sqlAddressable are the item types that have a T-SQL endpoint. A workspace
// role reaches all of them, which is what workspaceGrants encodes.
var sqlAddressable = []string{"Lakehouse", "Warehouse", "SQLDatabase"}

// dbRung is the database rung a principal gets on one item, from its workspace
// role and its effective access to the item.
//
// NOT the same question as read-only: a Contributor may write but must not be
// able to rewrite the security policy that constrains them. Admin and Member
// own the item in Fabric's model, "can edit OneLake security roles" is exactly
// those two, so they are the ones who can author here too.
//
// Below Contributor the rung is the item permission: ReadData reads ("read data
// through T-SQL"), Read alone only connects, and without Read there is no access
// to the database at all — which is what makes a revoked grant stop working
// rather than lingering in a database the principal was once provisioned in.
func dbRung(role string, access store.Access, readOnly bool) tds.Role {
	switch {
	case store.RoleRank(role) >= store.RoleRank(store.RoleMember):
		return tds.RoleOwner
	case !readOnly:
		return tds.RoleWriter
	case access.Has(store.PermReadData):
		return tds.RoleReader
	case access.Has(store.PermRead):
		return tds.RoleConnect
	default:
		return tds.RoleNone
	}
}

// workspaceGrants is every SQL-addressable item in the workspace, with the rung
// this caller gets on each — including RoleNone for items it may not reach.
//
// WHY THE WHOLE WORKSPACE AND NOT JUST THE ONE CONNECTED TO. Gold is a
// Warehouse that reads silver out of a Lakehouse by three-part name. That
// crosses databases, and SQL Server needs the caller to exist in BOTH: with a
// user in only the connect target it answers 916, "not able to access the
// database under the current security context", from inside a statement on a
// login that already succeeded. Fabric grants by workspace role, so having
// access to one item's endpoint and not its neighbour's is not a shape the real
// service has — except through item permissions, which is why each item's rung
// is decided separately.
//
// WHY RoleNone IS LISTED RATHER THAN OMITTED. A principal shared two warehouses
// who loses one keeps a database user in it, and a three-part name from the one
// it still holds would reach straight in. Naming the lost item lets the
// provisioner take CONNECT away there.
//
// FAILS CLOSED. This used to be best effort, which was safe while it only ever
// added access. It now also removes access, and a store error that silently
// skipped an item would leave a revoked grant working — so the error refuses the
// connection instead.
func workspaceGrants(st *store.Store, workspaceID, principalID, role string) ([]tds.Grant, error) {
	var out []tds.Grant
	for _, t := range sqlAddressable {
		items, err := st.ListItems(workspaceID, t)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			g, err := st.GetItemAccess(it.ID, principalID)
			if err != nil {
				return nil, err
			}
			access := store.MergeAccess(role, it.Type, g)
			// A lakehouse in user identity mode takes its table access from OneLake
			// security — as a three-part name from a neighbour too, since the rung
			// and memberships are the same whichever database is connected to.
			if t == "Lakehouse" {
				mode, err := st.DataAccessMode(it)
				if err != nil {
					return nil, err
				}
				if mode == store.AccessModeUserIdentity {
					og, err := oneLakeGrant(st, it, principalID, role, access)
					if err != nil {
						return nil, err
					}
					out = append(out, og)
					continue
				}
			}
			// A lakehouse endpoint is read-only whatever the role; a warehouse
			// follows the role. Same rule the target uses.
			readOnly := t == "Lakehouse" || store.RoleRank(role) < store.RoleRank(store.RoleContributor)
			out = append(out, tds.Grant{Database: it.ID, Role: dbRung(role, access, readOnly)})
		}
	}
	return out, nil
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

// sqlDBFor builds the control-plane SQL hook the pipeline Script/StoredProcedure
// activities use: only a Warehouse or SQLDatabase item has a SQL endpoint,
// ensured (its own database prepared) before handing back the connection.
func sqlDBFor(be warehouseBackend, st *store.Store) func(ctx context.Context, itemID string) (*sql.DB, error) {
	return func(ctx context.Context, itemID string) (*sql.DB, error) {
		it, err := st.GetItemByID(itemID)
		if err != nil {
			return nil, fmt.Errorf("item %q not found", itemID)
		}
		if it.Type != "Warehouse" && it.Type != "SQLDatabase" {
			return nil, fmt.Errorf("item %q (type %s) has no SQL endpoint", itemID, it.Type)
		}
		if err := be.EnsureDatabase(ctx, itemID); err != nil {
			return nil, fmt.Errorf("preparing database: %w", err)
		}
		return be.DB(itemID), nil
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

// lakehouseDBFor builds the hook the SQL-analytics-endpoint refresh uses: a
// Lakehouse's own SQL Server database, prepared if it does not exist.
//
// Separate from sqlDBFor, which refuses a Lakehouse ON PURPOSE — it serves the
// pipeline Script/StoredProcedure activities, and letting those write into a
// lakehouse's reflected database would invent a write path Fabric's read-only
// analytics endpoint does not have. Same backend, different permission.
func lakehouseDBFor(be warehouseBackend, st *store.Store) func(ctx context.Context, itemID string) (*sql.DB, error) {
	return func(ctx context.Context, itemID string) (*sql.DB, error) {
		it, err := st.GetItemByID(itemID)
		if err != nil {
			return nil, fmt.Errorf("item %q not found", itemID)
		}
		if it.Type != "Lakehouse" {
			return nil, fmt.Errorf("item %q (type %s) has no SQL analytics endpoint", itemID, it.Type)
		}
		if err := be.EnsureDatabase(ctx, itemID); err != nil {
			return nil, fmt.Errorf("preparing database: %w", err)
		}
		return be.DB(itemID), nil
	}
}

// principalBackend is a backend that can log in as a caller, not only as the
// service account. The SQL Server backend is one; the router's fakes are not.
type principalBackend interface {
	warehouseBackend
	DBAs(ctx context.Context, database, principal string, grants []tds.Grant) (*sql.DB, error)
}

// sqlDBAsFor builds the hook Direct Lake on SQL reads through (docs/59): the
// item's database, logged into AS the caller after the same access decision and
// provisioning a relayed connection gets, so SELECT grants, row-level security,
// column denials and masking all apply to the read. A lakehouse's analytics
// endpoint is reflected first, as a connection to it is.
func sqlDBAsFor(be principalBackend, st *store.Store, external warehouse.ExternalDelta) func(ctx context.Context, itemID, principal string) (*sql.DB, error) {
	reflector := &warehouse.Reflector{External: external}
	return func(ctx context.Context, itemID, principal string) (*sql.DB, error) {
		it, err := st.GetItemByID(itemID)
		if err != nil {
			return nil, fmt.Errorf("item %q not found", itemID)
		}
		if it.Type != "Lakehouse" && it.Type != "Warehouse" && it.Type != "SQLDatabase" {
			return nil, fmt.Errorf("item %q (type %s) has no SQL endpoint", itemID, it.Type)
		}
		_, grants, err := sqlAccess(st, it, principal)
		if err != nil {
			return nil, err
		}
		if err := be.EnsureDatabase(ctx, it.ID); err != nil {
			return nil, fmt.Errorf("preparing database: %w", err)
		}
		if it.Type == "Lakehouse" {
			if _, err := reflector.Reflect(ctx, be.DB(it.ID), st, it.ID); err != nil {
				return nil, fmt.Errorf("reflecting lakehouse: %w", err)
			}
			if err := syncIfUserIdentity(ctx, be, st, it); err != nil {
				return nil, err
			}
		}
		return be.DBAs(ctx, it.ID, principal, grants)
	}
}
