package tds

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
)

// blockingBackend holds its first query until release is closed, so a test can
// cancel the client while the server is still working on it.
type blockingBackend struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBackend) Query(_ context.Context, query string) (*Result, error) {
	if strings.Contains(query, "slow") {
		b.once.Do(func() { close(b.started) })
		<-b.release
	}
	return &Result{Columns: []Column{{Name: "n", Type: ColInt}}, Rows: [][]any{{int64(1)}}}, nil
}

// attnDialer reports when the client writes an ATTENTION packet, which is how
// the test knows the cancel reached the wire before it lets the query finish.
type attnDialer struct{ sent chan struct{} }

func (d attnDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &attnConn{Conn: c, sent: d.sent}, nil
}

type attnConn struct {
	net.Conn
	sent chan struct{}
	once sync.Once
}

func (c *attnConn) Write(p []byte) (int, error) {
	if len(p) >= headerLen && p[0] == PktAttention {
		c.once.Do(func() { close(c.sent) })
	}
	return c.Conn.Write(p)
}

// A cancelled query is acknowledged. The client sends an ATTENTION and then
// waits for a DONE carrying the ATTN bit; go-mssqldb gives up after five
// seconds with "did not get cancellation confirmation from the server" and
// throws the connection away. The re-encode relay used to drop every non-batch
// packet, ATTENTION included, so any cancel ended that way. It is also what
// QueryRow does on a loaded machine: Rows.Close cancels before the reader has
// queued the final DONE, which is how two unrelated tests flaked at 5.00s.
func TestAttentionIsAcknowledged(t *testing.T) {
	be := &blockingBackend{started: make(chan struct{}), release: make(chan struct{})}
	srv := &Server{Auth: func(string) error { return nil }, Backend: be,
		OnConnect: func(_ context.Context, _, database, _ string) (Connection, error) {
			return Connection{TargetDB: database}, nil
		}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = srv.Serve(ln) }()

	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;database=db;encrypt=disable;dial timeout=5", ln.Addr().(*net.TCPAddr).Port)
	c, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return "a.b.c", nil })
	if err != nil {
		t.Fatal(err)
	}
	attn := make(chan struct{})
	c.(*mssql.Connector).Dialer = attnDialer{sent: attn}
	db := sql.OpenDB(c)
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() {
		var v int
		res <- conn.QueryRowContext(ctx, "select 'slow'").Scan(&v)
	}()
	guard := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
	guard(be.started, "the backend to receive the query")
	cancel()
	guard(attn, "the client to send an ATTENTION")
	close(be.release)

	select {
	case err = <-res:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled query never returned")
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query: err = %v, want context.Canceled", err)
	}

	// The session survives the cancel, as it does against SQL Server.
	var v int
	if err := conn.QueryRowContext(context.Background(), "SELECT 1").Scan(&v); err != nil || v != 1 {
		t.Fatalf("query after cancel: v=%d err=%v", v, err)
	}
}
