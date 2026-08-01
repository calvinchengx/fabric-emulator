package tds

import (
	"runtime"
	"testing"

	"github.com/microsoft/go-mssqldb/msdsn"
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
