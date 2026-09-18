package api

import (
	"net/http"
	"net/url"
	"strings"
)

// Power BI's GLOBAL SERVICE DISCOVERY, the call a client makes before it has
// authenticated anything.
//
// WHY IT IS HERE, and it is a correction rather than a feature request.
// MicrosoftPowerBIMgmt 1.3.84 — Microsoft's own PowerShell module — cannot
// connect to a tenant without it. Its stock `Public` environment carries no
// authority and no resource of its own, only `cloudName: GlobalCloud`, so
// Connect-PowerBIServiceAccount resolves every endpoint it needs by POSTing
// here first. Against an emulator that does not serve this route the module
// fails at `Connect-` with "Failed to populate environments in settings",
// before any API this repository grades has been reached.
//
// Eighteen claims were witnessed by `ci:az-rest` and not one of them could
// have caught this: `az rest` is a TRANSPORT. It sends the URL the script
// wrote and hands back the body, so it never performs a client's bootstrap.
// The gap was invisible for exactly as long as every witness was one that
// skipped the step.
//
// THE ORACLE IS THE CLIENT, unusually, and the reason is worth recording. The
// public endpoint is not readable: `GET` and `POST` against
// api.powerbi.com/powerbi/globalservice/v201606/environments/discover both
// answer 404 without whatever gating the real service applies, so there is no
// response to copy. What IS knowable is what a client requires, and that is
// pinned in Microsoft's own shipped assemblies — `GSEnvironments`,
// `GSEnvironment`, `GSEnvironmentService` in
// Microsoft.PowerBI.Commands.Common.dll, together with the three service names
// and one client name the module looks up by string. This file implements that
// contract and nothing beyond it, because a field no client reads is a field
// with no oracle behind it.
//
// WHAT IS NOT CLAIMED. The client entry carries NO `appId`. That field is read
// by the interactive and device-code flows, which this emulator does not
// implement, and the two ways to fill it are both false: the public
// powerbi-powershell registration names an application that does not exist in
// this directory, and a seeded local id would dress an unimplemented flow up
// as a working one. An absent field is a client's own signal that the flow is
// unavailable; a plausible number is the same refusal wearing a green badge.
// Service-principal and client-credential callers pass their own id and never
// read this.

// gsService is one entry of a discovered environment's `services` or `clients`
// array. The field names are the module's, and the JSON tags are lower-camel
// because that is the casing its deserialiser accepts.
type gsService struct {
	Name           string   `json:"name"`
	Endpoint       string   `json:"endpoint,omitempty"`
	ResourceID     string   `json:"resourceId,omitempty"`
	AllowedDomains []string `json:"allowedDomains,omitempty"`
	AppID          string   `json:"appId,omitempty"`
	RedirectURI    string   `json:"redirectUri,omitempty"`
}

type gsEnvironment struct {
	CloudName string      `json:"cloudName"`
	Services  []gsService `json:"services"`
	Clients   []gsService `json:"clients"`
}

type gsEnvironments struct {
	Environments []gsEnvironment `json:"environments"`
}

// powerBIResource is the legacy Analysis Services resource every Power BI
// token is issued for, and the one internal/auth already accepts. Discovery
// must name the SAME string the validator expects, or a client authenticates
// successfully against a resource the emulator then rejects.
const powerBIResource = "https://analysis.windows.net/powerbi/api"

// globalServiceCloudName is the only cloud this emulator answers FOR.
const globalServiceCloudName = "GlobalCloud"

// The national clouds a client knows about and this deployment is not.
//
// THEY ARE LISTED, AND THEY ARE LISTED EMPTY, which is the whole of the design
// here. MicrosoftPowerBIMgmt populates EVERY environment in its settings when
// it connects to any one of them, so a response carrying GlobalCloud alone
// fails the whole connect with "Unable to find cloud name: USGovCloud" — the
// caller cannot reach the cloud that IS served because of the ones that are
// not. Omitting them is therefore not an option.
//
// The two ways to fill them are both worse than empty:
//
//   - POINTING THEM AT THIS EMULATOR would make `-Environment USGov` succeed
//     against a deployment that is not USGov. That is a fabricated success
//     with a sovereignty boundary inside it, which is the most expensive kind.
//   - POINTING THEM AT THE REAL national endpoints would mean writing host
//     names nothing here can check. They would be four more documentation-
//     derived constants able to rot silently, in a file whose entire argument
//     is that the client is the oracle.
//
// An EMPTY service list is not the answer either, and this was measured rather
// than assumed: the module resolves each cloud's services with a LINQ First()
// and an empty list fails the whole connect with "Sequence contains no
// matching element" — again taking the served cloud down with the unserved
// ones.
//
// So each unserved cloud carries a full, well-formed set of services pointing
// at a host under .invalid, the TLD RFC 2606 reserves to never resolve. The
// module populates cleanly, GlobalCloud works, and a caller who selects one of
// these fails at DNS against a name that says what happened. It is a refusal
// by name, in the one form a client's own bootstrap will accept.
var otherCloudNames = []string{
	"USGovCloud", "USGovDoDL4Cloud", "USGovDoDL5Cloud", "ChinaCloud",
}

