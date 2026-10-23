package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	// Five claims at a bounded per-delivery timeout stay inside the server's
	// two-minute lease even in the pathological all-timeouts case.
	defaultMailboxClaimLimit   = 5
	defaultMailboxPollInterval = 5 * time.Second

	mailboxErrorAgentBusy       = "agent_busy"
	mailboxErrorHostUnreachable = "paseo_daemon_unreachable"
	mailboxErrorLeaseExpired    = "lease_expired"
	mailboxErrorOutcomeUnknown  = "agent_request_outcome_unknown"
	mailboxErrorUnsupported     = "paseo_dependency_unsupported"
	mailboxErrorDeliveryFailed  = "paseo_delivery_failed"
	mailboxErrorDaemonMismatch  = "daemon_mismatch"
)

// MailboxDeliveryErrorKind classifies a host delivery outcome. The distinction
// between a known no-effect outcome and an ambiguous one is the difference
// between a safe retry and a duplicate potentially accepted request.
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

// MailboxHostTransport delivers one mailbox message to its external session.
// Implementations must be idempotent by messageID and must use the exact
// existing session id; they must never create, import, or resume a session.
type MailboxHostTransport interface {
	Deliver(ctx context.Context, session ExternalSession, messageID, content string) (hostReceipt string, err error)
}

type mailboxDeliveryClient interface {
	ClaimMailbox(ctx context.Context, workspaceID string, limit int) ([]MailboxClaim, error)
	AckMailbox(ctx context.Context, workspaceID, messageID string, ack MailboxAckRequest) (*MailboxMessage, error)
}

type mailboxPendingAck struct {
	WorkspaceID   string
	DeliveryToken string
	Ack           MailboxAckRequest
	// terminal prevents a second host delivery when the prior outcome was
	// already accepted or is ambiguous. The ack itself may still be retried.
	terminal bool
	// Keep a fenced terminal outcome until a new lease allows its acknowledgement.
	awaitingLease bool
}

type mailboxDeliveryLoop struct {
	client       mailboxDeliveryClient
	transport    MailboxHostTransport
	workspaces   func() []string
	daemonID     string
	logger       *slog.Logger
	now          func() time.Time
	claimLimit   int
	pollInterval time.Duration
	pending      map[string]mailboxPendingAck
}

func newMailboxDeliveryLoop(client mailboxDeliveryClient, transport MailboxHostTransport, workspaces func() []string, daemonID string, logger *slog.Logger) *mailboxDeliveryLoop {
	if logger == nil {
		logger = slog.Default()
	}
	if workspaces == nil {
		workspaces = func() []string { return nil }
	}
	return &mailboxDeliveryLoop{
		client:       client,
		transport:    transport,
		workspaces:   workspaces,
		daemonID:     daemonID,
		logger:       logger,
		now:          time.Now,
		claimLimit:   defaultMailboxClaimLimit,
		pollInterval: defaultMailboxPollInterval,
		pending:      make(map[string]mailboxPendingAck),
	}
}

