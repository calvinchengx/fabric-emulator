package tds

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

func TestAcceptedReadsTheOutcome(t *testing.T) {
	okDone := done(doneFinal, 0)
	if ok, understood := accepted(okDone); !ok || !understood {
		t.Fatalf("clean DONE = (%v, %v); want accepted", ok, understood)
	}
	// An ERROR token means nothing moved.
	failed := concat(errorToken(2714, "There is already an object named 'x'."), done(doneError, 0))
	if ok, understood := accepted(failed); ok || !understood {
		t.Fatalf("ERROR stream = (%v, %v); want a understood rejection", ok, understood)
	}
	// The DONE error bit is the other way a failure is reported.
	if ok, understood := accepted(done(doneError, 0)); ok || !understood {
		t.Fatalf("DONE(error) = (%v, %v); want a understood rejection", ok, understood)
	}
	// A token this file cannot size leaves the outcome unknown — and unknown is
	// never treated as success.
	if ok, understood := accepted([]byte{0x81, 0x01, 0x02}); ok || understood {
		t.Fatalf("COLMETADATA stream = (%v, %v); want unknown", ok, understood)
	}
	if ok, understood := accepted(okDone[:5]); ok || understood {
		t.Fatalf("truncated DONE = (%v, %v); want unknown", ok, understood)
	}
}

func TestObserveBatchOnlyReportsAcceptedWrites(t *testing.T) {
	var got []string
	obs := func(db string, flows []tsql.Flow) {
		for _, f := range flows {
			got = append(got, db+"|"+f.Kind+"|"+strings.Join(f.Target, "."))
		}
	}
	batch := func(q string) []byte { return batchMsg(q) }

	// A write the engine accepted is reported, with the resolved database.
	observeBatch(obs, "wh-guid", PktSQLBatch, batch("SELECT a INTO dbo.dst FROM dbo.src"), done(doneFinal, 0))
	if len(got) != 1 || got[0] != "wh-guid|"+tsql.FlowSelectInto+"|dbo.dst" {
		t.Fatalf("accepted write = %v", got)
	}

	got = nil
	// A read moves nothing; a failed write moved nothing; an unreadable outcome
	// is not success; no database means no item to attribute to.
	observeBatch(obs, "wh-guid", PktSQLBatch, batch("SELECT * FROM dbo.src"), done(doneFinal, 0))
	observeBatch(obs, "wh-guid", PktSQLBatch, batch("DROP TABLE dbo.t"),
		concat(errorToken(3701, "Cannot drop"), done(doneError, 0)))
	observeBatch(obs, "wh-guid", PktSQLBatch, batch("DROP TABLE dbo.t"), []byte{0x81, 0x00})
	observeBatch(obs, "", PktSQLBatch, batch("DROP TABLE dbo.t"), done(doneFinal, 0))
	observeBatch(nil, "wh-guid", PktSQLBatch, batch("DROP TABLE dbo.t"), done(doneFinal, 0))
	if got != nil {
		t.Fatalf("reported something it should not have: %v", got)
	}
}

