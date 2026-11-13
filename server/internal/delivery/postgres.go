package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

// Postgres stores deliveries in agent_mailbox_message. That table already owns
// the mailbox UUIDs, leases and receipts; evolving it in place keeps one
// authoritative delivery state instead of a second table written alongside it.
type Postgres struct {
	q *db.Queries
}

// NewPostgres binds the adapter to a query handle. Pass h.Queries.WithTx(tx)
// to enqueue inside a caller's transaction, so a business write and its
// delivery intent commit together.
func NewPostgres(q *db.Queries) *Postgres {
	return &Postgres{q: q}
}

// Enqueue persists one immutable delivery. A replay of the same scope,
// recipient, source and source key returns the original row; different content
// under the same key is ErrConflict.
func (p *Postgres) Enqueue(ctx context.Context, req EnqueueRequest) (Delivery, error) {
	if err := req.Validate(); err != nil {
		return Delivery{}, err
	}
	params, err := createDeliveryParams(req)
	if err != nil {
		return Delivery{}, err
	}
	row, err := p.q.CreateDelivery(ctx, params)
	if err == nil {
		return deliveryFromRow(row), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, fmt.Errorf("delivery: enqueue: %w", err)
	}

	existing, found, err := p.findExisting(ctx, req)
	if err != nil {
		return Delivery{}, err
	}
	if !found {
		return Delivery{}, errors.New("delivery: idempotency conflict but the existing row is not readable")
	}
	if !req.sameContent(existing) {
		return Delivery{}, fmt.Errorf("%w: existing delivery %s", ErrConflict, existing.ID)
	}
	return existing, nil
}

