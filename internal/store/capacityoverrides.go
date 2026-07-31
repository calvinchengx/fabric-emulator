package store

import "encoding/json"

// Capacity-level tenant setting overrides
// (admin/tenants/list-capacities-tenant-settings-overrides). A capacity admin
// may override a setting the tenant admin delegated to capacities.
//
// The documented CapacityTenantSetting is a TenantSetting plus `delegatedFrom`
// and `delegateToWorkspace`, and *without* delegateToCapacity/delegateToDomain
// — a setting already delegated to a capacity cannot be re-delegated sideways.
type CapacityTenantSetting struct {
	SettingName              string                     `json:"settingName"`
	Title                    string                     `json:"title"`
	Enabled                  bool                       `json:"enabled"`
	CanSpecifySecurityGroups bool                       `json:"canSpecifySecurityGroups"`
	TenantSettingGroup       string                     `json:"tenantSettingGroup,omitempty"`
	EnabledSecurityGroups    []TenantSettingSecurityGrp `json:"enabledSecurityGroups,omitempty"`
	ExcludedSecurityGroups   []TenantSettingSecurityGrp `json:"excludedSecurityGroups,omitempty"`
	Properties               []TenantSettingProperty    `json:"properties,omitempty"`
	DelegateToWorkspace      bool                       `json:"delegateToWorkspace,omitempty"`
	DelegatedFrom            string                     `json:"delegatedFrom"`
}

// DelegatedFrom values, verbatim from the reference enumeration. Only
// `Tenant` is ever produced today — an override created at a capacity was, by
// definition, delegated from the tenant — but the full set is recorded because
// the field is an enum a client may switch on.
const (
	DelegatedFromTenant   = "Tenant"
	DelegatedFromCapacity = "Capacity"
	DelegatedFromDomain   = "Domain"
)

var _ = []string{DelegatedFromCapacity, DelegatedFromDomain}

// SetCapacityOverride records (or replaces) one capacity's override of a
// tenant setting.
func (s *Store) SetCapacityOverride(capacityID string, setting *CapacityTenantSetting) error {
	raw, err := json.Marshal(setting)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO capacity_setting_overrides (capacity_id, setting_name, setting_json)
		 VALUES (?,?,?)
		 ON CONFLICT(capacity_id, setting_name) DO UPDATE SET setting_json = excluded.setting_json`,
		capacityID, setting.SettingName, string(raw))
	return err
}

// CapacityOverrides returns every capacity's overrides, grouped by capacity in
// a stable order — the shape the list API returns.
func (s *Store) CapacityOverrides() ([]string, map[string][]CapacityTenantSetting, error) {
	rows, err := s.db.Query(
		`SELECT capacity_id, setting_json FROM capacity_setting_overrides
		 ORDER BY capacity_id, setting_name`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	order := []string{}
	byCapacity := map[string][]CapacityTenantSetting{}
	for rows.Next() {
		var capacityID, raw string
		if err := rows.Scan(&capacityID, &raw); err != nil {
			return nil, nil, err
		}
		var setting CapacityTenantSetting
		if err := json.Unmarshal([]byte(raw), &setting); err != nil {
			continue
		}
		if _, seen := byCapacity[capacityID]; !seen {
			order = append(order, capacityID)
		}
		byCapacity[capacityID] = append(byCapacity[capacityID], setting)
	}
	return order, byCapacity, rows.Err()
}
