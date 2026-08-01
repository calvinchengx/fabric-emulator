package testsupport

import "testing"

// withDatabase has to survive the DSN shapes CI actually sets, and one of them
// is hostile: SQL Server LocalDB on Windows is reached over a named pipe whose
// name contains BOTH '#' and '\'.
//
//	np:\\.\pipe\LOCALDB#A1B2C3D4\tsql\query
//
// In the URL form ("sqlserver://…?pipe=…") '#' starts a fragment, so everything
// after it is discarded by the URL parser and the connection fails with a
// truncated pipe name and nothing pointing at why. The ADO/keyword form has no
// URL parsing, so those characters stay literal — which is why CI should set
// the keyword form for LocalDB, and why this test exists to pin that.
func TestWithDatabase(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "keyword form, as the Linux/macOS CI legs set it",
			dsn:  "server=localhost,1433;user id=sa;password=p;encrypt=disable",
			want: "server=localhost,1433;user id=sa;password=p;encrypt=disable;database=t_X",
		},
		{
			name: "keyword form already terminated",
			dsn:  "server=localhost;",
			want: "server=localhost;database=t_X",
		},
		{
			// The safe LocalDB form. '#' and '\' are literal here.
			name: "keyword form, LocalDB named pipe",
			dsn:  `server=np:\\.\pipe\LOCALDB#A1B2C3D4\tsql\query`,
			want: `server=np:\\.\pipe\LOCALDB#A1B2C3D4\tsql\query;database=t_X`,
		},
		{
			name: "url form without a query",
			dsn:  "sqlserver://sa:p@localhost:1433",
			want: "sqlserver://sa:p@localhost:1433?database=t_X",
		},
		{
			name: "url form with an existing query",
			dsn:  "sqlserver://sa:p@localhost:1433?encrypt=disable",
			want: "sqlserver://sa:p@localhost:1433?encrypt=disable&database=t_X",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := withDatabase(c.dsn, "t_X"); got != c.want {
				t.Errorf("withDatabase(%q)\n  got  %q\n  want %q", c.dsn, got, c.want)
			}
		})
	}
}

// dbName has to produce something CREATE DATABASE will accept from any test
// name, including subtests, which carry '/' and spaces.
func TestDBNameIsLegal(t *testing.T) {
	t.Run("sub test/with slash and space", func(t *testing.T) {
		got := dbName(t)
		for _, r := range got {
			ok := r == '_' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				t.Fatalf("dbName produced %q, which contains %q — not a bare identifier", got, r)
			}
		}
		if len(got) > 128 {
			t.Errorf("dbName is %d chars; SQL Server caps an identifier at 128", len(got))
		}
	})
}

// Only ']' is special inside a bracket-quoted T-SQL identifier — it must be
// doubled or the quoting ends early and the rest of the name becomes syntax.
// '[' is literal and must NOT be doubled.
func TestQuoteEscapesBrackets(t *testing.T) {
	if got, want := quote("we][rd"), "[we]][rd]"; got != want {
		t.Errorf("quote = %q, want %q", got, want)
	}
	if got, want := quote("plain"), "[plain]"; got != want {
		t.Errorf("quote = %q, want %q", got, want)
	}
}
