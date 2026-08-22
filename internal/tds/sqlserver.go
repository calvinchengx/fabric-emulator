package tds

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
	// Registers the "np" (named pipe) protocol with msdsn, and only when
	// GOOS=windows — the init is a no-op everywhere else, so this costs nothing
	// on Linux or macOS.
	//
	// Without it msdsn.Parse does NOT reject a named-pipe DSN. It degrades to
	// the TCP parser, which splits on the first backslash and returns no error:
	//
	//	server=np:\\.\pipe\LOCALDB#659D5BB9\tsql\query
	//	  -> Host "np:", Instance `\.\pipe\LOCALDB#659D5BB9\tsql\query`, Protocols [tcp]
	//
	// The emulator then tries a SQL Browser lookup for an "instance" that is
	// really a pipe path, on a "host" that is really a protocol prefix, and
	// fails at connect time with an error that names neither. Silent
	// misparsing, not a parse failure — which is why this import is load-bearing
	// rather than cosmetic. See internal/testsupport/mssql.go, which needs the
	// same registration for the tests that open SQL Server directly.
	_ "github.com/microsoft/go-mssqldb/namedpipe"
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
	// CollationOf returns the collation an item's database must be created
	// with — the Warehouse `collationType` the caller declared, or "" for
	// Fabric's default. A function rather than a value because the answer is
	// per item and lives in the store, which this package must not import.
	// Nil means every database gets Fabric's default (see collation.go).
	CollationOf func(database string) string
}

