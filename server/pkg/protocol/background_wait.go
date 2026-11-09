package protocol

import "time"

// WaitAddress is resolved from an authenticated external session before execution.
type WaitAddress struct {
	WorkspaceID string `json:"workspace_id"`
	AgentID     string `json:"agent_id"`
	SessionID   string `json:"session_id"`
	DaemonID    string `json:"daemon_id"`
}

type WaitStart struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	SessionRef  string    `json:"session_ref"`
	Argv        []string  `json:"argv"`
	Cwd         string    `json:"cwd"`
	Deadline    time.Time `json:"deadline"`
}

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
