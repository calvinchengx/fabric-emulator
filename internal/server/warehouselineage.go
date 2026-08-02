package server

// Warehouse lineage: what a T-SQL build moved, as a lineage edge.
//
// Gold is a Warehouse, and dbt builds it over TDS. Every other hop in a
// medallion writes through OneLake, where the store sees it — so before this,
// the flow graph drew landing → bronze → silver and then stopped, one step
// short of the layer a consumer actually reads. The movement was never
// invisible; nothing was looking at it.
//
// internal/tds observes the statements the engine accepted and hands over the
// parsed flows (internal/tsql.DataFlows). This file answers the question that
// needs the store: which Fabric ITEM is each SQL name?
//
// # Naming, and its one ambiguity
//
// Every item gets its own SQL Server database named by the item id, so a
// three-part name carries the item explicitly: [<item-guid>].[dbo].[t] is how
// dbt addresses the reflected lakehouse from the warehouse, and that is what
// makes silver → gold a CROSS-ITEM edge. A one- or two-part name has no
// database part and belongs to the connection's own item.
//
// A name whose database part is not a known item id resolves to nothing, and
// the edge is dropped rather than guessed at.
//
// # Temp tables are followed, not recorded
//
// dbt materialises into `x__dbt_temp` and renames. Recording the temp name
// would put scaffolding in the graph and leave the real table absent, so a
// rename REWRITES the edges already recorded against the temp name. That is
// why sp_rename is observed at all.
//
// Views are recorded too, and matter more than they look: dbt's model body
// lives in a temp VIEW, and the CTAS then selects from that view. Without the
// view's own edges the chain from silver would break at the view. The view is
// resolved through when it is a `__dbt_tmp_vw` scaffold — its sources are
// attributed straight to the table built from it — so the graph shows
// silver_orders → fct_order_lines, not a scaffold node in between.

import (
	"log"
	"strings"
	"sync"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// warehouseLineage records lineage for T-SQL writes observed on the TDS front.
type warehouseLineage struct {
	st *store.Store
	mu sync.Mutex
	// views maps a recorded view (item id + object name, lower-cased) to the
	// table sources it selects from, so a CTAS reading a dbt scaffold view is
	// attributed to the real tables behind it.
	views map[string][]lineageRef
}

// lineageRef is one resolved end of a movement: a Fabric item and the OneLake
// path a table there corresponds to.
type lineageRef struct {
	workspaceID string
	itemID      string
	path        string // Tables/<name>, the same shape OneLake edges use
}

func newWarehouseLineage(st *store.Store) *warehouseLineage {
	return &warehouseLineage{st: st, views: map[string][]lineageRef{}}
}

// observe is the tds.Observer: it turns the flows of one accepted statement
// into lineage edges. database is the connection's own item id.
func (w *warehouseLineage) observe(database string, flows []tsql.Flow) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range flows {
		switch f.Kind {
		case tsql.FlowRename:
			w.rename(database, f)
		case tsql.FlowDropTable, tsql.FlowDropView:
			w.drop(database, f)
		case tsql.FlowCreateView:
			w.recordView(database, f)
		default:
			w.recordEdges(database, f)
		}
	}
}

// recordEdges writes one edge per source of a materialising statement.
func (w *warehouseLineage) recordEdges(database string, f tsql.Flow) {
	target, ok := w.resolve(database, f.Target)
	if !ok {
		return
	}
	for _, src := range w.sources(database, f.Sources) {
		if src == target {
			continue // a table rebuilt from itself is not a movement worth drawing
		}
		err := w.st.CreateLineageEdge(&store.LineageEdge{
			WorkspaceID:       target.workspaceID,
			ActivityName:      activityNameFor(f.Kind),
			SourceWorkspaceID: src.workspaceID,
			SourceItemID:      src.itemID,
			SourcePath:        src.path,
			TargetWorkspaceID: target.workspaceID,
			TargetItemID:      target.itemID,
			TargetPath:        target.path,
			Producer:          store.ProducerWarehouse,
		})
		// Logged, never swallowed: the first version of this discarded the error
		// and a foreign-key rejection looked exactly like a warehouse that moved
		// no data — the graph was simply empty, with nothing to say why.
		if err != nil {
			log.Printf("lineage: recording %s -> %s: %v", src.path, target.path, err)
		}
	}
}

