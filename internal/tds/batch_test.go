package tds

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// PrepareBatch is the TDS wire's rules as text, for a caller that is not on
// the wire (Fabric's Data Warehouse MCP server). Each row is a rule the relay
// already applies, so a batch is refused or adapted the same way on both.
func TestPrepareBatchAppliesTheWireRules(t *testing.T) {
	warehouse := Connection{TargetDB: "wh"}
	viewer := Connection{TargetDB: "wh", ReadOnly: true}
	endpoint := Connection{TargetDB: "lh", ReadOnly: true, AnalyticsEndpoint: true}
	for _, tc := range []struct {
		name    string
		conn    Connection
		query   string
		strict  bool
		wantSQL string // "" with wantRefusal set
		refusal string
	}{
		{"a warehouse writer writes", warehouse, "insert into t values (1)", false, "insert into t values (1)", ""},
		{"a viewer may not write", viewer, "insert into t values (1)", false, "", sessionReadOnly},
		{"a viewer reads", viewer, "select 1", false, "select 1", ""},
		{"the endpoint's data is read-only", endpoint, "update t set a = 1", false, "", endpointReadOnly},
		// isEndpointWrite, not isWriteStatement: the endpoint is where its
		// security is authored, so a view is forwarded where a viewer's
		// session would refuse it.
		{"the endpoint forwards its own objects", endpoint, "create view v as select 1 x", false, "create view v as select 1 x", ""},
		{"a viewer may not create a view", viewer, "create view v as select 1 x", false, "", sessionReadOnly},
		{"a nested CTE is flattened", warehouse, nestedSQL, false, "", ""},
		{"Fabric's restriction is refused by name", warehouse,
			"with o as (with i as (select 1 x) select * from i) insert into t select * from o", false, "", "select-only"},
		{"strict refuses what Fabric does not run", warehouse, "create trigger t on x after insert as select 1", true, "", "trigger"},
		{"strict off forwards it", warehouse, "create trigger t on x after insert as select 1", false,
			"create trigger t on x after insert as select 1", ""},
		{"a parse failure is the engine's to report", warehouse, "select (", false, "select (", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql, refusal := PrepareBatch(tc.conn, tc.query, tc.strict)
			if tc.refusal != "" {
				if sql != "" || !strings.Contains(strings.ToLower(refusal), strings.ToLower(tc.refusal)) {
					t.Fatalf("got (%q, %q), want a refusal naming %q", sql, refusal, tc.refusal)
				}
				return
			}
			if refusal != "" {
				t.Fatalf("refused: %s", refusal)
			}
			if tc.wantSQL != "" && sql != tc.wantSQL {
				t.Fatalf("sql = %q, want %q", sql, tc.wantSQL)
			}
			if tc.query == nestedSQL && (sql == nestedSQL || strings.Count(strings.ToLower(sql), "with") > 1) {
				t.Fatalf("nested CTE not flattened: %q", sql)
			}
		})
	}
}

// Time travel is resolved for the batch's connection, and a resolver's
// refusal is Fabric's refusal, not a forwarded statement.
func TestPrepareBatchResolvesTimeTravelForItsConnection(t *testing.T) {
	q := "select * from t option (for timestamp as of '2026-01-01T00:00:00')"
	refusing := Connection{TargetDB: "lh", TimeTravel: func(string, time.Time) (*tsql.TimeTravelSnapshot, bool, error) {
		return nil, false, errors.New("no history")
	}}
	if sql, refusal := PrepareBatch(refusing, q, false); sql != "" || refusal == "" {
		t.Fatalf("a resolver that cannot answer must refuse: (%q, %q)", sql, refusal)
	}
	// A connection with no resolver (a Warehouse without versioning) forwards
	// the hint for the engine to judge, as the wire does.
	if sql, refusal := PrepareBatch(Connection{TargetDB: "wh"}, q, false); sql != q || refusal != "" {
		t.Fatalf("no resolver: (%q, %q), want the batch forwarded untouched", sql, refusal)
	}
}

func TestObserveBatchReportsWhatAnAcceptedWriteMoved(t *testing.T) {
	var got []tsql.Flow
	var db string
	obs := func(database string, flows []tsql.Flow) { db, got = database, flows }
	ObserveBatch(obs, "wh", "insert into dbo.gold select * from dbo.silver")
	if db != "wh" || len(got) == 0 {
		t.Fatalf("observed %q %v", db, got)
	}
	for _, quiet := range []struct {
		obs Observer
		db  string
		sql string
	}{
		{nil, "wh", "insert into a select * from b"}, // nobody watching
		{obs, "", "insert into a select * from b"},   // no database
		{obs, "wh", "select 1"},                      // moves nothing
		{obs, "wh", "exec sp_who"},                   // looks like a write, moves nothing
	} {
		got = nil
		ObserveBatch(quiet.obs, quiet.db, quiet.sql)
		if got != nil {
			t.Errorf("%q observed %v", quiet.sql, got)
		}
	}
}

func TestTargetFirstCarriesTheRoutersRungAndTheTargetsOneLakeGrant(t *testing.T) {
	c := Connection{TargetDB: "lh", Role: RoleReader, Grants: []Grant{
		{Database: "other", Role: RoleOwner},
		{Database: "lh", Role: RoleOwner, OneLake: true, OneLakeRoles: []string{"r"},
			ShortcutTables: []string{"s"}, DeniedTables: []string{"d"}},
	}}
	got := TargetFirst(c)
	if len(got) != 3 || got[0].Database != "lh" || got[0].Role != RoleReader {
		t.Fatalf("target first with the router's rung: %+v", got)
	}
	if !got[0].OneLake || got[0].OneLakeRoles[0] != "r" || got[0].ShortcutTables[0] != "s" || got[0].DeniedTables[0] != "d" {
		t.Errorf("the target's OneLake grant is carried: %+v", got[0])
	}
	if only := TargetFirst(Connection{TargetDB: "wh", Role: RoleOwner}); len(only) != 1 || only[0].OneLake {
		t.Errorf("no sweep: just the target, %+v", only)
	}
}
