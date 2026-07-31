package store

import (
	"database/sql"
	"errors"
)

// Domains are the tenant-level governance grouping Fabric exposes through the
// admin APIs (fabric-docs governance/domains.md): a domain groups workspaces,
// a subdomain refines that grouping one level deeper, and two domain-scoped
// roles sit beneath the tenant admin.
//
// Modelled here: the domain tree, workspace assignment, and role assignment.
// Not modelled: settings delegation and domain images, which are admin-portal
// presentation rather than API contract.
type Domain struct {
	ID                string `json:"id"`
	DisplayName       string `json:"displayName"`
	Description       string `json:"description"`
	ParentDomainID    string `json:"parentDomainId,omitempty"`
	ContributorsScope string `json:"contributorsScope"`
}

// Domain role names. A subdomain has no admins of its own — the docs are
// explicit that its admins are its parent's — so DomainRoleAdmin is only ever
// stored against a root domain.
const (
	DomainRoleAdmin       = "Admins"
	DomainRoleContributor = "Contributors"
)

// ContributorsScope values: who may assign workspaces to the domain.
const (
	ContributorsScopeAdminsOnly     = "AdminsOnly"
	ContributorsScopeAllTenant      = "AllTenant"
	ContributorsScopeSpecificUsers  = "SpecificUsers"
	defaultContributorsScopeInitial = ContributorsScopeAllTenant
)

// ErrSubdomainDepth rejects a subdomain of a subdomain: Fabric's hierarchy is
// exactly two levels (domain → subdomain).
var ErrSubdomainDepth = errors.New("a subdomain cannot have subdomains")

// nullableParent maps a root domain's empty parent onto SQL NULL, which is
// what the foreign key requires.
func nullableParent(id string) any {
	if id == "" {
		return nil
	}
	return id
}

// CreateDomain inserts a domain, or a subdomain when ParentDomainID is set.
func (s *Store) CreateDomain(d *Domain) error {
	if d.ID == "" {
		d.ID = NewID()
	}
	if d.ContributorsScope == "" {
		d.ContributorsScope = defaultContributorsScopeInitial
	}
	if d.ParentDomainID != "" {
		parent, err := s.GetDomain(d.ParentDomainID)
		if err != nil {
			return err
		}
		if parent.ParentDomainID != "" {
			return ErrSubdomainDepth
		}
	}
	_, err := s.db.Exec(
		`INSERT INTO domains (id, display_name, description, parent_domain_id, contributors_scope)
		 VALUES (?,?,?,?,?)`,
		d.ID, d.DisplayName, d.Description, nullableParent(d.ParentDomainID), d.ContributorsScope)
	return nameConflict(err)
}

