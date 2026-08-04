package server_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

type tenantSetting struct {
	SettingName              string `json:"settingName"`
	Title                    string `json:"title"`
	Enabled                  bool   `json:"enabled"`
	CanSpecifySecurityGroups bool   `json:"canSpecifySecurityGroups"`
	TenantSettingGroup       string `json:"tenantSettingGroup"`
	DelegateToCapacity       bool   `json:"delegateToCapacity"`
	DelegateToDomain         bool   `json:"delegateToDomain"`
	DelegateToWorkspace      bool   `json:"delegateToWorkspace"`
	EnabledSecurityGroups    []struct {
		GraphID string `json:"graphId"`
		Name    string `json:"name"`
	} `json:"enabledSecurityGroups"`
	Properties []struct {
		Name, Type, Value string
	} `json:"properties"`
}

func (f *fixture) tenantSettings(t *testing.T) map[string]tenantSetting {
	t.Helper()
	var page struct {
		Value []tenantSetting `json:"value"`
	}
	f.mustStatus(f.call("GET", "/v1/admin/tenantsettings", f.token, nil, &page),
		http.StatusOK, "list tenant settings")
	byName := map[string]tenantSetting{}
	for _, s := range page.Value {
		byName[s.SettingName] = s
	}
	return byName
}

// The list surface carries the documented TenantSetting fields, including the
// setting names the REST reference's own sample response uses.
func TestTenantSettingsList(t *testing.T) {
	f := newFixture(t)
	settings := f.tenantSettings(t)

	// The three settings named verbatim in the reference sample.
	for name, wantGroup := range map[string]string{
		"AdminApisIncludeDetailedMetadata": "AdminApiSettings",
		"DatamartTenant":                   "DatamartSettings",
		"CertifyDatasets":                  "ExportAndSharing",
	} {
		s, ok := settings[name]
		if !ok {
			t.Fatalf("no tenant setting %q", name)
		}
		if s.TenantSettingGroup != wantGroup {
			t.Fatalf("%s group = %q, want %q", name, s.TenantSettingGroup, wantGroup)
		}
		if s.Title == "" {
			t.Fatalf("%s has no title", name)
		}
	}
	// Delegation flags are modelled, not always-false.
	if !settings["GitIntegration"].DelegateToWorkspace {
		t.Fatalf("GitIntegration should be delegable to workspaces: %+v", settings["GitIntegration"])
	}
	if !settings["AllowDomainAdminsToOverrideWorkspaceAssignment"].DelegateToDomain {
		t.Fatal("the domain-override setting should be delegable to domains")
	}
}

// The reference omits the optional arrays rather than sending empty ones.
func TestTenantSettingsOmitsEmptyOptionalFields(t *testing.T) {
	f := newFixture(t)
	var raw struct {
		Value []map[string]json.RawMessage `json:"value"`
	}
	f.mustStatus(f.call("GET", "/v1/admin/tenantsettings", f.token, nil, &raw),
		http.StatusOK, "list tenant settings")
	if len(raw.Value) == 0 {
		t.Fatal("no tenant settings returned")
	}
	for _, s := range raw.Value {
		for _, field := range []string{"enabledSecurityGroups", "excludedSecurityGroups", "properties"} {
			if v, ok := s[field]; ok && string(v) == "null" {
				t.Fatalf("%s serialised as null; the reference omits it when unset", field)
			}
		}
		// The required fields are always present.
		for _, field := range []string{"settingName", "title", "enabled", "canSpecifySecurityGroups"} {
			if _, ok := s[field]; !ok {
				t.Fatalf("required field %q missing from %v", field, s)
			}
		}
	}
}

