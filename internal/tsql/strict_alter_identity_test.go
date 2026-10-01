package tsql

import (
	"errors"
	"testing"
)

// Fabric allows IDENTITY only on a column declared when the table is created.
// The sidecar runs `ALTER TABLE … ADD id BIGINT IDENTITY` without complaint, so
// this is the silent-divergence shape strict mode exists to catch — including
// the non-BIGINT IDENTITY that createtable.go only looks for in CREATE TABLE.
//
// The accepted half is the point of the check's narrowness: the keyword has to
// be a Word after an ADD, which is what keeps a column merely named
// identity_provider, the word in a literal or a comment, and a NOT ENFORCED
// constraint out of it.
func TestAlterTableAddIdentityRefusedInStrictMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sql     string
		refused bool
	}{
		{"ALTER ADD BIGINT IDENTITY", "ALTER TABLE dbo.t ADD id BIGINT IDENTITY", true},
		{"ALTER ADD non-BIGINT IDENTITY", "ALTER TABLE dbo.t ADD id INT IDENTITY", true},
		{"leading semicolon and quoted names", ";ALTER TABLE [dbo].[t] ADD [id] BIGINT IDENTITY NOT NULL", true},

		{"CREATE TABLE with a BIGINT IDENTITY", "CREATE TABLE dbo.t (id BIGINT IDENTITY, n INT)", false},
		{"a column merely named identity_provider", "ALTER TABLE dbo.t ADD identity_provider VARCHAR(50) NULL", false},
		{"IDENTITY in a string literal", "ALTER TABLE dbo.t ADD note VARCHAR(50) NOT NULL DEFAULT 'IDENTITY'", false},
		{"IDENTITY in a comment", "ALTER TABLE dbo.t ADD note INT NULL -- no IDENTITY here", false},
		{"a NOT ENFORCED constraint", "ALTER TABLE dbo.t ADD CONSTRAINT pk_t PRIMARY KEY NONCLUSTERED (id) NOT ENFORCED", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckStrict(tc.sql)
			if !tc.refused {
				if err != nil {
					t.Fatalf("legitimate statement refused: %v", err)
				}
				return
			}
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("expected UnsupportedError, got %v", err)
			}
			if ue.Feature != "identity-alter-add" {
				t.Fatalf("feature = %q, want %q (%s)", ue.Feature, "identity-alter-add", ue.Detail)
			}
		})
	}
}