// TestSpliceObservesAcceptedWrite drives the real splice loop: the observation
// must happen on the byte-forwarding path (the one a real ODBC client uses),
// and only after the backend has answered.
func TestSpliceObservesAcceptedWrite(t *testing.T) {
	clientA, clientB := net.Pipe()
	backendA, backendB := net.Pipe()
	seen := make(chan string, 4)
	go spliceSession(clientA, backendA, false, false,
		func(db string, flows []tsql.Flow) {
			for _, f := range flows {
				seen <- db + "|" + f.Kind + "|" + strings.Join(f.Target, ".") +
					"<-" + strings.Join(f.Sources[0], ".")
			}
		}, "wh-guid")

	// Client sends a CTAS; the fake backend accepts it.
	go func() {
		typ, data, err := ReadMessage(backendB)
		if err != nil || typ != PktSQLBatch {
			return
		}
		_ = data
		_ = WriteMessage(backendB, PktTabular, done(doneFinal, 0))
	}()
	if err := WriteMessage(clientB, PktSQLBatch, batchMsg("CREATE TABLE dbo.gold AS SELECT * FROM dbo.silver")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMessage(clientB); err != nil { // the client's response comes first
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		// The statement reaching the observer is post-dialect-fix — the one the
		// engine actually ran (CTAS rewritten to SELECT … INTO), and the flow
		// names both ends of the movement.
		if got != "wh-guid|"+tsql.FlowSelectInto+"|dbo.gold<-dbo.silver" {
			t.Fatalf("observed %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the splice path never observed the accepted write")
	}
}

// TestObserveBatchReadsEveryRPCTextParam is the regression for the bug that
// made dbt's entire warehouse build invisible while a plain batch through the
// same front recorded fine.
//
// sp_prepexec's parameters are (@handle, @params, @stmt): the FIRST text
// parameter is the declaration "@P1 int", not the statement. Taking it meant
// mightMove saw a non-statement, rejected it, and every gold table dbt built
// went unobserved — with nothing logged, because nothing had gone wrong.
func TestObserveBatchReadsEveryRPCTextParam(t *testing.T) {
	var got []string
	obs := func(db string, flows []tsql.Flow) {
		for _, f := range flows {
			got = append(got, f.Kind+"|"+strings.Join(f.Target, "."))
		}
	}
	// The statement is the THIRD parameter, behind @handle and @params.
	observeBatch(obs, "wh-guid", PktRPC,
		spPrepexec("SELECT * INTO dbo.gold FROM dbo.silver"), done(doneFinal, 0))
	if len(got) != 1 || got[0] != tsql.FlowSelectInto+"|dbo.gold" {
		t.Fatalf("sp_prepexec write = %v; want the statement behind the declaration", got)
	}

	// sp_executesql carries the statement first; both shapes must work.
	got = nil
	observeBatch(obs, "wh-guid", PktRPC,
		rpcMsg(10, nvarcharParam("@stmt", "CREATE TABLE dbo.g AS SELECT * FROM dbo.s")),
		done(doneFinal, 0))
	if len(got) != 1 || got[0] != tsql.FlowCTAS+"|dbo.g" {
		t.Fatalf("sp_executesql write = %v", got)
	}

	// A parameter declaration alone is not a statement and moves nothing.
	got = nil
	observeBatch(obs, "wh-guid", PktRPC,
		rpcMsg(10, nvarcharParam("@stmt", "SELECT * FROM dbo.s"),
			nvarcharParam("@params", "@P1 int")), done(doneFinal, 0))
	if got != nil {
		t.Fatalf("a read recorded something: %v", got)
	}
}

// TestObserveDbtsRealStatementShape uses the statements captured verbatim from
// a dbt-fabric run against this emulator (examples/.../gold/logs/dbt.log).
//
// Every one arrives behind a `/* {"app": "dbt", …} */` metadata comment and a
// `USE [<database>];`. The prefilter judged "the first keyword" and saw `/*`
// or `USE`, so the whole gold build was discarded before parsing — the graph
// showed gold with no incoming edges and nothing to explain why.
func TestObserveDbtsRealStatementShape(t *testing.T) {
	const db = "f590f0f7-0657-4019-9b10-bc86b5b5f74b"
	var got []string
	obs := func(_ string, flows []tsql.Flow) {
		for _, f := range flows {
			got = append(got, f.Kind+"|"+strings.Join(f.Target, "."))
		}
	}
	run := func(sql string) { observeBatch(obs, db, PktSQLBatch, batchMsg(sql), done(doneFinal, 0)) }

	// The CTAS, exactly as dbt sends it.
	run(`/* {"app": "dbt", "dbt_version": "1.12.0", "node_id": "model.contoso_gold.dim_customer"} */
    USE [` + db + `];
    EXEC('CREATE TABLE [` + db + `].[dbo].[dim_customer__dbt_temp]  AS SELECT * FROM [` + db + `].[dbo].[dim_customer__dbt_temp__dbt_tmp_vw] OPTION (LABEL = ''dbt-fabric-dw'');');`)
	if len(got) != 1 || got[0] != tsql.FlowCTAS+"|"+db+".dbo.dim_customer__dbt_temp" {
		t.Fatalf("dbt CTAS = %v; want the table it built", got)
	}

	// Its catalog probe is a read behind the same preamble and must stay silent.
	got = nil
	run(`/* {"app": "dbt"} */
        USE [` + db + `];
        select sch.name as schema_name, obj.name as view_name
        from sys.sql_expression_dependencies refs
        inner join sys.objects obj on refs.referencing_id = obj.object_id
    OPTION (LABEL = 'dbt-fabric-dw');`)
	if got != nil {
		t.Fatalf("a catalog read recorded %v", got)
	}

	// The swap, and the scaffold drop.
	got = nil
	run(`/* {"app": "dbt"} */
    USE [` + db + `];
    EXEC sp_rename 'dbo.dim_customer__dbt_temp', 'dim_customer';`)
	if len(got) != 1 || got[0] != tsql.FlowRename+"|dbo.dim_customer__dbt_temp" {
		t.Fatalf("dbt rename = %v", got)
	}
	got = nil
	run(`/* {"app": "dbt"} */
    USE [` + db + `];
    EXEC('DROP table IF EXISTS [dbo].[dim_customer__dbt_temp];');`)
	if len(got) != 1 || got[0] != tsql.FlowDropTable+"|dbo.dim_customer__dbt_temp" {
		t.Fatalf("dbt drop = %v", got)
	}
}

func TestLeadingKeywordSkipsCommentsAndPreamble(t *testing.T) {
	for sql, want := range map[string]string{
		"  select 1":                     "SELECT",
		"-- a note\nCREATE TABLE t":      "CREATE",
		"/* {\"app\":\"dbt\"} */ USE [x]": "USE",
		"/* one *//* two */ drop table t": "DROP",
		"/* never closed":                 "",
		"":                                "",
	} {
		if got := leadingKeyword(sql); got != want {
			t.Errorf("leadingKeyword(%q) = %q; want %q", sql, got, want)
		}
	}
}
