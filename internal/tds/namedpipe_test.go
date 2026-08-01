package tds

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/microsoft/go-mssqldb/msdsn"

	"github.com/calvinchengx/fabric-emulator/internal/testsupport"
)

// localDBPipeDSN is the shape CI actually sets on the Windows leg, taken
// verbatim from `SqlLocalDB info MSSQLLocalDB`. The '#' and the backslashes are
// both load-bearing: see TestWithDatabase in internal/testsupport for why the
// keyword form is used rather than the URL form.
const localDBPipeDSN = `server=np:\\.\pipe\LOCALDB#659D5BB9\tsql\query`

// The named-pipe protocol must be registered wherever the emulator parses a
// DSN, because msdsn does NOT reject a pipe DSN when it isn't — it silently
// mis-parses it as TCP and fails much later at connect time.
//
// This is a regression test for a real CI failure: the blank import lived only
// in internal/testsupport, so internal/warehouse (which uses it) passed on
// Windows LocalDB while internal/api and internal/server — which reach SQL
// Server through this package — failed with errors that named neither the pipe
// nor the missing protocol:
//
//	no instance matching '\.\pipe\LOCALDB#659D5BB9\tsql\query' from host 'np:'
//	backend connect failed: dial tcp :1433
//
// Dropping the `_ "github.com/microsoft/go-mssqldb/namedpipe"` import from
// sqlserver.go turns this test red on Windows.
func TestNamedPipeProtocolIsRegistered(t *testing.T) {
	if runtime.GOOS != "windows" {
		// The namedpipe package's init is GOOS-gated, so off Windows there is
		// no registration to assert. Pin the hazard instead — this is exactly
		// the misparse that made the CI failure so hard to read.
		cfg, err := msdsn.Parse(localDBPipeDSN)
		if err != nil {
			t.Skipf("msdsn now rejects a pipe DSN outright (err=%v); the silent-misparse hazard is gone", err)
		}
		if cfg.Host != "np:" {
			t.Skipf("msdsn no longer misparses a pipe DSN as TCP (Host=%q); this test's premise has changed", cfg.Host)
		}
		t.Logf("off Windows: pipe DSN silently parses as TCP (Host=%q, Protocols=%v, err=nil) — "+
			"which is why the registration on Windows is load-bearing", cfg.Host, cfg.Protocols)
		return
	}

	var found bool
	for _, p := range msdsn.ProtocolParsers {
		if p.Protocol() == "np" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal(`the "np" protocol is not registered: internal/tds must blank-import ` +
			`github.com/microsoft/go-mssqldb/namedpipe, or every named-pipe DSN ` +
			`silently degrades to a TCP parse`)
	}

	cfg, err := msdsn.Parse(localDBPipeDSN)
	if err != nil {
		t.Fatalf("parsing the LocalDB pipe DSN: %v", err)
	}
	// The give-away of the bug is Host being the protocol prefix.
	if cfg.Host == "np:" {
		t.Errorf("pipe DSN still parsed by the TCP parser: Host=%q, Instance=%q", cfg.Host, cfg.Instance)
	}
	var viaPipe bool
	for _, proto := range cfg.Protocols {
		if proto == "np" {
			viaPipe = true
		}
	}
	if !viaPipe {
		t.Errorf("Protocols = %v, want it to include \"np\"", cfg.Protocols)
	}
}

