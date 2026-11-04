package protocol

// AcceptedReturn identifies a frozen automatic-return relationship. It carries
// no session credentials, filesystem paths, or mutable routing lookup.
type AcceptedReturn struct {
	ReturnID          string `json:"return_id"`
	RecipientAgentID  string `json:"recipient_agent_id"`
	Kind              string `json:"kind"`
	SourceTaskID      string `json:"source_task_id,omitempty"`
	ExternalSessionID string `json:"external_session_id,omitempty"`
}