// NewSQLServerBackend opens a pooled connection to a SQL Server DSN, e.g.
// "sqlserver://sa:pw@host:1433?database=warehouse". It does not dial until the
// first query, so the emulator starts even if the sidecar is still coming up.
func NewSQLServerBackend(dsn string) (*sqlServerBackend, error) {
	cfg, err := msdsn.Parse(dsn)
	if err != nil {
		return nil, err
	}
	if err := checkProtocolResolved(dsn, cfg); err != nil {
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
	// COLLATE is not optional here: without it the database inherits the
	// server's case-INSENSITIVE collation while Fabric's is case-sensitive, so
	// a casing mistake passes locally and fails on a tenant (collation.go).
	_, err := b.db.ExecContext(ctx,
		"IF DB_ID('"+database+"') IS NULL CREATE DATABASE ["+database+"] COLLATE "+b.collationFor(database))
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
func (b *sqlServerBackend) Dial(ctx context.Context, database, principal string, role Role) (net.Conn, []byte, error) {
	if b.base == nil {
		return nil, nil, fmt.Errorf("no backend DSN configured for splicing")
	}
	// AUTHENTICATE AS THE CALLER, so the engine's own RLS, CLS and masking have
	// somebody to restrict. Without this every client is the DSN account — one
	// identity for everyone, which a filter predicate cannot tell apart and a
	// column DENY has no grantee for (docs/55-tsql-security.md).
	//
	// Provision first: a login that does not exist yet cannot be logged in as,
	// and a reconnect is the normal case so this is idempotent. A FAILURE HERE
	// FAILS THE CONNECTION rather than falling back to the DSN account —
	// falling back would hand the caller a sysadmin session and look like it
	// worked.
	user, password := b.base.User, b.base.Password
	if principal != "" {
		if err := EnsurePrincipal(ctx, b.pool(""), b.pool(database), principal, role); err != nil {
			return nil, nil, fmt.Errorf("provisioning %s: %w", principal, err)
		}
		user, password = principal, principalPassword(principal)
	}
	conn, err := b.dialBackend(ctx)
	if err != nil {
		return nil, nil, err
	}
	serverName := b.base.Host
	if serverName == "" {
		serverName = "." // a pipe DSN carries no host; "." is the local server
	}
	loginResp, err := clientLogin(conn, user, password, database, serverName)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, loginResp, nil
}

// dialBackend opens a raw connection to the backend over whichever protocol the
// DSN resolved to, trying them in the order msdsn put them in.
//
// This used to be a bare `net.Dial("tcp", JoinHostPort(base.Host, port))`, which
// is correct for every TCP DSN and silently wrong for any other. A named-pipe
// DSN parses with an EMPTY Host — the pipe path goes into ProtocolParameters,
// not Host — so that formatting produced the address ":1433" and the splice
// failed with `dial tcp :1433`, an error naming a protocol the DSN never asked
// for. It cost a CI cycle to read because only the splice path was affected:
// ordinary queries go through go-mssqldb's own dialer, which has always honoured
// the protocol, so warehouse tests passed while the mirror and two-surface e2es
// failed on the same DSN.
//
// TCP stays hand-rolled rather than delegated because the splice genuinely needs
// a raw net.Conn: go-mssqldb's TCP dialer is an MssqlProtocolDialer and wants a
// *Connector to build a driver connection, which is the opposite of what this
// path wants. Non-TCP protocols implement the simpler ProtocolDialer interface
// and hand back exactly the raw conn required.
func (b *sqlServerBackend) dialBackend(ctx context.Context) (net.Conn, error) {
	var attempts []string
	for _, proto := range b.base.Protocols {
		var (
			conn net.Conn
			err  error
		)
		switch proto {
		case "tcp", "admin":
			port := b.base.Port
			if port == 0 {
				port = 1433
			}
			var d net.Dialer
			conn, err = d.DialContext(ctx, "tcp",
				net.JoinHostPort(b.base.Host, strconv.FormatUint(port, 10)))
		default:
			d, ok := msdsn.ProtocolDialers[proto]
			if !ok {
				attempts = append(attempts, proto+": no dialer registered")
				continue
			}
			conn, err = dialWithRetry(ctx, d, b.base)
		}
		if err == nil {
			return conn, nil
		}
		attempts = append(attempts, proto+": "+err.Error())
	}
	if len(attempts) == 0 {
		// Parse resolved no protocol at all. Saying so beats dialing ":1433".
		return nil, fmt.Errorf("backend DSN resolved to no usable protocol")
	}
	return nil, fmt.Errorf("backend dial failed (%s)", strings.Join(attempts, "; "))
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

// checkProtocolResolved refuses a DSN that named a protocol msdsn did not
// actually resolve.
//
// msdsn does not reject an unhandled protocol prefix. Its ADO parser strips
// "<proto>:" from the server value only for protocols in the REGISTERED parser
// list; anything else stays attached and falls through to the TCP parser, which
// splits on the first backslash and returns no error:
//
//	server=np:\\.\pipe\LOCALDB#1AAF806D\tsql\query
//	  -> Host "np:", Instance `\.\pipe\LOCALDB#1AAF806D\tsql\query`, Protocols [tcp]
//
// A wrong-but-valid config is worse than a rejected one. Nothing fails until
// the dial, and then it fails as `dial tcp :1433` or a SQL Browser lookup for
// an "instance" that is really a pipe path — errors that name a protocol nobody
// configured and point at the network rather than at the DSN. That cost two CI
// cycles to read, so the emulator now refuses at construction and says which
// import is missing.
//
// Checked against the DSN text rather than the config alone because the
// evidence is the mismatch between the two: the user asked for a protocol and
// the parse did not deliver it.
func checkProtocolResolved(dsn string, cfg msdsn.Config) error {
	prefix, ok := dsnProtocolPrefix(dsn)
	if !ok {
		return nil
	}
	for _, p := range cfg.Protocols {
		if p == prefix {
			return nil
		}
	}
	return fmt.Errorf("DSN asks for the %q protocol but it is not registered, so the "+
		"server name parsed as TCP (host %q, instance %q, protocols %v) and any "+
		"connection would fail somewhere unrelated. Blank-import the driver's "+
		"protocol package (for %q: github.com/microsoft/go-mssqldb/namedpipe) in a "+
		"package this binary links",
		prefix, cfg.Host, cfg.Instance, cfg.Protocols, prefix)
}

// dsnProtocolPrefix reports the "<proto>:" a keyword-form DSN put on its server
// value. Only the keyword form is inspected: the URL form has no such prefix,
// it carries the protocol as a query parameter that msdsn validates itself.
func dsnProtocolPrefix(dsn string) (string, bool) {
	if strings.HasPrefix(dsn, "sqlserver://") {
		return "", false
	}
	for _, kv := range strings.Split(dsn, ";") {
		k, v, found := strings.Cut(kv, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(k), "server") {
			continue
		}
		proto, rest, found := strings.Cut(strings.TrimSpace(v), ":")
		// A bare "host,port" has no colon; "tcp:host" and "np:\\..." do. Guard
		// against a stray colon inside a value by requiring a plausible scheme.
		if !found || proto == "" || strings.ContainsAny(proto, `\/,`) || rest == "" {
			return "", false
		}
		return strings.ToLower(proto), true
	}
	return "", false
}

// dialPipeAttempts / dialPipeBackoff bound the retry in dialWithRetry. Small on
// purpose: this is for a transient busy pipe, not for a server that is down.
const (
	dialPipeAttempts = 4
	dialPipeBackoff  = 75 * time.Millisecond
)

// dialWithRetry opens a non-TCP backend connection, retrying briefly.
//
// A Windows named-pipe client is EXPECTED to retry: the documented protocol is
// that CreateFile on a busy pipe fails and the caller waits for an instance to
// free up. Go's dialer surfaces those as "All pipe instances are busy" and
// "No process is on the other end of the pipe", and the second is what the
// Windows LocalDB leg reports for exactly these three splice tests.
//
// The splice is the only thing here that needs this. Ordinary queries go
// through database/sql, which POOLS — a handful of connections, reused for the
// life of the process. The splice opens a FRESH backend connection for every
// client session and never reuses one, so it is the only path that produces a
// stream of short-lived pipe connects, and the only one that fails. That
// asymmetry is why internal/warehouse, internal/api and internal/tds all pass
// against the same LocalDB in the same run while internal/server does not.
//
// Deliberately bounded and deliberately not protocol-specific: any protocol
// dialer may have a transient busy state, and a real failure still surfaces
// after four attempts and ~225ms rather than being retried away.
func dialWithRetry(ctx context.Context, d msdsn.ProtocolDialer, cfg *msdsn.Config) (net.Conn, error) {
	var err error
	for i := 0; i < dialPipeAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(dialPipeBackoff):
			}
		}
		var conn net.Conn
		conn, err = d.DialConnection(ctx, cfg)
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", dialPipeAttempts, err)
}