// The documented update API: POST .../{name}/update, `enabled` required, and a
// {"tenantSettings":[...]} response wrapper rather than the bare object.
func TestTenantSettingsUpdate(t *testing.T) {
	f := newFixture(t)
	const name = "DatamartTenant"

	// Flip enabled; the other fields survive untouched.
	var wrapped struct {
		TenantSettings []tenantSetting `json:"tenantSettings"`
	}
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/"+name+"/update", f.token,
		map[string]any{"enabled": false}, &wrapped), http.StatusOK, "disable setting")
	if len(wrapped.TenantSettings) != 1 {
		t.Fatalf("response = %+v; the documented shape is {tenantSettings:[...]}", wrapped)
	}
	got := wrapped.TenantSettings[0]
	if got.Enabled {
		t.Fatal("setting still enabled after patch")
	}
	if got.Title == "" || got.TenantSettingGroup != "DatamartSettings" {
		t.Fatalf("patch dropped untouched fields: %+v", got)
	}
	// It persists.
	if f.tenantSettings(t)[name].Enabled {
		t.Fatal("disable did not persist")
	}

	// Security groups round-trip with the documented graphId/name shape.
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/"+name+"/update", f.token, map[string]any{
		"enabled":                  false,
		"canSpecifySecurityGroups": true,
		"enabledSecurityGroups": []map[string]string{
			{"graphId": "f51b705f-a409-4d40-9197-c5d5f349e2f0", "name": "TestComputeCdsa"},
		},
	}, &wrapped), http.StatusOK, "set security groups")
	got = wrapped.TenantSettings[0]
	if len(got.EnabledSecurityGroups) != 1 ||
		got.EnabledSecurityGroups[0].Name != "TestComputeCdsa" {
		t.Fatalf("security groups = %+v", got.EnabledSecurityGroups)
	}

	// Typed properties: the documented types are accepted...
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/"+name+"/update", f.token, map[string]any{
		"enabled":    false,
		"properties": []map[string]string{{"name": "MaxRows", "type": "Integer", "value": "1000"}},
	}, &wrapped), http.StatusOK, "typed property")
	got = wrapped.TenantSettings[0]
	if len(got.Properties) != 1 || got.Properties[0].Type != "Integer" {
		t.Fatalf("properties = %+v", got.Properties)
	}
	// ...and anything else is refused.
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/"+name+"/update", f.token, map[string]any{
		"enabled":    false,
		"properties": []map[string]string{{"name": "X", "type": "NotAType", "value": "1"}},
	}, nil), http.StatusBadRequest, "invalid property type")

	// Naming enabled groups while the setting applies to the whole org is
	// contradictory and is refused.
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/CertifyDatasets/update", f.token, map[string]any{
		"enabled":                  true,
		"canSpecifySecurityGroups": false,
		"enabledSecurityGroups": []map[string]string{
			{"graphId": "f51b705f-a409-4d40-9197-c5d5f349e2f0", "name": "G"},
		},
	}, nil), http.StatusBadRequest, "groups without canSpecifySecurityGroups")

	// Unknown settings and malformed bodies.
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/NoSuchSetting/update", f.token,
		map[string]any{"enabled": true}, nil), http.StatusNotFound, "unknown setting")
}

// TestTenantSettingsUpdateRequiresEnabled: the reference documents `enabled` as
// required, and a PATCH that omits it must be refused rather than silently
// treated as false — which would DISABLE the setting the caller was trying to
// adjust the delegation flags on.
func TestTenantSettingsUpdateRequiresEnabled(t *testing.T) {
	f := newFixture(t)
	const name = "DatamartTenant"
	if !f.tenantSettings(t)[name].Enabled {
		t.Fatal("fixture precondition: DatamartTenant should start enabled")
	}

	var errBody struct {
		ErrorCode string `json:"errorCode"`
		Message   string `json:"message"`
	}
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/"+name+"/update", f.token,
		map[string]any{"delegateToCapacity": true}, &errBody),
		http.StatusBadRequest, "update with no enabled")
	if errBody.Message == "" {
		t.Errorf("the refusal does not say what is missing: %+v", errBody)
	}
	// The decisive part: refusing must not have changed anything.
	if !f.tenantSettings(t)[name].Enabled {
		t.Fatal("a rejected patch disabled the setting")
	}
}

