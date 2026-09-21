package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// syncOneLakeRoles brings the endpoint's OLS_ objects in line with the
// lakehouse's OneLake security:
//
//   - one database role per OneLake role, holding SELECT on exactly the tables
//     it grants — on the permitted columns when it narrows them — and no other
//     object permission; a role whose OneLake role is gone is emptied and
//     dropped;
//   - for each table some role filters by rows, one security policy,
//     OLS_rls_<table hash>, over one predicate function, OLS_rlsfn_<table hash>, admitting a row when the reader is
//     in a role that filters it and the filter holds, or in a role that grants
//     the table unfiltered, or in no role that grants it at all — so filters
//     union across roles as OneLake's do, and a Contributor or above in no
//     filtering role reads the table whole (docs/60: inferred);
//   - the guard trigger.
//
// Memberships are not set here: they travel with each principal's grant into
// provisioning.
//
// A restriction is never served as none. A column list naming a column the table
// lacks grants nothing on that table ("denying all access to the resource"), and
// so does a row filter outside OneLake's grammar or not matching the table ("no
// rows being shown to users").
//
// ONE TRANSACTION, AND ONLY WHEN SOMETHING CHANGED. The batch replaces policies,
// and a reader must never see the moment between dropping one and creating the
// next; so it runs under XACT_ABORT in a transaction. Its hash — which covers the
// tables' object ids, so a table reflection recreated counts as a change — is
// kept as a database extended property, and an unchanged sync does nothing.
func syncOneLakeRoles(ctx context.Context, db *sql.DB, st *store.Store, lake *store.Item) error {
	roles, err := st.EvaluatableRoles(lake.ID)
	if err != nil {
		return err
	}
	endpoint, err := endpointColumns(ctx, db)
	if err != nil {
		return err
	}
	tables := make([]string, 0, len(endpoint.columns))
	for t := range endpoint.columns {
		tables = append(tables, t)
	}
	sort.Strings(tables)

	type tableAccess struct {
		filtered   []roleFilter
		unfiltered []string // role name literals granting the table whole
		used       []string // columns the filters read
	}
	access := map[string]*tableAccess{}

	var b strings.Builder
	b.WriteString("SET XACT_ABORT ON;\nBEGIN TRANSACTION;\n")
	b.WriteString("IF NOT EXISTS (SELECT 1 FROM sys.triggers WHERE parent_class = 0 AND name = N'OLS_guard') EXEC(N'" +
		strings.ReplaceAll(syncGuard, "'", "''") + "');\n")
	b.WriteString(dropRowPolicies)
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
			var grantCols []string
			if n != nil && len(n.Columns) > 0 {
				for _, c := range n.Columns {
					actual, ok := endpoint.columns[t][strings.ToLower(c)]
					if !ok {
						grantCols = nil
						break
					}
					grantCols = append(grantCols, sqlIdent(actual.Name))
				}
				if grantCols == nil {
					continue
				}
			}
			ta := access[t]
			if ta == nil {
				ta = &tableAccess{}
				access[t] = ta
			}
			if n != nil && n.Rows != "" {
				filter, cols, err := translateRowFilters(n.Rows, t, endpoint.columns[t])
				if err != nil {
					// No rows for any member: a Contributor in the role reads none, and
					// a Viewer is not granted the table at all, so their query errors
					// as the SQL analytics endpoint's does.
					ta.filtered = append(ta.filtered, roleFilter{role: lit, expr: "(1 = 0)"})
					continue
				}
				ta.filtered = append(ta.filtered, roleFilter{role: lit, expr: filter})
				for _, c := range cols {
					if !containsString(ta.used, c) {
						ta.used = append(ta.used, c)
					}
				}
			} else {
				ta.unfiltered = append(ta.unfiltered, lit)
			}
			on := "[dbo]." + sqlIdent(t)
			if grantCols == nil {
				fmt.Fprintf(&b, "GRANT SELECT ON %s TO %s;\n", on, ident)
			} else {
				fmt.Fprintf(&b, "GRANT SELECT ON %s (%s) TO %s;\n", on, strings.Join(grantCols, ", "), ident)
			}
		}
	}
	for _, t := range tables {
		ta := access[t]
		if ta == nil || len(ta.filtered) == 0 {
			continue
		}
		b.WriteString(rowPolicy(t, ta.filtered, ta.unfiltered, ta.used, endpoint.columns[t]))
	}
	fmt.Fprintf(&b, `DECLARE @stale nvarchar(max) = N'';
SELECT @stale += N'ALTER ROLE ' + QUOTENAME(r.name) + N' DROP MEMBER ' + QUOTENAME(m.name) + N';'
FROM sys.database_role_members rm
JOIN sys.database_principals r ON r.principal_id = rm.role_principal_id
JOIN sys.database_principals m ON m.principal_id = rm.member_principal_id
WHERE r.name LIKE 'OLS[_]%%' AND r.name NOT IN (%[1]s);
SELECT @stale += N'DROP ROLE ' + QUOTENAME(name) + N';'
FROM sys.database_principals WHERE type = 'R' AND name LIKE 'OLS[_]%%' AND name NOT IN (%[1]s);
EXEC sp_executesql @stale;
`, strings.Join(keep, ", "))

	sum := sha256.Sum256([]byte(b.String() + "\x00" + endpoint.shape))
	hash := hex.EncodeToString(sum[:])
	if endpoint.synced == hash {
		return nil
	}
	fmt.Fprintf(&b, `IF EXISTS (SELECT 1 FROM sys.extended_properties WHERE class = 0 AND name = N'OLS_sync')
  EXEC sp_updateextendedproperty @name = N'OLS_sync', @value = N'%[1]s';
ELSE
  EXEC sp_addextendedproperty @name = N'OLS_sync', @value = N'%[1]s';
COMMIT;`, hash)
	if _, err := db.ExecContext(ctx, b.String()); err != nil {
		return fmt.Errorf("syncing OneLake security into the SQL analytics endpoint: %w", err)
	}
	return nil
}

