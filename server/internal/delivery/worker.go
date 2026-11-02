package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultWorkerPollInterval = 5 * time.Second
	// Bound concurrent host calls. One per recipient session is already
	// enforced by the claim, so the limit only caps parallel sessions.
	defaultWorkerConcurrency = 4
	defaultWorkerClaimLimit  = 5
	// A decided outcome whose lease was superseded is re-recorded only if this
	// worker reclaims the row. If another daemon owns it, stop retrying after
	// this bound: that holder verifies the original message instead.
	pendingOutcomeMaxAge = 15 * time.Minute
)

// HostTarget is the already-authorized session a delivery must reach. It never
// contains a worktree, model or agent configuration: those stay with the host.
type HostTarget struct {
	Recipient Recipient
	Provider  string
	HostID    string
	SessionID string
	DaemonID  string
}

// ReceiptStatus is the host's read-only answer for one stable message id.
type ReceiptStatus string

const (
	// ReceiptAccepted means the host recorded the request as accepted.
	ReceiptAccepted ReceiptStatus = "accepted"
	// ReceiptPending means a receipt exists but acceptance was never completed.
	ReceiptPending ReceiptStatus = "pending"
	// ReceiptAbsent means no receipt was found. It is not proof that the
	// request never ran.
	ReceiptAbsent ReceiptStatus = "absent"
)

// Receipt is the host's answer for one delivery id, optionally with a stable
// reference the delivery row can store.
type Receipt struct {
	Status    ReceiptStatus
	Reference string
}

// HostErrorKind classifies a host result. Only a proven no-effect result may be
// sent again with the same id.
type HostErrorKind int

const (
	// HostRejected is a terminal refusal: replaying cannot succeed.
	HostRejected HostErrorKind = iota
	// HostBusy means the session is working and nothing was accepted. Retry
	// quickly; this is not a failure.
	HostBusy
	// HostUnavailable means the host could not be reached. Nothing was sent.
	HostUnavailable
	// HostUnsupported means the installed host cannot serve the capability.
	// Nothing was sent and there is no fallback path.
	HostUnsupported
	// HostOutcomeUnknown means the request may have been accepted.
	HostOutcomeUnknown
)

// HostError is a typed host result. Message is safe to persist: it never
// contains prompt content or credentials.
type HostError struct {
	Kind    HostErrorKind
	Code    string
	Message string
	Err     error
}

func (e *HostError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code
}

func (e *HostError) Unwrap() error { return e.Err }

// Host is the adapter contract between the worker and one session host. Both
// calls are keyed by the original session and stable delivery id; neither may
// create, resume or replace a session, and DeliverIfIdle has no ordinary-send
// fallback.
type Host interface {
	DeliverIfIdle(ctx context.Context, target HostTarget, d Delivery) (hostReceipt string, err error)
	LookupReceipt(ctx context.Context, target HostTarget, deliveryID string) (Receipt, error)
}

// OutcomeClient is the worker's server transport. ClaimDeliveries leases rows
// for one workspace; RecordDeliveryOutcome persists the fenced result.
type OutcomeClient interface {
	ClaimDeliveries(ctx context.Context, workspaceID string, limit int) ([]Claim, error)
	RecordDeliveryOutcome(ctx context.Context, workspaceID string, req OutcomeRequest) (Delivery, error)
}

type pendingOutcome struct {
	workspaceID   string
	leaseToken    string
	request       OutcomeRequest
	awaitingLease bool
	decidedAt     time.Time
}

// Worker is the issue-independent consume loop: it claims generic deliveries,
// asks the host to accept them while idle, and records what the host proved.
// It never calls a model, never batches messages and never invents a second
// queue: all state lives in the delivery row.
type Worker struct {
	client     OutcomeClient
	host       Host
	workspaces func() []string
	daemonID   string
	logger     *slog.Logger

	pollInterval time.Duration
	concurrency  int
	claimLimit   int

	mu      sync.Mutex
	pending map[string]*pendingOutcome
}

