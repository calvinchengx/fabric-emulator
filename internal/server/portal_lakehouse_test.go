package server_test

import (
	"net/http"
	"strings"
	"testing"

	entra "github.com/calvinchengx/entra-emulator/emulator"
	"github.com/calvinchengx/fabric-emulator/internal/server"
)

// The browse half of the lakehouse view. docs/44 calls this the largest genuine
// UI gap: the portal could already PREVIEW a table (/portal/table, reached from
// the flow graph) and nothing let you find one.
//
// Everything asserted here is stored state rendered back, which is what keeps
// the view on the right side of that document's thin/thick line.

// olWrite creates a OneLake file through the DFS surface — the same three-step
// create/append/flush a real client makes, so the rows under test arrive the way
// they actually arrive.
func (f *fixture) olWrite(t *testing.T, storage, wsID, itemID, rel string, body []byte) {
	t.Helper()
	base := "/" + wsID + "/" + itemID + "/" + rel
	olStatus(t, f.ol(t, "PUT", base+"?resource=file", storage, nil), http.StatusCreated, "create "+rel)
	if len(body) > 0 {
		olStatus(t, f.ol(t, "PATCH", base+"?action=append&position=0", storage, body),
			http.StatusAccepted, "append "+rel)
		olStatus(t, f.ol(t, "PATCH", base+"?action=flush&position="+itoa(len(body)), storage, nil),
			http.StatusOK, "flush "+rel)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

type lakehouseList struct {
	Lakehouses []struct {
		ItemID    string   `json:"itemId"`
		Name      string   `json:"name"`
		Workspace string   `json:"workspace"`
		Schemas   bool     `json:"schemaEnabled"`
		Tables    []string `json:"tables"`
		Files     []string `json:"files"`
		FileCount int      `json:"fileCount"`
	} `json:"lakehouses"`
}

func TestPortalLakehousesListsTablesAndFiles(t *testing.T) {
	f := newFixture(t)
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": "lh-ws"}, &ws), 201, "create workspace")
	lake := f.createItemNow(t, ws.ID, "Lakehouse", "lake")

	// A table as delta-rs writes one: a data file and a log entry, and NO
	// directory rows. Deriving table names from paths rather than listing
	// directories is what makes this appear at all.
	f.olWrite(t, storage, ws.ID, lake, "Tables/orders/part-0.parquet", []byte("PAR1"))
	f.olWrite(t, storage, ws.ID, lake, "Tables/orders/_delta_log/00000000000000000000.json",
		[]byte(`{"commitInfo":{}}`))
	f.olWrite(t, storage, ws.ID, lake, "Files/landing/day1.csv", []byte("id\n1\n"))

	var got lakehouseList
	if code := f.portalJSON(t, "/_emulator/portal/lakehouses", &got); code != 200 {
		t.Fatalf("portal/lakehouses = %d", code)
	}
	if len(got.Lakehouses) != 1 {
		t.Fatalf("want 1 lakehouse, got %d", len(got.Lakehouses))
	}
	lh := got.Lakehouses[0]
	if lh.Name != "lake" || lh.Workspace != "lh-ws" || lh.ItemID != lake {
		t.Fatalf("identity wrong: %+v", lh)
	}
	// The table is named ONCE despite two paths contributing to it.
	if strings.Join(lh.Tables, ",") != "orders" {
		t.Fatalf("tables = %v, want [orders]", lh.Tables)
	}
	if strings.Join(lh.Files, ",") != "landing/day1.csv" {
		t.Fatalf("files = %v", lh.Files)
	}
	if lh.Schemas {
		t.Fatal("a plain lakehouse must not report schemaEnabled")
	}
}

func TestPortalLakehousesDetectsSchemaEnabledTables(t *testing.T) {
	// `Tables/<schema>/<name>/...` is the schema-enabled shape. Reporting it as
	// a table called `<schema>` would be a plausible-looking wrong answer — the
	// preview would then fail to open a table that exists.
	f := newFixture(t)
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": "sch-ws"}, &ws), 201, "create workspace")
	lake := f.createItemNow(t, ws.ID, "Lakehouse", "lake")

	f.olWrite(t, storage, ws.ID, lake, "Tables/silver/orders/part-0.parquet", []byte("PAR1"))
	f.olWrite(t, storage, ws.ID, lake, "Tables/silver/orders/_delta_log/00000000000000000000.json",
		[]byte(`{"commitInfo":{}}`))

	var got lakehouseList
	f.portalJSON(t, "/_emulator/portal/lakehouses", &got)
	lh := got.Lakehouses[0]
	if strings.Join(lh.Tables, ",") != "silver/orders" {
		t.Fatalf("tables = %v, want [silver/orders]", lh.Tables)
	}
	if !lh.Schemas {
		t.Fatal("a two-segment table means the lakehouse is schema-enabled")
	}
}

func TestPortalLakehousesReportsTheFullFileCountWhenTruncating(t *testing.T) {
	// A landing zone can hold thousands of files. The list is bounded for the
	// view's sake, but a truncated list that also reported a truncated COUNT
	// would read as an empty-ish lakehouse — so the count is always the truth.
	f := newFixture(t)
	storage := f.forgeToken(t, map[string]any{
		"clientId": entra.DaemonClientID, "audience": "https://storage.azure.com",
	})

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": "many-ws"}, &ws), 201, "create workspace")
	lake := f.createItemNow(t, ws.ID, "Lakehouse", "lake")

	const n = server.PortalLakehouseFileLimit + 5
	for i := 0; i < n; i++ {
		f.olWrite(t, storage, ws.ID, lake, "Files/bulk/f"+itoa(i)+".txt", []byte("x"))
	}

	var got lakehouseList
	f.portalJSON(t, "/_emulator/portal/lakehouses", &got)
	lh := got.Lakehouses[0]
	if len(lh.Files) != server.PortalLakehouseFileLimit {
		t.Fatalf("listed %d files, want the %d cap", len(lh.Files), server.PortalLakehouseFileLimit)
	}
	if lh.FileCount != n {
		t.Fatalf("fileCount = %d, want the true %d", lh.FileCount, n)
	}
}

func TestPortalLakehousesIgnoresNonLakehouseItems(t *testing.T) {
	// A warehouse or notebook in the same workspace must not appear: this view
	// browses lakehouses, and listing anything with OneLake paths would make it
	// something else.
	f := newFixture(t)

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": "mixed-ws"}, &ws), 201, "create workspace")
	f.createItemNow(t, ws.ID, "Warehouse", "wh")
	f.createItemNow(t, ws.ID, "Notebook", "nb")

	var got lakehouseList
	f.portalJSON(t, "/_emulator/portal/lakehouses", &got)
	if len(got.Lakehouses) != 0 {
		t.Fatalf("non-lakehouse items leaked into the browse view: %+v", got.Lakehouses)
	}
}

func TestPortalLakehousesEmptyLakehouseIsListedWithNoTables(t *testing.T) {
	// A lakehouse with nothing in it is a real state — freshly created, or the
	// target of a deployment that copies metadata only. It must appear, with
	// empty ARRAYS rather than nulls, so the view renders "no tables yet"
	// instead of failing to iterate.
	f := newFixture(t)

	var ws struct {
		ID string `json:"id"`
	}
	f.mustStatus(f.call("POST", "/v1/workspaces", f.token,
		map[string]any{"displayName": "empty-ws"}, &ws), 201, "create workspace")
	f.createItemNow(t, ws.ID, "Lakehouse", "fresh")

	var got lakehouseList
	f.portalJSON(t, "/_emulator/portal/lakehouses", &got)
	if len(got.Lakehouses) != 1 {
		t.Fatalf("an empty lakehouse must still be listed: %+v", got.Lakehouses)
	}
	lh := got.Lakehouses[0]
	if lh.Tables == nil || lh.Files == nil {
		t.Fatalf("empty arrays, not nulls: tables=%v files=%v", lh.Tables, lh.Files)
	}
	if len(lh.Tables) != 0 || lh.FileCount != 0 {
		t.Fatalf("unexpected content: %+v", lh)
	}
}
