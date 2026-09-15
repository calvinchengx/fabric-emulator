package tds

import (
	"context"
	"strings"
	"testing"
)

// DBAs logs in as a caller only after provisioning them, and refuses — never
// falls back to the service account — when it cannot.
func TestDBAsRefusesWhatItCannotLogInAs(t *testing.T) {
	ctx := context.Background()
	if _, err := (&sqlServerBackend{}).DBAs(ctx, "db", "u", nil); err == nil || !strings.Contains(err.Error(), "no backend DSN") {
		t.Errorf("no DSN: %v", err)
	}
	// Nothing listens on port 1: provisioning cannot reach an engine.
	be, err := NewSQLServerBackend("sqlserver://sa:pw@127.0.0.1:1?database=master&dial+timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.DBAs(ctx, "db", "", nil); err == nil || !strings.Contains(err.Error(), "no principal") {
		t.Errorf("no principal: %v", err)
	}
	if _, err := be.DBAs(ctx, "db", "u", []Grant{{Database: "db", Role: RoleNone}}); err == nil ||
		!strings.Contains(err.Error(), "no access to db") {
		t.Errorf("a revoked target: %v", err)
	}
	if _, err := be.DBAs(ctx, "db", "u", []Grant{{Database: "db", Role: RoleReader}}); err == nil ||
		!strings.Contains(err.Error(), "provisioning u") {
		t.Errorf("an unreachable engine: %v", err)
	}
}
