package api

import (
	"encoding/json"
	"testing"
)

func fmtHint(v string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"format": json.RawMessage(v)}
}

func TestLookupFormat(t *testing.T) {
	cases := []struct {
		tp   map[string]json.RawMessage
		path string
		want string
	}{
		{nil, "x.json", "json"},
		{nil, "x.CSV", "csv"},
		{nil, "x.txt", "csv"}, // default
		{fmtHint(`"json"`), "x.dat", "json"},
		{fmtHint(`"csv"`), "x.json", "csv"},               // explicit hint wins over extension
		{fmtHint(`{"type":"DelimitedText"}`), "x", "csv"}, // object form
		{fmtHint(`{"type":"JsonFormat"}`), "x", "json"},
		{fmtHint(`"weird"`), "x.json", "json"}, // unrecognized hint → fall through to extension
	}
	for _, c := range cases {
		if got := lookupFormat(c.tp, c.path); got != c.want {
			t.Errorf("lookupFormat(%v,%q) = %q, want %q", c.tp, c.path, got, c.want)
		}
	}
}

func TestParseRows(t *testing.T) {
	// CSV with a short record (fewer cols than header) keeps present columns.
	rows, err := parseRows([]byte("a,b\n1\n2,3\n"), "csv")
	if err != nil || len(rows) != 2 {
		t.Fatalf("csv rows = %v (err %v)", rows, err)
	}
	if rows[0].(map[string]any)["a"] != "1" {
		t.Errorf("row0 = %+v", rows[0])
	}
	// Empty CSV → no rows.
	if rows, _ := parseRows(nil, "csv"); rows != nil {
		t.Errorf("empty csv = %v", rows)
	}
	// JSON single object → one row.
	rows, err = parseRows([]byte(`{"k":1}`), "json")
	if err != nil || len(rows) != 1 {
		t.Fatalf("json object = %v (err %v)", rows, err)
	}
	// Invalid inputs error.
	if _, err := parseRows([]byte(`{bad`), "json"); err == nil {
		t.Error("invalid json accepted")
	}
	if _, err := parseRows([]byte("a,\"b\n1,2"), "csv"); err == nil {
		t.Error("invalid csv accepted")
	}
}

func TestBaseNameAndItemType(t *testing.T) {
	if baseName("Files/a/b.txt") != "b.txt" || baseName("Files/d/") != "d" || baseName("x") != "x" {
		t.Error("baseName")
	}
	if itemType(true) != "Folder" || itemType(false) != "File" {
		t.Error("itemType")
	}
}