// unservedCloudHost is where an unserved cloud's endpoints point. `.invalid`
// is reserved by RFC 2606 precisely so that a name can be guaranteed not to
// resolve, which is the property wanted: not a redirect, not a stub, a DNS
// failure carrying the cloud's own name.
func unservedCloudHost(cloudName string) string {
	return "https://" + strings.ToLower(cloudName) + ".not-served-by-this-emulator.invalid"
}

func (a *API) registerGlobalService(mux *http.ServeMux) {
	// BOTH METHODS, because the module's own choice is not documented and the
	// real service answers neither publicly. A discovery call is a read with
	// no body in either spelling, so serving both costs nothing and removes a
	// guess from the critical path of every client's first call.
	mux.HandleFunc("GET /powerbi/globalservice/v201606/environments/discover", a.discoverEnvironments)
	mux.HandleFunc("POST /powerbi/globalservice/v201606/environments/discover", a.discoverEnvironments)
}

// authorityFromIssuer turns the configured Entra issuer into the AAD authority
// a client should log in against.
//
// The issuer is a token's `iss` — origin, tenant, and a version segment, e.g.
// https://login.microsoftonline.com/<tenant>/v2.0. What a client needs is the
// ORIGIN: MSAL appends its own tenant and path, and passing the whole issuer
// through produces an authority with the tenant in it twice.
//
// Derived rather than configured, so the emulator cannot advertise a login
// host it does not itself validate tokens from — the two would drift apart on
// the first deployment that set only one of them.
func authorityFromIssuer(issuer string) string {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	// MSAL REQUIRES A PATH SEGMENT: an authority of https://host/ is rejected
	// outright with "The authority URI should have at least one segment in the
	// path". The segment used is the one Microsoft's own settings.json writes
	// for its real public environments (DXT, Daily), so the shape is taken from
	// the client rather than invented here.
	return parsed.Scheme + "://" + parsed.Host + "/common/oauth2/authorize"
}

// discoverEnvironments answers the pre-authentication discovery call.
//
// The Power BI endpoint is reported as the host the CALLER reached, not a
// configured origin. That is what keeps the answer true under every way this
// emulator is deployed: reached through the api.powerbi.com alias it names
// that alias, reached on localhost it names localhost, and in neither case
// does it send a client somewhere it cannot get back to. A configured origin
// would be one more value able to disagree with reality.
func (a *API) discoverEnvironments(w http.ResponseWriter, r *http.Request) {
	if name := r.URL.Query().Get("cloudName"); name != "" && !strings.EqualFold(name, globalServiceCloudName) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "unknown cloudName " + name + "; this emulator serves " + globalServiceCloudName + " only",
		})
		return
	}

	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	backend := scheme + "://" + r.Host

	authority := ""
	if a.Auth != nil {
		authority = authorityFromIssuer(a.Auth.Issuer)
	}

	env := gsEnvironment{
		CloudName: globalServiceCloudName,
		Services: []gsService{
			// The three names the module looks up by string. `aad` carries the
			// authority; `powerbi-backend` the API root and the resource a
			// token is requested for; `powerbi-msolap` the XMLA surface, whose
			// endpoint is the `powerbi://` form every AS client expects and
			// which internal/api/xmla.go already serves behind this host.
			{Name: "aad", Endpoint: authority, ResourceID: authority},
			{Name: "powerbi-backend", Endpoint: backend, ResourceID: powerBIResource},
			{Name: "powerbi-msolap", Endpoint: "powerbi://" + r.Host, ResourceID: powerBIResource},
		},
		Clients: []gsService{
			// No appId: see the file header. The name is present because a
			// client looks the entry up by it, and an absent entry and an
			// entry with no credential fail differently — the first reads as
			// "this cloud has no PowerShell client", which is untrue.
			{Name: "powerbi-powershell", RedirectURI: backend},
		},
	}
	environments := []gsEnvironment{env}
	for _, name := range otherCloudNames {
		// Services and Clients are non-nil and empty: `[]` rather than `null`,
		// because a client deserialising into IEnumerable<T> may iterate what
		// it is given without a nil check, and the difference between "no
		// services" and "no answer" is the difference between a clean failure
		// and a NullReferenceException inside somebody else's code.
		host := unservedCloudHost(name)
		environments = append(environments, gsEnvironment{
			CloudName: name,
			Services: []gsService{
				{Name: "aad", Endpoint: host + "/common/oauth2/authorize", ResourceID: host},
				{Name: "powerbi-backend", Endpoint: host, ResourceID: powerBIResource},
				{Name: "powerbi-msolap", Endpoint: "powerbi://" + strings.TrimPrefix(host, "https://"), ResourceID: powerBIResource},
			},
			Clients: []gsService{{Name: "powerbi-powershell", RedirectURI: host}},
		})
	}
	writeJSON(w, http.StatusOK, gsEnvironments{Environments: environments})
}
