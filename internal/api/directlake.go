package api

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

var directLakeURL = regexp.MustCompile(`(?i)https://onelake\.dfs\.fabric\.microsoft\.com/([^/"?]+?)/([^/"?]+)`)

func (a *API) loadDirectLakeData(ctx context.Context, model *semanticmodel.Model, data semanticmodel.Data, principal *auth.Principal) error {
	for _, table := range model.Tables {
		if table.DirectLake == nil {
			continue
		}
		expression, ok := model.Expressions[table.DirectLake.ExpressionSource]
		if !ok {
			return fmt.Errorf("Direct Lake table %q references missing expression %q", table.Name, table.DirectLake.ExpressionSource)
		}
		workspaceRef, lakehouseRef, err := parseDirectLakeLocation(expression)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: %w", table.Name, err)
		}
		ws, err := a.resolveDirectLakeWorkspace(workspaceRef)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: workspace is not available", table.Name)
		}
		role, err := a.Store.RoleOf(ws.ID, principal.ID)
		if err != nil || store.RoleRank(role) < store.RoleRank(store.RoleViewer) {
			return fmt.Errorf("Direct Lake table %q: caller cannot read source workspace", table.Name)
		}
		source, err := a.resolveDirectLakeSource(ws.ID, lakehouseRef)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: %w", table.Name, err)
		}
		var delta *warehouse.Table
		if source.Type == "Lakehouse" {
			delta, err = warehouse.ReadDeltaTable(a.Store, source.ID, table.DirectLake.EntityName)
			if err != nil && table.DirectLake.SchemaName != "" {
				delta, err = warehouse.ReadDeltaTable(a.Store, source.ID,
					path.Join(table.DirectLake.SchemaName, table.DirectLake.EntityName))
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

func parseDirectLakeLocation(expression string) (string, string, error) {
	match := directLakeURL.FindStringSubmatch(expression)
	if len(match) != 3 {
		return "", "", fmt.Errorf("shared expression must contain an onelake.dfs.fabric.microsoft.com workspace/lakehouse URL")
	}
	workspace, err := url.PathUnescape(match[1])
	if err != nil {
		return "", "", fmt.Errorf("invalid workspace path")
	}
	lakehouse, err := url.PathUnescape(match[2])
	if err != nil {
		return "", "", fmt.Errorf("invalid lakehouse path")
	}
	return workspace, lakehouse, nil
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
