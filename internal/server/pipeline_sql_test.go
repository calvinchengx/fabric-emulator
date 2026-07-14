package server_test

// Pipeline SQL leaf activities e2e (gated on WAREHOUSE_MSSQL_DSN): a real entra
// token drives the jobs API to run a DataPipeline whose Script and
// SqlServerStoredProcedure activities execute real T-SQL against a Warehouse
// item's own SQL Server database — the same per-item backend the TDS endpoint
// and the SQLDatabase mirror share.

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/config"
	"github.com/calvinchengx/fabric-emulator/internal/server"
	"github.com/calvinchengx/fabric-emulator/internal/store"
)

func TestPipelineSQLActivitiesE2E(t *testing.T) {
	dsn := os.Getenv("WAREHOUSE_MSSQL_DSN")
	if dsn == "" {
		t.Skip("set WAREHOUSE_MSSQL_DSN (a reachable SQL Server) to run the pipeline SQL-activity e2e")
	}

	emu := entra.StartT(t)
	cfg := &config.Config{
		EntraIssuer:     emu.Origin + "/" + emu.TenantID + "/v2.0",
		SQLTDSAddr:      "127.0.0.1:0",
		WarehouseSQLURL: dsn,
	}
	if err := cfg.Finish(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(cfg, emu.HTTPClient())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	fabric := httptest.NewServer(srv.Handler())
	t.Cleanup(fabric.Close)
	token := forgeAppToken(t, emu, "https://api.fabric.microsoft.com")
	f := &fixture{t: t, emu: emu, srv: srv, fabric: fabric, token: token}

	var ws struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token, map[string]string{"displayName": "sql-activity-ws"}, &ws),
		http.StatusCreated, "create workspace")
	var wh struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "wh", "type": "Warehouse"}, &wh), http.StatusCreated, "create warehouse")

	// Script: DDL + DML (NonQuery) then a SELECT (Query) in one activity, plus a
	// stored procedure the second activity calls with a parameter.
	content := `{"properties":{"activities":[
        {"name":"Setup","type":"Script","typeProperties":{
          "database":{"itemId":"` + wh.ID + `"},
          "scripts":[
            {"type":"NonQuery","text":"CREATE TABLE dbo.people (id INT, name NVARCHAR(20))"},
            {"type":"NonQuery","text":"INSERT INTO dbo.people VALUES (1,'ada'),(2,'bob')"},
            {"type":"Query","text":"SELECT id, name FROM dbo.people ORDER BY id"},
            {"type":"NonQuery","text":"CREATE PROCEDURE dbo.GetPerson @id INT AS BEGIN SELECT id, name FROM dbo.people WHERE id = @id END"}
          ]}},
        {"name":"CallProc","type":"SqlServerStoredProcedure","dependsOn":[{"activity":"Setup","dependencyConditions":["Succeeded"]}],
          "typeProperties":{
            "database":{"itemId":"` + wh.ID + `"},
            "storedProcedureName":"dbo.GetPerson",
            "storedProcedureParameters":{"id":{"value":2}}}}
      ]}}`
	var pl struct{ ID string }
	f.mustStatus(f.call("POST", "/v1/workspaces/"+ws.ID+"/items", f.token,
		map[string]string{"displayName": "sql-pl", "type": "DataPipeline"}, &pl), http.StatusCreated, "create pipeline")
	payload := base64.StdEncoding.EncodeToString([]byte(content))
	if err := srv.API.Store.SetDefinition(pl.ID, []store.DefinitionPart{
		{Path: "pipeline-content.json", Payload: payload, PayloadType: "InlineBase64"},
	}); err != nil {
		t.Fatal(err)
	}

	base := "/v1/workspaces/" + ws.ID + "/items/" + pl.ID + "/jobs/instances"
	run := f.call("POST", base+"?jobType=Pipeline", f.token, map[string]any{}, nil)
	f.mustStatus(run, http.StatusAccepted, "run pipeline")
	loc := run.Header.Get("Location")
	jid := loc[strings.LastIndex(loc, "/")+1:]

	var job struct{ Status string }
	f.mustStatus(f.call("GET", base+"/"+jid, f.token, nil, &job), http.StatusOK, "get job")
	if job.Status != "Completed" {
		t.Fatalf("pipeline job status = %s", job.Status)
	}

	var runs struct {
		Value []struct {
			Name   string         `json:"activityName"`
			Output map[string]any `json:"output"`
		}
	}
	f.mustStatus(f.call("POST", base+"/"+jid+"/queryactivityruns", f.token, nil, &runs), http.StatusOK, "activity runs")
	outputOf := func(name string) map[string]any {
		for _, r := range runs.Value {
			if r.Name == name {
				return r.Output
			}
		}
		t.Fatalf("no activity run named %q", name)
		return nil
	}

	// The Script activity's SELECT (its 3rd script, index 2) returned the real
	// rows the INSERT wrote.
	setup := outputOf("Setup")
	resultSets := setup["resultSets"].([]any)
	if len(resultSets) != 4 {
		t.Fatalf("resultSets count = %d, want 4", len(resultSets))
	}
	selectRS := resultSets[2].(map[string]any)
	rows := selectRS["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("SELECT rows = %d, want 2", len(rows))
	}
	if rows[0].(map[string]any)["name"] != "ada" || rows[1].(map[string]any)["name"] != "bob" {
		t.Fatalf("SELECT rows = %+v", rows)
	}

	// The stored procedure, called with id=2, returned exactly bob's row.
	proc := outputOf("CallProc")
	procRS := proc["resultSets"].([]any)[0].(map[string]any)
	procRows := procRS["rows"].([]any)
	if len(procRows) != 1 {
		t.Fatalf("stored procedure rows = %d, want 1", len(procRows))
	}
	if procRows[0].(map[string]any)["name"] != "bob" {
		t.Fatalf("stored procedure row = %+v, want name=bob", procRows[0])
	}
}
