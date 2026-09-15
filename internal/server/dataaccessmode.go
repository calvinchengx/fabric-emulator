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
			disabled, err := enterUserIdentity(ctx, db)
			if err != nil {
				return err
			}
			raw, _ := json.Marshal(disabled) // a []string always marshals
			update[propDisabledPolicies] = string(raw)
		} else {
			var disabled []string
			if raw := props[propDisabledPolicies]; raw != "" {
				if err := json.Unmarshal([]byte(raw), &disabled); err != nil {
					return fmt.Errorf("reading the policies a switch turned off: %w", err)
				}
			}
			if err := enterDelegated(ctx, db, disabled); err != nil {
				return err
			}
			update[propDisabledPolicies] = ""
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

// enterUserIdentity turns off the enabled SQL security policies, returning their
// names, drops custom database roles and drops unbound functions — one batch, so
// the switch's SQL half either runs or reports one error.
func enterUserIdentity(ctx context.Context, db *sql.DB) ([]string, error) {
	var disabled sql.NullString
	err := db.QueryRowContext(ctx, `
DECLARE @policies nvarchar(max) = (
  SELECT STRING_AGG(CAST(QUOTENAME(SCHEMA_NAME(schema_id)) + N'.' + QUOTENAME(name) AS nvarchar(max)), NCHAR(1))
         WITHIN GROUP (ORDER BY name)
  FROM sys.security_policies WHERE is_enabled = 1 AND name NOT LIKE 'OLS[_]%');
DECLARE @off nvarchar(max) = N'';
SELECT @off += N'ALTER SECURITY POLICY ' + QUOTENAME(SCHEMA_NAME(schema_id)) + N'.' + QUOTENAME(name) + N' WITH (STATE = OFF);'
FROM sys.security_policies WHERE is_enabled = 1 AND name NOT LIKE 'OLS[_]%';
EXEC sp_executesql @off;
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
SELECT @policies;`).Scan(&disabled)
	if err != nil {
		return nil, fmt.Errorf("switching to user identity: %w", err)
	}
	if !disabled.Valid {
		return nil, nil
	}
	return strings.Split(disabled.String, "\x01"), nil
}

// enterDelegated drops unbound functions and turns the remembered policies back
// on.
func enterDelegated(ctx context.Context, db *sql.DB, policies []string) error {
	var b strings.Builder
	b.WriteString(dropFunctions)
	for _, p := range policies {
		fmt.Fprintf(&b, "\nIF OBJECT_ID(N'%s') IS NOT NULL ALTER SECURITY POLICY %s WITH (STATE = ON);",
			strings.ReplaceAll(p, "'", "''"), p)
	}
	if _, err := db.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("switching to delegated identity: %w", err)
	}
	return nil
}