// dropRowPolicies removes every synced row policy and its predicate function.
const dropRowPolicies = `DECLARE @rls nvarchar(max) = N'';
SELECT @rls += N'DROP SECURITY POLICY ' + QUOTENAME(SCHEMA_NAME(schema_id)) + N'.' + QUOTENAME(name) + N';'
FROM sys.security_policies WHERE name LIKE 'OLS[_]rls[_]%';
SELECT @rls += N'DROP FUNCTION ' + QUOTENAME(SCHEMA_NAME(schema_id)) + N'.' + QUOTENAME(name) + N';'
FROM sys.objects WHERE type IN ('IF', 'TF', 'FN') AND name LIKE 'OLS[_]rlsfn[_]%';
EXEC sp_executesql @rls;
`

// roleFilter is one role's translated row filter on a table, with the role as a
// SQL literal.
type roleFilter struct {
	role, expr string
}

// rowPolicy creates one table's predicate function and security policy.
func rowPolicy(table string, filtered []roleFilter, unfiltered, used []string, columns map[string]endpointColumn) string {
	sum := sha256.Sum256([]byte(table))
	name := "OLS_rls_" + hex.EncodeToString(sum[:8])
	fnName := "OLS_rlsfn_" + hex.EncodeToString(sum[:8])
	if len(used) == 0 {
		// A filter reading no column still needs one to attach to.
		for _, c := range columns {
			if len(used) == 0 || c.Name < used[0] {
				used = []string{c.Name}
			}
		}
	}
	var params, projections, args []string
	for i, c := range used {
		col := columns[strings.ToLower(c)]
		params = append(params, fmt.Sprintf("@p%d %s", i, col.Type))
		projections = append(projections, fmt.Sprintf("@p%d AS %s", i, sqlIdent(col.Name)))
		args = append(args, sqlIdent(col.Name))
	}
	var terms, none []string
	for _, f := range filtered {
		terms = append(terms, "(IS_MEMBER("+f.role+") = 1 AND "+f.expr+")")
		none = append(none, "IS_MEMBER("+f.role+") = 0")
	}
	for _, u := range unfiltered {
		terms = append(terms, "IS_MEMBER("+u+") = 1")
		none = append(none, "IS_MEMBER("+u+") = 0")
	}
	terms = append(terms, "("+strings.Join(none, " AND ")+")")
	fn := fmt.Sprintf("CREATE FUNCTION [dbo].%s(%s) RETURNS TABLE AS RETURN SELECT 1 AS ok FROM (SELECT %s) AS r WHERE %s",
		sqlIdent(fnName), strings.Join(params, ", "), strings.Join(projections, ", "), strings.Join(terms, " OR "))
	policy := fmt.Sprintf("CREATE SECURITY POLICY [dbo].%s ADD FILTER PREDICATE [dbo].%s(%s) ON [dbo].%s WITH (STATE = ON, SCHEMABINDING = OFF)",
		sqlIdent(name), sqlIdent(fnName), strings.Join(args, ", "), sqlIdent(table))
	return fmt.Sprintf("EXEC(N'%s');\nEXEC(N'%s');\nGRANT SELECT ON [dbo].%s TO [public];\n",
		strings.ReplaceAll(fn, "'", "''"), strings.ReplaceAll(policy, "'", "''"), sqlIdent(fnName))
}

