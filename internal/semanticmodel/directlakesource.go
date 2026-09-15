package semanticmodel

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// DirectLakeFlavor is which of Fabric's two Direct Lake storage options a
// shared expression binds a table to.
type DirectLakeFlavor int

const (
	// DirectLakeOnOneLake reads Delta through OneLake: "AzureStorage.DataLake for
	// Direct Lake on OneLake", over a onelake.dfs.fabric.microsoft.com URL.
	DirectLakeOnOneLake DirectLakeFlavor = iota + 1
	// DirectLakeOnSQL checks permissions through a SQL analytics endpoint:
	// "Sql.Database for Direct Lake on SQL analytics endpoints".
	DirectLakeOnSQL
)

// DirectLakeSource is where one shared expression points.
type DirectLakeSource struct {
	Flavor DirectLakeFlavor
	// Workspace and Item are the OneLake URL's two path segments, unescaped:
	// each a GUID or a name (the item optionally suffixed .Lakehouse).
	Workspace, Item string
	// Server and Database are Sql.Database's two arguments: the endpoint's host
	// and the SQL analytics endpoint or warehouse, by GUID or by name.
	Server, Database string
}

var oneLakeURL = regexp.MustCompile(`(?i)https://onelake\.dfs\.fabric\.microsoft\.com/([^/"?]+?)/([^/"?]+)`)

// sqlDatabaseCall finds the Power Query function, which is case-sensitive in M.
// The word boundary keeps Sql.Databases — a different function, listing every
// database on a server — from reading as a binding to one of them.
var sqlDatabaseCall = regexp.MustCompile(`\bSql\.Database\s*\(`)

// ParseDirectLakeSource classifies a Direct Lake shared expression.
//
// Both flavours are written the way Fabric writes them — the M text is not
// evaluated, only read for its source, since "Direct Lake doesn't use these
// functions to read the source Delta tables". What is neither, or both, is
// refused by name rather than guessed at.
func ParseDirectLakeSource(expression string) (DirectLakeSource, error) {
	lake := oneLakeURL.FindStringSubmatch(expression)
	call := sqlDatabaseCall.FindStringIndex(expression)
	switch {
	case lake != nil && call != nil:
		return DirectLakeSource{}, fmt.Errorf("shared expression names both a OneLake URL and Sql.Database; " +
			"a Direct Lake expression binds to one source")
	case call != nil:
		return parseSQLDatabase(expression[call[1]:])
	case lake != nil:
		ws, err := url.PathUnescape(lake[1])
		if err != nil {
			return DirectLakeSource{}, fmt.Errorf("invalid workspace path")
		}
		item, err := url.PathUnescape(lake[2])
		if err != nil {
			return DirectLakeSource{}, fmt.Errorf("invalid lakehouse path")
		}
		return DirectLakeSource{Flavor: DirectLakeOnOneLake, Workspace: ws, Item: item}, nil
	}
	return DirectLakeSource{}, fmt.Errorf("shared expression is neither Direct Lake on OneLake (a " +
		"onelake.dfs.fabric.microsoft.com workspace/item URL) nor Direct Lake on SQL (Sql.Database)")
}

// parseSQLDatabase reads `"server", "database")` — the rest of the call after
// its opening parenthesis. Two text literals and nothing else: an options
// record changes how the connector behaves, and none of its fields is modelled.
func parseSQLDatabase(rest string) (DirectLakeSource, error) {
	var args []string
	for {
		rest = strings.TrimSpace(rest)
		lit, after, err := mText(rest)
		if err != nil {
			return DirectLakeSource{}, fmt.Errorf("Sql.Database argument %d: %w", len(args)+1, err)
		}
		args = append(args, lit)
		rest = strings.TrimSpace(after)
		if len(args) == 2 {
			break
		}
		if !strings.HasPrefix(rest, ",") {
			return DirectLakeSource{}, fmt.Errorf("Sql.Database takes a server and a database; got one argument")
		}
		rest = rest[1:]
	}
	switch {
	case strings.HasPrefix(rest, ")"):
	case strings.HasPrefix(rest, ","):
		return DirectLakeSource{}, fmt.Errorf("Sql.Database options are not supported: a Direct Lake on SQL " +
			"expression names only the server and the database")
	default:
		return DirectLakeSource{}, fmt.Errorf("Sql.Database: expected \")\" after the database")
	}
	if strings.TrimSpace(args[0]) == "" || strings.TrimSpace(args[1]) == "" {
		return DirectLakeSource{}, fmt.Errorf("Sql.Database needs a non-empty server and database")
	}
	return DirectLakeSource{Flavor: DirectLakeOnSQL, Server: args[0], Database: args[1]}, nil
}

// mText reads one M text literal from the start of s: double-quoted, with a
// doubled quote standing for one. Anything else — a variable, a function call —
// is refused, since the emulator does not evaluate M.
func mText(s string) (string, string, error) {
	if !strings.HasPrefix(s, `"`) {
		return "", "", fmt.Errorf("expected a text literal, as Fabric writes it")
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			b.WriteByte('"')
			i++
			continue
		}
		return b.String(), s[i+1:], nil
	}
	return "", "", fmt.Errorf("unterminated text literal")
}

// Direct Lake behaviours, TOM's DirectLakeBehavior as TMSL spells it. It "only
// applies to Direct Lake on SQL analytics endpoints".
const (
	DirectLakeAutomatic       = "automatic"
	DirectLakeOnly            = "directLakeOnly"
	DirectLakeDirectQueryOnly = "directQueryOnly"
)

// directLakeBehavior normalises a declared behaviour; empty is the default,
// Automatic. An unknown value is refused: reading it as Automatic would fall
// back where the author asked for a failure.
func directLakeBehavior(v string) (string, error) {
	for _, known := range []string{DirectLakeAutomatic, DirectLakeOnly, DirectLakeDirectQueryOnly} {
		if strings.EqualFold(v, known) {
			return known, nil
		}
	}
	if v == "" {
		return DirectLakeAutomatic, nil
	}
	return "", fmt.Errorf("unknown directLakeBehavior %q (automatic, directLakeOnly or directQueryOnly)", v)
}
