package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/calvinchengx/fabric-emulator/internal/auth"
)

// The contract under test is not this emulator's: it is what
// MicrosoftPowerBIMgmt requires in order to connect at all, and every
// assertion below was derived from watching the module fail. They are written
// as a group because each one, on its own, is a shape that let the module get
// exactly one step further.

// newAPI leaves Auth nil, and the authority is DERIVED from the issuer rather
// than invented, so a discovery test without one exercises only the
// unconfigured case. The package's existing testIssuer is reused rather than a
// second one declared beside it: two issuers in one package is exactly the
// drift this file argues against.
func discoveryMux(t *testing.T) *http.ServeMux {
	t.Helper()
	a, _ := newAPI(t)
	a.Auth = &auth.Validator{Issuer: testIssuer}
	mux := http.NewServeMux()
	a.registerGlobalService(mux)
	return mux
}

func discover(t *testing.T, target string) gsEnvironments {
	t.Helper()
	mux := discoveryMux(t)

	req := httptest.NewRequest(http.MethodPost, target, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got gsEnvironments
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not the shape a client deserialises: %v", err)
	}
	return got
}

func environment(t *testing.T, envs gsEnvironments, cloud string) gsEnvironment {
	t.Helper()
	for _, e := range envs.Environments {
		if e.CloudName == cloud {
			return e
		}
	}
	t.Fatalf("no environment for %s; got %d", cloud, len(envs.Environments))
	return gsEnvironment{}
}

func service(env gsEnvironment, name string) (gsService, bool) {
	for _, s := range env.Services {
		if s.Name == name {
			return s, true
		}
	}
	return gsService{}, false
}

// TestDiscoveryServesBothMethods pins the one thing that could not be
// measured: the real endpoint answers neither GET nor POST publicly, so the
// module's choice is unknown and both are served rather than guessed at.
func TestDiscoveryServesBothMethods(t *testing.T) {
	mux := discoveryMux(t)

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method,
			"https://api.powerbi.com/powerbi/globalservice/v201606/environments/discover", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200", method, rec.Code)
		}
	}
}

// TestEveryCloudTheModuleKnowsIsPresent is the assertion that took the module
// past "Unable to find cloud name: USGovCloud". It populates EVERY environment
// in its settings when connecting to any one of them, so an answer carrying
// only the served cloud fails the whole connect — the unserved clouds take the
// served one down with them.
func TestEveryCloudTheModuleKnowsIsPresent(t *testing.T) {
	got := discover(t, "https://api.powerbi.com/powerbi/globalservice/v201606/environments/discover")

	want := append([]string{globalServiceCloudName}, otherCloudNames...)
	if len(got.Environments) != len(want) {
		t.Fatalf("got %d environments, want %d", len(got.Environments), len(want))
	}
	for _, cloud := range want {
		environment(t, got, cloud) // fails the test if absent
	}
}

// TestEveryEnvironmentCarriesTheServicesTheModuleResolves is the assertion that
// took it past "Sequence contains no matching element". The module resolves
// each cloud's services with a LINQ First(), so an EMPTY service list throws —
// including for a cloud the caller never selected. An unserved cloud is
// therefore fully formed and pointed somewhere that does not resolve, which is
// the next test.
func TestEveryEnvironmentCarriesTheServicesTheModuleResolves(t *testing.T) {
	got := discover(t, "https://api.powerbi.com/powerbi/globalservice/v201606/environments/discover")

	for _, env := range got.Environments {
		for _, name := range []string{"aad", "powerbi-backend", "powerbi-msolap"} {
			if _, ok := service(env, name); !ok {
				t.Errorf("%s: no %q service; the module resolves this by name", env.CloudName, name)
			}
		}
		if len(env.Clients) == 0 {
			t.Errorf("%s: no clients", env.CloudName)
		}
	}
}

// TestUnservedCloudsPointAtNothingThatResolves is the honesty assertion, and
// the one worth breaking a build over. Pointing a national cloud at this
// emulator would make `-Environment USGov` SUCCEED against a deployment that
// is not USGov — a fabricated success with a sovereignty boundary inside it.
func TestUnservedCloudsPointAtNothingThatResolves(t *testing.T) {
	got := discover(t, "https://api.powerbi.com/powerbi/globalservice/v201606/environments/discover")

	for _, cloud := range otherCloudNames {
		env := environment(t, got, cloud)
		for _, svc := range env.Services {
			if !strings.Contains(svc.Endpoint, ".invalid") {
				t.Errorf("%s/%s endpoint %q must be unresolvable, not a real host",
					cloud, svc.Name, svc.Endpoint)
			}
			if strings.Contains(svc.Endpoint, "powerbi.com") {
				t.Errorf("%s/%s points at the served cloud", cloud, svc.Name)
			}
		}
	}
}

