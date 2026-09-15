package semanticmodel

import "strings"

// Admits reports whether the role names a principal, identified by its Entra
// object id and its UPN.
//
// A member matches by memberId against the object id, or by memberName against
// the UPN — the service resolves a member added by name, and `memberName` is the
// UPN a role was given. What does NOT match, deliberately:
//
//   - A GROUP. Role membership through a security group is how the service is
//     most often configured, and group membership is not modelled here; a group
//     member admits nobody rather than everybody. Fail closed, and stated.
//   - A member from another identity provider. Only Entra identities reach this
//     emulator, so a Windows member names nobody who could be calling.
//   - An empty identity on either side, which would otherwise match an empty
//     memberName to a principal with no UPN — the zero-value bug.
func (r Role) Admits(objectID, upn string) bool {
	for _, mb := range r.Members {
		if strings.EqualFold(mb.Type, "group") {
			continue
		}
		if mb.IdentityProvider != "" && !strings.EqualFold(mb.IdentityProvider, "AzureAD") {
			continue
		}
		if objectID != "" && strings.EqualFold(mb.ID, objectID) {
			return true
		}
		if upn != "" && strings.EqualFold(mb.Name, upn) {
			return true
		}
	}
	return false
}

// RolesFor is every role in the model that admits the principal. Roles are
// additive, so the caller unions what they grant; an empty result on a model
// with roles is a principal who "typically see[s] no data".
func (m *Model) RolesFor(objectID, upn string) []Role {
	var out []Role
	for _, r := range m.Roles {
		if r.Admits(objectID, upn) {
			out = append(out, r)
		}
	}
	return out
}
