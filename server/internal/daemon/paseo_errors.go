package daemon

import "fmt"

// Host delivery failures are classified by the frozen Paseo CLI contract. The
// distinction between a known no-effect outcome and an ambiguous one is the
// difference between a safe retry and a duplicate potentially accepted request.
const (
	mailboxErrorAgentBusy       = "agent_busy"
	mailboxErrorHostUnreachable = "paseo_daemon_unreachable"
	mailboxErrorOutcomeUnknown  = "agent_request_outcome_unknown"
	mailboxErrorUnsupported     = "paseo_dependency_unsupported"
	mailboxErrorDeliveryFailed  = "paseo_delivery_failed"
)

// MailboxDeliveryErrorKind classifies a host delivery outcome.
type MailboxDeliveryErrorKind int

const (
	// MailboxDeliveryFailed is a terminal, known-failed delivery. The session
	// or message needs human attention; replay would not help.
	MailboxDeliveryFailed MailboxDeliveryErrorKind = iota
	// MailboxDeliveryBusy means the host rejected the request without accepting
	// it because the session's current turn is active. Retry later.
	MailboxDeliveryBusy
	// MailboxDeliveryUnavailable means the host could not be reached. Retry later.
	MailboxDeliveryUnavailable
	// MailboxDeliveryUnsupported means the installed host/CLI does not support
	// atomic if-idle delivery. The request was never sent, so it stays queued
	// until the dependency is installed or upgraded. Never fall back to
	// ordinary send.
	MailboxDeliveryUnsupported
	// MailboxDeliveryOutcomeUnknown means the request may have been accepted.
	// It must not be replayed without a host receipt.
	MailboxDeliveryOutcomeUnknown
)

// MailboxDeliveryError carries a typed host failure. Message is safe to send to
// the server and logs: it never contains prompt content or credentials.
type MailboxDeliveryError struct {
	Kind    MailboxDeliveryErrorKind
	Code    string
	Message string
	Err     error
}

func (e *MailboxDeliveryError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code
}

func (e *MailboxDeliveryError) Unwrap() error { return e.Err }
