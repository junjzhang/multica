package daemon

// Error codes are part of the frozen Paseo CLI contract. They are decoded into
// delivery.HostError by the transport; there is no separate consumer taxonomy.
const (
	mailboxErrorAgentBusy      = "agent_busy"
	mailboxErrorOutcomeUnknown = "agent_request_outcome_unknown"
	mailboxErrorUnsupported    = "paseo_dependency_unsupported"
)
