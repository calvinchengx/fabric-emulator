package tds

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// dbKey carries the target database (a Fabric item id) through the query
// context, so the Backend interface stays database-agnostic (tests inject a
// fake) while the real backend routes each query to that item's own database.
type ctxKey int

const dbKey ctxKey = 0

// withDatabase returns ctx tagged with the target database.
func withDatabase(ctx context.Context, database string) context.Context {
	return context.WithValue(ctx, dbKey, database)
}

// sqlServerBackend runs queries against a real SQL Server (the warehouse
// sidecar) over go-mssqldb with a SQL login — the FedAuth-terminating proxy has
// already authenticated the client, so the backend leg uses the fixed service
// credential in the DSN.
//
// Each Fabric item (lakehouse or warehouse) maps to its **own SQL Server
// database** named by the item id, so items are isolated (a lakehouse and a
// warehouse — or two lakehouses — never collide). Per-database connection pools
// are opened lazily from one parsed base config; `db` (no item context) targets
// the DSN's default database and is what unit tests inject (SQLite).
type sqlServerBackend struct {
	db    *sql.DB       // default pool (no item context); tests set this directly
	base  *msdsn.Config // base config for per-database pools (nil in tests)
	mu    sync.Mutex
	pools map[string]*sql.DB
}

// NewSQLServerBackend opens a pooled connection to a SQL Server DSN, e.g.
// "sqlserver://sa:pw@host:1433?database=warehouse". It does not dial until the
// first query, so the emulator starts even if the sidecar is still coming up.
func NewSQLServerBackend(dsn string) (*sqlServerBackend, error) {
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		return nil, err
	}
	master := sql.OpenDB(mssql.NewConnectorConfig(cfg))
	return &sqlServerBackend{db: master, base: &cfg, pools: map[string]*sql.DB{}}, nil
}

// pool returns (opening + caching) the connection pool for a per-item database.
func (b *sqlServerBackend) pool(database string) *sql.DB {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.pools[database]; ok {
		return p
	}
	cfg := *b.base // copy; only Database differs
	cfg.Database = database
	p := sql.OpenDB(mssql.NewConnectorConfig(cfg))
	b.pools[database] = p
	return p
}

// DB returns the connection pool for a Fabric item's database (used by
// reflection to CREATE/INSERT into it). Falls back to the default pool if no
// base config (tests).
func (b *sqlServerBackend) DB(database string) *sql.DB {
	if b.base == nil || database == "" {
		return b.db
	}
	return b.pool(database)
}

// EnsureDatabase creates the item's SQL Server database if it doesn't exist —
// idempotent. CREATE DATABASE can't be parameterised, so the name is validated
// (Fabric item ids are GUIDs) and interpolated; safeDBName guarantees no quote
// or bracket can appear, so the string literal and bracket-quoted forms are safe.
func (b *sqlServerBackend) EnsureDatabase(ctx context.Context, database string) error {
	if b.base == nil || database == "" {
		return nil // test/default backend: single database, nothing to create
	}
	if !safeDBName(database) {
		return fmt.Errorf("unsafe database name %q", database)
	}
	_, err := b.db.ExecContext(ctx,
		"IF DB_ID('"+database+"') IS NULL CREATE DATABASE ["+database+"]")
	return err
}

// safeDBName allows only the characters a Fabric item id (GUID) uses, so the
// name can be interpolated into DDL without injection or quoting hazards.
func safeDBName(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// Query executes the batch in the context's target database and materialises
// the first result set. Values scan into `any`; []byte is normalised to string
// so resultTokens emits it as text.
func (b *sqlServerBackend) Query(ctx context.Context, query string) (*Result, error) {
	db := b.DB(dbFromCtx(ctx))
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return materialize(rows)
}

// Dial opens a raw TCP connection to the SQL Server backend and completes a
// SQL-auth TDS login into the item's database, returning the connection ready
// for post-login traffic. The server splices the client's session onto it, so
// SQL Server itself produces every response token (full fidelity). The service
// credential and address come from the base DSN.
func (b *sqlServerBackend) Dial(ctx context.Context, database string) (net.Conn, []byte, error) {
	if b.base == nil {
		return nil, nil, fmt.Errorf("no backend DSN configured for splicing")
	}
	port := b.base.Port
	if port == 0 {
		port = 1433
	}
	addr := net.JoinHostPort(b.base.Host, strconv.FormatUint(port, 10))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	loginResp, err := clientLogin(conn, b.base.User, b.base.Password, database, b.base.Host)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, loginResp, nil
}

// colTypeFromDB maps a driver's column type name to a wire ColType. Integer,
// float, and bit families get their real TDS type; everything else (varchar,
// datetime, decimal, …) falls back to NVARCHAR text. Covers SQL Server names
// and the SQLite spellings used in tests.
func colTypeFromDB(name string) ColType {
	switch strings.ToUpper(name) {
	case "INT", "BIGINT", "SMALLINT", "TINYINT", "INTEGER":
		return ColInt
	case "FLOAT", "REAL", "DOUBLE":
		return ColFloat
	case "BIT", "BOOLEAN":
		return ColBit
	}
	return ColNVarchar
}

// dbFromCtx reads the target database threaded through the context (empty when
// none — the default pool).
func dbFromCtx(ctx context.Context) string {
	s, _ := ctx.Value(dbKey).(string)
	return s
}

// materialize scans a result set into a Result. Extracted so the row/type
// handling can be unit-tested against SQLite without a SQL Server.
func materialize(rows *sql.Rows) (*Result, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, _ := rows.ColumnTypes()
	res := &Result{Columns: make([]Column, len(cols))}
	for i, c := range cols {
		ct := ColNVarchar
		if i < len(types) {
			ct = colTypeFromDB(types[i].DatabaseTypeName())
		}
		res.Columns[i] = Column{Name: c, Type: ct}
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
