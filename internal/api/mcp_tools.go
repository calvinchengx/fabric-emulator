package api

import (
	"encoding/json"
	"net/url"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// Published Core MCP tool names — rest/api/fabric/articles/mcp-servers/core-remote/tools-core-mcp-server.
var mcpToolNames = []string{
	"search_catalog",
	"list_workspaces", "get_workspace", "create_workspace", "update_workspace", "delete_workspace",
	"add_workspace_role", "list_workspace_roles", "get_workspace_role", "update_workspace_role", "delete_workspace_role",
	"list_items", "get_item", "create_item", "update_item", "delete_item",
	"get_item_definition", "update_item_definition", "bulk_move_items",
	"create_folder", "list_folders", "get_folder", "update_folder", "delete_folder", "move_folder",
	"list_capacities", "get_operation_state", "get_operation_result", "get_knowledge",
}

type mcpToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpTools []mcpToolSpec
var mcpDispatch map[string]func(*API, *auth.Principal, map[string]any) mcpToolResult

func init() {
	str := func(required []string, props map[string]any) map[string]any {
		if props == nil {
			props = map[string]any{}
		}
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	prop := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	mcpTools = []mcpToolSpec{
		{Name: "search_catalog", Description: "Search the OneLake catalog for items across workspaces.", InputSchema: str(nil, map[string]any{
			"search":    prop("Text query over display name, description, workspace name"),
			"filter":    prop("Type filter, e.g. Type eq 'Lakehouse'"),
			"page_size": map[string]any{"type": "integer"}, "continuation_token": prop(""),
		})},
		{Name: "list_workspaces", Description: "List all workspaces you have access to.", InputSchema: str(nil, nil)},
		{Name: "get_workspace", Description: "Get a workspace.", InputSchema: str([]string{"workspace_id"}, map[string]any{"workspace_id": prop("Workspace ID")})},
		{Name: "create_workspace", Description: "Create a workspace.", InputSchema: str([]string{"display_name"}, map[string]any{
			"display_name": prop(""), "description": prop(""), "capacity_id": prop(""),
		})},
		{Name: "update_workspace", Description: "Update a workspace name or description.", InputSchema: str([]string{"workspace_id"}, map[string]any{
			"workspace_id": prop(""), "display_name": prop(""), "description": prop(""),
		})},
		{Name: "delete_workspace", Description: "Delete a workspace.", InputSchema: str([]string{"workspace_id"}, map[string]any{"workspace_id": prop("")})},
		{Name: "add_workspace_role", Description: "Grant a principal a workspace role.", InputSchema: str([]string{"workspace_id", "principal_id", "role"}, map[string]any{
			"workspace_id": prop(""), "principal_id": prop(""), "principal_type": prop("User or ServicePrincipal"), "role": prop("Admin, Member, Contributor, Viewer"),
		})},
		{Name: "list_workspace_roles", Description: "List role assignments for a workspace.", InputSchema: str([]string{"workspace_id"}, map[string]any{"workspace_id": prop("")})},
		{Name: "get_workspace_role", Description: "Get one role assignment.", InputSchema: str([]string{"workspace_id", "role_assignment_id"}, map[string]any{
			"workspace_id": prop(""), "role_assignment_id": prop(""),
		})},
		{Name: "update_workspace_role", Description: "Change a principal's workspace role.", InputSchema: str([]string{"workspace_id", "role_assignment_id", "role"}, map[string]any{
			"workspace_id": prop(""), "role_assignment_id": prop(""), "role": prop(""),
		})},
		{Name: "delete_workspace_role", Description: "Remove a principal's workspace access.", InputSchema: str([]string{"workspace_id", "role_assignment_id"}, map[string]any{
			"workspace_id": prop(""), "role_assignment_id": prop(""),
		})},
		{Name: "list_items", Description: "List items in a workspace.", InputSchema: str([]string{"workspace_id"}, map[string]any{
			"workspace_id": prop(""), "type": prop("Optional item type filter"),
		})},
		{Name: "get_item", Description: "Get an item.", InputSchema: str([]string{"workspace_id", "item_id"}, map[string]any{
			"workspace_id": prop(""), "item_id": prop(""),
		})},
		{Name: "create_item", Description: "Create an item in a workspace.", InputSchema: str([]string{"workspace_id", "display_name", "type"}, map[string]any{
			"workspace_id": prop(""), "display_name": prop(""), "type": prop(""), "description": prop(""), "folder_id": prop(""),
		})},
		{Name: "update_item", Description: "Update an item's name or description.", InputSchema: str([]string{"workspace_id", "item_id"}, map[string]any{
			"workspace_id": prop(""), "item_id": prop(""), "display_name": prop(""), "description": prop(""),
		})},
		{Name: "delete_item", Description: "Delete an item.", InputSchema: str([]string{"workspace_id", "item_id"}, map[string]any{
			"workspace_id": prop(""), "item_id": prop(""),
		})},
		{Name: "get_item_definition", Description: "Get an item's definition.", InputSchema: str([]string{"workspace_id", "item_id"}, map[string]any{
			"workspace_id": prop(""), "item_id": prop(""),
		})},
		{Name: "update_item_definition", Description: "Update an item's definition.", InputSchema: str([]string{"workspace_id", "item_id", "definition"}, map[string]any{
			"workspace_id": prop(""), "item_id": prop(""), "definition": map[string]any{"type": "object"},
		})},
		{Name: "bulk_move_items", Description: "Move items into a folder.", InputSchema: str([]string{"workspace_id", "item_ids"}, map[string]any{
			"workspace_id": prop(""), "item_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"target_folder_id": prop("Empty for workspace root"),
		})},
		{Name: "create_folder", Description: "Create a folder.", InputSchema: str([]string{"workspace_id", "display_name"}, map[string]any{
			"workspace_id": prop(""), "display_name": prop(""), "parent_folder_id": prop(""),
		})},
		{Name: "list_folders", Description: "List folders in a workspace.", InputSchema: str([]string{"workspace_id"}, map[string]any{"workspace_id": prop("")})},
		{Name: "get_folder", Description: "Get a folder.", InputSchema: str([]string{"workspace_id", "folder_id"}, map[string]any{
			"workspace_id": prop(""), "folder_id": prop(""),
		})},
		{Name: "update_folder", Description: "Rename a folder.", InputSchema: str([]string{"workspace_id", "folder_id", "display_name"}, map[string]any{
			"workspace_id": prop(""), "folder_id": prop(""), "display_name": prop(""),
		})},
		{Name: "delete_folder", Description: "Delete an empty folder.", InputSchema: str([]string{"workspace_id", "folder_id"}, map[string]any{
			"workspace_id": prop(""), "folder_id": prop(""),
		})},
		{Name: "move_folder", Description: "Move a folder under a new parent.", InputSchema: str([]string{"workspace_id", "folder_id"}, map[string]any{
			"workspace_id": prop(""), "folder_id": prop(""), "target_folder_id": prop(""),
		})},
		{Name: "list_capacities", Description: "List Fabric capacities.", InputSchema: str(nil, nil)},
		{Name: "get_operation_state", Description: "Poll a long-running operation.", InputSchema: str([]string{"operation_id"}, map[string]any{"operation_id": prop("")})},
		{Name: "get_operation_result", Description: "Get a completed operation's result.", InputSchema: str([]string{"operation_id"}, map[string]any{"operation_id": prop("")})},
		{Name: "get_knowledge", Description: "Guidelines for an item type. Does not reproduce unpublished Microsoft docs.", InputSchema: str(nil, map[string]any{"item_type": prop("")})},
	}
	mcpDispatch = map[string]func(*API, *auth.Principal, map[string]any) mcpToolResult{
		"search_catalog":         toolSearchCatalog,
		"list_workspaces":        toolListWorkspaces,
		"get_workspace":          toolGetWorkspace,
		"create_workspace":       toolCreateWorkspace,
		"update_workspace":       toolUpdateWorkspace,
		"delete_workspace":       toolDeleteWorkspace,
		"add_workspace_role":     toolAddRole,
		"list_workspace_roles":   toolListRoles,
		"get_workspace_role":     toolGetRole,
		"update_workspace_role":  toolUpdateRole,
		"delete_workspace_role":  toolDeleteRole,
		"list_items":             toolListItems,
		"get_item":               toolGetItem,
		"create_item":            toolCreateItem,
		"update_item":            toolUpdateItem,
		"delete_item":            toolDeleteItem,
		"get_item_definition":    toolGetDefinition,
		"update_item_definition": toolUpdateDefinition,
		"bulk_move_items":        toolBulkMove,
		"create_folder":          toolCreateFolder,
		"list_folders":           toolListFolders,
		"get_folder":             toolGetFolder,
		"update_folder":          toolUpdateFolder,
		"delete_folder":          toolDeleteFolder,
		"move_folder":            toolMoveFolder,
		"list_capacities":        toolListCapacities,
		"get_operation_state":    toolGetOp,
		"get_operation_result":   toolGetOpResult,
		"get_knowledge":          toolGetKnowledge,
	}
}

func toolSearchCatalog(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	body := map[string]any{"search": arg(args, "search")}
	if f := arg(args, "filter"); f != "" {
		body["filter"] = f
	}
	if t := arg(args, "continuation_token", "continuationToken"); t != "" {
		body["continuationToken"] = t
	}
	if n, ok := args["page_size"].(float64); ok {
		body["pageSize"] = int(n)
	} else if n, ok := args["pageSize"].(float64); ok {
		body["pageSize"] = int(n)
	}
	b, _ := json.Marshal(body)
	return mcpFromHTTP(a.invoke(a.searchCatalog, p, "POST", string(b), nil, nil))
}

func toolListWorkspaces(a *API, p *auth.Principal, _ map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.listWorkspaces, p, "GET", "", nil, nil))
}

