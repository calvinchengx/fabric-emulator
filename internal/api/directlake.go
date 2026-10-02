package api

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"path"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
	"github.com/calvinchengx/fabric-emulator/pkg/onelakesec"
)

func (a *API) loadDirectLakeData(ctx context.Context, modelItemID string, model *semanticmodel.Model, data semanticmodel.Data, principal *auth.Principal) error {
	binding, err := directLakeBinding(model)
	if err != nil {
		return err
	}
	if binding.flavor == semanticmodel.DirectLakeOnSQL {
		return a.loadDirectLakeSQLData(ctx, modelItemID, model, binding, data, principal)
	}
	for _, table := range model.Tables {
		if table.DirectLake == nil {
			continue
		}
		workspaceRef, lakehouseRef := binding.tables[table.Name].Workspace, binding.tables[table.Name].Item
		ws, err := a.resolveDirectLakeWorkspace(workspaceRef)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: workspace is not available", table.Name)
		}
		source, err := a.resolveDirectLakeSource(ws.ID, lakehouseRef)
		if err != nil {
			// Somebody with no role in the source workspace learns only that they
			// cannot read it — not whether an item of that name exists.
			if role, rerr := a.Store.RoleOf(ws.ID, principal.ID); rerr == nil && role == "" {
				return fmt.Errorf("Direct Lake table %q: caller cannot read the source", table.Name)
			}
			return fmt.Errorf("Direct Lake table %q: %w", table.Name, err)
		}
		// The same decision every OneLake reader asks, so Direct Lake can never
		// admit what the storage surface would refuse. "If OneLake security isn't
		// on, Direct Lake on OneLake needs the effective identity to have Read and
		// ReadAll"; when it is on, the roles decide. Either can come from a grant
		// on the source, which is how a model reaches a lakehouse in a workspace
		// its reader has no role in.
		read, err := a.Store.OneLakeReadAccess(source, principal.ID, onelakesec.InputTables)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: %w", table.Name, err)
		}
		if !read.Allowed {
			return fmt.Errorf("Direct Lake table %q: caller cannot read the source: Direct Lake on OneLake "+
				"needs Read and ReadAll on it, or a OneLake security role", table.Name)
		}
		var delta *warehouse.Table
		if source.Type == "Lakehouse" {
			entity := table.DirectLake.EntityName
			delta, err = warehouse.ReadDeltaTable(a.Store, source.ID, entity)
			if err != nil && table.DirectLake.SchemaName != "" {
				entity = path.Join(table.DirectLake.SchemaName, table.DirectLake.EntityName)
				delta, err = warehouse.ReadDeltaTable(a.Store, source.ID, entity)
			}
			if err == nil {
				delta, err = secureDirectLakeTable(read, entity, &table, delta)
			}
		} else {
			// A WAREHOUSE source. On real Fabric a warehouse persists to OneLake as
			// Delta, so Direct Lake over one reads those files — the same mechanism
			// as a lakehouse. The emulator's warehouse is a real SQL Server database
			// and its bytes are not Delta, so the equivalent rows come from SQL.
			//
			// That is a BACKEND difference, not a contract one: the model's
			// definition is identical on both targets (an `entity` partition over an
			// OneLake expression), and the rows the evaluator sees are the same rows.
			// Reading them over SQL is the same posture the rest of the warehouse
			// surface takes — our contract, a real engine's compute.
			//
			// Why this matters beyond neatness: without it a model over GOLD had to
			// carry its rows inline in a `data.json` part, which real Fabric has no
			// concept of. So the one artifact a BI consumer actually reads was the
			// one thing in the examples that could not be deployed to a tenant.
			//
			// A warehouse carries no OneLake security roles, so an allowed reader
			// here always reads it whole: ReadAll is the whole of the decision.
			delta, err = a.readWarehouseTable(ctx, source, table.DirectLake)
		}
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: %w", table.Name, err)
		}
		rows, err := directLakeRows(&table, delta)
		if err != nil {
			return err
		}
		data[table.Name] = rows
	}
	return nil
}

