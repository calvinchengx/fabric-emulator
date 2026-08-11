package api

// A Warehouse's connection string — the address a SQL client dials.
//
// WHY THIS EXISTS. Real Fabric returns `properties.connectionString` on a
// Warehouse, and it is how any client that is not already inside the stack
// finds the SQL endpoint: a BI tool, sqlcmd, or the M expression in a semantic
// model's partition (`Sql.Database("<connectionString>", "<warehouse>")`).
// Without it a consumer has to be TOLD the address out of band, which means
// hardcoding a host — the exact shape that works locally and cannot work
// against production, because on Fabric the endpoint is per-workspace and only
// the API knows it.
//
// DERIVED FROM THE REQUEST, not configured. The right answer depends on where
// the caller is standing: a container on the compose network reaches the
// emulator as `fabric-emulator`, a laptop reaches the same emulator as
// `localhost`. Answering with a single configured hostname would be wrong for
// one of them, and wrong in a way that surfaces as a connection timeout rather
// than as a bad answer. So the host is echoed from the request the client just
// made — by definition an address that works from where it is.
//
// THE PORT IS THE LIMIT OF THAT TRICK. The emulator knows the port it LISTENS
// on, not the port Docker published it to. Reached over the compose network
// those are the same. Reached from the host with a remap (`11533:1433`, as an
// isolated stack does) they are not, and the advertised port is the internal
// one. Fabric has no such distinction — 1433 always — so rather than invent a
// configuration knob for a case Fabric does not have, the shape stays honest
// and the mismatch is documented here.

import (
	"net"
	"net/http"
	"strings"

	"github.com/calvinchengx/fabric-emulator/internal/store"
	"github.com/calvinchengx/fabric-emulator/internal/tds"
)

// warehouseConnectionString returns the host:port a SQL client should dial to
// reach this Warehouse, or "" when no SQL endpoint is running.
func (a *API) warehouseConnectionString(r *http.Request) string {
	if a.SQLEndpointPort == "" {
		return ""
	}
	host := r.Host
	if host == "" {
		return ""
	}
	// Strip the HTTP port: the SQL endpoint is a different listener, so the
	// port the client used to reach the REST API says nothing about it.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// An IPv6 literal has to keep its brackets to be dialable.
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + a.SQLEndpointPort
}

// SQLPortOf extracts the port from a listen address like ":1433" or
// "0.0.0.0:1433". A bare port ("1433") is accepted too, since that is a natural
// thing to write in a compose file.
func SQLPortOf(addr string) string {
	if addr == "" {
		return ""
	}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	if !strings.Contains(addr, ":") {
		return addr
	}
	return ""
}

// warehouseCollation reports the collation a warehouse's database was created
// with, defaulting to Fabric's own default when nothing was declared. Never
// blank: a consumer reading this to decide how to quote an identifier needs an
// answer, and "absent" would make case-sensitivity look like a local quirk
// rather than the documented default it is.
func (a *API) warehouseCollation(itemID string) string {
	if props, err := a.Store.ItemProperties(itemID); err == nil {
		if c := props[store.PropCollationType]; tds.ValidCollation(c) {
			return c
		}
	}
	return tds.CollationCaseSensitive
}
