package semanticmodel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateOrReplaceTMSLRetail(t *testing.T) {
	m := loadModel(t)
	raw, err := os.ReadFile(filepath.Join(fixturesDir(), "seed_data.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := ParseData(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := CreateOrReplaceTMSL(m, data)
	if err != nil {
		t.Fatal(err)
	}
	var cmd map[string]any
	if err := json.Unmarshal(out, &cmd); err != nil {
		t.Fatal(err)
	}
	cor := cmd["createOrReplace"].(map[string]any)
	if cor["object"].(map[string]any)["database"] != "RetailAnalysis" {
		t.Fatalf("object.database = %v", cor["object"])
	}
	db := cor["database"].(map[string]any)
	if db["name"] != "RetailAnalysis" {
		t.Fatalf("database.name = %v", db["name"])
	}
	tmsl, _ := json.Marshal(db)
	s := string(tmsl)
	if !strings.Contains(s, "DATATABLE") {
		t.Fatal("expected DATATABLE partitions")
	}
	if !strings.Contains(s, `"Store A"`) && !strings.Contains(s, `Store A`) {
		t.Fatalf("expected seed row in DATATABLE: %s", s)
	}
	if !strings.Contains(s, "SUM(Sales[Units])") {
		t.Fatal("expected measure expression to survive")
	}
	if !strings.Contains(s, `"type":"calculated"`) && !strings.Contains(s, `"type": "calculated"`) {
		t.Fatal("expected calculated partitions")
	}
	if !strings.Contains(s, `"sourceColumn":"StoreId"`) && !strings.Contains(s, `"sourceColumn": "StoreId"`) {
		t.Fatal("expected sourceColumn on DATATABLE columns (Desktop rejects the omit)")
	}
}

func TestCreateOrReplaceTMSLRefusesDirectLake(t *testing.T) {
	m := &Model{
		Name: "Lake",
		Tables: []Table{{
			Name:       "Sales",
			Columns:    []Column{{Name: "Amount", DataType: "int64"}},
			DirectLake: &DirectLakePartition{EntityName: "sales", ExpressionSource: "DatabaseQuery"},
		}},
	}
	if _, err := CreateOrReplaceTMSL(m, nil); err == nil || !strings.Contains(err.Error(), "Direct Lake") {
		t.Fatalf("err = %v; want Direct Lake refusal", err)
	}
}

func TestCreateOrReplaceTMSLEscapesQuotes(t *testing.T) {
	m := &Model{
		Name: "Q",
		Tables: []Table{{
			Name:    "T",
			Columns: []Column{{Name: "Note", DataType: "string"}},
		}},
	}
	data := Data{"T": {{"Note": `say "hi"`}}}
	out, err := CreateOrReplaceTMSL(m, data)
	if err != nil {
		t.Fatal(err)
	}
	var cmd map[string]any
	if err := json.Unmarshal(out, &cmd); err != nil {
		t.Fatal(err)
	}
	expr := deployExpr(t, cmd)
	if !strings.Contains(expr, `"say ""hi"""`) {
		t.Fatalf("quote escape missing: %q", expr)
	}
}

func deployExpr(t *testing.T, cmd map[string]any) string {
	t.Helper()
	db := cmd["createOrReplace"].(map[string]any)["database"].(map[string]any)
	model := db["model"].(map[string]any)
	table := model["tables"].([]any)[0].(map[string]any)
	part := table["partitions"].([]any)[0].(map[string]any)
	src := part["source"].(map[string]any)
	expr, _ := src["expression"].(string)
	return expr
}

func TestCreateOrReplaceTMSLBlankAndInt(t *testing.T) {
	m := &Model{
		Name: "N",
		Tables: []Table{{
			Name: "T",
			Columns: []Column{
				{Name: "Id", DataType: "int64"},
				{Name: "Label", DataType: "string"},
			},
		}},
	}
	data := Data{"T": {{"Id": float64(3), "Label": nil}}}
	out, err := CreateOrReplaceTMSL(m, data)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "BLANK()") {
		t.Fatalf("expected BLANK(): %s", s)
	}
	if !strings.Contains(s, "{3, BLANK()}") && !strings.Contains(s, "{3,BLANK()}") {
		t.Fatalf("expected integer literal 3: %s", s)
	}
}

func TestCreateOrReplaceTMSLRequiresName(t *testing.T) {
	m := &Model{Tables: []Table{{Name: "T", Columns: []Column{{Name: "A", DataType: "string"}}}}}
	if _, err := CreateOrReplaceTMSL(m, nil); err == nil {
		t.Fatal("nameless model should fail")
	}
}
