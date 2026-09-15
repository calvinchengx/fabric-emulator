package server_test

// Direct Lake on SQL, end to end (docs/59): a Power BI token runs a DAX query
// over a model whose shared expression is Sql.Database, and the rows come
// through the warehouse's SQL analytics endpoint AS THE CALLER — so the
// endpoint's own row-level security, column denials and SELECT grants decide
// what each caller gets. Two callers, one query, different answers.
//
// Needs a real SQL Server; skips without one, like the relay's security e2e.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

const powerBIAudience = "https://analysis.windows.net/powerbi/api"

// sqlFlavourModel creates a Direct Lake on SQL model over the fixture's
// warehouse: one table, Sales, over dbo.<entity> with the given source columns,
// under the given directLakeBehavior ("" for the default).
func (f *secFixture) sqlFlavourModel(t *testing.T, name, entity, behavior string, columns ...string) *store.Item {
	t.Helper()
	var cols []map[string]string
	for _, c := range columns {
		cols = append(cols, map[string]string{"name": c, "dataType": "string", "sourceColumn": c})
	}
	model := map[string]any{
		"expressions": []map[string]string{{"name": "DL", "kind": "m",
			"expression": fmt.Sprintf(`let database = Sql.Database("tenant.datawarehouse.fabric.microsoft.com", "%s") in database`, f.wh.ID)}},
		"tables": []map[string]any{{"name": "Sales", "columns": cols, "partitions": []map[string]any{{
			"name": "p", "mode": "directLake",
			"source": map[string]string{"type": "entity", "entityName": entity, "schemaName": "dbo", "expressionSource": "DL"}}}}},
	}
	if behavior != "" {
		model["directLakeBehavior"] = behavior
	}
	bim, err := json.Marshal(map[string]any{"name": name, "compatibilityLevel": 1604, "model": model})
	if err != nil {
		t.Fatal(err)
	}
	it := &store.Item{WorkspaceID: f.ws.ID, Type: "SemanticModel", DisplayName: name}
	if err := f.srv.Store.CreateItem(it, []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(bim)}}); err != nil {
		t.Fatal(err)
	}
	return it
}

