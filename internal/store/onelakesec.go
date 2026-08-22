package store

// OneLake security roles, stored per item.
//
// WHAT IS STORED AND WHY IT IS A BLOB. `PUT …/dataAccessRoles` replaces the
// whole role set for an item, and the payload is an open shape: the API
// documents `decisionRules`, `members.microsoftEntraMembers` and
// `members.fabricItemMembers`, and says more may follow. A field we do not read
// is still a field the client sent and expects to read back, so the body is
// round-tripped verbatim and only the parts the evaluator needs are projected
// out. The same choice the Atlas typedefs make, for the same reason.
//
// WHAT IS NOT HERE. No evaluation: that lives in pkg/onelakesec, which takes
// roles as values and has no store handle. This file's whole job is rows in and
// rows out, so the two callers of the evaluator cannot drift apart by one of
// them doing its own lookup.

import (
	"encoding/json"
	"fmt"

	"github.com/calvinchengx/fabric-emulator/pkg/onelakesec"
)

// OneLakeRole is one stored role: its name, the verbatim body, and the parsed
// view the evaluator consumes.
type OneLakeRole struct {
	ItemID string
	Name   string
	Body   json.RawMessage
}

// dataAccessRole is the documented payload shape, as much of it as we read.
type dataAccessRole struct {
	Name          string `json:"name"`
	DecisionRules []struct {
		Effect     string `json:"effect"`
		Permission []struct {
			AttributeName          string   `json:"attributeName"`
			AttributeValueIncluded []string `json:"attributeValueIncludedIn"`
		} `json:"permission"`
		// Row and column narrowing, which the model carries on the rule.
		Rows    string   `json:"rows,omitempty"`
		Columns []string `json:"columns,omitempty"`
	} `json:"decisionRules"`
	Members struct {
		MicrosoftEntraMembers []struct {
			ObjectID string `json:"objectId"`
		} `json:"microsoftEntraMembers"`
		FabricItemMembers []struct {
			SourcePath string   `json:"sourcePath"`
			ItemAccess []string `json:"itemAccess"`
		} `json:"fabricItemMembers"`
	} `json:"members"`
}

// PutOneLakeRoles replaces every role on an item, which is what the PUT verb
// means here: "This API updates role definitions by creating, updating, and
// deleting roles to match the payload you send." A partial write would leave a
// role the caller believes it deleted.
func (s *Store) PutOneLakeRoles(itemID string, roles []OneLakeRole) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM onelake_roles WHERE item_id = ?`, itemID); err != nil {
		return err
	}
	for _, r := range roles {
		if r.Name == "" {
			return fmt.Errorf("onelake role on item %s has no name", itemID)
		}
		if _, err := tx.Exec(
			`INSERT INTO onelake_roles (item_id, name, body) VALUES (?, ?, ?)`,
			itemID, r.Name, string(r.Body)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListOneLakeRoles returns an item's roles, verbatim.
func (s *Store) ListOneLakeRoles(itemID string) ([]OneLakeRole, error) {
	rows, err := s.db.Query(
		`SELECT name, body FROM onelake_roles WHERE item_id = ? ORDER BY name`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OneLakeRole{}
	for rows.Next() {
		r := OneLakeRole{ItemID: itemID}
		var body string
		if err := rows.Scan(&r.Name, &body); err != nil {
			return nil, err
		}
		r.Body = json.RawMessage(body)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EvaluatableRoles projects the stored bodies into the evaluator's value types.
//
// A body that will not parse is SKIPPED rather than failing the read, because
// the alternative is one malformed role making an item unreadable — the failure
// direction this family avoids. It cannot grant anything either way: an
// unparsed role contributes no rules, so the outcome is deny, which is the
// model's default.
func (s *Store) EvaluatableRoles(itemID string) ([]onelakesec.Role, error) {
	stored, err := s.ListOneLakeRoles(itemID)
	if err != nil {
		return nil, err
	}
	out := make([]onelakesec.Role, 0, len(stored))
	for _, r := range stored {
		var d dataAccessRole
		if err := json.Unmarshal(r.Body, &d); err != nil {
			continue
		}
		role := onelakesec.Role{Name: r.Name}
		for _, dr := range d.DecisionRules {
			rule := onelakesec.DecisionRule{
				Effect:  onelakesec.Effect(dr.Effect),
				Rows:    dr.Rows,
				Columns: dr.Columns,
			}
			// The permission list is attribute/value pairs, not fields: Path
			// carries the scope and Action carries what is granted.
			for _, perm := range dr.Permission {
				switch perm.AttributeName {
				case "Path":
					rule.Paths = append(rule.Paths, perm.AttributeValueIncluded...)
				case "Action":
					rule.Actions = append(rule.Actions, perm.AttributeValueIncluded...)
				}
			}
			role.DecisionRules = append(role.DecisionRules, rule)
		}
		for _, m := range d.Members.MicrosoftEntraMembers {
			role.Members.Entra = append(role.Members.Entra, m.ObjectID)
		}
		// fabricItemMembers is how a default role includes everyone holding a
		// permission, without storing them. The item path is not consulted yet:
		// roles are scoped to their own item, so the permissions are what
		// decides membership.
		for _, m := range d.Members.FabricItemMembers {
			role.Members.ItemAccess = append(role.Members.ItemAccess, m.ItemAccess...)
		}
		out = append(out, role)
	}
	return out, nil
}

// DeleteOneLakeRoles drops every role on an item. Item deletion cascades, so
// this exists for the explicit "clear the policy" case.
//
// Deleting nothing is not an error: a DELETE that matches no rows leaves the
// item with no policy, which is what the caller asked for.
func (s *Store) DeleteOneLakeRoles(itemID string) error {
	_, err := s.db.Exec(`DELETE FROM onelake_roles WHERE item_id = ?`, itemID)
	return err
}
