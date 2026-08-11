package store

// Workspace is a Fabric workspace (the container everything hangs off).
// description is always present on the wire (fabric-cicd indexes it), so no
// omitempty.
type Workspace struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Type        string `json:"type"` // always "Workspace"
	CapacityID  string `json:"capacityId,omitempty"`
	CreatedAt   int64  `json:"-"`
}

// Workspace roles, in descending privilege order.
const (
	RoleAdmin       = "Admin"
	RoleMember      = "Member"
	RoleContributor = "Contributor"
	RoleViewer      = "Viewer"
)

// RoleRank orders roles for "equal or lower" grant checks; higher is more
// privileged. Unknown roles rank -1.
func RoleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleMember:
		return 2
	case RoleContributor:
		return 1
	case RoleViewer:
		return 0
	}
	return -1
}

// RoleAssignment grants a principal a role on a workspace.
type RoleAssignment struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"-"`
	Principal   Principal `json:"principal"`
	Role        string    `json:"role"`
}

// Principal identifies a user or service principal in a role assignment.
type Principal struct {
	ID   string `json:"id"`
	Type string `json:"type"` // "User" | "ServicePrincipal"
}

// Item is a generic Fabric item; typed collections alias over this.
// description is always present on the wire, matching real Fabric.
type Item struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	Type        string `json:"type"` // Notebook, Lakehouse, Warehouse, …
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	// FolderID is the workspace folder the item lives in ("" = root). Omitted
	// from JSON when empty, matching Fabric: a root item has no folderId.
	FolderID  string `json:"folderId,omitempty"`
	CreatedAt int64  `json:"-"`
}

// DefinitionPart is one file of an item definition — the CI/CD source format
// (path + base64 payload + payloadType, typically InlineBase64).
type DefinitionPart struct {
	Path        string `json:"path"`
	Payload     string `json:"payload"`
	PayloadType string `json:"payloadType"`
}

// Operation statuses (the LRO state machine).
const (
	OpNotStarted = "NotStarted"
	OpRunning    = "Running"
	OpSucceeded  = "Succeeded"
	OpFailed     = "Failed"
)

// Operation is a long-running operation. Status is *derived on read*: an
// operation whose CompleteAt has passed on the controllable clock reports
// Succeeded (or Failed when FailWith is set) without any background worker —
// deterministic for tests.
type Operation struct {
	ID         string
	Kind       string // e.g. "CreateItem"
	CreatedAt  int64
	CompleteAt int64  // epoch seconds on the emulator clock
	ResultRef  string // e.g. item id the operation produced
	FailWith   string // non-empty forces Failed with this errorCode
}

// PercentCompleteAt derives operation progress from the SAME clock the status
// comes from, so the two can never disagree.
//
// Fabric documents `percentComplete` as an integer 0-100 on every operation
// state, and the emulator returned none. A client rendering progress therefore
// saw nothing locally and a real number on a tenant — the shape of divergence
// that only shows up in production.
//
// It is COMPUTED, not faked: the emulator knows when the operation was created
// and when its clock completes it, so the fraction between them is a real
// answer rather than a plausible-looking constant. A terminal operation is 100
// whether it succeeded or failed — the work stopped progressing either way, and
// the status field is what says which.
func (o Operation) PercentCompleteAt(now int64) int {
	if now >= o.CompleteAt {
		return 100
	}
	span := o.CompleteAt - o.CreatedAt
	if span <= 0 {
		return 0
	}
	done := int((now - o.CreatedAt) * 100 / span)
	if done < 0 {
		return 0
	}
	if done > 100 {
		return 100
	}
	return done
}

// LastUpdatedAt is when this operation's state last CHANGED, which for a
// clock-derived operation is its completion once complete, and its creation
// before that. Reporting `now` instead would make every poll look like a fresh
// update and defeat the field's purpose.
func (o Operation) LastUpdatedAt(now int64) int64 {
	if now >= o.CompleteAt {
		return o.CompleteAt
	}
	return o.CreatedAt
}

// StatusAt derives the wire status at the given clock time.
func (o Operation) StatusAt(now int64) string {
	if now < o.CompleteAt {
		if now == o.CreatedAt {
			return OpNotStarted
		}
		return OpRunning
	}
	if o.FailWith != "" {
		return OpFailed
	}
	return OpSucceeded
}