// GetDomain fetches one domain.
func (s *Store) GetDomain(id string) (*Domain, error) {
	d := &Domain{}
	var parent sql.NullString
	err := s.db.QueryRow(
		`SELECT id, display_name, description, parent_domain_id, contributors_scope
		 FROM domains WHERE id = ?`, id).
		Scan(&d.ID, &d.DisplayName, &d.Description, &parent, &d.ContributorsScope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	d.ParentDomainID = parent.String
	return d, err
}

// ListDomains returns every domain. nonEmptyOnly drops those with no
// workspaces assigned, which is the filter the list API documents.
func (s *Store) ListDomains(nonEmptyOnly bool) ([]*Domain, error) {
	q := `SELECT id, display_name, description, parent_domain_id, contributors_scope
	      FROM domains`
	if nonEmptyOnly {
		q += ` WHERE id IN (SELECT domain_id FROM domain_workspaces)`
	}
	rows, err := s.db.Query(q + ` ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Domain{}
	for rows.Next() {
		d := &Domain{}
		var parent sql.NullString
		if err := rows.Scan(&d.ID, &d.DisplayName, &d.Description,
			&parent, &d.ContributorsScope); err != nil {
			return nil, err
		}
		d.ParentDomainID = parent.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDomain applies the non-empty fields of patch.
func (s *Store) UpdateDomain(id string, patch *Domain) (*Domain, error) {
	d, err := s.GetDomain(id)
	if err != nil {
		return nil, err
	}
	if patch.DisplayName != "" {
		d.DisplayName = patch.DisplayName
	}
	if patch.Description != "" {
		d.Description = patch.Description
	}
	if patch.ContributorsScope != "" {
		d.ContributorsScope = patch.ContributorsScope
	}
	if _, err := s.db.Exec(
		`UPDATE domains SET display_name = ?, description = ?, contributors_scope = ? WHERE id = ?`,
		d.DisplayName, d.Description, d.ContributorsScope, id); err != nil {
		return nil, nameConflict(err)
	}
	return d, nil
}

// DeleteDomain removes a domain. Its subdomains, workspace assignments and
// role assignments go with it (ON DELETE CASCADE), matching the admin
// portal's behaviour of deleting a domain and everything scoped to it.
func (s *Store) DeleteDomain(id string) error {
	res, err := s.db.Exec(`DELETE FROM domains WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AssignWorkspaces assigns workspaces to a domain. A workspace belongs to at
// most one domain, so re-assigning moves it. Unknown workspace ids are
// rejected rather than silently stored.
func (s *Store) AssignWorkspaces(domainID string, workspaceIDs []string) error {
	if _, err := s.GetDomain(domainID); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, wid := range workspaceIDs {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(1) FROM workspaces WHERE id = ?`, wid).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(
			`INSERT INTO domain_workspaces (workspace_id, domain_id) VALUES (?,?)
			 ON CONFLICT(workspace_id) DO UPDATE SET domain_id = excluded.domain_id`,
			wid, domainID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnassignWorkspaces removes the given workspaces from a domain. Ids that are
// not assigned to it are ignored, so the call is idempotent.
func (s *Store) UnassignWorkspaces(domainID string, workspaceIDs []string) error {
	if _, err := s.GetDomain(domainID); err != nil {
		return err
	}
	for _, wid := range workspaceIDs {
		if _, err := s.db.Exec(
			`DELETE FROM domain_workspaces WHERE domain_id = ? AND workspace_id = ?`,
			domainID, wid); err != nil {
			return err
		}
	}
	return nil
}

// UnassignAllWorkspaces empties a domain.
func (s *Store) UnassignAllWorkspaces(domainID string) error {
	if _, err := s.GetDomain(domainID); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM domain_workspaces WHERE domain_id = ?`, domainID)
	return err
}

// DomainWorkspaces lists the workspaces assigned to a domain.
func (s *Store) DomainWorkspaces(domainID string) ([]*Workspace, error) {
	if _, err := s.GetDomain(domainID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT w.id, w.display_name, w.description, w.capacity_id
		 FROM workspaces w JOIN domain_workspaces dw ON dw.workspace_id = w.id
		 WHERE dw.domain_id = ? ORDER BY w.rowid`, domainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Workspace{}
	for rows.Next() {
		w := &Workspace{Type: "Workspace"}
		if err := rows.Scan(&w.ID, &w.DisplayName, &w.Description, &w.CapacityID); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// AssignDomainRole grants principals a domain role in bulk.
func (s *Store) AssignDomainRole(domainID, role string, principals []Principal) error {
	if _, err := s.GetDomain(domainID); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range principals {
		if _, err := tx.Exec(
			`INSERT INTO domain_role_assignments (domain_id, principal_id, principal_type, role)
			 VALUES (?,?,?,?)
			 ON CONFLICT(domain_id, principal_id, role) DO NOTHING`,
			domainID, p.ID, p.Type, role); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UnassignDomainRole revokes a domain role from principals in bulk.
func (s *Store) UnassignDomainRole(domainID, role string, principals []Principal) error {
	if _, err := s.GetDomain(domainID); err != nil {
		return err
	}
	for _, p := range principals {
		if _, err := s.db.Exec(
			`DELETE FROM domain_role_assignments
			 WHERE domain_id = ? AND principal_id = ? AND role = ?`,
			domainID, p.ID, role); err != nil {
			return err
		}
	}
	return nil
}

// DomainRoleAssignments lists a domain's role assignments.
func (s *Store) DomainRoleAssignments(domainID string) ([]RoleAssignment, error) {
	if _, err := s.GetDomain(domainID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT principal_id, principal_type, role FROM domain_role_assignments
		 WHERE domain_id = ? ORDER BY rowid`, domainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoleAssignment{}
	for rows.Next() {
		var ra RoleAssignment
		if err := rows.Scan(&ra.Principal.ID, &ra.Principal.Type, &ra.Role); err != nil {
			return nil, err
		}
		out = append(out, ra)
	}
	return out, rows.Err()
}

// WorkspaceDomains maps workspace id → domain id for every assigned
// workspace. The admin workspace listing reports each workspace's domainId,
// and doing it in one query avoids a lookup per workspace.
func (s *Store) WorkspaceDomains() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT workspace_id, domain_id FROM domain_workspaces`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var ws, dom string
		if err := rows.Scan(&ws, &dom); err != nil {
			return nil, err
		}
		out[ws] = dom
	}
	return out, rows.Err()
}
