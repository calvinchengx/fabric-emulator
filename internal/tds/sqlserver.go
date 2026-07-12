package tds

import (
	"context"
	"database/sql"

	_ "github.com/microsoft/go-mssqldb" // registers the "sqlserver" driver
)

// sqlServerBackend runs queries against a real SQL Server (the warehouse
// sidecar) over go-mssqldb with a SQL login — the FedAuth-terminating proxy has
// already authenticated the client, so the backend leg uses the fixed service
// credential in the DSN.
type sqlServerBackend struct {
	db *sql.DB
}

// NewSQLServerBackend opens a pooled connection to a SQL Server DSN, e.g.
// "sqlserver://sa:pw@host:1433?database=warehouse". It does not dial until the
// first query, so the emulator starts even if the sidecar is still coming up.
func NewSQLServerBackend(dsn string) (Backend, error) {
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, err
	}
	return &sqlServerBackend{db: db}, nil
}

// Query executes the batch and materialises the first result set. Values scan
// into `any`; []byte is normalised to string so resultTokens emits it as text.
func (b *sqlServerBackend) Query(ctx context.Context, query string) (*Result, error) {
	rows, err := b.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	res := &Result{Columns: make([]Column, len(cols))}
	for i, c := range cols {
		res.Columns[i] = Column{Name: c}
	}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if bs, ok := v.([]byte); ok {
				vals[i] = string(bs)
			}
		}
		res.Rows = append(res.Rows, vals)
	}
	return res, rows.Err()
}
