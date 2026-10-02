package server_test

// One OneLake row filter, two engines, the same rows (docs/54, docs/60).
//
// Direct Lake on OneLake evaluates a role's row filter in Go over the Delta it
// read; the SQL analytics endpoint in user identity mode has the same filter
// synced in as a SQL Server security policy. Both come from one parser in
// pkg/onelakesec, but parsing the same text is not the same as admitting the
// same rows: case, trailing spaces, NULL and number semantics are where two
// engines drift. So each filter here is run through both, against a real SQL
// Server, and the row sets must match — and must not be every row, or the
// comparison proves nothing.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/parquet-go/parquet-go"

	"github.com/calvinchengx/fabric-emulator/internal/store"
)

type rlsRow struct {
	Region *string `parquet:"region,optional"`
	Amount int64   `parquet:"amount"`
	Price  float64 `parquet:"price"`
}

func ptr(s string) *string { return &s }

func TestDirectLakeAndTheEndpointAdmitTheSameRows(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	viewer := "aaaa1111-0000-0000-0000-0000000da7a1"
	f.grantRole(t, viewer, store.RoleViewer)

	rows := []rlsRow{
		{ptr("west"), 10, 1.5}, {ptr("West  "), 20, 2.25}, {nil, 30, 3}, {ptr("east"), 40, 4.5},
		{ptr("north"), 50, 2.5}, {ptr(""), 60, 3.25},
	}
	var buf bytes.Buffer
	pw := parquet.NewGenericWriter[rlsRow](&buf)
	if _, err := pw.Write(rows); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	for rel, content := range map[string][]byte{
		"Tables/sales/part-0.parquet":                       buf.Bytes(),
		"Tables/sales/_delta_log/00000000000000000000.json": []byte(`{"add":{"path":"part-0.parquet"}}`),
	} {
		if err := f.srv.Store.CreateOneLakePath(&store.OneLakePath{WorkspaceID: f.ws.ID, ItemID: lake.ID, RelPath: rel, Content: content}, false); err != nil {
			t.Fatal(err)
		}
	}

	// A Direct Lake on OneLake model over the lakehouse, shared with the viewer.
	bim, _ := json.Marshal(map[string]any{"name": "Parity", "compatibilityLevel": 1604, "model": map[string]any{
		"expressions": []map[string]string{{"name": "DL", "kind": "m", "expression": fmt.Sprintf(
			`let Source = AzureStorage.DataLake("https://onelake.dfs.fabric.microsoft.com/%s/%s", [HierarchicalNavigation=true]) in Source`, f.ws.ID, lake.ID)}},
		"tables": []map[string]any{{"name": "Sales", "columns": []map[string]string{
			{"name": "Region", "dataType": "string", "sourceColumn": "region"},
			{"name": "Amount", "dataType": "int64", "sourceColumn": "amount"},
			{"name": "Price", "dataType": "double", "sourceColumn": "price"}},
			"partitions": []map[string]any{{"name": "p", "mode": "directLake",
				"source": map[string]string{"type": "entity", "entityName": "sales", "expressionSource": "DL"}}}}},
	}})
	model := &store.Item{WorkspaceID: f.ws.ID, Type: "SemanticModel", DisplayName: "Parity"}
	if err := f.srv.Store.CreateItem(model, []store.DefinitionPart{{Path: "model.bim", PayloadType: "InlineBase64",
		Payload: base64.StdEncoding.EncodeToString(bim)}}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.Store.PutItemAccess(store.ItemAccess{ItemID: model.ID, PrincipalID: viewer, PrincipalType: "User",
		Permissions: []string{store.PermRead, store.PermExplore}}); err != nil {
		t.Fatal(err)
	}
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}

	key := func(region any, amount, price float64) string {
		r := "<null>"
		if s, ok := region.(string); ok {
			r = strings.TrimRight(s, " ")
		}
		return fmt.Sprintf("%s|%g|%g", r, amount, price)
	}
	viaDirectLake := func(filter string) []string {
		t.Helper()
		code, body := f.dax(t, web, viewer, model)
		if code != http.StatusOK {
			t.Fatalf("%s: Direct Lake = %d %s", filter, code, body)
		}
		var out struct {
			Results []struct {
				Tables []struct {
					Rows []map[string]any `json:"rows"`
				} `json:"tables"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range out.Results[0].Tables[0].Rows {
			got = append(got, key(r["Sales[Region]"], r["Sales[Amount]"].(float64), r["Sales[Price]"].(float64)))
		}
		sort.Strings(got)
		return got
	}
	viaEndpoint := func(filter string) []string {
		t.Helper()
		db, err := f.open(t, viewer, lake.ID)
		if err != nil {
			t.Fatalf("%s: endpoint connect: %v", filter, err)
		}
		rs, err := db.QueryContext(context.Background(), `SELECT region, amount, price FROM dbo.sales`)
		if err != nil {
			t.Fatalf("%s: endpoint: %v", filter, err)
		}
		defer rs.Close()
		var got []string
		for rs.Next() {
			var region *string
			var amount int64
			var price float64
			if err := rs.Scan(&region, &amount, &price); err != nil {
				t.Fatal(err)
			}
			var r any
			if region != nil {
				r = *region
			}
			got = append(got, key(r, float64(amount), price))
		}
		sort.Strings(got)
		return got
	}

	for _, filter := range []string{
		"SELECT * FROM sales WHERE region = 'WEST'",
		"SELECT * FROM sales WHERE region <> 'west'",
		"SELECT * FROM sales WHERE NOT region = 'west'",
		"SELECT * FROM sales WHERE region > 'M'",
		"SELECT * FROM sales WHERE amount >= 20 AND price < 3.25",
		"SELECT * FROM sales WHERE region IN ('west', 'North') OR amount = 30",
		"SELECT * FROM sales WHERE region NOT IN ('east')",
		"SELECT * FROM sales WHERE region IS NULL",
		"SELECT * FROM sales WHERE region IS NOT BLANK",
		"SELECT * FROM sales WHERE price > 2.25",
	} {
		role := store.OneLakeRole{ItemID: lake.ID, Name: "Filtered", Body: []byte(fmt.Sprintf(`{"name":"Filtered","decisionRules":[{"effect":"Permit","permission":[
		  {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
		  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
		  "constraints":{"rows":[{"tablePath":"/Tables/sales","value":%q}]}}],
		  "members":{"microsoftEntraMembers":[{"objectId":%q}]}}`, filter, viewer))}
		if err := f.srv.Store.PutOneLakeRoles(lake.ID, []store.OneLakeRole{role}); err != nil {
			t.Fatal(err)
		}
		dl, ep := viaDirectLake(filter), viaEndpoint(filter)
		if strings.Join(dl, ";") != strings.Join(ep, ";") {
			t.Errorf("%s:\n  Direct Lake %v\n  endpoint    %v", filter, dl, ep)
		}
		t.Logf("%s -> %v", filter, dl)
		if len(dl) == 0 {
			t.Errorf("%s: no row admitted — an empty match proves nothing either", filter)
		}
		if len(dl) == len(rows) {
			t.Errorf("%s: every row admitted — the filter excluded nothing, so the comparison proves nothing", filter)
		}
	}
}

// The endpoint syncs each OneLake role on its own, so a reader in a row role
// and a different column role would get the union. A predicate cannot raise
// Fabric's query error; the reader is shown no rows instead — while each role's
// members alone read what their role grants.
func TestTheEndpointShowsNoRowsToMixedRowAndColumnRoles(t *testing.T) {
	f := newSecFixture(t)
	web := httptest.NewServer(f.srv.Handler())
	t.Cleanup(web.Close)
	lake, endpoint := f.lakehouse(t)
	both := "aaaa1111-0000-0000-0000-0000000b07a1"
	rowsOnly := "bbbb2222-0000-0000-0000-0000000b07a2"
	colsOnly := "cccc3333-0000-0000-0000-0000000b07a3"
	for _, oid := range []string{both, rowsOnly, colsOnly} {
		f.grantRole(t, oid, store.RoleViewer)
	}
	var buf bytes.Buffer
	pw := parquet.NewGenericWriter[rlsRow](&buf)
	if _, err := pw.Write([]rlsRow{{ptr("west"), 10, 1}, {ptr("east"), 20, 2}}); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	for rel, content := range map[string][]byte{
		"Tables/sales/part-0.parquet":                       buf.Bytes(),
		"Tables/sales/_delta_log/00000000000000000000.json": []byte(`{"add":{"path":"part-0.parquet"}}`),
	} {
		if err := f.srv.Store.CreateOneLakePath(&store.OneLakePath{WorkspaceID: f.ws.ID, ItemID: lake.ID, RelPath: rel, Content: content}, false); err != nil {
			t.Fatal(err)
		}
	}
	role := func(name, constraints string, members ...string) store.OneLakeRole {
		var ms []string
		for _, m := range members {
			ms = append(ms, fmt.Sprintf(`{"objectId":%q}`, m))
		}
		return store.OneLakeRole{ItemID: lake.ID, Name: name, Body: []byte(fmt.Sprintf(`{"name":%q,"decisionRules":[{"effect":"Permit","permission":[
		  {"attributeName":"Path","attributeValueIncludedIn":["Tables/sales"]},
		  {"attributeName":"Action","attributeValueIncludedIn":["Read"]}],
		  "constraints":%s}],"members":{"microsoftEntraMembers":[%s]}}`, name, constraints, strings.Join(ms, ",")))}
	}
	if err := f.srv.Store.PutOneLakeRoles(lake.ID, []store.OneLakeRole{
		role("West", `{"rows":[{"tablePath":"/Tables/sales","value":"SELECT * FROM sales WHERE region = 'west'"}]}`, both, rowsOnly),
		role("NoPrice", `{"columns":[{"tablePath":"/Tables/sales","columnNames":["region","amount"],"columnEffect":"Permit","columnAction":["Read"]}]}`, both, colsOnly),
	}); err != nil {
		t.Fatal(err)
	}
	if code, body := f.switchMode(t, web, endpoint, entra.DaemonClientID, "UserIdentity"); code != http.StatusOK {
		t.Fatalf("switch = %d %s", code, body)
	}
	count := func(oid string) int {
		t.Helper()
		db, err := f.open(t, oid, lake.ID)
		if err != nil {
			t.Fatal(err)
		}
		return scalar(t, db, `SELECT COUNT(*) FROM (SELECT region, amount FROM dbo.sales) AS s`)
	}
	for oid, want := range map[string]int{both: 0, rowsOnly: 1, colsOnly: 2} {
		if got := count(oid); got != want {
			t.Errorf("%s reads %d row(s), want %d", oid[:8], got, want)
		}
	}
}