// secureDirectLakeTable narrows one Direct Lake table to what the reader's
// OneLake access allows. read must be Allowed: the caller has checked.
//
// WHY THIS IS THE QUERY PATH'S JOB. Direct Lake does not read through the SQL
// analytics endpoint, so no engine upstream has already filtered: "Direct Lake
// on OneLake doesn't use a SQL analytics endpoint to check permissions. It uses
// OneLake security. When OneLake security is on, Direct Lake on OneLake uses the
// current user (or fixed identity) to figure out OneLake security roles and
// enforce OLS and RLS on the target Fabric item." A query path that skipped this
// would hand a Viewer every row of a table a policy narrows, which is the one
// failure this whole family exists to prevent.
//
// THE SHAPE OF A REFUSAL IS NAME RESOLUTION, NOT AUTHORIZATION. The product's
// own troubleshooting list for this case is "Can't find table", "Column can't be
// found", "Failed to resolve name" — errors "when object permissions are missing
// after applying OneLake security roles". A secured object is absent from the
// namespace rather than present and forbidden, so that is how it is reported
// here, and the evaluator's existing unresolved-name error carries the rest.
func secureDirectLakeTable(read store.OneLakeRead, entity string, modelTable *semanticmodel.Table,
	delta *warehouse.Table,
) (*warehouse.Table, error) {
	if read.Full {
		return delta, nil
	}
	rel := path.Join("Tables", entity)
	if !onelakesec.Allows(read.Entries, rel) {
		return nil, fmt.Errorf("can't be found: no OneLake security role grants this principal access to it")
	}
	narrowing := onelakesec.Narrowing(read.Entries, rel)
	if narrowing == nil {
		return delta, nil
	}
	// "Users that try to access tables that are part of an unsupported role
	// combination receive query errors" — before anything is read from the rows.
	if narrowing.Unsupported != "" {
		return nil, fmt.Errorf("can't be served: it is subject to %s", narrowing.Unsupported)
	}
	// ROWS FIRST, THEN COLUMNS. A role may filter on a column it does not grant
	// — "the two policies have to be applied using a single OneLake security
	// role" — so the filter reads the full row before the projection drops it.
	if narrowing.Rows != "" {
		filtered, err := filterDirectLakeRows(narrowing.Rows, entity, delta)
		if err != nil {
			// "Access to a table might be blocked if the RLS statement contains
			// syntax errors that prevent it from being evaluated" — blocked, by
			// name, rather than served unfiltered or silently empty.
			return nil, fmt.Errorf("can't be served: its OneLake row-level security filter cannot be applied: %w", err)
		}
		delta = filtered
	}
	if len(narrowing.Columns) == 0 {
		return delta, nil
	}
	return projectDirectLakeColumns(modelTable, narrowing.Columns, delta)
}

// filterDirectLakeRows keeps the rows a OneLake row filter admits. The filter
// is parsed by pkg/onelakesec, the same parser the SQL analytics endpoint's
// security sync renders into SQL Server predicates (docs/60), so Direct Lake and
// the endpoint admit the same rows: text case-insensitively, numbers exactly,
// SQL's three-valued logic over NULL. Several grants filtering one table arrive
// joined by " UNION " and a row survives if any admits it. entity is the table's
// path under Tables/: "sales", or "<schema>/sales" in a schema-enabled lakehouse.
func filterDirectLakeRows(rows, entity string, delta *warehouse.Table) (*warehouse.Table, error) {
	schema, table := "dbo", entity
	if i := strings.Index(entity, "/"); i >= 0 {
		schema, table = entity[:i], entity[i+1:]
	}
	index := make(map[string]int, len(delta.Columns))
	columns := make(map[string]onelakesec.FilterColumn, len(delta.Columns))
	for i, c := range delta.Columns {
		index[c] = i
		columns[strings.ToLower(c)] = onelakesec.FilterColumn{Name: c}
	}
	filters, err := onelakesec.ParseRowFilters(rows, schema, table, columns)
	if err != nil {
		return nil, err
	}
	out := &warehouse.Table{Columns: delta.Columns, Skipped: delta.Skipped}
	for _, r := range delta.Rows {
		cell := func(c string) any { return filterValue(r[index[c]]) }
		for _, f := range filters {
			ok, err := f.Admits(cell)
			if err != nil {
				return nil, err
			}
			if ok {
				out.Rows = append(out.Rows, r)
				break
			}
		}
	}
	return out, nil
}

