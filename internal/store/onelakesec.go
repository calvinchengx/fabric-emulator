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
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
//
// The ROLE is read leniently — it carries fields this layer has no use for
// (`id`, `kind`) and more may follow. Its decision rules are not: they are where
// restrictions live, and they are decoded strictly by parseDecisionRule.
type dataAccessRole struct {
	Name          string            `json:"name"`
	DecisionRules []json.RawMessage `json:"decisionRules"`
	Members       struct {
		MicrosoftEntraMembers []struct {
			ObjectID string `json:"objectId"`
		} `json:"microsoftEntraMembers"`
		FabricItemMembers []struct {
			SourcePath string   `json:"sourcePath"`
			ItemAccess []string `json:"itemAccess"`
		} `json:"fabricItemMembers"`
	} `json:"members"`
}

// decisionRule, constraints, rowConstraint and columnConstraint are the
// documented shapes (REST reference, Create Or Update Data Access Roles), and
// every one of them is decoded with unknown fields DISALLOWED.
//
// WHY STRICT, AND WHY HERE. Row and column security arrive inside
// `decisionRules[].constraints`. This layer used to read `rows` and `columns` as
// flat fields on the rule — a shape that appears nowhere in the reference, and
// is the principalAccess RESPONSE's shape rather than the authoring payload's.
// A policy written the documented way therefore had its constraints silently
// ignored, and a narrowed principal read the whole table. Nothing failed: an
// unread field is invisible. The durable fix is not to read the right fields
// but to refuse to guess at any field — a restriction this parser does not
// understand must deny, never grant.
type decisionRule struct {
	Effect     string `json:"effect"`
	Permission []struct {
		AttributeName          string   `json:"attributeName"`
		AttributeValueIncluded []string `json:"attributeValueIncludedIn"`
	} `json:"permission"`
	Constraints json.RawMessage `json:"constraints"`
}

type constraints struct {
	Rows    []json.RawMessage `json:"rows"`
	Columns []json.RawMessage `json:"columns"`
}

type rowConstraint struct {
	TablePath string `json:"tablePath"`
	Value     string `json:"value"`
}

type columnConstraint struct {
	TablePath    string   `json:"tablePath"`
	ColumnNames  []string `json:"columnNames"`
	ColumnEffect string   `json:"columnEffect"`
	ColumnAction []string `json:"columnAction"`
}

// decodeStrict decodes raw into v, failing on any field v does not declare.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// parseDecisionRule projects one documented rule into the evaluator's type, and
// reports false for anything it cannot read with certainty.
//
// FALSE DROPS THE RULE, AND DROPPING IS ALWAYS SAFE. A constraint narrows only
// the rule it is written in, so removing the whole rule can take access away but
// never add it: whatever else the principal holds, they held it without this
// rule too. The alternative — keep the grant and skip the constraint we could
// not read — is exactly the over-grant this parser exists to prevent.
func parseDecisionRule(raw json.RawMessage) (onelakesec.DecisionRule, bool) {
	var dr decisionRule
	if decodeStrict(raw, &dr) != nil {
		return onelakesec.DecisionRule{}, false
	}
	rule := onelakesec.DecisionRule{Effect: onelakesec.Effect(dr.Effect)}
	// The permission list is attribute/value pairs, not fields: Path carries
	// the scope and Action carries what is granted. The reference defines no
	// other attribute, and one we cannot name might be a restriction.
	for _, perm := range dr.Permission {
		switch perm.AttributeName {
		case "Path":
			rule.Paths = append(rule.Paths, perm.AttributeValueIncluded...)
		case "Action":
			rule.Actions = append(rule.Actions, perm.AttributeValueIncluded...)
		default:
			return onelakesec.DecisionRule{}, false
		}
	}
	if len(dr.Constraints) == 0 || string(dr.Constraints) == "null" {
		return rule, true
	}
	cs, ok := parseConstraints(dr.Constraints)
	if !ok {
		return onelakesec.DecisionRule{}, false
	}
	rule.Constraints = cs
	return rule, true
}

// parseConstraints merges a rule's row and column constraints by table.
//
// Every refusal below is a constraint whose meaning is not certain, so the rule
// is dropped rather than read one way or the other:
//
//   - an empty `tablePath` or row `value` constrains nothing nameable;
//   - `columnNames: []` would mean "no columns", which the evaluator's nil
//     cannot say — and reading it as nil would mean "all columns";
//   - a `columnEffect` other than Permit or a `columnAction` other than Read is
//     outside the only values the reference allows;
//   - two row constraints, or two column constraints, on one table in one rule
//     leave open whether they union or intersect.
func parseConstraints(raw json.RawMessage) ([]onelakesec.Constraint, bool) {
	var c constraints
	if decodeStrict(raw, &c) != nil {
		return nil, false
	}
	byTable := map[string]*onelakesec.Constraint{}
	var order []string
	at := func(tablePath string) *onelakesec.Constraint {
		key := strings.ToLower(strings.Trim(tablePath, "/"))
		if byTable[key] == nil {
			byTable[key] = &onelakesec.Constraint{Table: tablePath}
			order = append(order, key)
		}
		return byTable[key]
	}
	rowsSeen, colsSeen := map[string]bool{}, map[string]bool{}

	for _, r := range c.Rows {
		var rc rowConstraint
		if decodeStrict(r, &rc) != nil || strings.Trim(rc.TablePath, "/ ") == "" || strings.TrimSpace(rc.Value) == "" {
			return nil, false
		}
		key := strings.ToLower(strings.Trim(rc.TablePath, "/"))
		if rowsSeen[key] {
			return nil, false
		}
		rowsSeen[key] = true
		at(rc.TablePath).Rows = rc.Value
	}
	for _, col := range c.Columns {
		var cc columnConstraint
		if decodeStrict(col, &cc) != nil || strings.Trim(cc.TablePath, "/ ") == "" ||
			len(cc.ColumnNames) == 0 || cc.ColumnEffect != "Permit" || !onlyRead(cc.ColumnAction) {
			return nil, false
		}
		key := strings.ToLower(strings.Trim(cc.TablePath, "/"))
		if colsSeen[key] {
			return nil, false
		}
		colsSeen[key] = true
		target := at(cc.TablePath)
		// `*` is "all columns in the table", which is the evaluator's nil.
		all := false
		for _, name := range cc.ColumnNames {
			if name == "*" {
				all = true
			}
		}
		if !all {
			target.Columns = cc.ColumnNames
		}
	}

	sort.Strings(order)
	out := make([]onelakesec.Constraint, 0, len(order))
	for _, key := range order {
		out = append(out, *byTable[key])
	}
	return out, true
}

// onlyRead reports whether actions is non-empty and grants Read and nothing
// else — the one value the reference allows for a column action.
func onlyRead(actions []string) bool {
	if len(actions) == 0 {
		return false
	}
	for _, a := range actions {
		if a != "Read" {
			return false
		}
	}
	return true
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
// model's default. The same holds one level down for a decision rule this layer
// cannot read with certainty; see parseDecisionRule.
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
		for _, raw := range d.DecisionRules {
			if rule, ok := parseDecisionRule(raw); ok {
				role.DecisionRules = append(role.DecisionRules, rule)
			}
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
