package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// A SQL analytics endpoint's data access mode, switched (docs/60).

// propDisabledPolicies records the SQL security policies a switch to user
// identity turned off, so a switch back to delegated turns exactly those on
// again: "SQL roles and security policies become active".
const propDisabledPolicies = "delegatedSecurityPoliciesDisabled"

// propRevokedPermissions records the table permissions a switch to user
// identity revoked — "SQL RLS, CLS, and table-level permissions are ignored" —
// as the statements that restore them on the way back.
const propRevokedPermissions = "delegatedTablePermissionsRevoked"

// sessionCloser is the TDS front's session registry.
type sessionCloser interface {
	CloseSessions(databases ...string) int
}

// dataAccessModeSwitch builds the hook behind the emulator-native mode switch.
//
// Fabric documents three effects, and each is applied rather than only
// recorded:
//
//   - "Changing the security mode makes SQL analytics endpoints temporarily
//     unavailable across the entire workspace. This action cancels all running
//     and queued queries" — every live session to a SQL item in the workspace is
//     closed.
//   - Switching to user identity, "existing SQL roles are deleted and can't be
//     recovered", and SQL RLS is "ignored" — custom database roles are dropped,
//     and enabled security policies are turned off and remembered.
//   - Switching "in either direction currently removes inline metadata objects,
//     including table-valued functions (TVFs) and scalar-valued functions" —
//     functions are dropped, except a security policy's predicate: dropping it
//     would destroy the policy that "becomes active" again on the way back, so
//     it is kept. A stated divergence.
//
// The mode is written last, so a switch whose SQL fails leaves the endpoint in
// the mode it was in.
func dataAccessModeSwitch(be warehouseBackend, st *store.Store, sessions sessionCloser) func(ctx context.Context, endpoint, lakehouse *store.Item, to string) error {
	return func(ctx context.Context, endpoint, lakehouse *store.Item, to string) error {
		var dbs []string
		for _, t := range sqlAddressable {
			items, err := st.ListItems(lakehouse.WorkspaceID, t)
			if err != nil {
				return err
			}
			for _, it := range items {
				dbs = append(dbs, it.ID)
			}
		}
		sessions.CloseSessions(dbs...)

		if err := be.EnsureDatabase(ctx, lakehouse.ID); err != nil {
			return fmt.Errorf("preparing database: %w", err)
		}
		db := be.DB(lakehouse.ID)
		props, err := st.ItemProperties(endpoint.ID)
		if err != nil {
			return err
		}
		update := map[string]string{store.PropDataAccessMode: to}
		if to == store.AccessModeUserIdentity {
			disabled, revoked, err := enterUserIdentity(ctx, db)
			if err != nil {
				return err
			}
			rawPolicies, _ := json.Marshal(disabled) // a []string always marshals
			rawPerms, _ := json.Marshal(revoked)
			update[propDisabledPolicies], update[propRevokedPermissions] = string(rawPolicies), string(rawPerms)
		} else {
			var disabled, revoked []string
			for prop, into := range map[string]*[]string{propDisabledPolicies: &disabled, propRevokedPermissions: &revoked} {
				if raw := props[prop]; raw != "" {
					if err := json.Unmarshal([]byte(raw), into); err != nil {
						return fmt.Errorf("reading what the switch to user identity set aside: %w", err)
					}
				}
			}
			if err := enterDelegated(ctx, db, disabled, revoked); err != nil {
				return err
			}
			update[propDisabledPolicies], update[propRevokedPermissions] = "", ""
		}
		return st.SetItemProperties(endpoint.ID, update)
	}
}

// dropFunctions removes table-valued and scalar functions that no security
// policy depends on.
const dropFunctions = `
DECLARE @sql nvarchar(max) = N'';
SELECT @sql += N'DROP FUNCTION ' + QUOTENAME(SCHEMA_NAME(o.schema_id)) + N'.' + QUOTENAME(o.name) + N';'
FROM sys.objects o
WHERE o.type IN ('IF', 'TF', 'FN') AND o.is_ms_shipped = 0
  AND NOT EXISTS (SELECT 1 FROM sys.sql_expression_dependencies d
                    JOIN sys.security_policies p ON p.object_id = d.referencing_id
                   WHERE d.referenced_id = o.object_id);
EXEC sp_executesql @sql;`

// tablePermissions selects each explicit permission a user holds on a table, or
// SELECT/CONTROL on a schema or the database, with the statement that grants it
// and the one that revokes it.
const tablePermissions = `
WITH perms AS (
  SELECT p.state, p.permission_name COLLATE DATABASE_DEFAULT AS perm, QUOTENAME(u.name) AS grantee,
    CASE p.class
      WHEN 1 THEN N' ON ' + QUOTENAME(OBJECT_SCHEMA_NAME(p.major_id)) + N'.' + QUOTENAME(OBJECT_NAME(p.major_id))
                + CASE WHEN p.minor_id > 0 THEN N' (' + QUOTENAME(COL_NAME(p.major_id, p.minor_id)) + N')' ELSE N'' END
      WHEN 3 THEN N' ON SCHEMA::' + QUOTENAME(SCHEMA_NAME(p.major_id))
      ELSE N'' END AS target
  FROM sys.database_permissions p
  JOIN sys.database_principals u ON u.principal_id = p.grantee_principal_id
  LEFT JOIN sys.objects o ON p.class = 1 AND o.object_id = p.major_id
  WHERE u.type IN ('S', 'U', 'E', 'X', 'G') AND p.state IN ('G', 'D', 'W')
    AND ((p.class = 1 AND o.type = 'U')
      OR (p.class IN (0, 3) AND p.permission_name IN ('SELECT', 'CONTROL'))))
SELECT
  CASE state WHEN 'D' THEN N'DENY ' ELSE N'GRANT ' END + perm + target + N' TO ' + grantee
    + CASE state WHEN 'W' THEN N' WITH GRANT OPTION' ELSE N'' END AS grant_stmt,
  N'REVOKE ' + perm + target + N' FROM ' + grantee + N' CASCADE' AS revoke_stmt
FROM perms`