// TestTenantSettingsUpdateRejectsMalformedBody: a broken body is a client
// error, not a 500 and not a no-op 200.
func TestTenantSettingsUpdateRejectsMalformedBody(t *testing.T) {
	f := newFixture(t)
	resp := f.call("POST", "/v1/admin/tenantsettings/DatamartTenant/update", f.token,
		json.RawMessage(`{"enabled": tru`), nil)
	f.mustStatus(resp, http.StatusBadRequest, "malformed JSON body")
}

// TestTenantSettingsUpdateOnAnUnknownSetting is the 404 half of the contract.
func TestTenantSettingsUpdateOnAnUnknownSetting(t *testing.T) {
	f := newFixture(t)
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/NoSuchSetting/update", f.token,
		map[string]any{"enabled": true}, nil), http.StatusNotFound, "unknown setting")
}

// TestTenantSettingsOrgWideCannotNameSecurityGroups pins the reference's own
// rule: canSpecifySecurityGroups false MEANS "enabled for the entire
// organization", so naming the groups it is enabled for contradicts itself.
// Accepting both would store a setting whose two halves disagree, and whichever
// half a reader trusts would be a coin toss.
func TestTenantSettingsOrgWideCannotNameSecurityGroups(t *testing.T) {
	f := newFixture(t)
	const name = "DatamartTenant"

	var errBody struct {
		Message string `json:"message"`
	}
	f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/"+name+"/update", f.token, map[string]any{
		"enabled":                  true,
		"canSpecifySecurityGroups": false,
		"enabledSecurityGroups": []map[string]string{
			{"graphId": "f51b705f-a409-4d40-9197-c5d5f349e2f0", "name": "Group"},
		},
	}, &errBody), http.StatusBadRequest, "org-wide plus named groups")
	if errBody.Message == "" {
		t.Errorf("the refusal does not explain the conflict: %+v", errBody)
	}
	if len(f.tenantSettings(t)[name].EnabledSecurityGroups) != 0 {
		t.Fatal("the contradictory patch was stored anyway")
	}
}

// TestTenantSettingsUpdateSetsEachDelegationFlagIndependently.
//
// The three delegation flags are patched by three near-identical blocks, which
// is the shape where a copy-paste error assigns the wrong field and nothing
// notices — every flag is a bool, so a swap still round-trips a plausible
// value. Setting them ONE AT A TIME is what makes a swap visible.
func TestTenantSettingsUpdateSetsEachDelegationFlagIndependently(t *testing.T) {
	f := newFixture(t)
	const name = "DatamartTenant"

	for _, field := range []string{"delegateToCapacity", "delegateToDomain", "delegateToWorkspace"} {
		var wrapped struct {
			TenantSettings []tenantSetting `json:"tenantSettings"`
		}
		f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/"+name+"/update", f.token,
			map[string]any{"enabled": true, field: true}, &wrapped),
			http.StatusOK, "set "+field)
		if len(wrapped.TenantSettings) != 1 {
			t.Fatalf("%s: response = %+v", field, wrapped)
		}
		got := wrapped.TenantSettings[0]
		set := map[string]bool{
			"delegateToCapacity":  got.DelegateToCapacity,
			"delegateToDomain":    got.DelegateToDomain,
			"delegateToWorkspace": got.DelegateToWorkspace,
		}
		if !set[field] {
			t.Errorf("%s was not applied: %+v", field, set)
		}
		for other, v := range set {
			if other != field && v {
				t.Errorf("setting %s also set %s — the patch blocks are crossed", field, other)
			}
		}
		// Put it back so each iteration starts from all-false.
		f.mustStatus(f.call("POST", "/v1/admin/tenantsettings/"+name+"/update", f.token,
			map[string]any{"enabled": true, field: false}, nil), http.StatusOK, "reset "+field)
	}
}