// dax runs EVALUATE 'Sales' as oid and returns the status and body.
func (f *secFixture) dax(t *testing.T, web *httptest.Server, oid string, model *store.Item) (int, string) {
	t.Helper()
	body := []byte(`{"queries":[{"query":"EVALUATE 'Sales'"}]}`)
	req, err := http.NewRequest("POST", web.URL+"/v1.0/myorg/datasets/"+model.ID+"/executeQueries", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+forgeTokenAs(t, f.emu, powerBIAudience, oid))
	req.Header.Set("Content-Type", "application/json")
	resp, err := web.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestDirectLakeOnSQLAppliesTheEndpointsSecurityToTheCaller(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	owner := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	alice := "aaaa1111-0000-0000-0000-0000000d1a51"
	bob := "bbbb2222-0000-0000-0000-0000000d1a52"
	f.connectAs(t, alice, store.RoleContributor) // provisioned, so the owner can DENY to her
	f.connectAs(t, bob, store.RoleContributor)

	mustExec(t, owner,
		`CREATE TABLE dbo.dl_sales (owner_name sysname, region varchar(10), margin int)`,
		`INSERT INTO dbo.dl_sales VALUES (N'`+alice+`', 'west', 1), (N'`+bob+`', 'east', 2)`,
		`CREATE SCHEMA dlsec`,
		`CREATE FUNCTION dlsec.fn_owner(@owner sysname) RETURNS TABLE WITH SCHEMABINDING
		   AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME() OR IS_ROLEMEMBER('db_owner') = 1`,
		`CREATE SECURITY POLICY dlsec.p ADD FILTER PREDICATE dlsec.fn_owner(owner_name) ON dbo.dl_sales WITH (STATE = ON)`,
		`DENY SELECT ON dbo.dl_sales(margin) TO [`+alice+`]`)

	// ROW-LEVEL SECURITY: one model, one query, each caller their own row — and
	// the owner, whom the predicate exempts, both.
	regions := f.sqlFlavourModel(t, "Regions", "dl_sales", "", "owner_name", "region")
	for who, want := range map[string]string{alice: "west", bob: "east"} {
		code, body := f.dax(t, web, who, regions)
		other := map[string]string{"west": "east", "east": "west"}[want]
		if code != http.StatusOK || !strings.Contains(body, `"`+want+`"`) || strings.Contains(body, `"`+other+`"`) {
			t.Errorf("RLS: %s = %d %s, want only %s", who[:8], code, body, want)
		}
	}
	if code, body := f.dax(t, web, entra.DaemonClientID, regions); code != http.StatusOK ||
		!strings.Contains(body, `"west"`) || !strings.Contains(body, `"east"`) {
		t.Errorf("RLS: owner = %d %s, want both rows", code, body)
	}

	// COLUMN SECURITY: a model reading the denied column fails for the caller
	// it is denied to, and serves the caller it is not.
	margins := f.sqlFlavourModel(t, "Margins", "dl_sales", "", "region", "margin")
	if code, body := f.dax(t, web, alice, margins); code != http.StatusBadRequest || !strings.Contains(body, "refused the read") {
		t.Errorf("CLS: alice = %d %s, want the endpoint's refusal", code, body)
	}
	if code, body := f.dax(t, web, bob, margins); code != http.StatusOK || !strings.Contains(body, `"east"`) {
		t.Errorf("CLS: bob = %d %s", code, body)
	}
}

// SELECT is the endpoint's to give. A principal shared the warehouse for Read
// alone connects but cannot select, until the owner grants SELECT; and without
// Read at all they cannot reach the source.
func TestDirectLakeOnSQLNeedsSelectThroughTheEndpoint(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	owner := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	carol := "cccc3333-0000-0000-0000-0000000d1a53"
	mustExec(t, owner,
		`CREATE TABLE dbo.dl_sales (owner_name sysname, region varchar(10))`,
		`INSERT INTO dbo.dl_sales VALUES (N'x', 'west')`)
	model := f.sqlFlavourModel(t, "Shared", "dl_sales", "", "region")
	share := func(itemID string, perms ...string) {
		t.Helper()
		if err := f.srv.Store.PutItemAccess(store.ItemAccess{ItemID: itemID, PrincipalID: carol, PrincipalType: "User",
			Permissions: perms}); err != nil {
			t.Fatal(err)
		}
	}
	share(model.ID, store.PermRead, store.PermExplore)

	if code, body := f.dax(t, web, carol, model); code != http.StatusBadRequest || !strings.Contains(body, "caller cannot read the source") {
		t.Fatalf("no Read on the warehouse = %d %s", code, body)
	}
	share(f.wh.ID, store.PermRead)
	if code, body := f.dax(t, web, carol, model); code != http.StatusBadRequest || !strings.Contains(body, "refused the read") {
		t.Fatalf("Read without SELECT = %d %s, want the endpoint's refusal", code, body)
	}
	mustExec(t, owner, `GRANT SELECT ON dbo.dl_sales TO [`+carol+`]`)
	if code, body := f.dax(t, web, carol, model); code != http.StatusOK || !strings.Contains(body, `"west"`) {
		t.Fatalf("after GRANT SELECT = %d %s", code, body)
	}
	if err := f.srv.Store.DeleteItemAccess(f.wh.ID, carol); err != nil {
		t.Fatal(err)
	}
	if code, body := f.dax(t, web, carol, model); code != http.StatusBadRequest || !strings.Contains(body, "caller cannot read the source") {
		t.Fatalf("after revoking Read = %d %s", code, body)
	}
}

// directLakeBehavior against a real catalog: under directLakeOnly, each thing
// Fabric falls back to DirectQuery for fails — row-level security, masking, a
// view — while a plain table is served; under automatic every one is served, as
// the caller, which is what the fallback returns.
func TestDirectLakeOnlyFailsWhereTheEndpointWouldFallBack(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	owner := f.connectAs(t, entra.DaemonClientID, store.RoleAdmin)
	dana := "dddd4444-0000-0000-0000-0000000d1a54"
	f.connectAs(t, dana, store.RoleContributor)
	mustExec(t, owner,
		`CREATE TABLE dbo.dl_plain (region varchar(10))`,
		`INSERT INTO dbo.dl_plain VALUES ('west')`,
		`CREATE TABLE dbo.dl_rls (owner_name sysname, region varchar(10))`,
		`INSERT INTO dbo.dl_rls VALUES (N'`+dana+`', 'west'), (N'x', 'east')`,
		`CREATE SCHEMA dlonly`,
		`CREATE FUNCTION dlonly.fn(@owner sysname) RETURNS TABLE WITH SCHEMABINDING AS RETURN SELECT 1 AS ok WHERE @owner = USER_NAME()`,
		`CREATE SECURITY POLICY dlonly.p ADD FILTER PREDICATE dlonly.fn(owner_name) ON dbo.dl_rls WITH (STATE = ON)`,
		`CREATE TABLE dbo.dl_masked (email varchar(40) MASKED WITH (FUNCTION = 'email()'))`,
		`INSERT INTO dbo.dl_masked VALUES ('dana@example.test')`,
		`CREATE VIEW dbo.dl_view AS SELECT region FROM dbo.dl_plain`)

	for _, tc := range []struct {
		entity, column, cause, servedAs string
	}{
		{"dl_plain", "region", "", `"west"`},
		{"dl_rls", "region", "enforces row-level security on [dbo].[dl_rls]", `"west"`},
		{"dl_masked", "email", "masks columns of [dbo].[dl_masked]", `XXX`},
		{"dl_view", "region", "[dbo].[dl_view] is a SQL view", `"west"`},
	} {
		only := f.sqlFlavourModel(t, "only-"+tc.entity, tc.entity, "directLakeOnly", tc.column)
		code, body := f.dax(t, web, dana, only)
		switch {
		case tc.cause == "" && (code != http.StatusOK || !strings.Contains(body, tc.servedAs)):
			t.Errorf("directLakeOnly over %s = %d %s, want served", tc.entity, code, body)
		case tc.cause != "" && (code != http.StatusBadRequest || !strings.Contains(body, tc.cause)):
			t.Errorf("directLakeOnly over %s = %d %s, want a refusal naming %q", tc.entity, code, body, tc.cause)
		}
		auto := f.sqlFlavourModel(t, "auto-"+tc.entity, tc.entity, "automatic", tc.column)
		if code, body := f.dax(t, web, dana, auto); code != http.StatusOK || !strings.Contains(body, tc.servedAs) || strings.Contains(body, `"east"`) {
			t.Errorf("automatic over %s = %d %s, want what the endpoint returns dana (%s)", tc.entity, code, body, tc.servedAs)
		}
	}
}
