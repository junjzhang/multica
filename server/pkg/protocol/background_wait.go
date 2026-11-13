package protocol

import "time"

// WaitAddress is resolved from an authenticated external session before execution.
type WaitAddress struct {
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id"`
	SessionID   string `json:"session_id"`
	DaemonID    string `json:"daemon_id"`
	TaskID      string `json:"task_id,omitempty"`
	RuntimeID   string `json:"runtime_id,omitempty"`
	Provider    string `json:"provider,omitempty"`
	WorkDir     string `json:"work_dir,omitempty"`
	IssueID     string `json:"issue_id,omitempty"`
}

type WaitStart struct {
	Description string    `json:"description,omitempty"`
	IssueRef    string    `json:"issue_ref,omitempty"`
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	SessionRef  string    `json:"session_ref"`
	TaskID      string    `json:"task_id,omitempty"`
	Argv        []string  `json:"argv"`
	Cwd         string    `json:"cwd"`
	Deadline    time.Time `json:"deadline"`
}

// ManagedWait is the task's durable continuation obligation. Command arguments
// and credentials stay in the daemon; accepted returns remain on the same task.
type ManagedWait struct {
	ID          string      `json:"id"`
	Generation  string      `json:"generation"`
	Address     WaitAddress `json:"address"`
	RequestHash string      `json:"request_hash"`
	Deadline    time.Time   `json:"deadline"`
	Suspended   bool        `json:"suspended"`
	Consumed    bool        `json:"consumed"`
	DeliveryID  string      `json:"delivery_id,omitempty"`
	Result      *WaitResult `json:"result,omitempty"`
}

type WaitAuthorization struct {
	IssueRef    string    `json:"issue_ref,omitempty"`
	DaemonID    string    `json:"daemon_id"`
	ID          string    `json:"id,omitempty"`
	RequestHash string    `json:"request_hash,omitempty"`
	Deadline    time.Time `json:"deadline,omitempty"`
}

const DaemonCapabilityManagedWaitV1 = "managed_background_wait_v1"

// WaitResult contains no command output: tools may print credentials. Logs stay local.
type WaitResult struct {
	ID         string      `json:"id"`
	Address    WaitAddress `json:"address"`
	State      string      `json:"state"`
	ExitCode   *int        `json:"exit_code,omitempty"`
	FinishedAt time.Time   `json:"finished_at"`
	LogPath    string      `json:"log_path"`
}

func WaitTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "timed_out", "cancelled", "lost":
		return true
	default:
		return false
	}
}

type WaitStatus struct {
	Description string     `json:"description,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	Consumed    bool       `json:"consumed,omitempty"`
	WaitResult
	Deadline    time.Time `json:"deadline"`
	DeliveryID  string    `json:"delivery_id,omitempty"`
	NotifyError string    `json:"notify_error,omitempty"`
}

// WaitEndpoint is a private local capability, never a server API response.
type WaitEndpoint struct {
	URL      string `json:"url"`
	Secret   string `json:"secret"`
	DaemonID string `json:"daemon_id"`
}

// TaskTurn fences replayed callbacks when one logical task resumes across turns.
type TaskTurn struct {
	Generation   string   `json:"turn_generation,omitempty"`
	KnownWaitIDs []string `json:"known_wait_ids"`
}

// WaitListItem deliberately excludes argv, cwd and local log paths.
type WaitListItem struct {
	ID          string             `json:"id"`
	Address     WaitAddress        `json:"address"`
	State       string             `json:"state"`
	Description string             `json:"description,omitempty"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	FinishedAt  *time.Time         `json:"finished_at,omitempty"`
	Deadline    time.Time          `json:"deadline"`
	ExitCode    *int               `json:"exit_code,omitempty"`
	NotifyError string             `json:"notify_error,omitempty"`
	Delivery    WaitDeliveryStatus `json:"delivery"`
}

type WaitDeliveryStatus struct {
	Known            bool   `json:"known"`
	Enqueued         bool   `json:"enqueued"`
	DeliveryID       string `json:"delivery_id,omitempty"`
	DeliveryStatus   string `json:"delivery_status,omitempty"`
	ProcessingStatus string `json:"processing_status,omitempty"`
}

type WaitLookup struct {
	ID      string      `json:"id"`
	Address WaitAddress `json:"address"`
}

type WaitDeliveryRequest struct {
	Waits []WaitLookup `json:"waits"`
}

type WaitDeliveryResponse struct {
	Statuses   map[string]WaitDeliveryStatus `json:"statuses"`
	ObservedAt time.Time                     `json:"observed_at"`
}

type WaitList struct {
	Items              []WaitListItem `json:"items"`
	SnapshotVersion    string         `json:"snapshot_version"`
	ObservedAt         time.Time      `json:"observed_at"`
	DeliveryObservedAt *time.Time     `json:"delivery_observed_at,omitempty"`
	NextCursor         string         `json:"next_cursor,omitempty"`
}
