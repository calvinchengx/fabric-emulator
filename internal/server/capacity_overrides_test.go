package server_test

import (
	"net/http"
	"testing"
)

type capacityOverridesPage struct {
	// This surface goes back to `value` — unlike admin workspaces
	// (`workspaces`) and admin items (`itemEntities`).
	Value []struct {
		ID             string `json:"id"`
		TenantSettings []struct {
			SettingName         string `json:"settingName"`
			Title               string `json:"title"`
			Enabled             bool   `json:"enabled"`
			DelegatedFrom       string `json:"delegatedFrom"`
			DelegateToWorkspace bool   `json:"delegateToWorkspace"`
			Properties          []struct {
				Name, Type, Value string
			} `json:"properties"`
		} `json:"tenantSettings"`
	} `json:"value"`
}

func (f *fixture) capacityOverrides(t *testing.T) capacityOverridesPage {
	t.Helper()
	var page capacityOverridesPage
	f.mustStatus(f.call("GET", "/v1/admin/capacities/delegatedTenantSettingOverrides", f.token, nil, &page),
		http.StatusOK, "capacity overrides")
	return page
}

// capacityID reads the seeded capacity from the Core surface — note that is
// where capacities live; there is no /v1/admin/capacities list API.
func (f *fixture) capacityID(t *testing.T) string {
	t.Helper()
	var page struct {
		Value []struct{ ID string } `json:"value"`
	}
	f.mustStatus(f.call("GET", "/v1/capacities", f.token, nil, &page), http.StatusOK, "capacities")
	if len(page.Value) == 0 {
		t.Fatal("no capacities seeded")
	}
	return page.Value[0].ID
}

// An override starts from the tenant setting and reports where it was
// delegated from.
func TestCapacityOverridesRoundTrip(t *testing.T) {
	f := newFixture(t)
	cid := f.capacityID(t)

	// Empty until something is overridden.
	if got := f.capacityOverrides(t); len(got.Value) != 0 {
		t.Fatalf("expected no overrides initially, got %+v", got.Value)
	}

	// GitIntegration is seeded with delegateToCapacity=true.
	base := "/v1/admin/capacities/" + cid + "/delegatedTenantSettingOverrides/GitIntegration/update"
	var wrapped struct {
		Overrides []struct {
			SettingName   string `json:"settingName"`
			Title         string `json:"title"`
			Enabled       bool   `json:"enabled"`
			DelegatedFrom string `json:"delegatedFrom"`
		} `json:"overrides"`
	}
	f.mustStatus(f.call("POST", base, f.token, map[string]any{"enabled": false}, &wrapped),
		http.StatusOK, "update override")
	if len(wrapped.Overrides) != 1 {
		t.Fatalf("response = %+v; the documented shape is {overrides:[...]}", wrapped)
	}
	applied := wrapped.Overrides[0]
	if applied.SettingName != "GitIntegration" || applied.Enabled {
		t.Fatalf("override = %+v", applied)
	}
	// Title is inherited from the tenant setting, not blank.
	if applied.Title == "" {
		t.Fatal("override lost the tenant setting's title")
	}
	// Defaults to Tenant, per the documented DelegatedFrom enum.
	if applied.DelegatedFrom != "Tenant" {
		t.Fatalf("delegatedFrom = %q, want Tenant", applied.DelegatedFrom)
	}

	page := f.capacityOverrides(t)
	if len(page.Value) != 1 || page.Value[0].ID != cid {
		t.Fatalf("overrides = %+v, want one for capacity %s", page.Value, cid)
	}
	if len(page.Value[0].TenantSettings) != 1 ||
		page.Value[0].TenantSettings[0].SettingName != "GitIntegration" {
		t.Fatalf("tenantSettings = %+v", page.Value[0].TenantSettings)
	}
	if page.Value[0].TenantSettings[0].Enabled {
		t.Fatal("the override did not persist the disabled state")
	}

	// Re-applying replaces rather than duplicating.
	f.mustStatus(f.call("POST", base, f.token,
		map[string]any{"enabled": true}, nil), http.StatusOK, "replace")
	page = f.capacityOverrides(t)
	if len(page.Value) != 1 || len(page.Value[0].TenantSettings) != 1 {
		t.Fatalf("re-apply duplicated the override: %+v", page.Value)
	}
	if !page.Value[0].TenantSettings[0].Enabled {
		t.Fatalf("replacement not applied: %+v", page.Value[0].TenantSettings[0])
	}
}

// The rules an override has to obey.
func TestCapacityOverrideValidation(t *testing.T) {
	f := newFixture(t)
	cid := f.capacityID(t)
	path := func(c, n string) string {
		return "/v1/admin/capacities/" + c + "/delegatedTenantSettingOverrides/" + n + "/update"
	}

	// A setting that is not delegated to capacities cannot be overridden —
	// CertifyDatasets is seeded with delegateToCapacity unset.
	f.mustStatus(f.call("POST", path(cid, "CertifyDatasets"), f.token,
		map[string]any{"enabled": false}, nil), http.StatusBadRequest, "not delegated")

	// Unknown capacity and unknown setting are both 404, not 500.
	f.mustStatus(f.call("POST", path("00000000-0000-4000-8000-000000000000", "GitIntegration"), f.token,
		map[string]any{"enabled": false}, nil), http.StatusNotFound, "unknown capacity")
	f.mustStatus(f.call("POST", path(cid, "NoSuchSetting"), f.token,
		map[string]any{"enabled": false}, nil), http.StatusNotFound, "unknown setting")

	// `enabled` is documented as required.
	f.mustStatus(f.call("POST", path(cid, "GitIntegration"), f.token,
		map[string]any{"delegateToWorkspace": true}, nil), http.StatusBadRequest, "enabled omitted")
	f.mustStatus(f.call("POST", path(cid, "GitIntegration"), f.token,
		"not json", nil), http.StatusBadRequest, "malformed body")

	// The documented optional fields round-trip.
	var wrapped struct {
		Overrides []struct {
			DelegateToWorkspace   bool `json:"delegateToWorkspace"`
			EnabledSecurityGroups []struct {
				GraphID string `json:"graphId"`
				Name    string `json:"name"`
			} `json:"enabledSecurityGroups"`
			ExcludedSecurityGroups []struct {
				Name string `json:"name"`
			} `json:"excludedSecurityGroups"`
		} `json:"overrides"`
	}
	f.mustStatus(f.call("POST", path(cid, "GitIntegration"), f.token, map[string]any{
		"enabled":             true,
		"delegateToWorkspace": true,
		"enabledSecurityGroups": []map[string]string{
			{"graphId": "f51b705f-a409-4d40-9197-c5d5f349e2f0", "name": "TestComputeCdsa"},
		},
		"excludedSecurityGroups": []map[string]string{
			{"graphId": "1fecf19f-6e33-41b3-89fa-de8c821f3b79", "name": "Excluded"},
		},
	}, &wrapped), http.StatusOK, "optional fields")
	got := wrapped.Overrides[0]
	if !got.DelegateToWorkspace {
		t.Fatal("delegateToWorkspace not applied")
	}
	if len(got.EnabledSecurityGroups) != 1 || got.EnabledSecurityGroups[0].Name != "TestComputeCdsa" {
		t.Fatalf("enabledSecurityGroups = %+v", got.EnabledSecurityGroups)
	}
	if len(got.ExcludedSecurityGroups) != 1 || got.ExcludedSecurityGroups[0].Name != "Excluded" {
		t.Fatalf("excludedSecurityGroups = %+v", got.ExcludedSecurityGroups)
	}
}
