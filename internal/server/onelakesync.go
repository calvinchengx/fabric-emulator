package server

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
	"github.com/calvinchengx/fabric-emulator/pkg/onelakesec"
)

// OneLake security synced into a lakehouse's SQL analytics endpoint in user
// identity access mode (docs/60).
//
// Fabric's "security sync service … translat[es] OneLake-defined policies (RLS,
// CLS, OLS) into equivalent SQL-compatible database role structures", and
// "OneLake security roles are propagated to the SQL analytics endpoint with the
// OLS_ prefix". This does the same, into the real engine: the rules are SQL
// Server's to enforce once they are there.

// olsPrefix is Fabric's prefix for a synced OneLake security role.
const olsPrefix = "OLS_"

// syncGuard is the database DDL trigger that keeps table security OneLake's:
// in user identity mode "SQL GRANT/REVOKE isn't allowed" on tables, and "you
// can't define RLS, CLS, or OLS directly using T-SQL". A GRANT on a view,
// function or procedure is left alone, as Fabric allows. The sync itself runs as
// the service account and passes.
const syncGuard = `CREATE TRIGGER [OLS_guard] ON DATABASE
FOR DDL_GDR_DATABASE_EVENTS, CREATE_SECURITY_POLICY, ALTER_SECURITY_POLICY
AS
BEGIN
  SET NOCOUNT ON;
  IF IS_SRVROLEMEMBER('sysadmin') = 1 RETURN;
  DECLARE @e xml = EVENTDATA();
  DECLARE @event nvarchar(100) = @e.value('(/EVENT_INSTANCE/EventType)[1]', 'nvarchar(100)');
  DECLARE @object nvarchar(100) = @e.value('(/EVENT_INSTANCE/ObjectType)[1]', 'nvarchar(100)');
  IF @event IN ('CREATE_SECURITY_POLICY', 'ALTER_SECURITY_POLICY')
     OR @object = 'TABLE'
     OR (@object IN ('SCHEMA', 'DATABASE')
         AND @e.exist('/EVENT_INSTANCE/Permissions/Permission[.="SELECT" or .="CONTROL"]') = 1)
  BEGIN
    ROLLBACK;
    THROW 50000, N'This SQL analytics endpoint is in user identity access mode: table access, row-level and column-level security are governed by OneLake security, so GRANT, DENY and REVOKE on tables and security policies cannot be authored in T-SQL.', 1;
  END
END`

// oneLakeRoleName is the database role a OneLake security role syncs to.
func oneLakeRoleName(role string) (string, error) {
	name := olsPrefix + role
	if len(name) > 128 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("OneLake security role %q cannot be synced to a SQL role name", role)
	}
	return name, nil
}