// run is the daemon-owned mailbox delivery loop. It shares the daemon lifetime
// and stops on ctx cancellation; there is no per-session daemon and no second
// persistent queue.
func (m *mailboxDeliveryLoop) run(ctx context.Context) error {
	interval := m.pollInterval
	if interval <= 0 {
		interval = defaultMailboxPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := m.deliverOnce(ctx); err != nil {
			if ctx.Err() != nil {
				m.flushPendingAcksOnStop()
				return ctx.Err()
			}
			return err
		}
		if ctx.Err() != nil {
			m.flushPendingAcksOnStop()
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			m.flushPendingAcksOnStop()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// deliverOnce retries pending acks before claiming new work. Acks are retried
// separately from delivery so a delayed ack never causes a second host send.
func (m *mailboxDeliveryLoop) deliverOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for messageID, pending := range m.pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if pending.awaitingLease {
			continue
		}
		m.retryPendingAck(ctx, messageID)
	}

	for _, workspaceID := range m.workspaces() {
		if workspaceID == "" {
			continue
		}
		claims, err := m.client.ClaimMailbox(ctx, workspaceID, m.claimLimit)
		if err != nil {
			if ctx.Err() == nil {
				// A workspace-scoped 404 (removed workspace or lost membership)
				// or a server without the endpoint must not stop delivery for
				// every other workspace.
				if mailboxStatus(err) == http.StatusNotFound {
					m.logger.Debug("mailbox claim unavailable for workspace; skipping", "workspace_id", workspaceID, "error", err)
				} else {
					m.logger.Warn("mailbox claim failed", "workspace_id", workspaceID, "error", err)
				}
			}
			continue
		}
		for _, claim := range claims {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := m.handleClaim(ctx, claim); err != nil {
				m.logger.Warn("mailbox delivery failed",
					"workspace_id", claim.WorkspaceID,
					"message_id", claim.Message.ID,
					"recipient_agent_id", claim.Message.RecipientAgentID,
					"error", err)
			}
		}
	}
	return nil
}

func (m *mailboxDeliveryLoop) handleClaim(ctx context.Context, claim MailboxClaim) error {
	messageID := claim.Message.ID
	if messageID == "" {
		return errors.New("mailbox claim has no message id")
	}
	if claim.DeliveryToken == "" {
		return errors.New("mailbox claim has no delivery token")
	}
	if claim.ExternalSession.SessionID == "" {
		return errors.New("mailbox claim has no external session id")
	}

	if pending, ok := m.pending[messageID]; ok {
		if pending.terminal {
			if pending.DeliveryToken == claim.DeliveryToken {
				// The outcome is already decided and its ack is still pending.
				return nil
			}
			// A later lease supersedes the pending one. Never send the stale
			// token, and never deliver again: a decided or ambiguous outcome is
			// final. Re-ack it under the new lease instead.
			pending.WorkspaceID = claim.WorkspaceID
			pending.DeliveryToken = claim.DeliveryToken
			pending.Ack.DeliveryToken = claim.DeliveryToken
			pending.awaitingLease = false
			m.pending[messageID] = pending
			m.retryPendingAck(ctx, messageID)
			return nil
		}
		// Non-terminal outcomes are known no-effect, so a new lease may safely
		// attempt delivery again.
		delete(m.pending, messageID)
	}

	if claim.ExternalSession.DaemonID != m.daemonID {
		// The claim is not bound to this authenticated daemon. Delivering could
		// send to a session another daemon owns, so report it as not delivered
		// and let the server re-bind or re-lease.
		m.logger.Warn("mailbox claim is bound to another daemon; not delivering",
			"message_id", messageID,
			"claim_daemon_id", claim.ExternalSession.DaemonID,
			"daemon_id", m.daemonID)
		m.queueAck(ctx, claim, MailboxAckRequest{
			DeliveryToken: claim.DeliveryToken,
			Status:        mailboxAckRetry,
			Error:         mailboxErrorDaemonMismatch,
		}, false)
		return nil
	}

	expired, err := mailboxLeaseExpired(claim.LeaseExpiresAt, m.now())
	if err != nil {
		return err
	}
	if expired {
		m.queueAck(ctx, claim, MailboxAckRequest{
			DeliveryToken: claim.DeliveryToken,
			Status:        mailboxAckRetry,
			Error:         mailboxErrorLeaseExpired,
		}, false)
		return nil
	}

	receipt, deliverErr := m.transport.Deliver(ctx, claim.ExternalSession, messageID, claim.Content)
	if deliverErr != nil {
		return m.recordDeliveryError(ctx, claim, deliverErr)
	}
	m.queueAck(ctx, claim, MailboxAckRequest{
		DeliveryToken: claim.DeliveryToken,
		Status:        mailboxAckDelivered,
		HostReceipt:   receipt,
	}, true)
	return nil
}

func (m *mailboxDeliveryLoop) recordDeliveryError(ctx context.Context, claim MailboxClaim, deliverErr error) error {
	var typed *MailboxDeliveryError
	if !errors.As(deliverErr, &typed) {
		typed = &MailboxDeliveryError{
			Kind:    MailboxDeliveryOutcomeUnknown,
			Code:    mailboxErrorOutcomeUnknown,
			Message: "external host returned an unrecognized delivery result",
			Err:     deliverErr,
		}
	}

	if mailboxDeliveryRetryable(typed.Kind) {
		m.queueAck(ctx, claim, MailboxAckRequest{
			DeliveryToken: claim.DeliveryToken,
			Status:        mailboxAckRetry,
			Error:         mailboxRetryReason(typed),
		}, false)
		return nil
	}
	m.queueAck(ctx, claim, MailboxAckRequest{
		DeliveryToken: claim.DeliveryToken,
		Status:        mailboxAckFailed,
		Error:         mailboxTerminalReason(typed),
	}, true)
	return nil
}

// mailboxDeliveryRetryable is the only behavior that matters: a request known
// not to have reached the host can be retried, while an accepted or ambiguous
// outcome is terminal. Busy, unreachable, and unsupported dependencies all
// happen before the host accepts a request.
func mailboxDeliveryRetryable(kind MailboxDeliveryErrorKind) bool {
	switch kind {
	case MailboxDeliveryBusy, MailboxDeliveryUnavailable, MailboxDeliveryUnsupported:
		return true
	default:
		return false
	}
}

func mailboxRetryReason(typed *MailboxDeliveryError) string {
	switch typed.Kind {
	case MailboxDeliveryBusy:
		return mailboxErrorAgentBusy
	case MailboxDeliveryUnavailable:
		return mailboxErrorHostUnreachable
	default:
		reason := typed.Code
		if reason == "" {
			reason = mailboxErrorUnsupported
		}
		if typed.Message != "" {
			reason += ": " + typed.Message
		}
		return reason
	}
}

func mailboxTerminalReason(typed *MailboxDeliveryError) string {
	if typed.Kind == MailboxDeliveryOutcomeUnknown {
		return mailboxErrorOutcomeUnknown + ": not retried automatically to avoid duplicate work"
	}
	reason := typed.Code
	if reason == "" {
		reason = mailboxErrorDeliveryFailed
	}
	if typed.Message != "" {
		reason += ": " + typed.Message
	}
	return reason
}

// queueAck records the ack locally, then tries it once. A failed attempt stays
// pending so the next round retries the ack with the same lease token.
func (m *mailboxDeliveryLoop) queueAck(ctx context.Context, claim MailboxClaim, ack MailboxAckRequest, terminal bool) {
	m.pending[claim.Message.ID] = mailboxPendingAck{
		WorkspaceID:   claim.WorkspaceID,
		DeliveryToken: claim.DeliveryToken,
		Ack:           ack,
		terminal:      terminal,
	}
	ackCtx := ctx
	if terminal && ctx.Err() != nil {
		// A daemon stop can land after the host accepted the request. Try once
		// to persist that terminal outcome so the server does not release the
		// lease and cause a blind replay.
		var cancel context.CancelFunc
		ackCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
	}
	m.retryPendingAck(ackCtx, claim.Message.ID)
}

func (m *mailboxDeliveryLoop) retryPendingAck(ctx context.Context, messageID string) {
	pending, ok := m.pending[messageID]
	if !ok || pending.awaitingLease || ctx.Err() != nil {
		return
	}
	_, err := m.client.AckMailbox(ctx, pending.WorkspaceID, messageID, pending.Ack)
	if err == nil {
		delete(m.pending, messageID)
		return
	}
	switch {
	case mailboxAckIsStale(err):
		if pending.terminal {
			// Preserve the decided outcome and wait for the next claim's token.
			// The next handleClaim re-acks it without delivering again.
			pending.awaitingLease = true
			m.pending[messageID] = pending
			m.logger.Info("mailbox lease was superseded before ack; terminal outcome will be re-acked under the next lease", "message_id", messageID)
			return
		}
		delete(m.pending, messageID)
		m.logger.Info("mailbox lease was superseded before a known no-effect ack; dropping stale ack", "message_id", messageID)
	case mailboxAckIsGone(err):
		delete(m.pending, messageID)
		m.logger.Warn("mailbox message no longer exists; dropping ack", "message_id", messageID)
	case mailboxAckIsPermanent(err):
		delete(m.pending, messageID)
		m.logger.Warn("mailbox ack rejected permanently", "message_id", messageID, "error", err)
	default:
		m.logger.Debug("mailbox ack delayed; will retry", "message_id", messageID, "error", err)
	}
}

// flushPendingAcksOnStop makes one bounded attempt to persist terminal outcomes
// that were already decided but not yet acknowledged. Without it, a daemon stop
// would let the server re-lease a delivered or ambiguous message and cause a
// blind replay on the next start. Acks waiting for a newer lease cannot make
// progress here and are left for the next run.
func (m *mailboxDeliveryLoop) flushPendingAcksOnStop() {
	if len(m.pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for messageID := range m.pending {
		if ctx.Err() != nil {
			return
		}
		m.retryPendingAck(ctx, messageID)
	}
}

func mailboxStatus(err error) int {
	var reqErr *requestError
	if !errors.As(err, &reqErr) {
		return 0
	}
	return reqErr.StatusCode
}

// mailboxAckIsStale reports a lease-fencing conflict. The message still exists
// and a newer lease owns it.
func mailboxAckIsStale(err error) bool {
	return mailboxStatus(err) == http.StatusConflict
}

// mailboxAckIsGone reports that the message or its workspace no longer exists.
// Retrying the ack can never succeed.
func mailboxAckIsGone(err error) bool {
	return mailboxStatus(err) == http.StatusNotFound
}

func mailboxAckIsPermanent(err error) bool {
	switch mailboxStatus(err) {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// runMailboxDelivery starts the delivery loop as part of the daemon lifecycle.
func (d *Daemon) runMailboxDelivery(ctx context.Context) {
	loop := newMailboxDeliveryLoop(d.client, newPaseoCliTransport(), d.allWorkspaceIDs, d.cfg.DaemonID, d.logger)
	if err := loop.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		d.logger.Warn("mailbox delivery loop stopped", "error", err)
	}
}
