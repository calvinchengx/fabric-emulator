package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Tenant settings, shaped exactly as the Fabric REST reference documents the
// TenantSetting object (admin/tenants/list-tenant-settings).
//
// omitempty on the optional arrays matters: the reference's own sample omits
// enabledSecurityGroups entirely for settings that have none, rather than
// sending an empty array.
type TenantSetting struct {
	SettingName              string                     `json:"settingName"`
	Title                    string                     `json:"title"`
	Enabled                  bool                       `json:"enabled"`
	CanSpecifySecurityGroups bool                       `json:"canSpecifySecurityGroups"`
	TenantSettingGroup       string                     `json:"tenantSettingGroup,omitempty"`
	DelegateToCapacity       bool                       `json:"delegateToCapacity,omitempty"`
	DelegateToDomain         bool                       `json:"delegateToDomain,omitempty"`
	DelegateToWorkspace      bool                       `json:"delegateToWorkspace,omitempty"`
	EnabledSecurityGroups    []TenantSettingSecurityGrp `json:"enabledSecurityGroups,omitempty"`
	ExcludedSecurityGroups   []TenantSettingSecurityGrp `json:"excludedSecurityGroups,omitempty"`
	Properties               []TenantSettingProperty    `json:"properties,omitempty"`
}

// TenantSettingSecurityGrp is the documented TenantSettingSecurityGroup.
type TenantSettingSecurityGrp struct {
	GraphID string `json:"graphId"`
	Name    string `json:"name"`
}

// TenantSettingProperty is the documented typed property.
type TenantSettingProperty struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// TenantSettingPropertyType values, verbatim from the reference enumeration.
var tenantSettingPropertyTypes = map[string]bool{
	"FreeText": true, "Url": true, "Boolean": true,
	"MailEnabledSecurityGroup": true, "Integer": true,
}

// ValidTenantSettingPropertyType reports whether t is one of the documented
// property types. The reference notes more may be added over time, so this is
// a validation aid, not a closed-world claim.
func ValidTenantSettingPropertyType(t string) bool { return tenantSettingPropertyTypes[t] }

// seedTenantSettings installs a starting set (idempotent). The three named in
// the reference's own sample response are included verbatim — same
// settingName, title and tenantSettingGroup — so a client that looks for a
// real setting name finds one. The rest cover the groups the emulator's own
// surfaces relate to.
func (s *Store) seedTenantSettings() error {
	seed := []TenantSetting{
		{SettingName: "AdminApisIncludeDetailedMetadata",
			Title:   "Enhance admin APIs responses with detailed metadata",
			Enabled: true, CanSpecifySecurityGroups: true, TenantSettingGroup: "AdminApiSettings"},
		{SettingName: "DatamartTenant", Title: "Create Datamarts (Preview)",
			Enabled: true, CanSpecifySecurityGroups: true, TenantSettingGroup: "DatamartSettings"},
		{SettingName: "CertifyDatasets", Title: "Certification",
			Enabled: true, CanSpecifySecurityGroups: true, TenantSettingGroup: "ExportAndSharing"},
		{SettingName: "CreateWorkspaces", Title: "Create workspaces",
			Enabled: true, CanSpecifySecurityGroups: true, TenantSettingGroup: "WorkspaceSettings",
			DelegateToCapacity: true},
		{SettingName: "GitIntegration", Title: "Users can synchronize workspace items with their Git repositories",
			Enabled: true, CanSpecifySecurityGroups: true, TenantSettingGroup: "GitIntegrationSettings",
			DelegateToCapacity: true, DelegateToWorkspace: true},
		{SettingName: "AllowDomainAdminsToOverrideWorkspaceAssignment",
			Title:   "Allow tenant and domain admins to override workspace assignments",
			Enabled: true, CanSpecifySecurityGroups: false, TenantSettingGroup: "DomainManagementSettings",
			DelegateToDomain: true},
	}
	for _, t := range seed {
		if err := s.upsertTenantSetting(&t, true); err != nil {
			return err
		}
	}
	return nil
}

// upsertTenantSetting writes a setting. When keepExisting is true an existing
// row wins, which is what makes seeding idempotent across restarts without
// discarding changes a test made.
func (s *Store) upsertTenantSetting(t *TenantSetting, keepExisting bool) error {
	enabled, _ := json.Marshal(t.EnabledSecurityGroups)
	excluded, _ := json.Marshal(t.ExcludedSecurityGroups)
	props, _ := json.Marshal(t.Properties)
	conflict := `DO UPDATE SET
		title = excluded.title, enabled = excluded.enabled,
		can_specify_security_groups = excluded.can_specify_security_groups,
		tenant_setting_group = excluded.tenant_setting_group,
		delegate_to_capacity = excluded.delegate_to_capacity,
		delegate_to_domain = excluded.delegate_to_domain,
		delegate_to_workspace = excluded.delegate_to_workspace,
		enabled_groups_json = excluded.enabled_groups_json,
		excluded_groups_json = excluded.excluded_groups_json,
		properties_json = excluded.properties_json`
	if keepExisting {
		conflict = "DO NOTHING"
	}
	_, err := s.db.Exec(`
INSERT INTO tenant_settings
  (setting_name, title, enabled, can_specify_security_groups, tenant_setting_group,
   delegate_to_capacity, delegate_to_domain, delegate_to_workspace,
   enabled_groups_json, excluded_groups_json, properties_json)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(setting_name) `+conflict,
		t.SettingName, t.Title, t.Enabled, t.CanSpecifySecurityGroups, t.TenantSettingGroup,
		t.DelegateToCapacity, t.DelegateToDomain, t.DelegateToWorkspace,
		string(enabled), string(excluded), string(props))
	return err
}

// UpdateTenantSetting persists a changed setting.
func (s *Store) UpdateTenantSetting(t *TenantSetting) error { return s.upsertTenantSetting(t, false) }

func scanTenantSetting(scan func(...any) error) (*TenantSetting, error) {
	t := &TenantSetting{}
	var enabled, excluded, props string
	if err := scan(&t.SettingName, &t.Title, &t.Enabled, &t.CanSpecifySecurityGroups,
		&t.TenantSettingGroup, &t.DelegateToCapacity, &t.DelegateToDomain, &t.DelegateToWorkspace,
		&enabled, &excluded, &props); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(enabled), &t.EnabledSecurityGroups)
	_ = json.Unmarshal([]byte(excluded), &t.ExcludedSecurityGroups)
	_ = json.Unmarshal([]byte(props), &t.Properties)
	return t, nil
}

const tenantSettingCols = `setting_name, title, enabled, can_specify_security_groups,
	tenant_setting_group, delegate_to_capacity, delegate_to_domain, delegate_to_workspace,
	enabled_groups_json, excluded_groups_json, properties_json`

// GetTenantSetting fetches one setting by its settingName.
func (s *Store) GetTenantSetting(name string) (*TenantSetting, error) {
	row := s.db.QueryRow(`SELECT `+tenantSettingCols+` FROM tenant_settings WHERE setting_name = ?`, name)
	t, err := scanTenantSetting(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ListTenantSettings returns every setting, ordered by group then name so
// pagination is stable.
func (s *Store) ListTenantSettings() ([]*TenantSetting, error) {
	rows, err := s.db.Query(`SELECT ` + tenantSettingCols +
		` FROM tenant_settings ORDER BY tenant_setting_group, setting_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TenantSetting{}
	for rows.Next() {
		t, err := scanTenantSetting(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