func toolGetWorkspace(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.getWorkspace, p, "GET", "", map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, nil))
}

func toolCreateWorkspace(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	body := map[string]any{"displayName": arg(args, "display_name", "displayName")}
	if d := arg(args, "description"); d != "" {
		body["description"] = d
	}
	if c := arg(args, "capacity_id", "capacityId"); c != "" {
		body["capacityId"] = c
	}
	b, _ := json.Marshal(body)
	return mcpFromHTTP(a.invoke(a.createWorkspace, p, "POST", string(b), nil, nil))
}

func toolUpdateWorkspace(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	body := map[string]any{}
	if d := arg(args, "display_name", "displayName"); d != "" {
		body["displayName"] = d
	}
	if d := arg(args, "description"); args["description"] != nil {
		body["description"] = d
	}
	b, _ := json.Marshal(body)
	return mcpFromHTTP(a.invoke(a.updateWorkspace, p, "PATCH", string(b), map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, nil))
}

func toolDeleteWorkspace(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.deleteWorkspace, p, "DELETE", "", map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, nil))
}

func toolAddRole(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	pt := arg(args, "principal_type", "principalType")
	if pt == "" {
		pt = "User"
	}
	body := map[string]any{
		"principal": map[string]any{"id": arg(args, "principal_id", "principalId"), "type": pt},
		"role":      arg(args, "role"),
	}
	b, _ := json.Marshal(body)
	return mcpFromHTTP(a.invoke(a.createRoleAssignment, p, "POST", string(b), map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, nil))
}