// translateRowFilters translates a role's row filter for one table. Several
// rules filtering the same table arrive joined by " UNION ", as OneLake's
// consolidation writes them, and become an OR.
func translateRowFilters(rows, table string, columns map[string]endpointColumn) (string, []string, error) {
	var exprs, used []string
	for _, part := range strings.Split(rows, " UNION ") {
		f, err := translateRowFilter(part, table, columns)
		if err != nil {
			return "", nil, err
		}
		exprs = append(exprs, f.Expr)
		for _, c := range f.Columns {
			if !containsString(used, c) {
				used = append(used, c)
			}
		}
	}
	return "(" + strings.Join(exprs, " OR ") + ")", used, nil
}

func containsString(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// endpointState is what the sync reads from the endpoint: each dbo table's
// columns, keyed by lower-cased name; the tables' shape (names and object ids),
// which changes when reflection recreates one; and the hash of the last sync.
type endpointState struct {
	columns map[string]map[string]endpointColumn
	shape   string
	synced  string
}

// endpointColumns reads the endpoint's state in one aggregated row, so there is
// one way for it to fail.
func endpointColumns(ctx context.Context, db *sql.DB) (endpointState, error) {
	var agg, synced sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT
  (SELECT STRING_AGG(CAST(t.name + NCHAR(2) + c.name + NCHAR(2)
      + CASE
          WHEN ty.name IN ('varchar', 'char', 'varbinary', 'binary') THEN ty.name + '(' + CASE WHEN c.max_length = -1 THEN 'max' ELSE CAST(c.max_length AS varchar(10)) END + ')'
          WHEN ty.name IN ('nvarchar', 'nchar') THEN ty.name + '(' + CASE WHEN c.max_length = -1 THEN 'max' ELSE CAST(c.max_length / 2 AS varchar(10)) END + ')'
          WHEN ty.name IN ('decimal', 'numeric') THEN ty.name + '(' + CAST(c.precision AS varchar(10)) + ',' + CAST(c.scale AS varchar(10)) + ')'
          WHEN ty.name IN ('datetime2', 'time', 'datetimeoffset') THEN ty.name + '(' + CAST(c.scale AS varchar(10)) + ')'
          ELSE ty.name END
      + NCHAR(2) + CAST(t.object_id AS nvarchar(20)) AS nvarchar(max)), NCHAR(1))
     WITHIN GROUP (ORDER BY t.name, c.column_id)
   FROM sys.tables t
   JOIN sys.columns c ON c.object_id = t.object_id
   JOIN sys.types ty ON ty.user_type_id = c.user_type_id
   WHERE t.schema_id = SCHEMA_ID('dbo') AND t.is_ms_shipped = 0),
  (SELECT CAST(value AS nvarchar(100)) FROM sys.extended_properties WHERE class = 0 AND name = N'OLS_sync')`).Scan(&agg, &synced); err != nil {
		return endpointState{}, fmt.Errorf("listing the endpoint's tables: %w", err)
	}
	out := endpointState{columns: map[string]map[string]endpointColumn{}, shape: agg.String, synced: synced.String}
	for _, row := range splitAgg(agg) {
		f := strings.Split(row, "\x02")
		t, c, typ := f[0], f[1], f[2]
		if out.columns[t] == nil {
			out.columns[t] = map[string]endpointColumn{}
		}
		out.columns[t][strings.ToLower(c)] = endpointColumn{Name: c, Type: typ,
			Text: strings.Contains(typ, "char")}
	}
	return out, nil
}

func sqlIdent(s string) string   { return "[" + strings.ReplaceAll(s, "]", "]]") + "]" }
func sqlLiteral(s string) string { return "N'" + strings.ReplaceAll(s, "'", "''") + "'" }
