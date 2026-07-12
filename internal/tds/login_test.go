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

// TestLogin7FedAuthCapture drives the real go-mssqldb client through the
// PRELOGIN → LOGIN7 handshake against our listener using the SecurityToken
// FedAuth library, and asserts we extract the exact Entra access token the
// client presented. This validates the LOGIN7 + FeatureExt parsing against a
// real driver's bytes (not a hand-rolled fixture).
func TestLogin7FedAuthCapture(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		// PRELOGIN in, PRELOGIN response out (as packReply, advertising
		// FEDAUTHREQUIRED so the client sends its token).
		typ, _, err := ReadMessage(conn)
		if err != nil || typ != PktPreLogin {
			errCh <- fmt.Errorf("prelogin: err=%v type=%#x", err, typ)
			return
		}
		if err := WriteMessage(conn, PktTabular, ServerPreLogin(true)); err != nil {
			errCh <- err
			return
		}
		// LOGIN7 in.
		typ, data, err := ReadMessage(conn)
		if err != nil || typ != PktLogin7 {
			errCh <- fmt.Errorf("login7: err=%v type=%#x", err, typ)
			return
		}
		l, err := ParseLogin7(data)
		if err != nil {
			errCh <- err
			return
		}
		tokenCh <- l.FedAuthToken
	}()

	addr := ln.Addr().(*net.TCPAddr)
	// A JWT-shaped token with non-ASCII to prove UTF-16 decoding.
	token := "eyJhbGci.eyJhdWQ.sig-" + strings.Repeat("x", 40) + "-ünïcode"
	dsn := fmt.Sprintf("server=127.0.0.1;port=%d;encrypt=disable;dial timeout=5", addr.Port)
	connector, err := mssql.NewAccessTokenConnector(dsn, func() (string, error) { return token, nil })
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = db.PingContext(ctx) // errors after we close post-LOGIN7; we only need the token

	select {
	case got := <-tokenCh:
		if got != token {
			t.Fatalf("captured token mismatch:\n got  %q\n want %q", got, token)
		}
	case err := <-errCh:
		t.Fatalf("server handshake: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for LOGIN7")
	}
}