// Parsing a DSN is not connecting with it, and the difference cost a CI cycle.
//
// TestNamedPipeProtocolIsRegistered above asserts only that msdsn.Parse
// produces a pipe config. That passed on the Windows leg while internal/server
// and internal/api failed to connect at all with `dial tcp :1433` — a green
// parse test next to a red dial is worse than no test, because it says the area
// is covered.
//
// So this exercises the PRODUCTION constructor, NewSQLServerBackend, against
// whatever DSN CI actually set, and makes it dial. It is the same code path the
// emulator uses at startup, so a failure here names the emulator's own backend
// rather than a test helper's.
//
// Note the shape of the bug it guards: with protocol=np resolved, msdsn sets
// Protocols to exactly ["np"] and appends no TCP fallback. A `dial tcp` error
// therefore cannot come from a correctly parsed pipe DSN — it proves the DSN
// was parsed as TCP. That makes the assertion below diagnostic rather than
// merely red.
func TestSQLServerBackendConnectsWithTheCIDSN(t *testing.T) {
	// Skips unless WAREHOUSE_MSSQL_DSN is set, exactly like every other gated
	// warehouse test — the point is to run on the legs that have a server.
	db := testsupport.OpenMSSQL(t)
	_ = db // OpenMSSQL proves the DSN is reachable; below proves OUR constructor is.

	be, err := NewSQLServerBackend(testsupport.DSN(t))
	if err != nil {
		t.Fatalf("NewSQLServerBackend rejected the CI DSN: %v", err)
	}
	if err := be.db.PingContext(context.Background()); err != nil {
		t.Fatalf("the emulator's own backend cannot reach the CI DSN: %v\n"+
			"a `dial tcp` error here means the DSN was parsed as TCP — on Windows "+
			"that is the named-pipe protocol not being registered in THIS binary", err)
	}
}

// The splice path must dial the protocol the DSN resolved to, not always TCP.
//
// This is the second bug the LocalDB leg exposed, and it is independent of the
// registration one. Dial used to be a bare
//
//	net.Dial("tcp", net.JoinHostPort(base.Host, port))
//
// which is right for every TCP DSN and silently wrong for any other. A
// named-pipe config carries an EMPTY Host — the pipe path lives in
// ProtocolParameters — so that formatting produced ":1433" and the emulator
// reported `dial tcp :1433`, naming a protocol the DSN never asked for.
//
// Runs everywhere: on non-Windows the np dialer is deliberately absent, so the
// assertion is that dialBackend says SO rather than quietly falling back to a
// TCP dial of nothing. That is the property under test — never dial TCP for a
// non-TCP DSN — and it does not need a pipe to verify.
func TestDialBackendDoesNotFallBackToTCP(t *testing.T) {
	npOnly := &sqlServerBackend{base: &msdsn.Config{
		Protocols: []string{"np"},
		Host:      "", // exactly what a parsed pipe DSN leaves behind
	}}
	_, err := npOnly.dialBackend(context.Background())
	if err == nil {
		t.Fatal("dialBackend succeeded with no reachable pipe; expected an error")
	}
	if strings.Contains(err.Error(), "dial tcp :1433") {
		t.Errorf("fell back to a TCP dial of the empty host: %v", err)
	}
	if !strings.Contains(err.Error(), "np") {
		t.Errorf("error does not name the protocol it tried: %v", err)
	}

	// No protocol at all must say so, not dial a default.
	none := &sqlServerBackend{base: &msdsn.Config{}}
	_, err = none.dialBackend(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no usable protocol") {
		t.Errorf("empty Protocols gave %v, want a 'no usable protocol' error", err)
	}
}

// A DSN naming a protocol that did not resolve must be refused at construction.
// The whole hazard is that msdsn returns no error for this, so nothing fails
// until a dial somewhere unrelated.
func TestNewSQLServerBackendRefusesAnUnresolvedProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("np resolves on Windows; the unresolved case cannot be built here")
	}
	_, err := NewSQLServerBackend(localDBPipeDSN)
	if err == nil {
		t.Fatal("accepted a pipe DSN whose protocol is not registered")
	}
	for _, want := range []string{"np", "namedpipe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q so the fix is obvious: %v", want, err)
		}
	}

	// A plain TCP DSN must still be accepted — the guard must not be a tax on
	// the normal case.
	if _, err := NewSQLServerBackend("server=localhost,1433;user id=sa;password=p"); err != nil {
		t.Errorf("rejected an ordinary TCP DSN: %v", err)
	}
}
