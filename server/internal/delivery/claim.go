package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrLeaseLost reports an outcome acknowledged against a lease the server no
// longer recognises. A newer lease owns the row, so the caller must not treat
// its own result as recorded.
var ErrLeaseLost = errors.New("delivery lease is stale or does not belong to this daemon")

// ErrOutcomeRejected reports an outcome the server refuses to store at all
// (malformed or unauthorised). Retrying it can never succeed.
var ErrOutcomeRejected = errors.New("delivery outcome was rejected permanently")

const (
	// ClaimLimit bounds how many deliveries one claim round leases. Each leased
	// row consumes a worker slot until its outcome is recorded.
	DefaultClaimLimit = 5
	MaximumClaimLimit = 50

	// DefaultLeaseDuration must outlive the host call for the whole batch. The
	// existing daemon used the same two-minute window.
	DefaultLeaseDuration = 2 * time.Minute
)

// Projection separates the two consumers of the one mailbox table. The legacy
// comment entry renders an issue/comment notification from a frozen DTO; the
// generic entry carries kind+payload and never fabricates either field. Their
// filters are disjoint, so a row can only be leased by one of them.
type Projection int

const (
	CommentProjection Projection = iota
	GenericProjection
)

// ClaimRequest asks for a bounded batch of deliverable rows bound to the
// verified daemon. OwnerUserID and DaemonID are the same binding the mailbox
// claim already enforced: the recipient's session must belong to this daemon
// and its human owner.
type ClaimRequest struct {
	Scope         Scope
	DaemonID      string
	OwnerUserID   string
	Limit         int
	LeaseDuration time.Duration
	Projection    Projection
}

func (r ClaimRequest) validate() error {
	if strings.TrimSpace(r.Scope.WorkspaceID) == "" {
		return errors.New("delivery: workspace id is required")
	}
	if strings.TrimSpace(r.DaemonID) == "" {
		return errors.New("delivery: daemon id is required")
	}
	if strings.TrimSpace(r.OwnerUserID) == "" {
		return errors.New("delivery: daemon owner is required")
	}
	switch r.Projection {
	case CommentProjection, GenericProjection:
	default:
		return errors.New("delivery: unknown projection")
	}
	return nil
}

// Claim is one fenced lease plus everything a host adapter needs to deliver it.
type Claim struct {
	Delivery
	Session        HostTarget
	LeaseToken     string
	LeaseExpiresAt time.Time
	// VerifyOnly means the previous attempt ended without a trustworthy result
	// (an expired lease or an earlier unknown outcome). The holder must look up
	// the host receipt instead of sending the message a second time.
	VerifyOnly bool
	// ClaimError means the daemon could not use the lease it was handed (for
	// example an unparseable lease_expires_at). The host was not called, so the
	// worker must record a retry instead of delivering or verifying.
	ClaimError string
}

// OutcomeState is what the host proved about one delivery attempt.
type OutcomeState string

const (
	// OutcomeDelivered means the host accepted the turn start.
	OutcomeDelivered OutcomeState = "delivered"
	// OutcomeRetry means the host proved it did not accept the request. It is
	// safe to attempt again with the same delivery id.
	OutcomeRetry OutcomeState = "retry"
	// OutcomeRejected is a terminal refusal (archived session, revoked
	// permission, unsupported binding). Replay cannot fix it.
	OutcomeRejected OutcomeState = "rejected"
	// OutcomeUnknown means the request may have been accepted. The row stays
	// unconfirmed and is only reclaimed for receipt verification, never resent.
	OutcomeUnknown OutcomeState = "unknown"
)

// RetryClass separates a busy host from a broken dependency. A busy session is
// healthy and retries quickly; an unreachable host or missing capability backs
// off so it never hot-loops.
type RetryClass string

const (
	RetryBusy        RetryClass = "busy"
	RetryUnavailable RetryClass = "unavailable"
	RetryDependency  RetryClass = "dependency"
)

// OutcomeRequest records the result of one lease. The delivery token fences the
// write to the exact lease that produced the result.
type OutcomeRequest struct {
	Scope       Scope
	DeliveryID  string
	DaemonID    string
	LeaseToken  string
	State       OutcomeState
	RetryClass  RetryClass
	HostReceipt string
	Error       string
}

