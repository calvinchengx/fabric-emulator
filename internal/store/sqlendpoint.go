package store

import (
	"errors"
	"strings"
)

// A lakehouse's SQL analytics endpoint and its data access mode (docs/60).

// PropParentLakehouse links a SQLEndpoint item back to the lakehouse it serves,
// the same way a KQLDatabase names its Eventhouse.
const PropParentLakehouse = "parentLakehouseItemId"

// PropDataAccessMode is the SQL analytics endpoint property holding its data
// access mode. Absent is delegated identity: "Newly created SQL analytics
// endpoints start in delegated identity access mode by default."
const PropDataAccessMode = "dataAccessMode"

// The two data access modes of a SQL analytics endpoint.
const (
	// AccessModeDelegated: "the SQL analytics endpoint connects to OneLake by
	// using the identity of the workspace or item owner, and security is
	// governed exclusively by SQL permissions".
	AccessModeDelegated = "DelegatedIdentity"
	// AccessModeUserIdentity: "read access is governed entirely by the security
	// rules defined within OneLake".
	AccessModeUserIdentity = "UserIdentity"
)

// NormalizeAccessMode returns the canonical spelling of a data access mode, or
// "" for a value that is neither.
func NormalizeAccessMode(v string) string {
	for _, m := range []string{AccessModeDelegated, AccessModeUserIdentity} {
		if strings.EqualFold(v, m) {
			return m
		}
	}
	return ""
}

// SQLEndpointOf returns the SQLEndpoint item serving a lakehouse, or
// ErrNotFound when it has none.
func (s *Store) SQLEndpointOf(lakehouse *Item) (*Item, error) {
	ep, _, err := s.sqlEndpointOf(lakehouse)
	return ep, err
}

func (s *Store) sqlEndpointOf(lakehouse *Item) (*Item, map[string]string, error) {
	eps, err := s.ListItems(lakehouse.WorkspaceID, "SQLEndpoint")
	if err != nil {
		return nil, nil, err
	}
	for _, ep := range eps {
		props, err := s.ItemProperties(ep.ID)
		if err != nil {
			return nil, nil, err
		}
		if props[PropParentLakehouse] == lakehouse.ID {
			return ep, props, nil
		}
	}
	return nil, nil, ErrNotFound
}

// DataAccessMode is the mode governing a lakehouse's SQL analytics endpoint:
// delegated unless its endpoint was switched. A lakehouse whose endpoint item
// does not exist yet has had no chance to be switched.
func (s *Store) DataAccessMode(lakehouse *Item) (string, error) {
	_, props, err := s.sqlEndpointOf(lakehouse)
	if errors.Is(err, ErrNotFound) {
		return AccessModeDelegated, nil
	}
	if err != nil {
		return "", err
	}
	if m := NormalizeAccessMode(props[PropDataAccessMode]); m != "" {
		return m, nil
	}
	return AccessModeDelegated, nil
}
