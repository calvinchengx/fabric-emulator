package store

// Workspace is a Fabric workspace (the container everything hangs off).
type Workspace struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
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
type Item struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId"`
	Type        string `json:"type"` // Notebook, Lakehouse, Warehouse, …
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
	CreatedAt   int64  `json:"-"`
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
