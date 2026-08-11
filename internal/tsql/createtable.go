package tsql

// CREATE TABLE restrictions Fabric Warehouse enforces and SQL Server does not.
//
// MEASURED AGAINST A REAL TENANT, 2026-08-11, on a Fabric Warehouse:
//
//	CREATE TABLE dbo.t (id INT IDENTITY(1,1), n INT)
//	  -> "Identity column 'id' must be of data type BIGINT."
//	CREATE TABLE dbo.t (id INT NOT NULL PRIMARY KEY)
//	  -> "The PRIMARY KEY keyword is not supported in the CREATE TABLE statement…"
//	CREATE TABLE dbo.t (id INT NOT NULL,
//	                    CONSTRAINT pk PRIMARY KEY NONCLUSTERED (id) NOT ENFORCED)
//	  -> the same refusal: NOT ENFORCED does not buy an inline constraint either;
//	     Fabric wants ALTER TABLE … ADD CONSTRAINT.
//
// The emulator's backend is a SQL Server container, which accepts all three. That
// is the dangerous direction — a model using any of them builds locally and fails
// on the tenant it was written for — and it is the direction an emulator must
// close, because the alternative is that its users discover Fabric's restrictions
// in production.
//
// This REFUSES rather than rewrites. A rewrite would have to invent semantics
// Fabric does not have: dropping IDENTITY changes what the table does, and
// hoisting an inline PRIMARY KEY into an ALTER TABLE changes what the statement
// is. The author has to make that call, and a refusal that quotes the tenant's
// own message is the shortest path to them making it.

import (
	"fmt"
	"strings"
)

// checkCreateTableRestrictions refuses a CREATE TABLE that Fabric Warehouse
// would refuse. It looks at TOKENS rather than raw text so a column called
// `identity_provider`, a string literal containing "PRIMARY KEY", or a comment
// cannot trigger it.
func checkCreateTableRestrictions(toks []Token) error {
	if !startsCreateTable(toks) {
		return nil
	}
	for i, t := range toks {
		if t.Kind != Word {
			continue
		}
		switch strings.ToUpper(t.Text) {
		case "IDENTITY":
			// Fabric allows IDENTITY only on BIGINT. The type precedes the
			// keyword: `id BIGINT IDENTITY(1,1)`.
			if ty := previousTypeName(toks, i); ty != "" && ty != "BIGINT" {
				return fmt.Errorf(
					"Fabric Warehouse restriction: an IDENTITY column must be BIGINT, "+
						"not %s — the tenant answers \"Identity column must be of data "+
						"type BIGINT\". Change the type, or drop IDENTITY and generate "+
						"the key yourself", ty)
			}
		case "PRIMARY":
			if i+1 < len(toks) && strings.EqualFold(nextWord(toks, i), "KEY") {
				return fmt.Errorf(
					"Fabric Warehouse restriction: PRIMARY KEY is not supported in " +
						"CREATE TABLE — not even NOT ENFORCED. Create the table, then " +
						"ALTER TABLE … ADD CONSTRAINT … PRIMARY KEY NONCLUSTERED (…) " +
						"NOT ENFORCED")
			}
		}
	}
	return nil
}

// startsCreateTable reports whether the token stream begins a CREATE TABLE.
// Deliberately narrow: CREATE TABLE only, so CREATE VIEW/PROC/INDEX pass through.
func startsCreateTable(toks []Token) bool {
	var seen []string
	for _, t := range toks {
		if t.Kind != Word {
			continue
		}
		seen = append(seen, strings.ToUpper(t.Text))
		if len(seen) == 2 {
			break
		}
	}
	return len(seen) == 2 && seen[0] == "CREATE" && seen[1] == "TABLE"
}

// previousTypeName returns the identifier immediately before position i, upper
// cased — the column's declared type when i is an IDENTITY keyword. Empty when
// there is no preceding identifier.
func previousTypeName(toks []Token, i int) string {
	for j := i - 1; j >= 0; j-- {
		if toks[j].Kind == Word {
			return strings.ToUpper(toks[j].Text)
		}
	}
	return ""
}

// nextWord returns the next Word token's text after position i, skipping
// whitespace and comments — `PRIMARY /* c */ KEY` is still a PRIMARY KEY.
func nextWord(toks []Token, i int) string {
	for j := i + 1; j < len(toks); j++ {
		if toks[j].Trivia() {
			continue
		}
		if toks[j].Kind == Word {
			return toks[j].Text
		}
		return ""
	}
	return ""
}
