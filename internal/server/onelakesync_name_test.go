package server

import (
	"strings"
	"testing"
)

// "OneLake security role names cannot exceed 124 characters; otherwise, role
// creation or synchronization fails on the SQL analytics endpoint." A synced role
// carries the four-character OLS_ prefix, so 124 is the longest a role may be and
// still fit SQL Server's 128-character name.
func TestARoleNameLongerThan124CharactersDoesNotSync(t *testing.T) {
	name, err := oneLakeRoleName(strings.Repeat("r", 124))
	if err != nil || len(name) != 128 {
		t.Fatalf("124 characters: %q, %v; want a 128-character SQL role name", name, err)
	}
	if _, err := oneLakeRoleName(strings.Repeat("r", 125)); err == nil {
		t.Fatal("125 characters synced; the documented limit is 124")
	}
}
