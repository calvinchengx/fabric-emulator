package tds

// The collation a per-item database is created with, and why it is not the
// server's.
//
// MEASURED ON A REAL TENANT, 2026-08-11. A Fabric Warehouse reports
// `Latin1_General_100_BIN2_UTF8` as its database collation, and Microsoft's own
// Get Warehouse reference documents that value as "The default - case-sensitive
// (CS) collation". BIN2 is a BINARY collation: identifier and data comparisons
// are case- AND accent-sensitive.
//
// The emulator's backend is a SQL Server container whose server collation is
// `SQL_Latin1_General_CP1_CI_AS` — case-INSENSITIVE — and `CREATE DATABASE [x]`
// with no COLLATE clause inherits it. So every per-item database was
// case-insensitive while the thing it stands in for is case-sensitive, and the
// divergence pointed the dangerous way:
//
//	SELECT ... FROM information_schema.tables   -- 12 rows here, on a tenant:
//	                                            -- "Invalid object name"
//
// Green locally, broken in production, and undiscoverable by reading code. That
// asymmetry is worth more than the convenience of a forgiving default: a casing
// mistake should fail on the developer's machine, which is the only place it is
// cheap to fix.
//
// Fabric offers exactly two collations at create time, so those are the only two
// this accepts — an unrecognised value must not be silently coerced into the
// default, because "we ignored your collation" is how a case-sensitivity bug gets
// re-introduced by configuration.
const (
	// CollationCaseSensitive is Fabric's default: binary, case- and
	// accent-sensitive, UTF-8.
	CollationCaseSensitive = "Latin1_General_100_BIN2_UTF8"
	// CollationCaseInsensitive is the opt-in Fabric documents on
	// Warehouse creationPayload.collationType.
	CollationCaseInsensitive = "Latin1_General_100_CI_AS_KS_WS_SC_UTF8"
)

// ValidCollation reports whether `c` is one Fabric actually offers.
func ValidCollation(c string) bool {
	return c == CollationCaseSensitive || c == CollationCaseInsensitive
}

// collationFor resolves the collation to create `database` with: whatever the
// item declared, else Fabric's default. An unknown declared value falls back to
// the default rather than reaching the DDL, because an invalid COLLATE clause
// fails the CREATE DATABASE outright and a warehouse that cannot be created is a
// worse answer than one created case-sensitive.
func (b *sqlServerBackend) collationFor(database string) string {
	if b.CollationOf != nil {
		if c := b.CollationOf(database); ValidCollation(c) {
			return c
		}
	}
	return CollationCaseSensitive
}