// recordView remembers a view's sources so a later read of it resolves to the
// tables behind it. A view is scaffolding for lineage purposes: it holds no
// bytes, so drawing a node for it would add a hop the data never took.
func (w *warehouseLineage) recordView(database string, f tsql.Flow) {
	target, ok := w.resolve(database, f.Target)
	if !ok {
		return
	}
	w.views[viewKey(target)] = w.sources(database, f.Sources)
}

// sources resolves each source name, expanding any that names a known view.
func (w *warehouseLineage) sources(database string, names [][]string) []lineageRef {
	var out []lineageRef
	seen := map[lineageRef]bool{}
	for _, n := range names {
		ref, ok := w.resolve(database, n)
		if !ok {
			continue
		}
		expanded := []lineageRef{ref}
		if behind, isView := w.views[viewKey(ref)]; isView {
			expanded = behind
		}
		for _, e := range expanded {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// rename moves every edge recorded against the old name onto the new one —
// dbt's build-then-swap, which would otherwise leave the graph naming the
// temp table and never the real one.
func (w *warehouseLineage) rename(database string, f tsql.Flow) {
	from, ok := w.resolve(database, f.Target)
	if !ok {
		return
	}
	// sp_rename's new name is always unqualified: the object stays in its
	// schema and database, so only the object part changes.
	to := from
	to.path = tablePath(f.NewName)
	if from == to {
		return
	}
	if behind, isView := w.views[viewKey(from)]; isView {
		w.views[viewKey(to)] = behind
		delete(w.views, viewKey(from))
	}
	if err := w.st.RenameLineagePath(from.itemID, from.path, to.path); err != nil {
		log.Printf("lineage: renaming %s to %s: %v", from.path, to.path, err)
	}
}

// drop retires the edges into a dropped object: a table that no longer exists
// is not part of the flow, and leaving it drawn is how a graph starts lying.
func (w *warehouseLineage) drop(database string, f tsql.Flow) {
	ref, ok := w.resolve(database, f.Target)
	if !ok {
		return
	}
	delete(w.views, viewKey(ref))
	if err := w.st.DeleteLineageEdgesInto(ref.itemID, ref.path); err != nil {
		log.Printf("lineage: retiring edges into %s: %v", ref.path, err)
	}
}

// resolve maps a SQL name to a Fabric item and a OneLake table path. A
// three-part name carries the item id as its database; anything shorter belongs
// to the connection's own item.
func (w *warehouseLineage) resolve(database string, parts []string) (lineageRef, bool) {
	if len(parts) == 0 {
		return lineageRef{}, false
	}
	itemID := database
	if len(parts) >= 3 {
		itemID = parts[len(parts)-3]
	}
	it, err := w.st.GetItemByID(itemID)
	if err != nil {
		return lineageRef{}, false // not an item this emulator knows: never guessed at
	}
	return lineageRef{workspaceID: it.WorkspaceID, itemID: it.ID, path: tablePath(parts[len(parts)-1])}, true
}

// tablePath is the OneLake shape a warehouse table shares with a lakehouse one,
// so both ends of a silver → gold edge are addressed the same way and the graph
// can join them.
func tablePath(name string) string { return "Tables/" + name }

func viewKey(r lineageRef) string { return r.itemID + "|" + strings.ToLower(r.path) }

// activityNameFor labels the edge with the statement that caused it, which is
// what the portal shows as the producer of a hop.
func activityNameFor(kind string) string {
	switch kind {
	case tsql.FlowInsert:
		return "INSERT"
	default:
		return "CTAS" // CREATE TABLE AS / SELECT INTO — the same movement
	}
}