func (r OutcomeRequest) validate() error {
	if strings.TrimSpace(r.Scope.WorkspaceID) == "" {
		return errors.New("delivery: workspace id is required")
	}
	if strings.TrimSpace(r.DeliveryID) == "" {
		return errors.New("delivery: delivery id is required")
	}
	if strings.TrimSpace(r.DaemonID) == "" {
		return errors.New("delivery: daemon id is required")
	}
	if strings.TrimSpace(r.LeaseToken) == "" {
		return errors.New("delivery: delivery token is required")
	}
	switch r.State {
	case OutcomeUnknown:
		if strings.TrimSpace(r.Error) == "" {
			return errors.New("delivery: an unknown outcome requires a diagnostic")
		}
	}
	if r.State == OutcomeRetry && r.RetryClass != "" {
		switch r.RetryClass {
		case RetryBusy, RetryUnavailable, RetryDependency:
		default:
			return fmt.Errorf("delivery: unsupported retry class %q", r.RetryClass)
		}
	}
	return nil
}

func (r OutcomeRequest) storedStatus() string {
	switch r.State {
	case OutcomeDelivered:
		return "delivered"
	case OutcomeRejected:
		return "failed"
	case OutcomeUnknown:
		return "unknown"
	default:
		return "pending"
	}
}

// TxBeginner is the subset of a pool the claim needs: leasing several rows and
// minting their tokens is one database outcome.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Queue owns the consume side of the delivery core over the one mailbox table.
// Claim and RecordOutcome were previously open-coded in the mailbox handler and
// the daemon loop; both consumer entries now share this implementation.
type Queue struct {
	q  *db.Queries
	tx TxBeginner
}

// NewQueue binds claim/outcome operations to the caller's query handle and
// transaction starter. Enqueue uses NewPostgres: it may run inside a business
// transaction, while a claim always owns its transaction.
func NewQueue(q *db.Queries, tx TxBeginner) *Queue {
	return &Queue{q: q, tx: tx}
}

