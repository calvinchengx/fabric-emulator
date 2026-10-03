package server

import (
	"bytes"
	"database/sql"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// The observer versions Warehouse tables and nothing else: a Lakehouse is
// read-only on this wire, a name that is not an item is never guessed at, and
// only the dbo schema is versioned.
func TestWarehouseVersionerIgnoresWhatIsNotAWarehouseTable(t *testing.T) {
	f := newLineageFixture(t)
	v := newWarehouseVersioner(f.st, &fakeWH{}, 30)

	// A db handle is never touched for these, so a nil one proves it.
	for _, sql := range []string{
		`UPDATE [` + f.lake.ID + `].[dbo].[t] SET a = 1`, // a Lakehouse
		`UPDATE [no-such-item].[dbo].[t] SET a = 1`,      // not an item
		`DROP TABLE other.t`,                             // not dbo
		`EXEC sp_rename 'other.t', 'u'`,                  // not dbo
		`CREATE VIEW dbo.v AS SELECT 1 AS a`,             // a view holds no rows
		`DROP VIEW dbo.v`,
	} {
		v.observe(f.wh.ID, tsql.DataFlows(sql))
	}
	// And the empty target.
	v.observe(f.wh.ID, []tsql.Flow{{Kind: tsql.FlowModify}})

	for _, id := range []string{f.lake.ID, f.wh.ID} {
		if paths, _ := f.st.ListOneLakePaths(id, "Tables", true); len(paths) != 0 {
			t.Errorf("item %s got Delta files it should not have: %v", id, paths)
		}
	}
}

// A failed snapshot is logged with the table's name and never escapes: the
// observer runs after the client has its result, so it has nothing to fail.
func TestWarehouseVersionerLogsAFailureAndCarriesOn(t *testing.T) {
	db := testsupport.OpenMSSQL(t)
	f := newLineageFixture(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	v := newWarehouseVersioner(f.st, &fakeWH{db: db}, 30)
	// A three-part name addresses the warehouse explicitly; the table is absent,
	// so this is a skip, not a failure.
	v.observe(f.lake.ID, tsql.DataFlows(`UPDATE [`+f.wh.ID+`].[dbo].[ghost] SET a = 1`))
	if !strings.Contains(buf.String(), "ghost not versioned: not a base table") {
		t.Errorf("a skip was not logged: %q", buf.String())
	}

	// A closed handle makes the lookup itself fail: logged, not raised.
	closed, _ := sql.Open("sqlserver", "server=127.0.0.1,1;encrypt=disable")
	_ = closed.Close()
	buf.Reset()
	newWarehouseVersioner(f.st, &fakeWH{db: closed}, 30).observe(f.wh.ID, tsql.DataFlows(`UPDATE dbo.t SET a = 1`))
	if !strings.Contains(buf.String(), "versioning:") {
		t.Errorf("a failure was not logged: %q", buf.String())
	}

	// Drop and rename of a table with no history are quiet no-ops.
	v.observe(f.wh.ID, tsql.DataFlows(`DROP TABLE dbo.never`))
	v.observe(f.wh.ID, tsql.DataFlows(`EXEC sp_rename 'dbo.never', 'other'`))
}
