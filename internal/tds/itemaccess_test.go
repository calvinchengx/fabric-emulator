package tds

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/microsoft/go-mssqldb/msdsn"
)

// The two rungs item permissions added, unit-tested without an engine. What SQL
// Server then does with them is witnessed through the relay in
// internal/server/tds_itemaccess_test.go.

func TestConnectAndNoneCarryNoRole(t *testing.T) {
	for _, r := range []Role{RoleConnect, RoleNone} {
		if rights := principalRights(r); len(rights) != 0 {
			t.Errorf("rung %d rights = %v, want none", r, rights)
		}
	}
}

// The existing rungs keep their values: the new ones were appended, so nothing
// that stored or compared a rung before this sees a different number.
func TestTheExistingRungsDidNotMove(t *testing.T) {
	if RoleReader != 0 || RoleWriter != 1 || RoleOwner != 2 {
		t.Fatalf("rungs moved: reader=%d writer=%d owner=%d", RoleReader, RoleWriter, RoleOwner)
	}
}

// closedDB is a *sql.DB every statement fails on, with no server to reach.
func closedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlserver", "server=127.0.0.1;port=1")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	return db
}

// A revoke that fails is reported. A swallowed failure would leave the access it
// was meant to remove in place and report that it had gone.
func TestARevokeThatFailsIsReported(t *testing.T) {
	err := EnsurePrincipal(t.Context(), nil, closedDB(t), "11111111-0000-0000-0000-000000000001", RoleNone)
	if err == nil || !strings.Contains(err.Error(), "revoke access") {
		t.Fatalf("err = %v, want the revoke's failure", err)
	}
}

// RoleNone never creates a login: the master connection is not touched, which is
// why a nil one is safe here and would panic on the provisioning path.
func TestRevokingDoesNotCreateALogin(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RoleNone reached for the master connection: %v", r)
		}
	}()
	_ = EnsurePrincipal(t.Context(), nil, closedDB(t), "11111111-0000-0000-0000-000000000001", RoleNone)
}

// The splice refuses a target the caller has no access to BEFORE provisioning —
// no connection is opened, so an unreachable base is enough to prove it.
func TestTheSpliceRefusesATargetWithNoAccess(t *testing.T) {
	b := &sqlServerBackend{base: &msdsn.Config{Host: "127.0.0.1", Port: 1}}
	_, _, err := b.Dial(t.Context(), "db1", "22222222-0000-0000-0000-000000000002",
		[]Grant{{Database: "db1", Role: RoleNone}})
	if err == nil || !strings.Contains(err.Error(), "no access to db1") {
		t.Fatalf("err = %v, want a refusal naming the database", err)
	}
}