// Claim leases at most one deliverable row per physical session. Two workers
// (or two daemons) can therefore never deliver to the same session at once, and
// an unreachable recipient cannot hold more than one slot. Two sessions of the
// same agent are different targets and proceed independently. The per-session
// advisory lock serialises concurrent claim transactions; the conditional lease
// re-checks eligibility so a lost race simply skips the row.
//
// The session row is loaded by the delivery's frozen Target, never by the
// recipient agent's current binding.
func (q *Queue) Claim(ctx context.Context, req ClaimRequest) ([]Claim, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	workspaceID, err := util.ParseUUID(req.Scope.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("delivery: workspace id: %w", err)
	}
	ownerID, err := util.ParseUUID(req.OwnerUserID)
	if err != nil {
		return nil, fmt.Errorf("delivery: daemon owner: %w", err)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultClaimLimit
	}
	if limit > MaximumClaimLimit {
		limit = MaximumClaimLimit
	}
	leaseDuration := req.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = DefaultLeaseDuration
	}

	tx, err := q.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("delivery: begin claim: %w", err)
	}
	defer tx.Rollback(ctx)
	qtx := q.q.WithTx(tx)

	candidates, err := qtx.ListClaimableDeliveryIDs(ctx, db.ListClaimableDeliveryIDsParams{
		WorkspaceID:       workspaceID,
		CommentProjection: req.Projection == CommentProjection,
		OwnerUserID:       ownerID,
		DaemonID:          req.DaemonID,
		RowLimit:          int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("delivery: find claimable rows: %w", err)
	}

	claims := make([]Claim, 0, len(candidates))
	for _, candidate := range candidates {
		targetSessionID := util.UUIDToString(candidate.TargetSessionID)
		locked, err := qtx.TryLockDeliveryTarget(ctx, targetSessionID)
		if err != nil {
			return nil, fmt.Errorf("delivery: lock target %s: %w", targetSessionID, err)
		}
		if !locked {
			// Another claim transaction is already leasing this session.
			continue
		}
		token, err := newLeaseToken()
		if err != nil {
			return nil, err
		}
		leased, err := qtx.LeaseClaimableDelivery(ctx, db.LeaseClaimableDeliveryParams{
			ID:              candidate.ID,
			WorkspaceID:     workspaceID,
			TargetSessionID: candidate.TargetSessionID,
			LeaseTokenHash:  pgtype.Text{String: auth.HashToken(token), Valid: true},
			LeaseOwner:      pgtype.Text{String: req.DaemonID, Valid: true},
			LeaseExpiresAt:  pgtypeTime(time.Now().Add(leaseDuration)),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Another claimant leased or acknowledged the row first.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("delivery: lease %s: %w", util.UUIDToString(candidate.ID), err)
		}
		session, err := qtx.GetExternalAgentSessionByID(ctx, db.GetExternalAgentSessionByIDParams{
			ID:          candidate.TargetSessionID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return nil, fmt.Errorf("delivery: load target session %s: %w", targetSessionID, err)
		}
		d := deliveryFromRow(leased)
		claims = append(claims, Claim{
			Delivery: d,
			Session: HostTarget{
				Recipient: d.Recipient,
				Provider:  session.Provider,
				HostID:    session.HostID,
				SessionID: session.SessionID,
				DaemonID:  session.DaemonID,
			},
			LeaseToken:     token,
			LeaseExpiresAt: leased.LeaseExpiresAt.Time,
			VerifyOnly:     verifyOnly(req.Projection, candidate.DeliveryStatus),
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("delivery: commit claim: %w", err)
	}
	return claims, nil
}

// verifyOnly decides whether a reclaimed row may be sent again. Any lease that
// expired without a recorded outcome is ambiguous: the host may have accepted
// the request before the worker or its acknowledgement disappeared. Only a row
// the host explicitly refused (pending, with a known no-effect result) is safe
// to deliver again. The legacy comment projection keeps its existing replay
// behavior because its frozen consumer never looks up receipts.
func verifyOnly(projection Projection, previousStatus string) bool {
	if previousStatus == "unknown" {
		return true
	}
	return projection == GenericProjection && previousStatus == "leased"
}

func pgtypeTime(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func newLeaseToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("delivery: mint lease token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// RecordOutcome stores one fenced result. The retry delay is part of the state
// machine: busy keeps the session's slot warm, while a failure backs off so a
// dead host is not polled in a tight loop. An unknown outcome is re-checked on
// that same bounded schedule; it is never made claimable for delivery again.
func (q *Queue) RecordOutcome(ctx context.Context, req OutcomeRequest) (Delivery, error) {
	if err := req.validate(); err != nil {
		return Delivery{}, err
	}
	workspaceID, err := util.ParseUUID(req.Scope.WorkspaceID)
	if err != nil {
		return Delivery{}, fmt.Errorf("delivery: workspace id: %w", err)
	}
	deliveryID, err := util.ParseUUID(req.DeliveryID)
	if err != nil {
		return Delivery{}, fmt.Errorf("delivery: delivery id: %w", err)
	}
	retryClass := ""
	if req.State == OutcomeRetry {
		retryClass = string(req.RetryClass)
		if retryClass == "" {
			retryClass = string(RetryUnavailable)
		}
	}
	row, err := q.q.RecordDeliveryOutcome(ctx, db.RecordDeliveryOutcomeParams{
		ID:             deliveryID,
		WorkspaceID:    workspaceID,
		Status:         req.storedStatus(),
		RetryClass:     retryClass,
		HostReceipt:    nullableText(sanitizeDiagnostic(req.HostReceipt)),
		LastError:      nullableText(sanitizeDiagnostic(req.Error)),
		LeaseTokenHash: pgtype.Text{String: auth.HashToken(req.LeaseToken), Valid: true},
		DaemonID:       pgtype.Text{String: req.DaemonID, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, ErrLeaseLost
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("delivery: record outcome: %w", err)
	}
	return deliveryFromRow(row), nil
}

func nullableText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: value != ""}
}

var deliverySecretPattern = regexp.MustCompile(`(?i)(bearer\s+[a-z0-9._-]+|\b(?:mea|mat|mdt|mul|mcn)_[a-z0-9]+)`)

// sanitizeDiagnostic bounds persisted diagnostics and strips anything that
// looks like a bearer credential. Delivery errors are stored and shown to
// humans, so a host must not be able to leak its secrets through them.
func sanitizeDiagnostic(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = deliverySecretPattern.ReplaceAllString(s, "[redacted]")
	if len(s) > 2000 {
		s = s[:2000]
	}
	return s
}