// NewWorker builds the delivery worker for one daemon identity.
func NewWorker(client OutcomeClient, host Host, workspaces func() []string, daemonID string, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	if workspaces == nil {
		workspaces = func() []string { return nil }
	}
	return &Worker{
		client:       client,
		host:         host,
		workspaces:   workspaces,
		daemonID:     daemonID,
		logger:       logger,
		pollInterval: defaultWorkerPollInterval,
		concurrency:  defaultWorkerConcurrency,
		claimLimit:   defaultWorkerClaimLimit,
		pending:      make(map[string]*pendingOutcome),
	}
}

// Run polls until the context ends. Every round is bounded by the per-delivery
// host timeout, so one unreachable recipient cannot stall the batch.
func (w *Worker) Run(ctx context.Context) error {
	interval := w.pollInterval
	if interval <= 0 {
		interval = defaultWorkerPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		w.round(ctx)
		if ctx.Err() != nil {
			w.flushPending()
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			w.flushPending()
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// round retries delayed outcome acknowledgements before claiming new work. An
// ack that fails must not cause a second host send.
func (w *Worker) round(ctx context.Context) {
	w.retryPending(ctx)
	for _, workspaceID := range w.workspaces() {
		if workspaceID == "" {
			continue
		}
		claims, err := w.client.ClaimDeliveries(ctx, workspaceID, w.claimLimit)
		if err != nil {
			switch {
			case errors.Is(err, ErrNotFound):
				// This is the only claim entry, so a missing route stalls every
				// delivery source for the workspace: comments and generic
				// events alike. Keep the loop alive for other workspaces, but
				// make the outage visible instead of debug noise.
				w.logger.Warn("delivery claim endpoint unavailable; every delivery source for this workspace is stalled", "workspace_id", workspaceID, "error", err)
			case ctx.Err() == nil:
				// A revoked membership or removed workspace must not stop the
				// other workspaces this daemon serves.
				w.logger.Warn("delivery claim failed", "workspace_id", workspaceID, "error", err)
			}
			continue
		}
		w.dispatch(ctx, claims)
	}
}

// dispatch runs one claim batch with bounded concurrency. The server already
// leases at most one row per recipient session; the local dedupe is a second
// guard so a malformed batch can never overlap two calls to the same session.
func (w *Worker) dispatch(ctx context.Context, claims []Claim) {
	concurrency := w.concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	seen := make(map[string]struct{}, len(claims))
	for _, claim := range claims {
		key := claim.Session.SessionID
		if key == "" {
			key = claim.Recipient.ID
		}
		if _, duplicate := seen[key]; duplicate {
			w.logger.Warn("delivery batch held two rows for one session; using the first", "session_id", key)
			// A locally decided result for this row wins over the unusable
			// claim: re-ack it under the new lease instead of downgrading it to
			// unknown. Only a row with no local result is recorded as a retry,
			// so it does not stay leased until verification parks a never-sent
			// delivery as unknown.
			if w.adoptPending(claim) {
				w.retryPendingOne(ctx, claim.ID)
			} else {
				w.recordUnusable(ctx, claim, "the claim batch held a second row for this session, so the host was not called")
			}
			continue
		}
		seen[key] = struct{}{}

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(claim Claim) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := w.handleClaim(ctx, claim); err != nil {
				w.logger.Warn("delivery attempt failed",
					"delivery_id", claim.ID,
					"recipient_agent_id", claim.Recipient.ID,
					"error", err)
			}
		}(claim)
	}
	wg.Wait()
}

func (w *Worker) handleClaim(ctx context.Context, claim Claim) error {
	if claim.ID == "" {
		return errors.New("delivery claim has no delivery id")
	}
	if claim.LeaseToken == "" {
		return errors.New("delivery claim has no delivery token")
	}
	// A decided outcome that could not be acknowledged yet is re-acked under the
	// newest lease before the lease itself is judged. Replaying a known result
	// needs only the new token, not a usable session, and overwriting it with an
	// unusable-claim result would lose a confirmed delivery.
	if w.adoptPending(claim) {
		w.retryPendingOne(ctx, claim.ID)
		return nil
	}
	if claim.ClaimError != "" {
		// The lease itself was unusable, so the host was never called for it.
		return w.recordUnusable(ctx, claim, claim.ClaimError)
	}
	if claim.Session.SessionID == "" {
		// No host was called for this row either; leaving it leased would turn
		// a never-sent delivery into an unverifiable unknown row.
		return w.recordUnusable(ctx, claim, "delivery claim has no host session id, so the host was not called")
	}
	if claim.Session.DaemonID != "" && claim.Session.DaemonID != w.daemonID {
		return w.recordUnusable(ctx, claim, fmt.Sprintf(
			"delivery claim is bound to daemon %q, not %q, so the host was not called", claim.Session.DaemonID, w.daemonID))
	}

	if claim.VerifyOnly {
		return w.verify(ctx, claim, nil)
	}

	receipt, err := w.host.DeliverIfIdle(ctx, claim.Session, claim.Delivery)
	if err != nil {
		return w.handleHostFailure(ctx, claim, err)
	}
	return w.record(ctx, claim, OutcomeDelivered, "", receipt, "")
}

func (w *Worker) handleHostFailure(ctx context.Context, claim Claim, err error) error {
	var hostErr *HostError
	if !errors.As(err, &hostErr) {
		hostErr = &HostError{
			Kind:    HostOutcomeUnknown,
			Code:    "host_result_unrecognized",
			Message: "the host returned an unrecognized delivery result",
			Err:     err,
		}
	}
	switch hostErr.Kind {
	case HostBusy:
		return w.record(ctx, claim, OutcomeRetry, RetryBusy, "", hostReason(hostErr, "agent_busy"))
	case HostUnavailable:
		return w.record(ctx, claim, OutcomeRetry, RetryUnavailable, "", hostReason(hostErr, "host_unreachable"))
	case HostUnsupported:
		return w.record(ctx, claim, OutcomeRetry, RetryDependency, "", hostReason(hostErr, "host_capability_unsupported"))
	case HostRejected:
		return w.record(ctx, claim, OutcomeRejected, "", "", hostReason(hostErr, "host_rejected"))
	default:
		return w.verify(ctx, claim, hostErr)
	}
}

// verify asks the host whether the original message id was accepted. It never
// resends: accepted is the only result that completes the delivery, while
// pending, absent and an unsupported or failed lookup all leave the row in the
// unconfirmed state for a later check.
func (w *Worker) verify(ctx context.Context, claim Claim, cause *HostError) error {
	receipt, err := w.host.LookupReceipt(ctx, claim.Session, claim.ID)
	if err != nil {
		var hostErr *HostError
		if errors.As(err, &hostErr) && hostErr.Kind == HostUnsupported {
			return w.record(ctx, claim, OutcomeUnknown, "", "", unknownReason(cause,
				"the host cannot look up delivery receipts, so the outcome stays unconfirmed and is not replayed"))
		}
		return w.record(ctx, claim, OutcomeUnknown, "", "", unknownReason(cause,
			"the host receipt lookup failed, so the outcome stays unconfirmed and is not replayed"))
	}
	switch receipt.Status {
	case ReceiptAccepted:
		return w.record(ctx, claim, OutcomeDelivered, "", receipt.Reference, "")
	case ReceiptPending:
		return w.record(ctx, claim, OutcomeUnknown, "", "", unknownReason(cause,
			"the host receipt is still pending, so the request may have started; it is not replayed"))
	case ReceiptAbsent:
		return w.record(ctx, claim, OutcomeUnknown, "", "", unknownReason(cause,
			"the host has no receipt for this message id; absent is not proof of non-execution, so it is not replayed"))
	default:
		return w.record(ctx, claim, OutcomeUnknown, "", "", unknownReason(cause,
			fmt.Sprintf("the host returned an unrecognized receipt status %q; it is not replayed", receipt.Status)))
	}
}

func hostReason(hostErr *HostError, fallback string) string {
	reason := hostErr.Code
	if reason == "" {
		reason = fallback
	}
	if hostErr.Message != "" {
		reason += ": " + hostErr.Message
	}
	return reason
}

func unknownReason(cause *HostError, detail string) string {
	if cause == nil {
		return detail
	}
	return hostReason(cause, "outcome_unknown") + "; " + detail
}

// recordUnusable stores a lease the worker could not use. The host was not
// called for this lease, but a verification-only row may already have been
// accepted by an earlier attempt: it must stay unknown, never return to
// claimable delivery. Only a row whose previous result proved no effect goes
// back to pending, where the same id can be attempted again.
func (w *Worker) recordUnusable(ctx context.Context, claim Claim, reason string) error {
	if claim.VerifyOnly {
		return w.record(ctx, claim, OutcomeUnknown, "", "",
			reason+"; the earlier attempt is still unconfirmed and is not replayed")
	}
	return w.record(ctx, claim, OutcomeRetry, RetryDependency, "", reason)
}

// record stores the outcome locally before attempting the ack. A delayed or
// failed ack is retried under the same lease on the next round, so it can never
// turn into a second host start.
func (w *Worker) record(ctx context.Context, claim Claim, state OutcomeState, class RetryClass, receipt, errText string) error {
	request := OutcomeRequest{
		Scope:       claim.Scope,
		DeliveryID:  claim.ID,
		DaemonID:    w.daemonID,
		LeaseToken:  claim.LeaseToken,
		State:       state,
		RetryClass:  class,
		HostReceipt: receipt,
		Error:       errText,
	}
	w.mu.Lock()
	w.pending[claim.ID] = &pendingOutcome{
		workspaceID: claim.Scope.WorkspaceID,
		leaseToken:  claim.LeaseToken,
		request:     request,
		decidedAt:   time.Now(),
	}
	w.mu.Unlock()
	w.retryPendingOne(ctx, claim.ID)
	return nil
}

// adoptPending reports whether a locally decided outcome already exists for
// this row. A newer lease supersedes the token so the outcome can be recorded
// under it without delivering again.
func (w *Worker) adoptPending(claim Claim) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	pending, ok := w.pending[claim.ID]
	if !ok {
		return false
	}
	if pending.leaseToken != claim.LeaseToken {
		pending.leaseToken = claim.LeaseToken
		pending.request.LeaseToken = claim.LeaseToken
		pending.awaitingLease = false
		// This worker owns the row again, so the stale-entry bound restarts.
		pending.decidedAt = time.Now()
	}
	return true
}

func (w *Worker) retryPending(ctx context.Context) {
	w.mu.Lock()
	ids := make([]string, 0, len(w.pending))
	for id, pending := range w.pending {
		if pending.awaitingLease && time.Since(pending.decidedAt) > pendingOutcomeMaxAge {
			// Another lease holder owns the row. It verifies the original
			// message; keeping this ack forever would only grow the map.
			delete(w.pending, id)
			w.logger.Warn("dropping a decided outcome that another lease never re-acknowledged", "delivery_id", id)
			continue
		}
		ids = append(ids, id)
	}
	w.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		w.retryPendingOne(ctx, id)
	}
}

