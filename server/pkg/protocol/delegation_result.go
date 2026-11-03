package protocol

// DelegationResultTurn addresses one outcome-handling turn in an accepted
// original session. The task's ordinary scope fields still name that source's
// issue/chat/autopilot; DelegatedIssueID names the work whose outcome is ready.
type DelegationResultTurn struct {
	ResultTaskID       string `json:"result_task_id"`
	DelegationReturnID string `json:"delegation_return_id"`
	SourceTaskID       string `json:"source_task_id"`
	ScopeTaskID        string `json:"scope_task_id,omitempty"`
	DelegatedIssueID   string `json:"delegated_issue_id"`
	Outcome            string `json:"outcome"`
	RuntimeID          string `json:"runtime_id"`
	Provider           string `json:"provider"`
	SessionID          string `json:"session_id"`
	WorkDir            string `json:"work_dir"`
}

type ManagedResourceAddress struct {
	SourceTaskID string `json:"source_task_id"`
	WorkspaceID  string `json:"workspace_id"`
	AgentID      string `json:"agent_id"`
	ScopeType    string `json:"scope_type"`
	ScopeID      string `json:"scope_id"`
	RuntimeID    string `json:"runtime_id"`
	Provider     string `json:"provider"`
	SessionID    string `json:"session_id"`
	WorkDir      string `json:"work_dir"`
}

// ResourceReleaseRequest covers task finalization, an owned directory tree,
// or an existing provider-store group. ResourcePath is the actual directory
// being considered for removal; WorkDir remains the execution address.
type ResourceReleaseRequest struct {
	DaemonID     string `json:"daemon_id,omitempty"`
	Kind         string `json:"kind"`
	WorkspaceID  string `json:"workspace_id,omitempty"`
	SourceTaskID string `json:"source_task_id,omitempty"`
	RuntimeID    string `json:"runtime_id"`
	Provider     string `json:"provider"`
	AgentID      string `json:"agent_id,omitempty"`
	ScopeType    string `json:"scope_type,omitempty"`
	ScopeID      string `json:"scope_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	WorkDir      string `json:"work_dir,omitempty"`
	ResourcePath string `json:"resource_path,omitempty"`
}

const DaemonCapabilityDelegationResultsV1 = "delegation_results_v1"

const (
	DelegationResultInputUnknown = "delegation_result_input_unknown"
	DelegationResultUnavailable  = "delegation_result_unavailable"
)

type ResourceReleaseReceipt struct {
	ResourceReleaseRequest
	DurableWorkDir string `json:"durable_work_dir"`
}

type ResourceReleaseAcknowledgement struct {
	Recorded       bool   `json:"recorded"`
	DurableWorkDir string `json:"durable_work_dir,omitempty"`
}

// ResourceReleaseDecision is permission to release daemon-owned resources,
// never permission to remove a user-owned directory. Missing/unknown responses
// do not grant release.
type ResourceReleaseDecision struct {
	AdmissionClosed    bool                     `json:"admission_closed"`
	ControlRequired    bool                     `json:"resource_control_required"`
	ReleaseAllowed     bool                     `json:"release_allowed"`
	Reason             string                   `json:"reason"`
	ProtectedResources []ManagedResourceAddress `json:"protected_resources"`
}

// SessionPinRequest confirms the current provider attempt. A non-empty
// NextGeneration invalidates the old attempt before an ordinary fresh retry.
// Retrying the same transition acknowledges the new generation without
// invalidating a pin already confirmed for it.
type SessionPinRequest struct {
	SessionID      string `json:"session_id,omitempty"`
	WorkDir        string `json:"work_dir,omitempty"`
	Generation     string `json:"generation,omitempty"`
	NextGeneration string `json:"next_generation,omitempty"`
}

type SessionPinAcknowledgement struct {
	Generation string `json:"generation"`
}
