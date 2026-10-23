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
	defaultMailboxClaimLimit    = 5
	defaultMailboxPollInterval  = 5 * time.Second
	mailboxErrorAgentBusy       = "agent_busy"
	mailboxErrorHostUnreachable = "paseo_daemon_unreachable"
	mailboxErrorLeaseExpired    = "lease_expired"
	mailboxErrorOutcomeUnknown  = "agent_request_outcome_unknown"
	mailboxErrorUnsupported     = "paseo_dependency_unsupported"
	mailboxErrorDeliveryFailed  = "paseo_delivery_failed"
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
	// atomic if-idle delivery. Never fall back to ordinary send.
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
}

type mailboxDeliveryLoop struct {
	client       mailboxDeliveryClient
	transport    MailboxHostTransport
	workspaces   func() []string
	logger       *slog.Logger
	now          func() time.Time
	claimLimit   int
	pollInterval time.Duration
	pending      map[string]mailboxPendingAck
}

func newMailboxDeliveryLoop(client mailboxDeliveryClient, transport MailboxHostTransport, workspaces func() []string, logger *slog.Logger) *mailboxDeliveryLoop {
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
	for messageID := range m.pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.retryPendingAck(ctx, messageID)
	}

	for _, workspaceID := range m.workspaces() {
		if workspaceID == "" {
			continue
		}
		claims, err := m.client.ClaimMailbox(ctx, workspaceID, m.claimLimit)
		if err != nil {
			if isMailboxEndpointMissing(err) {
				m.logger.Info("server has no mailbox delivery endpoint; mailbox delivery disabled for this daemon session")
				return errMailboxDeliveryUnsupported
			}
			if ctx.Err() == nil {
				m.logger.Warn("mailbox claim failed", "workspace_id", workspaceID, "error", err)
			}
			continue
		}
		for i := range claims {
			claims[i].WorkspaceID = workspaceID
		}
		for _, claim := range claims {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := m.handleClaim(ctx, claim); err != nil {
				m.logger.Warn("mailbox delivery failed",
					"workspace_id", workspaceID,
					"message_id", claim.Message.ID,
					"recipient_agent_id", claim.Message.RecipientAgentID,
					"error", err)
			}
		}
	}
	return nil
}

var errMailboxDeliveryUnsupported = errors.New("mailbox delivery endpoint is not supported by this server")

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

	pending, hasPending := m.pending[messageID]
	if hasPending && pending.DeliveryToken != claim.DeliveryToken {
		// A later lease supersedes the pending one. Never send the stale token;
		// a terminal outcome stays terminal so an ambiguous request is not
		// replayed under the new lease.
		delete(m.pending, messageID)
		if pending.terminal {
			pending.WorkspaceID = claim.WorkspaceID
			pending.DeliveryToken = claim.DeliveryToken
			pending.Ack.DeliveryToken = claim.DeliveryToken
			m.pending[messageID] = pending
			m.retryPendingAck(ctx, messageID)
			return nil
		}
	}
	if hasPending && m.pending[messageID].terminal {
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

	switch typed.Kind {
	case MailboxDeliveryBusy:
		m.queueAck(ctx, claim, MailboxAckRequest{
			DeliveryToken: claim.DeliveryToken,
			Status:        mailboxAckRetry,
			Error:         mailboxErrorAgentBusy,
		}, false)
		return nil
	case MailboxDeliveryUnavailable:
		m.queueAck(ctx, claim, MailboxAckRequest{
			DeliveryToken: claim.DeliveryToken,
			Status:        mailboxAckRetry,
			Error:         mailboxErrorHostUnreachable,
		}, false)
		return nil
	case MailboxDeliveryUnsupported:
		m.queueAck(ctx, claim, MailboxAckRequest{
			DeliveryToken: claim.DeliveryToken,
			Status:        mailboxAckFailed,
			Error:         mailboxErrorUnsupported + ": upgrade Paseo to a version that supports atomic if-idle delivery",
		}, true)
		return nil
	case MailboxDeliveryOutcomeUnknown:
		m.queueAck(ctx, claim, MailboxAckRequest{
			DeliveryToken: claim.DeliveryToken,
			Status:        mailboxAckFailed,
			Error:         mailboxErrorOutcomeUnknown + ": not retried automatically to avoid duplicate work",
		}, true)
		return nil
	default:
		code := typed.Code
		if code == "" {
			code = mailboxErrorDeliveryFailed
		}
		m.queueAck(ctx, claim, MailboxAckRequest{
			DeliveryToken: claim.DeliveryToken,
			Status:        mailboxAckFailed,
			Error:         code,
		}, true)
		return nil
	}
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
	if !ok {
		return
	}
	if ctx.Err() != nil {
		return
	}
	_, err := m.client.AckMailbox(ctx, pending.WorkspaceID, messageID, pending.Ack)
	if err == nil {
		delete(m.pending, messageID)
		return
	}
	if mailboxAckIsStale(err) {
		delete(m.pending, messageID)
		m.logger.Info("mailbox lease was superseded before ack; dropping stale ack", "message_id", messageID)
		return
	}
	if mailboxAckIsPermanent(err) {
		delete(m.pending, messageID)
		m.logger.Warn("mailbox ack rejected permanently", "message_id", messageID, "error", err)
		return
	}
	m.logger.Debug("mailbox ack delayed; will retry", "message_id", messageID, "error", err)
}

// flushPendingAcksOnStop makes one bounded attempt to persist terminal outcomes
// that were already decided but not yet acknowledged. Without it, a daemon stop
// would let the server re-lease a delivered or ambiguous message and cause a
// blind replay on the next start.
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

func isMailboxEndpointMissing(err error) bool {
	var reqErr *requestError
	if !errors.As(err, &reqErr) {
		return false
	}
	return reqErr.StatusCode == http.StatusNotFound
}

func mailboxAckIsStale(err error) bool {
	var reqErr *requestError
	if !errors.As(err, &reqErr) {
		return false
	}
	return reqErr.StatusCode == http.StatusConflict || reqErr.StatusCode == http.StatusNotFound
}

func mailboxAckIsPermanent(err error) bool {
	var reqErr *requestError
	if !errors.As(err, &reqErr) {
		return false
	}
	switch reqErr.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// runMailboxDelivery starts the delivery loop as part of the daemon lifecycle.
// A server without the endpoint disables the loop for this process rather than
// retrying every tick.
func (d *Daemon) runMailboxDelivery(ctx context.Context) {
	loop := newMailboxDeliveryLoop(d.client, newPaseoCliTransport(), d.allWorkspaceIDs, d.logger)
	if err := loop.run(ctx); err != nil &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, errMailboxDeliveryUnsupported) {
		d.logger.Warn("mailbox delivery loop stopped", "error", err)
	}
}
