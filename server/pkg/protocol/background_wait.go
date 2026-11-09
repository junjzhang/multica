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
}

type WaitStart struct {
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
	Address     WaitAddress `json:"address"`
	RequestHash string      `json:"request_hash"`
	Deadline    time.Time   `json:"deadline"`
	Suspended   bool        `json:"suspended"`
	Consumed    bool        `json:"consumed"`
	DeliveryID  string      `json:"delivery_id,omitempty"`
	Result      *WaitResult `json:"result,omitempty"`
}

type WaitAuthorization struct {
	DaemonID    string    `json:"daemon_id"`
	ID          string    `json:"id,omitempty"`
	RequestHash string    `json:"request_hash,omitempty"`
	Deadline    time.Time `json:"deadline,omitempty"`
}

type WaitSuspend struct {
	Generation string `json:"generation"`
	SessionID  string `json:"session_id"`
	WorkDir    string `json:"work_dir"`
	BranchName string `json:"branch_name,omitempty"`
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