func toolListRoles(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.listRoleAssignments, p, "GET", "", map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, nil))
}

func toolGetRole(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.getRoleAssignment, p, "GET", "", map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "raid": arg(args, "role_assignment_id", "roleAssignmentId"),
	}, nil))
}

func toolUpdateRole(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	b, _ := json.Marshal(map[string]any{"role": arg(args, "role")})
	return mcpFromHTTP(a.invoke(a.updateRoleAssignment, p, "PATCH", string(b), map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "raid": arg(args, "role_assignment_id", "roleAssignmentId"),
	}, nil))
}

func toolDeleteRole(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.deleteRoleAssignment, p, "DELETE", "", map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "raid": arg(args, "role_assignment_id", "roleAssignmentId"),
	}, nil))
}

func toolListItems(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	q := url.Values{}
	if t := arg(args, "type"); t != "" {
		q.Set("type", t)
	}
	return mcpFromHTTP(a.invoke(a.listItems, p, "GET", "", map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, q))
}

func toolGetItem(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.getItem, p, "GET", "", map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "iid": arg(args, "item_id", "itemId"),
	}, nil))
}

func toolCreateItem(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	body := map[string]any{
		"displayName": arg(args, "display_name", "displayName"),
		"type":        arg(args, "type"),
	}
	if d := arg(args, "description"); d != "" {
		body["description"] = d
	}
	if f := arg(args, "folder_id", "folderId"); f != "" {
		body["folderId"] = f
	}
	b, _ := json.Marshal(body)
	return mcpFromHTTP(a.invoke(a.createItem, p, "POST", string(b), map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, nil))
}

func toolUpdateItem(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	body := map[string]any{}
	if d := arg(args, "display_name", "displayName"); d != "" {
		body["displayName"] = d
	}
	if args["description"] != nil {
		body["description"] = arg(args, "description")
	}
	b, _ := json.Marshal(body)
	return mcpFromHTTP(a.invoke(a.updateItem, p, "PATCH", string(b), map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "iid": arg(args, "item_id", "itemId"),
	}, nil))
}