// TestTheServedCloudPointsAtTheHostTheCallerReached. Discovery is answered
// relative to how it was reached, so the same binary is correct behind the
// api.powerbi.com alias and on localhost. A configured origin would be one
// more value able to disagree with reality.
func TestTheServedCloudPointsAtTheHostTheCallerReached(t *testing.T) {
	for _, host := range []string{"api.powerbi.com", "localhost:9443"} {
		got := discover(t, "https://"+host+"/powerbi/globalservice/v201606/environments/discover")
		env := environment(t, got, globalServiceCloudName)

		backend, ok := service(env, "powerbi-backend")
		if !ok {
			t.Fatalf("%s: no powerbi-backend", host)
		}
		if backend.Endpoint != "https://"+host {
			t.Errorf("backend endpoint = %q, want https://%s", backend.Endpoint, host)
		}
		if backend.ResourceID != powerBIResource {
			t.Errorf("resourceId = %q, want the resource internal/auth accepts (%q)",
				backend.ResourceID, powerBIResource)
		}
	}
}

// TestTheAuthorityCarriesAPathSegment. MSAL rejects an authority of
// https://host/ outright — "The authority URI should have at least one segment
// in the path" — and the failure surfaces at Connect, far from this file.
func TestTheAuthorityCarriesAPathSegment(t *testing.T) {
	got := discover(t, "https://api.powerbi.com/powerbi/globalservice/v201606/environments/discover")
	env := environment(t, got, globalServiceCloudName)

	aad, ok := service(env, "aad")
	if !ok {
		t.Fatal("no aad service")
	}
	rest, found := strings.CutPrefix(aad.Endpoint, "https://")
	if !found {
		t.Fatalf("authority %q is not https", aad.Endpoint)
	}
	if !strings.Contains(rest, "/") || strings.HasSuffix(rest, "/") {
		t.Errorf("authority %q has no path segment; MSAL rejects it", aad.Endpoint)
	}
}

// TestTheAuthorityIsDerivedFromTheConfiguredIssuer, rather than configured
// separately. A second knob could name a login host whose tokens this emulator
// does not accept, and the two would drift apart on the first deployment that
// set only one of them.
func TestTheAuthorityIsDerivedFromTheConfiguredIssuer(t *testing.T) {
	cases := map[string]string{
		"https://login.microsoftonline.com/tid/v2.0": "https://login.microsoftonline.com/common/oauth2/authorize",
		"http://entra-emulator:8443/tid/v2.0":        "http://entra-emulator:8443/common/oauth2/authorize",
		"":                                           "",
		"::not a url":                                "",
	}
	for issuer, want := range cases {
		if got := authorityFromIssuer(issuer); got != want {
			t.Errorf("authorityFromIssuer(%q) = %q, want %q", issuer, got, want)
		}
	}
}

// TestNoClientIDIsAdvertised. The field is read by the interactive and
// device-code flows, which this emulator does not implement. A plausible id
// would dress an unavailable flow as a working one; an absent field is the
// client's own signal that it is unavailable.
func TestNoClientIDIsAdvertised(t *testing.T) {
	got := discover(t, "https://api.powerbi.com/powerbi/globalservice/v201606/environments/discover")

	for _, env := range got.Environments {
		for _, c := range env.Clients {
			if c.AppID != "" {
				t.Errorf("%s/%s advertises appId %q for a flow the emulator does not implement",
					env.CloudName, c.Name, c.AppID)
			}
		}
	}
}

// TestAnUnknownCloudIsRefusedByName rather than handed GlobalCloud's endpoints
// under another label.
func TestAnUnknownCloudIsRefusedByName(t *testing.T) {
	mux := discoveryMux(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"https://api.powerbi.com/powerbi/globalservice/v201606/environments/discover?cloudName=MarsCloud", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MarsCloud") {
		t.Errorf("the refusal does not name the cloud asked for: %s", rec.Body.String())
	}
}
