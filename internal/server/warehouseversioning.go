package server

// Warehouse write versioning: the TDS observer that gives a Warehouse table a
// history (docs/35-warehouse-time-travel.md, Phase 4), and the retention that
// bounds it (Phase 5).
//
// It is the second consumer of the flows internal/tds hands over -- the first
// is warehouseLineage -- and it follows the same two rules: it acts only on a
// statement the engine ACCEPTED, and it acts on the item a name resolves to,
// never one it guessed.
//
// # It can never fail the statement
//
// The observer runs after the client already has its result, so nothing here
// can change what the statement did. A snapshot that fails is logged with the
// table's name and the statement still stands; the table simply has a gap in
// its history. That ordering is the whole reason this is safe to put on the path
// gold is built through.

import (
	"context"
	"log"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
	"github.com/calvinchengx/fabric-emulator/internal/warehouse"
)

type warehouseVersioner struct {
	st            *store.Store
	be            warehouseBackend
	retentionDays int
}

func newWarehouseVersioner(st *store.Store, be warehouseBackend, retentionDays int) *warehouseVersioner {
	return &warehouseVersioner{st: st, be: be, retentionDays: retentionDays}
}

// observe is a tds.Observer.
func (v *warehouseVersioner) observe(database string, flows []tsql.Flow) {
	for _, f := range flows {
		switch f.Kind {
		case tsql.FlowCreateView, tsql.FlowDropView:
			// A view holds no rows.
		case tsql.FlowRename:
			v.rename(database, f)
		case tsql.FlowDropTable:
			v.drop(database, f)
		default:
			v.snapshot(database, f)
		}
	}
}

// warehouseTarget resolves a SQL name to a Warehouse item, the schema and the
// table. ok is false for anything else -- a Lakehouse is read-only here and a
// SQL Database mirrors on demand, so neither is versioned by this path.
func (v *warehouseVersioner) warehouseTarget(database string, parts []string) (itemID, schema, name string, ok bool) {
	if len(parts) == 0 {
		return "", "", "", false
	}
	itemID = database
	if len(parts) >= 3 {
		itemID = parts[len(parts)-3]
	}
	it, err := v.st.GetItemByID(itemID)
	if err != nil || it.Type != "Warehouse" {
		return "", "", "", false
	}
	if len(parts) >= 2 {
		schema = parts[len(parts)-2]
	}
	return it.ID, schema, parts[len(parts)-1], true
}

func (v *warehouseVersioner) snapshot(database string, f tsql.Flow) {
	itemID, schema, name, ok := v.warehouseTarget(database, f.Target)
	if !ok {
		return
	}
	actual, skipped, err := warehouse.SnapshotTable(context.Background(), v.be.DB(itemID), v.st, itemID, schema, name)
	switch {
	case err != nil:
		log.Printf("versioning: %s %s.%s: %v", f.Kind, schema, name, err)
	case skipped != "":
		log.Printf("versioning: %s not versioned: %s", name, skipped)
	default:
		v.expire(itemID, actual)
	}
}

// expire applies the retention window to the table just written. Doing it on
// the write keeps expiry deterministic under the emulator's controllable clock
// -- there is no background timer to wait for -- and costs one log listing.
func (v *warehouseVersioner) expire(itemID, name string) {
	if _, err := warehouse.ExpireVersions(v.st, itemID, name, v.retentionDays); err != nil {
		log.Printf("versioning: expiring %s: %v", name, err)
	}
}

func (v *warehouseVersioner) drop(database string, f tsql.Flow) {
	itemID, schema, name, ok := v.warehouseTarget(database, f.Target)
	if !ok || (schema != "" && !strings.EqualFold(schema, "dbo")) {
		return
	}
	if err := warehouse.DropTableHistory(v.st, itemID, name); err != nil {
		log.Printf("versioning: dropping history of %s: %v", name, err)
	}
}

func (v *warehouseVersioner) rename(database string, f tsql.Flow) {
	itemID, schema, name, ok := v.warehouseTarget(database, f.Target)
	if !ok || (schema != "" && !strings.EqualFold(schema, "dbo")) {
		return
	}
	if err := warehouse.RenameTableHistory(v.st, itemID, name, f.NewName); err != nil {
		log.Printf("versioning: renaming history of %s to %s: %v", name, f.NewName, err)
	}
}

// chainObservers calls each observer in order.
func chainObservers(obs ...func(string, []tsql.Flow)) func(string, []tsql.Flow) {
	return func(database string, flows []tsql.Flow) {
		for _, o := range obs {
			o(database, flows)
		}
	}
}