// oneLakeMemberships are the synced roles a principal belongs to, by the same
// membership rules OneLake reads use: an Entra member, or a holder of the item
// permission a role names (DefaultReader's ReadAll).
func oneLakeMemberships(roles []onelakesec.Role, principal string, access store.Access) []string {
	held := append(append([]string{}, access.Permissions...), access.Additional...)
	var out []string
	for _, r := range roles {
		if len(onelakesec.Effective([]onelakesec.Role{r}, onelakesec.Principal{ObjectID: principal, ItemAccess: held}, onelakesec.InputTables)) == 0 {
			continue
		}
		if name, err := oneLakeRoleName(r.Name); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// oneLakeGrant is a principal's grant on a lakehouse in user identity mode:
// "Only users with Viewer permissions or shared read-only access are governed
// by OneLake security", so below Contributor the rung is CONNECT and table
// access comes only through the synced roles; Contributor and above keep their
// rung. Memberships are carried either way.
func oneLakeGrant(st *store.Store, lake *store.Item, principal, role string, access store.Access) (tds.Grant, error) {
	roles, err := st.EvaluatableRoles(lake.ID)
	if err != nil {
		return tds.Grant{}, err
	}
	g := tds.Grant{Database: lake.ID, OneLake: true, OneLakeRoles: oneLakeMemberships(roles, principal, access)}
	switch {
	case !access.Has(store.PermRead):
		g.Role = tds.RoleNone
	case store.RoleRank(role) >= store.RoleRank(store.RoleContributor):
		g.Role = dbRung(role, access, true)
	default:
		g.Role = tds.RoleConnect
	}
	return g, nil
}

// syncOneLakeRoles brings the endpoint's OLS_ roles in line with the
// lakehouse's OneLake security: one role per OneLake role, holding SELECT on the
// tables it grants — on its permitted columns when it narrows them — and no
// other object permission; roles whose OneLake role is gone are emptied and
// dropped; the guard trigger is in place. Memberships are not set here: they
// travel with each principal's grant into provisioning.
//
// A column list naming a column the table does not have grants nothing on that
// table: "Renaming or removing an allowed column invalidates the security rule
// … denying all access to the resource". A table the role filters by rows is
// not granted until row filters are synced (stage 3), so a filter is never
// served as no filter.
func syncOneLakeRoles(ctx context.Context, db *sql.DB, st *store.Store, lake *store.Item) error {
	roles, err := st.EvaluatableRoles(lake.ID)
	if err != nil {
		return err
	}
	columns, err := endpointColumns(ctx, db)
	if err != nil {
		return err
	}
	tables := make([]string, 0, len(columns))
	for t := range columns {
		tables = append(tables, t)
	}
	sort.Strings(tables)

	var b strings.Builder
	b.WriteString("IF NOT EXISTS (SELECT 1 FROM sys.triggers WHERE parent_class = 0 AND name = N'OLS_guard') EXEC(N'" +
		strings.ReplaceAll(syncGuard, "'", "''") + "');\n")
	keep := []string{"N''"}
	for _, r := range roles {
		name, err := oneLakeRoleName(r.Name)
		if err != nil {
			return err
		}
		lit, ident := sqlLiteral(name), sqlIdent(name)
		keep = append(keep, lit)
		fmt.Fprintf(&b, "IF DATABASE_PRINCIPAL_ID(%s) IS NULL CREATE ROLE %s;\n", lit, ident)
		fmt.Fprintf(&b, `DECLARE @revoke_%[1]d nvarchar(max) = N'';
SELECT @revoke_%[1]d += N'REVOKE ' + p.permission_name + N' ON ' + QUOTENAME(OBJECT_SCHEMA_NAME(p.major_id)) + N'.' + QUOTENAME(OBJECT_NAME(p.major_id)) + N' FROM ' + %[2]s + N';'
FROM sys.database_permissions p WHERE p.class = 1 AND p.grantee_principal_id = DATABASE_PRINCIPAL_ID(%[3]s);
EXEC sp_executesql @revoke_%[1]d;
`, len(keep), sqlLiteral(ident), lit)
		member := onelakesec.Role{Name: r.Name, DecisionRules: r.DecisionRules, Members: onelakesec.Members{Entra: []string{"sync"}}}
		entries := onelakesec.Effective([]onelakesec.Role{member}, onelakesec.Principal{ObjectID: "sync"}, onelakesec.InputTables)
		for _, t := range tables {
			path := "Tables/" + t
			if !onelakesec.Allows(entries, path) {
				continue
			}
			n := onelakesec.Narrowing(entries, path)
			if n != nil && n.Rows != "" {
				continue
			}
			on := "[dbo]." + sqlIdent(t)
			if n == nil || len(n.Columns) == 0 {
				fmt.Fprintf(&b, "GRANT SELECT ON %s TO %s;\n", on, ident)
				continue
			}
			var cols []string
			for _, c := range n.Columns {
				actual, ok := columns[t][strings.ToLower(c)]
				if !ok {
					cols = nil
					break
				}
				cols = append(cols, sqlIdent(actual))
			}
			if cols != nil {
				fmt.Fprintf(&b, "GRANT SELECT ON %s (%s) TO %s;\n", on, strings.Join(cols, ", "), ident)
			}
		}
	}
	fmt.Fprintf(&b, `DECLARE @stale nvarchar(max) = N'';
SELECT @stale += N'ALTER ROLE ' + QUOTENAME(r.name) + N' DROP MEMBER ' + QUOTENAME(m.name) + N';'
FROM sys.database_role_members rm
JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE r.name LIKE 'OLS[_]%%' AND r.name NOT IN (%[1]s);
SELECT @stale += N'DROP ROLE ' + QUOTENAME(name) + N';'
FROM sys.database_principals WHERE type = 'R' AND name LIKE 'OLS[_]%%' AND name NOT IN (%[1]s);
EXEC sp_executesql @stale;`, strings.Join(keep, ", "))
	if _, err := db.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("syncing OneLake security into the SQL analytics endpoint: %w", err)
	}
	return nil
}

// endpointColumns lists the endpoint's dbo tables and their columns, keyed by
// table name and then by lower-cased column name. One aggregated row, so there
// is one way for it to fail.
func endpointColumns(ctx context.Context, db *sql.DB) (map[string]map[string]string, error) {
	var agg sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT STRING_AGG(CAST(t.name + NCHAR(2) + c.name AS nvarchar(max)), NCHAR(1))
		FROM sys.tables t JOIN sys.columns c ON c.object_id = t.object_id
		WHERE t.schema_id = SCHEMA_ID('dbo') AND t.is_ms_shipped = 0`).Scan(&agg); err != nil {
		return nil, fmt.Errorf("listing the endpoint's tables: %w", err)
	}
	out := map[string]map[string]string{}
	for _, pair := range splitAgg(agg) {
		t, c, _ := strings.Cut(pair, "\x02")
		if out[t] == nil {
			out[t] = map[string]string{}
		}
		out[t][strings.ToLower(c)] = c
	}
	return out, nil
}

func sqlIdent(s string) string   { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }
func sqlLiteral(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }
