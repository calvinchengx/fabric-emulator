package api

import (
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

func (a *API) loadDirectLakeData(model *semanticmodel.Model, data semanticmodel.Data, principal *auth.Principal) error {
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
		lakehouse, err := a.resolveDirectLakeLakehouse(ws.ID, lakehouseRef)
		if err != nil {
			return fmt.Errorf("Direct Lake table %q: lakehouse is not available", table.Name)
		}
		delta, err := warehouse.ReadDeltaTable(a.Store, lakehouse.ID, table.DirectLake.EntityName)
		if err != nil && table.DirectLake.SchemaName != "" {
			delta, err = warehouse.ReadDeltaTable(a.Store, lakehouse.ID, path.Join(table.DirectLake.SchemaName, table.DirectLake.EntityName))
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

func (a *API) resolveDirectLakeLakehouse(workspaceID, ref string) (*store.Item, error) {
	if item, err := a.Store.GetItem(workspaceID, ref); err == nil && item.Type == "Lakehouse" {
		return item, nil
	}
	name := strings.TrimSuffix(ref, ".Lakehouse")
	return a.Store.GetItemByName(workspaceID, name, "Lakehouse")
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
