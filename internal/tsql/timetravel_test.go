package tsql

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Phase 2 of docs/35-warehouse-time-travel.md: the hint is recognised from
// tokens, and every case Microsoft documents as rejected is rejected here with
// Fabric's own reason rather than SQL Server's unrecognised-hint syntax error.
//
// The two halves carry equal weight. Accepting the documented example is the
// Class A fix; refusing the rest is what stops a local build going green on a
// time-travel query real Fabric would have turned down.
func TestParseTimeTravelHint(t *testing.T) {
	const docsExample = "SELECT * FROM [dbo].[dimension_customer] AS DC " +
		"OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35.28');"

	for _, tc := range []struct {
		name     string
		sql      string
		rule     string    // expected TimeTravelError.Rule; "" = accepted or absent
		absent   bool      // expect (nil, nil)
		at       time.Time // expected At, when accepted
		stripped string    // expected Stripped, when accepted
	}{
		{
			name:     "the documented example",
			sql:      docsExample,
			at:       time.Date(2024, 3, 13, 19, 39, 35, 280_000_000, time.UTC),
			stripped: "SELECT * FROM [dbo].[dimension_customer] AS DC;",
		},
		{
			name:     "no fractional seconds",
			sql:      "SELECT a FROM t OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35')",
			at:       time.Date(2024, 3, 13, 19, 39, 35, 0, time.UTC),
			stripped: "SELECT a FROM t",
		},
		{
			name:     "lowercase, split across lines, with a comment inside the hint",
			sql:      "select 1\noption\n(  /* as of */ for timestamp as of\t'2024-01-02T03:04:05.006' )\n",
			at:       time.Date(2024, 1, 2, 3, 4, 5, 6_000_000, time.UTC),
			stripped: "select 1\n\n",
		},
		{
			name:     "alongside dbt-fabric's LABEL hint",
			sql:      "SELECT 1 OPTION (LABEL = 'dbt-fabric-dw', FOR TIMESTAMP AS OF '2024-01-01T00:00:00')",
			at:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			stripped: "SELECT 1 OPTION (LABEL = 'dbt-fabric-dw')",
		},

		{name: "no hint at all", sql: "SELECT * FROM dbo.t WHERE x = 1", absent: true},
		{
			name:   "the hint text inside a string literal",
			sql:    "SELECT 'OPTION (FOR TIMESTAMP AS OF ''2024-03-13T19:39:35'')' AS s",
			absent: true,
		},
		{
			name:   "the hint text inside a comment",
			sql:    "SELECT 1 /* OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35') */",
			absent: true,
		},
		{
			name:   "a column merely named option",
			sql:    "SELECT option FROM t ORDER BY option",
			absent: true,
		},
		{
			name:   "another OPTION hint, left alone",
			sql:    "SELECT 1 OPTION (LABEL = 'dbt-fabric-dw')",
			absent: true,
		},

		{
			name: "four fractional digits",
			sql:  "SELECT 1 OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35.2800')",
			rule: "timestamp-format",
		},
		{
			name: "a malformed timestamp",
			sql:  "SELECT 1 OPTION (FOR TIMESTAMP AS OF 'yesterday')",
			rule: "timestamp-format",
		},
		{
			name: "a date with no time",
			sql:  "SELECT 1 OPTION (FOR TIMESTAMP AS OF '2024-03-13')",
			rule: "timestamp-format",
		},
		{
			name: "a Z designator",
			sql:  "SELECT 1 OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35Z')",
			rule: "timestamp-timezone",
		},
		{
			name: "a zone offset",
			sql:  "SELECT 1 OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35+05:30')",
			rule: "timestamp-timezone",
		},
		{
			name: "the hint twice",
			sql: "SELECT 1 OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35') " +
				"OPTION (FOR TIMESTAMP AS OF '2024-03-14T19:39:35')",
			rule: "hint-once",
		},
		{
			name: "twice inside one OPTION group",
			sql: "SELECT 1 OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35', " +
				"FOR TIMESTAMP AS OF '2024-03-14T19:39:35')",
			rule: "hint-once",
		},
		{
			name: "INSERT … SELECT under the hint",
			sql:  "INSERT INTO dbo.t SELECT * FROM dbo.s OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35')",
			rule: "select-only",
		},
		{
			name: "UPDATE under the hint",
			sql:  "UPDATE dbo.t SET a = 1 OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35')",
			rule: "select-only",
		},
		{
			name: "inside CREATE VIEW",
			sql:  "CREATE VIEW dbo.v AS SELECT * FROM dbo.t OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35')",
			rule: "view-definition",
		},
		{
			name: "inside CREATE OR ALTER VIEW",
			sql: "CREATE OR ALTER VIEW dbo.v AS SELECT * FROM dbo.t " +
				"OPTION (FOR TIMESTAMP AS OF '2024-03-13T19:39:35')",
			rule: "view-definition",
		},
		{
			name: "a variable instead of a literal",
			sql:  "SELECT 1 OPTION (FOR TIMESTAMP AS OF @ts)",
			rule: "non-deterministic",
		},
		{
			name: "GETDATE() instead of a literal",
			sql:  "SELECT 1 OPTION (FOR TIMESTAMP AS OF GETDATE())",
			rule: "non-deterministic",
		},
		{
			name: "a concatenation instead of a literal",
			sql:  "SELECT 1 OPTION (FOR TIMESTAMP AS OF '2024-03-13' + 'T19:39:35')",
			rule: "non-deterministic",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTimeTravelHint(tc.sql)

			if tc.rule != "" {
				var tte *TimeTravelError
				if !errors.As(err, &tte) {
					t.Fatalf("expected TimeTravelError, got hint %+v, err %v", got, err)
				}
				if tte.Rule != tc.rule {
					t.Fatalf("rule = %q, want %q (%s)", tte.Rule, tc.rule, tte.Detail)
				}
				if got != nil {
					t.Fatalf("a refused statement returned a hint: %+v", got)
				}
				// Fabric's Msg 22440 is what a consumer sees in production, so a
				// paraphrase would defeat the point of refusing here at all.
				if tc.rule == "timestamp-format" &&
					!strings.Contains(tte.Detail, "Please provide a timestamp in the format yyyy-MM-ddTHH:mm:ss[.fff]") {
					t.Fatalf("detail does not quote Msg 22440: %q", tte.Detail)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.absent {
				if got != nil {
					t.Fatalf("expected no hint, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected a hint, got nil")
			}
			if !got.At.Equal(tc.at) {
				t.Errorf("At = %s, want %s", got.At.Format(time.RFC3339Nano), tc.at.Format(time.RFC3339Nano))
			}
			if got.At.Location() != time.UTC {
				t.Errorf("At location = %s, want UTC", got.At.Location())
			}
			if got.Stripped != tc.stripped {
				t.Errorf("Stripped = %q, want %q", got.Stripped, tc.stripped)
			}
		})
	}
}