// enterUserIdentity turns off the enabled SQL security policies, revokes users'
// explicit table permissions, drops custom database roles and drops unbound
// functions — one batch, so the switch's SQL half either runs or reports one
// error — returning the policies it turned off and the statements that restore
// the permissions.
func enterUserIdentity(ctx context.Context, db *sql.DB) ([]string, []string, error) {
	var disabled, revoked sql.NullString
	err := db.QueryRowContext(ctx, `
DECLARE @policies nvarchar(max) = (
  SELECT STRING_AGG(CAST(QUOTENAME(SCHEMA_NAME(schema_id)) + N'.' + QUOTENAME(name) AS nvarchar(max)), NCHAR(1))
         WITHIN GROUP (ORDER BY name)
  FROM sys.security_policies WHERE is_enabled = 1 AND name NOT LIKE 'OLS[_]%');
DECLARE @off nvarchar(max) = N'';
SELECT @off += N'ALTER SECURITY POLICY ' + QUOTENAME(SCHEMA_NAME(schema_id)) + N'.' + QUOTENAME(name) + N' WITH (STATE = OFF);'
FROM sys.security_policies WHERE is_enabled = 1 AND name NOT LIKE 'OLS[_]%';
EXEC sp_executesql @off;
DECLARE @perms TABLE (grant_stmt nvarchar(max), revoke_stmt nvarchar(max));
INSERT INTO @perms EXEC (N'`+strings.ReplaceAll(tablePermissions, "'", "''")+`');
DECLARE @restore nvarchar(max) = (SELECT STRING_AGG(grant_stmt, NCHAR(1)) FROM @perms);
DECLARE @revoke nvarchar(max) = N'';
SELECT @revoke += revoke_stmt + N';' FROM @perms;
EXEC sp_executesql @revoke;
DECLARE @roles nvarchar(max) = N'';
SELECT @roles += N'ALTER ROLE ' + QUOTENAME(r.name) + N' DROP MEMBER ' + QUOTENAME(m.name) + N';'
FROM sys.database_role_members rm
JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE r.type = 'R' AND r.is_fixed_role = 0 AND r.name <> 'public' AND r.name NOT LIKE 'OLS[_]%';
SELECT @roles += N'DROP ROLE ' + QUOTENAME(name) + N';'
FROM sys.database_principals
WHERE type = 'R' AND is_fixed_role = 0 AND name <> 'public' AND name NOT LIKE 'OLS[_]%';
EXEC sp_executesql @roles;`+dropFunctions+`
SELECT @policies, @restore;`).Scan(&disabled, &revoked)
	if err != nil {
		return nil, nil, fmt.Errorf("switching to user identity: %w", err)
	}
	return splitAgg(disabled), splitAgg(revoked), nil
}

// splitAgg reads a STRING_AGG joined on NCHAR(1); NULL is none.
func splitAgg(v sql.NullString) []string {
	if !v.Valid {
		return nil
	}
	return strings.Split(v.String, "\x01")
}

// enterDelegated removes what the security sync put in — the guard trigger, the
// OLS_ roles and row policies, and its record of the last sync — drops unbound functions, restores the table permissions the
// switch in revoked and turns the remembered policies back on. A permission
// whose grantee or table no longer exists is skipped, not fatal: the rest are
// still restored.
func enterDelegated(ctx context.Context, db *sql.DB, policies, permissions []string) error {
	var b strings.Builder
	b.WriteString(`
IF EXISTS (SELECT 1 FROM sys.triggers WHERE parent_class = 0 AND name = N'OLS_guard') DROP TRIGGER [OLS_guard] ON DATABASE;
DECLARE @ols nvarchar(max) = N'';
SELECT @ols += N'ALTER ROLE ' + QUOTENAME(r.name) + N' DROP MEMBER ' + QUOTENAME(m.name) + N';'
FROM sys.database_role_members rm
JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE r.name LIKE 'OLS[_]%';
SELECT @ols += N'DROP ROLE ' + QUOTENAME(name) + N';'
FROM sys.database_principals WHERE type = 'R' AND name LIKE 'OLS[_]%';
EXEC sp_executesql @ols;
IF EXISTS (SELECT 1 FROM sys.extended_properties WHERE class = 0 AND name = N'OLS_sync') EXEC sp_dropextendedproperty @name = N'OLS_sync';
`)
	b.WriteString(dropRowPolicies)
	b.WriteString(dropFunctions)
	for _, stmt := range permissions {
		fmt.Fprintf(&b, "\nBEGIN TRY EXEC (N'%s'); END TRY BEGIN CATCH END CATCH;", strings.ReplaceAll(stmt, "'", "''"))
	}
	for _, p := range policies {
		fmt.Fprintf(&b, "\nIF OBJECT_ID(N'%s') IS NOT NULL ALTER SECURITY POLICY %s WITH (STATE = ON);",
			strings.ReplaceAll(p, "'", "''"), p)
	}
	if _, err := db.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("switching to delegated identity: %w", err)
	}
	return nil
}
