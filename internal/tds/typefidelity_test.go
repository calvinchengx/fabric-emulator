package tds

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

type typedBackend struct{ res *Result }

func (b *typedBackend) Query(context.Context, string) (*Result, error) { return b.res, nil }

// TestResultTypeFidelity drives typed columns (int/float/bit + text, with NULLs)
// through the real go-mssqldb driver and asserts the client reads the natural
// types — not text — and that the reported column types are the real SQL types.
func TestResultTypeFidelity(t *testing.T) {
	res := &Result{
		Columns: []Column{
			{Name: "id", Type: ColInt},
			{Name: "ratio", Type: ColFloat},
			{Name: "active", Type: ColBit},
			{Name: "note", Type: ColNVarchar},
		},
		Rows: [][]any{
			{int64(7), 1.5, true, "hi"},
			{int64(0), 0.0, false, nil}, // NULL note
			{nil, nil, nil, "last"},     // NULL numerics
		},
	}
	srv := &Server{Auth: func(string) error { return nil }, Backend: &typedBackend{res: res}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)
	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "SELECT id, ratio, active, note FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	// The driver reports the real column types — the numeric/bool columns are no
	// longer NVARCHAR, while the text column still is.
	cts, _ := rows.ColumnTypes()
	for i, want := range []bool{false, false, false, true} { // want NVARCHAR?
		isText := strings.Contains(strings.ToUpper(cts[i].DatabaseTypeName()), "CHAR")
		if isText != want {
			t.Errorf("col %d (%s) type = %s, isText=%v want %v", i, cts[i].Name(), cts[i].DatabaseTypeName(), isText, want)
		}
	}

	// Row 0 — typed scans (would fail into bool/float from text without fidelity).
	if !rows.Next() {
		t.Fatal("no row 0")
	}
	var id int64
	var ratio float64
	var active bool
	var note sql.NullString
	if err := rows.Scan(&id, &ratio, &active, &note); err != nil {
		t.Fatalf("row0 scan: %v", err)
	}
	if id != 7 || ratio != 1.5 || !active || note.String != "hi" {
		t.Errorf("row0 = id:%d ratio:%v active:%v note:%q", id, ratio, active, note.String)
	}

	// Row 1 — false/zero + a NULL text value.
	if !rows.Next() {
		t.Fatal("no row 1")
	}
	if err := rows.Scan(&id, &ratio, &active, &note); err != nil {
		t.Fatalf("row1 scan: %v", err)
	}
	if id != 0 || active || note.Valid {
		t.Errorf("row1 = id:%d active:%v noteValid:%v", id, active, note.Valid)
	}

	// Row 2 — NULL numerics scan into the Null* types.
	if !rows.Next() {
		t.Fatal("no row 2")
	}
	var nid sql.NullInt64
	var nr sql.NullFloat64
	var na sql.NullBool
	var last string
	if err := rows.Scan(&nid, &nr, &na, &last); err != nil {
		t.Fatalf("row2 scan: %v", err)
	}
	if nid.Valid || nr.Valid || na.Valid || last != "last" {
		t.Errorf("row2 = %+v %+v %+v note:%q", nid, nr, na, last)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// TestColTypeAndCoercions covers colTypeFromDB and the value coercions across
// every input branch (pure logic, no engine).
func TestColTypeAndCoercions(t *testing.T) {
	for _, n := range []string{"INT", "BIGINT", "smallint", "TinyInt", "INTEGER"} {
		if colTypeFromDB(n) != ColInt {
			t.Errorf("colTypeFromDB(%q) != ColInt", n)
		}
	}
	for _, n := range []string{"FLOAT", "real", "DOUBLE"} {
		if colTypeFromDB(n) != ColFloat {
			t.Errorf("colTypeFromDB(%q) != ColFloat", n)
		}
	}
	for _, n := range []string{"BIT", "boolean"} {
		if colTypeFromDB(n) != ColBit {
			t.Errorf("colTypeFromDB(%q) != ColBit", n)
		}
	}
	for _, n := range []string{"NVARCHAR", "DATETIME", "DECIMAL", ""} {
		if colTypeFromDB(n) != ColNVarchar {
			t.Errorf("colTypeFromDB(%q) != ColNVarchar", n)
		}
	}
	if toInt64(int64(5)) != 5 || toInt64(3) != 3 || toInt64(int32(7)) != 7 || toInt64(2.9) != 2 || toInt64(true) != 1 || toInt64("x") != 0 {
		t.Error("toInt64 branch")
	}
	if toFloat64(1.5) != 1.5 || toFloat64(float32(2)) != 2 || toFloat64(int64(4)) != 4 || toFloat64(3) != 3 || toFloat64("x") != 0 {
		t.Error("toFloat64 branch")
	}
	if !toBool(true) || toBool(false) || !toBool(int64(1)) || toBool(int64(0)) || !toBool(3) || toBool("x") {
		t.Error("toBool branch")
	}
}
