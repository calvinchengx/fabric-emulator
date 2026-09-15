package semanticmodel

import (
	"strings"
	"testing"
)

// The expression Fabric writes for Direct Lake on SQL: a let over Sql.Database
// naming the endpoint's host and the SQL analytics endpoint by GUID.
const sqlExpression = "let\n    database = Sql.Database(\"x6eps4xrq2xudenlfv6naeo3i4-abc.datawarehouse.fabric.microsoft.com\", \"803c8e33-c35c-4f1b-9b44-f40dce69e75e\")\nin\n    database"

func TestParseDirectLakeSource(t *testing.T) {
	for name, tc := range map[string]struct {
		expr string
		want DirectLakeSource
	}{
		"on SQL, as Fabric writes it": {sqlExpression, DirectLakeSource{Flavor: DirectLakeOnSQL,
			Server: "x6eps4xrq2xudenlfv6naeo3i4-abc.datawarehouse.fabric.microsoft.com", Database: "803c8e33-c35c-4f1b-9b44-f40dce69e75e"}},
		"on SQL, by name, with a doubled quote and loose spacing": {`Sql.Database ( "host" ,` + "\n" + `"Sales ""Gold""" )`,
			DirectLakeSource{Flavor: DirectLakeOnSQL, Server: "host", Database: `Sales "Gold"`}},
		"on OneLake": {`let Source = AzureStorage.DataLake("https://onelake.dfs.fabric.microsoft.com/ws-1/lake-1", [HierarchicalNavigation=true]) in Source`,
			DirectLakeSource{Flavor: DirectLakeOnOneLake, Workspace: "ws-1", Item: "lake-1"}},
		"on OneLake, escaped names": {`AzureStorage.DataLake("https://onelake.dfs.fabric.microsoft.com/My%20Space/Gold.Lakehouse")`,
			DirectLakeSource{Flavor: DirectLakeOnOneLake, Workspace: "My Space", Item: "Gold.Lakehouse"}},
	} {
		got, err := ParseDirectLakeSource(tc.expr)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
}

func TestParseDirectLakeSourceRefusals(t *testing.T) {
	for expr, want := range map[string]string{
		`Sql.Database("host", "db", [CommandTimeout=#duration(0,0,5,0)])`: "options are not supported",
		`Sql.Database("host")`:         "got one argument",
		`Sql.Database(Server, "db")`:   "argument 1: expected a text literal",
		`Sql.Database("host", Db)`:     "argument 2: expected a text literal",
		`Sql.Database("host", "db`:     "unterminated",
		`Sql.Database("host", "db" in`: `expected ")"`,
		`Sql.Database("", "db")`:       "non-empty",
		`Sql.Database("host", " ")`:    "non-empty",
		`Sql.Databases("host")`:        "neither",
		`sql.database("host", "db")`:   "neither", // M is case-sensitive
		`let s = Sql.Database("h", "d"), l = AzureStorage.DataLake("https://onelake.dfs.fabric.microsoft.com/w/i") in s`: "both",
		`Lakehouse.Contents(null)`: "neither",
		`AzureStorage.DataLake("https://onelake.dfs.fabric.microsoft.com/%zz/lake")`: "invalid workspace path",
		`AzureStorage.DataLake("https://onelake.dfs.fabric.microsoft.com/ws/%zz")`:   "invalid lakehouse path",
	} {
		if _, err := ParseDirectLakeSource(expr); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", expr, err, want)
		}
	}
}

func TestDirectLakeBehaviorIsParsed(t *testing.T) {
	tmsl := func(behavior string) string {
		return `{"name":"m","model":{` + behavior + `"tables":[{"name":"T","columns":[{"name":"C","dataType":"string"}]}]}}`
	}
	for decl, want := range map[string]string{
		``:                                       DirectLakeAutomatic,
		`"directLakeBehavior":"directLakeOnly",`: DirectLakeOnly,
		`"directLakeBehavior":"DirectQueryOnly",`: DirectLakeDirectQueryOnly,
		`"directLakeBehavior":"Automatic",`:       DirectLakeAutomatic,
	} {
		m, err := ParseTMSL([]byte(tmsl(decl)))
		if err != nil || m.DirectLakeBehavior != want {
			t.Errorf("TMSL %s: %q, %v; want %q", decl, m.DirectLakeBehavior, err, want)
		}
	}
	if _, err := ParseTMSL([]byte(tmsl(`"directLakeBehavior":"sometimes",`))); err == nil || !strings.Contains(err.Error(), "unknown directLakeBehavior") {
		t.Errorf("an unknown TMSL behaviour: %v", err)
	}

	table := []byte("table T\n\tcolumn C\n\t\tdataType: string\n")
	for model, want := range map[string]string{
		"":                                DirectLakeAutomatic,
		"model Model\n\tculture: en-US\n": DirectLakeAutomatic,
		"model Model\n\tdirectLakeBehavior: directLakeOnly\n": DirectLakeOnly,
	} {
		parts := map[string][]byte{"definition/tables/T.tmdl": table}
		if model != "" {
			parts["definition/model.tmdl"] = []byte(model)
		}
		m, err := ParseTMDL(parts)
		if err != nil || m.DirectLakeBehavior != want {
			t.Errorf("TMDL %q: %q, %v; want %q", model, m.DirectLakeBehavior, err, want)
		}
	}
	if _, err := ParseTMDL(map[string][]byte{"definition/tables/T.tmdl": table,
		"definition/model.tmdl": []byte("model Model\n\tdirectLakeBehavior: never\n")}); err == nil || !strings.Contains(err.Error(), "unknown directLakeBehavior") {
		t.Errorf("an unknown TMDL behaviour: %v", err)
	}
}
