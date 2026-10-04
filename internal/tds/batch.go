package tds

import (
	"errors"

	"github.com/calvinchengx/fabric-emulator/internal/tsql"
)

// A batch's journey to the engine, as text, for a caller that is not on the
// TDS wire. Fabric's Data Warehouse MCP server (internal/api/dwmcp.go) runs
// T-SQL on the same warehouse a TDS client reaches, so it has to be refused,
// adapted and observed by the same rules. These are those rules; the relays in
// server.go and splice.go call them too, so the two surfaces cannot disagree.

// What a read-only surface answers a write with. A lakehouse's SQL analytics
// endpoint is read-only for everyone; a Warehouse is read-only for a caller
// whose role does not allow writing (a Viewer), and saying "the lakehouse
// endpoint" to them would name a surface they are not on.
const (
	endpointReadOnly = "the lakehouse SQL analytics endpoint is read-only; writes require a Warehouse"
	sessionReadOnly  = "this session is read-only: your access to this warehouse does not include writing to it"
)

// refuserFor is the read-only guard a connection's surface applies: the
// refusal for a batch, or "" to forward it. A lakehouse's SQL analytics
// endpoint is read-only for DATA but is where its security is authored, so it
// forwards what a warehouse Viewer's read-only session does not.
func refuserFor(c Connection) func(string) string {
	switch {
	case c.AnalyticsEndpoint:
		return func(q string) string { return when(isEndpointWrite(q), endpointReadOnly) }
	case c.ReadOnly:
		return func(q string) string { return when(isWriteStatement(q), sessionReadOnly) }
	}
	return nil
}

// refusal is refuse's answer for query, or "" when the surface refuses nothing.
func refusal(refuse func(string) string, query string) string {
	if refuse == nil {
		return ""
	}
	return refuse(query)
}

func when(cond bool, msg string) string {
	if cond {
		return msg
	}
	return ""
}

// PrepareBatch is the statement text the engine should run for one batch on
// connection c, or a refusal in the words a TDS client would get: the
// surface's read-only guard first, then Fabric's dialect (strict-mode
// rejections, nested-CTE flattening, time travel).
func PrepareBatch(c Connection, query string, strict bool) (sql, reason string) {
	if msg := refusal(refuserFor(c), query); msg != "" {
		return "", msg
	}
	sql, _, reason = adaptText(query, strict, c.TimeTravel)
	if reason != "" {
		return "", reason
	}
	return sql, ""
}

// adaptText is Fabric's dialect applied to one batch's text: what to run,
// whether that differs from what was sent, or why Fabric would not run it.
func adaptText(raw string, strict bool, timeTravel tsql.TimeTravelResolver) (sql string, changed bool, reject string) {
	if msg := strictReject(raw, strict); msg != "" {
		return raw, false, msg
	}
	sql, changed, err := tsql.AdaptWithTimeTravel(raw, timeTravel)
	if err != nil {
		// A statement Fabric itself refuses, or one that cannot be flattened
		// without changing its meaning: say so, by name.
		var restriction *tsql.RestrictionError
		var shadowed *tsql.ShadowedNameError
		var timeTravelErr *tsql.TimeTravelError
		if errors.As(err, &restriction) || errors.As(err, &shadowed) || errors.As(err, &timeTravelErr) {
			return raw, false, err.Error()
		}
		// Anything else is a parse failure — forward untouched and let the
		// engine be the authority on its own dialect.
		return raw, false, ""
	}
	return sql, changed, ""
}

// ObserveBatch tells obs what a batch the engine accepted moved, as the relays
// do for each write they forward. The caller has already seen the engine
// accept it, which on the wire is read from the response tokens.
func ObserveBatch(obs Observer, database, sql string) {
	if obs == nil || database == "" || !mightMove(sql) {
		return
	}
	if flows := tsql.DataFlows(sql); len(flows) > 0 {
		obs(database, flows)
	}
}

// TargetFirst is every database c's caller must exist in, the connect target
// first and carrying the rung the router decided for it. Provisioning dedupes
// by first occurrence, so this is what wins if the workspace sweep also lists
// the target with a different rung.
func TargetFirst(c Connection) []Grant {
	target := Grant{Database: c.TargetDB, Role: c.Role}
	for _, g := range c.Grants {
		if g.Database == c.TargetDB {
			target.OneLake, target.OneLakeRoles = g.OneLake, g.OneLakeRoles
			target.ShortcutTables, target.DeniedTables = g.ShortcutTables, g.DeniedTables
			target.ShortcutColumns = g.ShortcutColumns
			break
		}
	}
	return append([]Grant{target}, c.Grants...)
}