// Get returns one stored delivery inside its workspace scope.
func (p *Postgres) Get(ctx context.Context, scope Scope, id string) (Delivery, error) {
	workspaceID, err := util.ParseUUID(scope.WorkspaceID)
	if err != nil {
		return Delivery{}, fmt.Errorf("delivery: workspace id: %w", err)
	}
	deliveryID, err := util.ParseUUID(id)
	if err != nil {
		return Delivery{}, fmt.Errorf("delivery: id: %w", err)
	}
	row, err := p.q.GetDelivery(ctx, db.GetDeliveryParams{ID: deliveryID, WorkspaceID: workspaceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, ErrNotFound
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("delivery: get: %w", err)
	}
	return deliveryFromRow(row), nil
}

func (p *Postgres) findExisting(ctx context.Context, req EnqueueRequest) (Delivery, bool, error) {
	workspaceID, err := util.ParseUUID(req.Scope.WorkspaceID)
	if err != nil {
		return Delivery{}, false, fmt.Errorf("delivery: workspace id: %w", err)
	}
	row, err := p.q.GetDeliveryByNotificationKey(ctx, db.GetDeliveryByNotificationKeyParams{
		WorkspaceID:     workspaceID,
		NotificationKey: pgtype.Text{String: req.NotificationKey, Valid: true},
	})
	if err == nil {
		return deliveryFromRow(row), true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, false, fmt.Errorf("delivery: read notification key: %w", err)
	}

	// Rows written before the core carry no notification_key. The legacy
	// (recipient, comment) unique key still identifies them, and the original
	// mailbox UUID is the idempotency result the caller must receive.
	if req.Source.Type != "comment" || req.Refs.CommentID == "" {
		return Delivery{}, false, nil
	}
	recipientID, err := util.ParseUUID(req.Recipient.ID)
	if err != nil {
		return Delivery{}, false, fmt.Errorf("delivery: recipient id: %w", err)
	}
	commentID, err := util.ParseUUID(req.Refs.CommentID)
	if err != nil {
		return Delivery{}, false, fmt.Errorf("delivery: comment id: %w", err)
	}
	legacy, err := p.q.GetLegacyCommentDelivery(ctx, db.GetLegacyCommentDeliveryParams{
		RecipientAgentID: recipientID,
		CommentID:        commentID,
	})
	if err == nil {
		return deliveryFromRow(legacy), true, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, false, nil
	}
	return Delivery{}, false, fmt.Errorf("delivery: read legacy comment delivery: %w", err)
}

func createDeliveryParams(req EnqueueRequest) (db.CreateDeliveryParams, error) {
	workspaceID, err := util.ParseUUID(req.Scope.WorkspaceID)
	if err != nil {
		return db.CreateDeliveryParams{}, fmt.Errorf("delivery: workspace id: %w", err)
	}
	recipientID, err := util.ParseUUID(req.Recipient.ID)
	if err != nil {
		return db.CreateDeliveryParams{}, fmt.Errorf("delivery: recipient id: %w", err)
	}
	targetID, err := util.ParseUUID(req.Target.SessionID)
	if err != nil {
		return db.CreateDeliveryParams{}, fmt.Errorf("delivery: target session id: %w", err)
	}
	issueID, err := optionalUUID(req.Refs.IssueID)
	if err != nil {
		return db.CreateDeliveryParams{}, fmt.Errorf("delivery: issue id: %w", err)
	}
	commentID, err := optionalUUID(req.Refs.CommentID)
	if err != nil {
		return db.CreateDeliveryParams{}, fmt.Errorf("delivery: comment id: %w", err)
	}
	sessionID, err := optionalUUID(req.Source.SessionID)
	if err != nil {
		return db.CreateDeliveryParams{}, fmt.Errorf("delivery: source session id: %w", err)
	}
	senderID, err := optionalUUID(req.Source.ActorID)
	if err != nil {
		return db.CreateDeliveryParams{}, fmt.Errorf("delivery: source actor id: %w", err)
	}
	payload, err := canonicalJSON(req.Payload)
	if err != nil {
		return db.CreateDeliveryParams{}, fmt.Errorf("delivery: canonical payload: %w", err)
	}
	senderType := pgtype.Text{}
	if req.Source.ActorType != "" {
		senderType = pgtype.Text{String: req.Source.ActorType, Valid: true}
	}
	return db.CreateDeliveryParams{
		ID:               dbid.NewV7(),
		WorkspaceID:      workspaceID,
		IssueID:          issueID,
		CommentID:        commentID,
		RecipientAgentID: recipientID,
		RecipientType:    req.Recipient.Type,
		TargetSessionID:  targetID,
		NotificationKey:  pgtype.Text{String: req.NotificationKey, Valid: true},
		SourceSessionID:  sessionID,
		SenderType:       senderType,
		SenderID:         senderID,
		SourceType:       req.Source.Type,
		SourceKey:        pgtype.Text{String: req.Source.Key, Valid: true},
		Kind:             req.Kind,
		Payload:          payload,
	}, nil
}

func optionalUUID(value string) (pgtype.UUID, error) {
	if value == "" {
		return pgtype.UUID{}, nil
	}
	return util.ParseUUID(value)
}

func deliveryFromRow(row db.AgentMailboxMessage) Delivery {
	out := Delivery{
		ID:               util.UUIDToString(row.ID),
		Scope:            Scope{WorkspaceID: util.UUIDToString(row.WorkspaceID)},
		Source:           Source{Type: row.SourceType, Key: row.SourceKey.String, ActorType: row.SenderType.String, ActorID: util.UUIDToString(row.SenderID), SessionID: util.UUIDToString(row.SourceSessionID)},
		Target:           Target{SessionID: util.UUIDToString(row.TargetSessionID)},
		Recipient:        Recipient{Type: row.RecipientType, ID: util.UUIDToString(row.RecipientAgentID)},
		NotificationKey:  row.NotificationKey.String,
		Kind:             row.Kind,
		Refs:             Refs{IssueID: util.UUIDToString(row.IssueID), CommentID: util.UUIDToString(row.CommentID)},
		DeliveryStatus:   row.DeliveryStatus,
		ProcessingStatus: row.ProcessingStatus,
		Attempts:         row.Attempts,
		HostReceipt:      row.HostReceipt.String,
		LastError:        row.LastError.String,
		CreatedAt:        row.CreatedAt.Time,
	}
	if len(row.Payload) > 0 {
		out.Payload = append(json.RawMessage(nil), row.Payload...)
	}
	if (row.DeliveryStatus == "pending" || row.DeliveryStatus == "unknown") && row.NextAttemptAt.Valid {
		next := row.NextAttemptAt.Time
		out.NextAttemptAt = &next
	}
	if row.DeliveredAt.Valid {
		deliveredAt := row.DeliveredAt.Time
		out.DeliveredAt = &deliveredAt
	}
	return out
}