func (w *Worker) retryPendingOne(ctx context.Context, deliveryID string) {
	w.mu.Lock()
	pending, ok := w.pending[deliveryID]
	if !ok || pending.awaitingLease {
		w.mu.Unlock()
		return
	}
	request := pending.request
	workspaceID := pending.workspaceID
	w.mu.Unlock()

	if ctx.Err() != nil {
		return
	}
	_, err := w.client.RecordDeliveryOutcome(ctx, workspaceID, request)
	if err == nil {
		w.mu.Lock()
		delete(w.pending, deliveryID)
		w.mu.Unlock()
		return
	}
	switch {
	case errors.Is(err, ErrLeaseLost):
		// The lease was superseded before the ack landed. Keep the decided
		// outcome and wait for the next claim of this row to re-ack it.
		w.mu.Lock()
		if current, stillPending := w.pending[deliveryID]; stillPending && current.leaseToken == request.LeaseToken {
			current.awaitingLease = true
		}
		w.mu.Unlock()
		w.logger.Info("delivery lease was superseded before the outcome landed; waiting for the next lease", "delivery_id", deliveryID)
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrOutcomeRejected):
		w.mu.Lock()
		delete(w.pending, deliveryID)
		w.mu.Unlock()
		w.logger.Warn("delivery outcome was refused permanently; dropping it", "delivery_id", deliveryID, "error", err)
	default:
		w.logger.Debug("delivery outcome delayed; will retry", "delivery_id", deliveryID, "error", err)
	}
}

// flushPending makes one bounded attempt to persist decided outcomes during
// shutdown. Without it a stop after the host accepted would let the lease
// expire and force a receipt lookup on restart.
func (w *Worker) flushPending() {
	w.mu.Lock()
	empty := len(w.pending) == 0
	w.mu.Unlock()
	if empty {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w.retryPending(ctx)
}