// filterValue converts a Delta cell to the types the row filter compares:
// dates and timestamps as times, decimals exactly.
func filterValue(v any) any {
	switch x := v.(type) {
	case warehouse.Date:
		return x.T
	case warehouse.Timestamp:
		return x.T
	case warehouse.Decimal:
		if x.Unscaled == nil {
			return nil
		}
		return new(big.Rat).SetFrac(x.Unscaled, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(x.Scale)), nil))
	}
	return v
}

// projectDirectLakeColumns keeps only the granted columns. A model column whose
// source is not among them cannot be answered, and the product reports that as
// the column not existing rather than as a denial.
func projectDirectLakeColumns(modelTable *semanticmodel.Table, granted []string, delta *warehouse.Table) (*warehouse.Table, error) {
	allowed := make(map[string]bool, len(granted))
	for _, c := range granted {
		allowed[strings.ToLower(c)] = true
	}
	for _, column := range modelTable.Columns {
		source := column.SourceColumn
		if source == "" {
			source = column.Name
		}
		if !allowed[strings.ToLower(source)] {
			return nil, fmt.Errorf("column %q can't be found: no OneLake security role grants "+
				"this principal access to it", column.Name)
		}
	}
	out := &warehouse.Table{}
	keep := make([]int, 0, len(delta.Columns))
	for i, name := range delta.Columns {
		if allowed[strings.ToLower(name)] {
			keep = append(keep, i)
			out.Columns = append(out.Columns, name)
		}
	}
	for _, row := range delta.Rows {
		projected := make([]any, 0, len(keep))
		for _, i := range keep {
			projected = append(projected, row[i])
		}
		out.Rows = append(out.Rows, projected)
	}
	return out, nil
}

// directLakeBinding classifies every Direct Lake table's shared expression and
// refuses the combinations Fabric does not hold: tables of both flavours in one
// model, and Direct Lake on SQL over more than one source — it "can use the data
// from a single Fabric data source". The SQL source is returned when there is
// one; a model with no Direct Lake table has flavour zero.
func directLakeBinding(m *semanticmodel.Model) (directLakeSources, error) {
	b := directLakeSources{tables: map[string]semanticmodel.DirectLakeSource{}}
	for _, table := range m.Tables {
		if table.DirectLake == nil {
			continue
		}
		expression, ok := m.Expressions[table.DirectLake.ExpressionSource]
		if !ok {
			return directLakeSources{}, fmt.Errorf("Direct Lake table %q references missing expression %q", table.Name, table.DirectLake.ExpressionSource)
		}
		src, err := semanticmodel.ParseDirectLakeSource(expression)
		if err != nil {
			return directLakeSources{}, fmt.Errorf("Direct Lake table %q: %w", table.Name, err)
		}
		if b.flavor != 0 && b.flavor != src.Flavor {
			return directLakeSources{}, fmt.Errorf("Direct Lake table %q: the model mixes Direct Lake on OneLake and Direct Lake "+
				"on SQL tables, which one semantic model cannot hold", table.Name)
		}
		b.flavor = src.Flavor
		b.tables[table.Name] = src
		if src.Flavor != semanticmodel.DirectLakeOnSQL {
			continue
		}
		if b.sql != nil && !strings.EqualFold(b.sql.Database, src.Database) {
			return directLakeSources{}, fmt.Errorf("Direct Lake table %q: Direct Lake on SQL uses a single source, and this model "+
				"names both %q and %q", table.Name, b.sql.Database, src.Database)
		}
		b.sql = &src
	}
	return b, nil
}

// directLakeSources is a model's Direct Lake binding: its one flavour, each
// table's source, and the single SQL source when the flavour is SQL.
type directLakeSources struct {
	flavor semanticmodel.DirectLakeFlavor
	tables map[string]semanticmodel.DirectLakeSource
	sql    *semanticmodel.DirectLakeSource
}

// parseDirectLakeLocation reads a Direct Lake on OneLake expression's workspace
// and item. A Direct Lake on SQL expression is refused by name.
func parseDirectLakeLocation(expression string) (string, string, error) {
	src, err := semanticmodel.ParseDirectLakeSource(expression)
	if err != nil {
		return "", "", err
	}
	if src.Flavor != semanticmodel.DirectLakeOnOneLake {
		return "", "", errNoOneLakeLocation
	}
	return src.Workspace, src.Item, nil
}

