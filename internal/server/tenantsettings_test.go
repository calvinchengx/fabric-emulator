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

// PATCH is an emulator affordance (real Fabric changes settings in the admin
// portal), but it must behave sanely: partial updates, validation, and
// persistence.
func TestTenantSettingsUpdate(t *testing.T) {
	f := newFixture(t)
	const name = "DatamartTenant"

	// Flip enabled; the other fields survive untouched.
	var got tenantSetting
	f.mustStatus(f.call("PATCH", "/v1/admin/tenantsettings/"+name, f.token,
		map[string]any{"enabled": false}, &got), http.StatusOK, "disable setting")
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
	f.mustStatus(f.call("PATCH", "/v1/admin/tenantsettings/"+name, f.token, map[string]any{
		"canSpecifySecurityGroups": true,
		"enabledSecurityGroups": []map[string]string{
			{"graphId": "f51b705f-a409-4d40-9197-c5d5f349e2f0", "name": "TestComputeCdsa"},
		},
	}, &got), http.StatusOK, "set security groups")
	if len(got.EnabledSecurityGroups) != 1 ||
		got.EnabledSecurityGroups[0].Name != "TestComputeCdsa" {
		t.Fatalf("security groups = %+v", got.EnabledSecurityGroups)
	}

	// Typed properties: the documented types are accepted...
	f.mustStatus(f.call("PATCH", "/v1/admin/tenantsettings/"+name, f.token, map[string]any{
		"properties": []map[string]string{{"name": "MaxRows", "type": "Integer", "value": "1000"}},
	}, &got), http.StatusOK, "typed property")
	if len(got.Properties) != 1 || got.Properties[0].Type != "Integer" {
		t.Fatalf("properties = %+v", got.Properties)
	}
	// ...and anything else is refused.
	f.mustStatus(f.call("PATCH", "/v1/admin/tenantsettings/"+name, f.token, map[string]any{
		"properties": []map[string]string{{"name": "X", "type": "NotAType", "value": "1"}},
	}, nil), http.StatusBadRequest, "invalid property type")

	// Naming enabled groups while the setting applies to the whole org is
	// contradictory and is refused.
	f.mustStatus(f.call("PATCH", "/v1/admin/tenantsettings/CertifyDatasets", f.token, map[string]any{
		"canSpecifySecurityGroups": false,
		"enabledSecurityGroups": []map[string]string{
			{"graphId": "f51b705f-a409-4d40-9197-c5d5f349e2f0", "name": "G"},
		},
	}, nil), http.StatusBadRequest, "groups without canSpecifySecurityGroups")

	// Unknown settings and malformed bodies.
	f.mustStatus(f.call("PATCH", "/v1/admin/tenantsettings/NoSuchSetting", f.token,
		map[string]any{"enabled": true}, nil), http.StatusNotFound, "unknown setting")
}