func toolDeleteItem(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.deleteItem, p, "DELETE", "", map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "iid": arg(args, "item_id", "itemId"),
	}, nil))
}

func toolGetDefinition(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.getDefinition, p, "POST", "{}", map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "iid": arg(args, "item_id", "itemId"),
	}, nil))
}

func toolUpdateDefinition(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	def := argJSON(args, "definition")
	body := `{"definition":` + def + `}`
	return mcpFromHTTP(a.invoke(a.updateDefinition, p, "POST", body, map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "iid": arg(args, "item_id", "itemId"),
	}, nil))
}

func toolBulkMove(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	ids := []string{}
	for _, key := range []string{"item_ids", "itemIds", "items"} {
		raw, ok := args[key].([]any)
		if !ok {
			continue
		}
		for _, v := range raw {
			if id, ok := v.(string); ok && id != "" {
				ids = append(ids, id)
			}
		}
		break
	}
	body := map[string]any{"items": ids}
	if t := arg(args, "target_folder_id", "targetFolderId"); t != "" {
		body["targetFolderId"] = t
	}
	b, _ := json.Marshal(body)
	return mcpFromHTTP(a.invoke(a.bulkMoveItems, p, "POST", string(b), map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"),
	}, nil))
}

func toolCreateFolder(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	body := map[string]any{"displayName": arg(args, "display_name", "displayName")}
	if f := arg(args, "parent_folder_id", "parentFolderId"); f != "" {
		body["parentFolderId"] = f
	}
	b, _ := json.Marshal(body)
	return mcpFromHTTP(a.invoke(a.createFolder, p, "POST", string(b), map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, nil))
}

func toolListFolders(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.listFolders, p, "GET", "", map[string]string{"wid": arg(args, "workspace_id", "workspaceId")}, nil))
}

func toolGetFolder(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.getFolder, p, "GET", "", map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "fid": arg(args, "folder_id", "folderId"),
	}, nil))
}

func toolUpdateFolder(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	b, _ := json.Marshal(map[string]any{"displayName": arg(args, "display_name", "displayName")})
	return mcpFromHTTP(a.invoke(a.updateFolder, p, "PATCH", string(b), map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "fid": arg(args, "folder_id", "folderId"),
	}, nil))
}

func toolDeleteFolder(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.deleteFolder, p, "DELETE", "", map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "fid": arg(args, "folder_id", "folderId"),
	}, nil))
}

func toolMoveFolder(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	b, _ := json.Marshal(map[string]any{"targetFolderId": arg(args, "target_folder_id", "targetFolderId")})
	return mcpFromHTTP(a.invoke(a.moveFolder, p, "POST", string(b), map[string]string{
		"wid": arg(args, "workspace_id", "workspaceId"), "fid": arg(args, "folder_id", "folderId"),
	}, nil))
}

func toolListCapacities(a *API, p *auth.Principal, _ map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.listCapacities, p, "GET", "", nil, nil))
}

func toolGetOp(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.getOperation, p, "GET", "", map[string]string{"oid": arg(args, "operation_id", "operationId")}, nil))
}

func toolGetOpResult(a *API, p *auth.Principal, args map[string]any) mcpToolResult {
	return mcpFromHTTP(a.invoke(a.getOperationResult, p, "GET", "", map[string]string{"oid": arg(args, "operation_id", "operationId")}, nil))
}

func toolGetKnowledge(_ *API, _ *auth.Principal, args map[string]any) mcpToolResult {
	text := "Fabric Core MCP manages workspaces, items, folders, roles and capacities through Fabric REST. It does not execute notebooks, write lakehouse tables, or run pipelines. Use jobs/instances or a notebook host for those."
	switch arg(args, "item_type", "itemType", "topic") {
	case "Lakehouse", "lakehouse":
		text = "A Lakehouse stores Files/ and Delta tables in OneLake. Create it as an item of type Lakehouse. Data-plane access is ADLS/Blob, not this MCP server."
	case "Notebook", "notebook":
		text = "A Notebook item holds .ipynb definition parts. Execution is a RunNotebook job, which this MCP server does not start. Use the jobs API or a Fabric notebook host."
	case "Warehouse", "warehouse":
		text = "A Warehouse is a T-SQL item. Queries go through the TDS endpoint, not Core MCP."
	case "DataPipeline", "pipeline":
		text = "A DataPipeline item is created and defined here; running it is jobs/instances, not an MCP tool."
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: text}}}
}
