package tds

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// fakeBackend returns a scripted result (and records the query it saw), so the
// SQLBatch-parse + result-encoding path can be exercised by the real driver
// without a SQL Server.
type fakeBackend struct {
	res     *Result
	err     error
	lastSQL string
}

func (f *fakeBackend) Query(_ context.Context, sql string) (*Result, error) {
	f.lastSQL = sql
	return f.res, f.err
}

// TestBackendRelayResultSet drives the real go-mssqldb client against a Server
// backed by a fake engine: a multi-column, multi-row result (with a NULL) round
// trips, the driver decodes every value, and the backend saw the query text.
func TestBackendRelayResultSet(t *testing.T) {
	be := &fakeBackend{res: &Result{
		Columns: []Column{{Name: "region"}, {Name: "total"}},
		Rows: [][]any{
			{"us", 80},
			{"eu", 60},
			{nil, 0}, // NULL region
		},
	}}
	srv := &Server{Auth: func(string) error { return nil }, Backend: be}
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

	rows, err := db.QueryContext(ctx, "SELECT r.name region, SUM(s.amount) total FROM sales s JOIN regions r ON ...")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	if len(cols) != 2 || cols[0] != "region" || cols[1] != "total" {
		t.Fatalf("columns = %v", cols)
	}

	type row struct {
		region sql.NullString
		total  int
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.region, &r.total); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	// Values decoded (text → the caller's scan types).
	if got[0].region.String != "us" || got[0].total != 80 {
		t.Errorf("row0 = %+v", got[0])
	}
	if got[1].region.String != "eu" || got[1].total != 60 {
		t.Errorf("row1 = %+v", got[1])
	}
	if got[2].region.Valid {
		t.Errorf("row2 region should be NULL, got %q", got[2].region.String)
	}
	// The backend received the real query text (ALL_HEADERS stripped).
	if want := "SELECT r.name region"; len(be.lastSQL) < len(want) || be.lastSQL[:len(want)] != want {
		t.Errorf("backend saw query %q", be.lastSQL)
	}
}

// TestBackendQueryError surfaces a backend error as a SQL error to the client.
func TestBackendQueryError(t *testing.T) {
	srv := &Server{Auth: func(string) error { return nil },
		Backend: &fakeBackend{err: fmt.Errorf("Invalid object name 'nope'")}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go srv.Serve(ln)
	addr := ln.Addr().(*net.TCPAddr)
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)

	c, _ := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	db := sql.OpenDB(c)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "select * from nope"); err == nil {
		t.Fatal("expected the backend error to surface to the client")
	}
}
