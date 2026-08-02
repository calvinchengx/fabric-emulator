package api

// The semantic-model end of a data flow.
//
// A medallion does not stop at gold: something reads it. In Fabric that
// something is a semantic model, and Power BI behind it — the two hops a
// pipeline exists to serve, and the two the flow graph could not draw.
//
// # What is recorded, and what is not
//
// A DIRECT LAKE table names its source: the model's shared expression carries
// a onelake.dfs URL and the partition names the entity, so "this model table
// reads that Delta table" is DECLARED, not inferred. loadDirectLakeData
// already resolves exactly that binding to serve a query, so recording it as
// lineage says nothing the emulator was not already acting on.
//
// An IMPORT model is the opposite. Its rows arrive in the definition, already
// detached from wherever they were selected — the emulator sees the bytes and
// not their history. No edge is recorded for one, because the only way to
// produce one would be to guess. An import model that wants to appear in the
// graph reports its own reads, the way a notebook engine does (POST
// .../lineage, docs/31-flow-observability.md), and the medallion example does
// exactly that in semantic_model.py.
//
// # The Power BI hop is an event, not an edge
//
// A query moves no data into anything — it reads. So consumption is published
// on the flow bus (a `query` event naming the dataset and the tables the DAX
// touched) rather than written to lineage_edges, which stays a record of
// movement. Watching a Flow view, that is the pulse at the end of the chain:
// the model lights up when Power BI asks it something.

import (
	"github.com/calvinchengx/fabric-emulator/internal/auth"
	"github.com/calvinchengx/fabric-emulator/internal/semanticmodel"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

// recordModelLineage records an edge for every Direct Lake table in a model:
// the lakehouse Delta it reads → the model table it serves.
//
// Called after a semantic model's definition is created or updated, which is
// the moment the binding becomes true.
func (a *API) recordModelLineage(item *store.Item, p *auth.Principal) {
	m, err := a.parseModelDefinition(item.ID)
	if err != nil {
		return // not a model this emulator can parse: nothing is claimed about it
	}
	for _, table := range m.Tables {
		if table.DirectLake == nil {
			continue // an import table's provenance is not knowable here
		}
		src, ok := a.directLakeSource(m, &table, p)
		if !ok {
			continue
		}
		_ = a.Store.CreateLineageEdge(&store.LineageEdge{
			WorkspaceID:       item.WorkspaceID,
			ActivityName:      "DirectLake",
			SourceWorkspaceID: src.WorkspaceID,
			SourceItemID:      src.ItemID,
			SourcePath:        src.Path,
			TargetWorkspaceID: item.WorkspaceID,
			TargetItemID:      item.ID,
			TargetPath:        "Tables/" + table.Name,
			Producer:          store.ProducerDirectLake,
		})
	}
}

// modelSource is a resolved Direct Lake binding.
type modelSource struct {
	WorkspaceID string
	ItemID      string
	Path        string
}

// directLakeSource resolves one table's binding to a lakehouse table, using the
// same expression parsing that serves a query — so lineage and reads can never
// disagree about where a table comes from.
func (a *API) directLakeSource(m *semanticmodel.Model, table *semanticmodel.Table, p *auth.Principal) (modelSource, bool) {
	expression, ok := m.Expressions[table.DirectLake.ExpressionSource]
	if !ok {
		return modelSource{}, false
	}
	wsRef, lakeRef, err := parseDirectLakeLocation(expression)
	if err != nil {
		return modelSource{}, false
	}
	ws, err := a.resolveDirectLakeWorkspace(wsRef)
	if err != nil {
		return modelSource{}, false
	}
	lake, err := a.resolveDirectLakeLakehouse(ws.ID, lakeRef)
	if err != nil {
		return modelSource{}, false
	}
	entity := table.DirectLake.EntityName
	if table.DirectLake.SchemaName != "" {
		entity = table.DirectLake.SchemaName + "/" + entity
	}
	return modelSource{WorkspaceID: ws.ID, ItemID: lake.ID, Path: "Tables/" + entity}, true
}

// publishQuery reports that a dataset was queried — the Power BI hop, as a
// flow event. It carries the query count rather than the DAX itself: the
// stream is a live view, and a client's query text is its own business.
func (a *API) publishQuery(item *store.Item, queries int, failed bool) {
	status := "Completed"
	if failed {
		status = "Failed"
	}
	a.Store.PublishQuery(item.WorkspaceID, item.ID, item.DisplayName, queries, status)
}