func (a *API) resolveDirectLakeWorkspace(ref string) (*store.Workspace, error) {
	if ws, err := a.Store.GetWorkspace(ref); err == nil {
		return ws, nil
	}
	return a.Store.GetWorkspaceByName(ref)
}

// resolveDirectLakeSource finds the item a Direct Lake expression points at: a
// Lakehouse or a Warehouse, both of which real Fabric supports as Direct Lake
// sources because both persist to OneLake.
func (a *API) resolveDirectLakeSource(workspaceID, ref string) (*store.Item, error) {
	if item, err := a.Store.GetItem(workspaceID, ref); err == nil && directLakeSourceType(item.Type) {
		return item, nil
	}
	for _, typ := range []string{"Lakehouse", "Warehouse"} {
		name := strings.TrimSuffix(ref, "."+typ)
		if item, err := a.Store.GetItemByName(workspaceID, name, typ); err == nil {
			return item, nil
		}
	}
	return nil, fmt.Errorf("no lakehouse or warehouse %q in the source workspace", ref)
}

func directLakeSourceType(t string) bool {
	return t == "Lakehouse" || t == "Warehouse" || t == "SQLDatabase"
}

// readWarehouseTable materialises a warehouse table as the same shape a Delta
// read produces, so directLakeRows cannot tell them apart.
func (a *API) readWarehouseTable(ctx context.Context, item *store.Item, dl *semanticmodel.DirectLakePartition) (*warehouse.Table, error) {
	schema := dl.SchemaName
	if schema == "" {
		schema = "dbo"
	}
	// Identifiers, not parameters: SQL has no placeholder for a table name. Both
	// come from a definition the caller published, and the warehouse is
	// case-SENSITIVE (internal/tds/collation.go), so they are quoted rather than
	// normalised — a name that does not match exactly must fail, as it would on a
	// tenant.
	//
	// Checked BEFORE the database is opened. A test caught the other order: a
	// doomed request should not acquire a connection, and a validation that runs
	// after the thing it guards is one refactor away from not running at all.
	if !safeSQLIdent(schema) || !safeSQLIdent(dl.EntityName) {
		return nil, fmt.Errorf("unsafe entity name %q.%q", schema, dl.EntityName)
	}
	if a.SQLDB == nil {
		return nil, fmt.Errorf("this emulator serves no SQL: a Direct Lake model over a warehouse needs one")
	}
	db, err := a.SQLDB(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT * FROM ["+schema+"].["+dl.EntityName+"]")
	if err != nil {
		return nil, err
	}
	return scanSQLTable(rows)
}

// scanSQLTable materialises a result set as the same shape a Delta read
// produces, so directLakeRows cannot tell them apart.
func scanSQLTable(rows *sql.Rows) (*warehouse.Table, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := &warehouse.Table{Columns: cols}
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, c := range cells {
			// The evaluator's text type is string; a driver may hand back bytes.
			if b, ok := c.([]byte); ok {
				cells[i] = string(b)
			}
		}
		out.Rows = append(out.Rows, cells)
	}
	return out, rows.Err()
}

// safeSQLIdent allows only what a Fabric table or schema name needs, so an
// identifier can be bracket-quoted into a SELECT without a quoting hazard.
func safeSQLIdent(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

func directLakeRows(modelTable *semanticmodel.Table, delta *warehouse.Table) ([]semanticmodel.Row, error) {
	indexes := map[string]int{}
	for i, name := range delta.Columns {
		indexes[strings.ToLower(name)] = i
	}
	rows := make([]semanticmodel.Row, 0, len(delta.Rows))
	for _, sourceRow := range delta.Rows {
		row := semanticmodel.Row{}
		for _, column := range modelTable.Columns {
			source := column.SourceColumn
			if source == "" {
				source = column.Name
			}
			index, ok := indexes[strings.ToLower(source)]
			if !ok {
				return nil, fmt.Errorf("Direct Lake table %q is missing source column %q", modelTable.Name, source)
			}
			row[column.Name] = sourceRow[index]
		}
		rows = append(rows, row)
	}
	return rows, nil
}
