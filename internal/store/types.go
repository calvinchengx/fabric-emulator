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

// PercentCompleteAt reports progress the way a tenant does, which is NOT a
// progress bar. nil means "no figure", i.e. a JSON null.
//
// MEASURED against real Fabric on 2026-08-11 by local_0cdd48fd, twice: a
// Warehouse create that succeeded, and a SemanticModel create that failed
// asynchronously. percentComplete is `100` ON SUCCESS AND NULL EVERYWHERE ELSE
// — running, and failed. It never takes an intermediate value.
//
// The failed case was my inference and it was WRONG. I argued 100 because the
// work stopped progressing either way and `status` is the field that says
// which. The tenant does not do that, and the reason it does not is the better
// argument: a client branching on `percentComplete == 100` would read a failure
// as a completion. A sound-sounding inference in the fabricated-success
// direction is exactly the kind this repo keeps having to undo.
//
// The first version of this function interpolated between CreatedAt and
// CompleteAt and returned figures like 47. Every one of those is a number real
// Fabric does not send, and the comment defending it — "COMPUTED, not faked" —
// was answering the wrong question: the value was honestly derived from this
// emulator's clock and still wrong, because the tenant does not publish that
// quantity at all. A client rendering a bar would animate smoothly here and sit
// at null against Fabric, and its null-handling branch would never once execute
// locally. That is the emulator-green/tenant-broken direction.
func (o Operation) PercentCompleteAt(now int64) *int {
	if o.StatusAt(now) != OpSucceeded {
		return nil
	}
	done := 100
	return &done
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
