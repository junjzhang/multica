package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func openDeliveryTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dbURL)
	if err != nil {
		t.Skipf("database not available: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("database not reachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// cleanupDelivery removes the row a test created. agent_mailbox_message has no
// foreign keys, so tests can enqueue against fresh scope/recipient ids without
// creating workspace or agent fixtures.
func cleanupDelivery(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_mailbox_message WHERE id = $1`, id)
	})
}

func genericRequest(workspaceID, recipientID string) EnqueueRequest {
	return EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "ci_run", Key: "run-42-attempt-1"},
		Target:          Target{SessionID: uuid.NewString()},
		Recipient:       Recipient{Type: "agent", ID: recipientID},
		NotificationKey: "ci:run-42-attempt-1",
		Kind:            "ci_terminal",
		Payload:         json.RawMessage(`{"run_id":"run-42","conclusion":"success"}`),
	}
}

// TestPostgresEnqueueWithoutIssueOrComment is the core's independence proof:
// the request carries no issue or comment, and the test creates neither.
func TestPostgresEnqueueWithoutIssueOrComment(t *testing.T) {
	pool := openDeliveryTestPool(t)
	store := NewPostgres(db.New(pool))
	workspaceID := uuid.NewString()
	recipientID := uuid.NewString()

	stored, err := store.Enqueue(context.Background(), genericRequest(workspaceID, recipientID))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	cleanupDelivery(t, pool, stored.ID)
	if stored.ID == "" {
		t.Fatal("enqueue returned an empty delivery id")
	}
	if stored.Refs != (Refs{}) {
		t.Fatalf("refs = %+v, want empty: a generic delivery must not invent issue/comment references", stored.Refs)
	}
	if stored.DeliveryStatus != "pending" || stored.ProcessingStatus != "unread" {
		t.Fatalf("status = %s/%s, want pending/unread", stored.DeliveryStatus, stored.ProcessingStatus)
	}

	got, err := store.Get(context.Background(), Scope{WorkspaceID: workspaceID}, stored.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != stored.ID || got.Kind != stored.Kind {
		t.Fatalf("get = %+v, want id %s kind %s", got, stored.ID, stored.Kind)
	}
}

func TestPostgresEnqueueIdempotencyAndConflict(t *testing.T) {
	pool := openDeliveryTestPool(t)
	store := NewPostgres(db.New(pool))
	req := genericRequest(uuid.NewString(), uuid.NewString())

	first, err := store.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	cleanupDelivery(t, pool, first.ID)

	// A replay with the same immutable content returns the original row, even
	// if the payload's key spelling differs.
	replay := req
	replay.Payload = json.RawMessage(`{"conclusion": "success", "run_id": "run-42"}`)
	second, err := store.Enqueue(context.Background(), replay)
	if err != nil {
		t.Fatalf("replay enqueue: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("replay id = %s, want original %s", second.ID, first.ID)
	}

	conflict := req
	conflict.Payload = json.RawMessage(`{"run_id":"run-42","conclusion":"failure"}`)
	if _, err := store.Enqueue(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("different content error = %v, want ErrConflict", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_mailbox_message WHERE workspace_id = $1 AND recipient_agent_id = $2`,
		req.Scope.WorkspaceID, req.Recipient.ID).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("stored rows = %d, want exactly 1", count)
	}
}

// TestPostgresRetryKeepsFrozenTargetAndID proves the retry boundary: the same
// notification key returns the original row even after the producer resolves a
// different session, while a new key under the same content is its own
// notification.
func TestPostgresRetryKeepsFrozenTargetAndID(t *testing.T) {
	pool := openDeliveryTestPool(t)
	store := NewPostgres(db.New(pool))
	workspaceID := uuid.NewString()
	recipientID := uuid.NewString()

	first := genericRequest(workspaceID, recipientID)
	first.Target.SessionID = uuid.NewString()
	stored, err := store.Enqueue(context.Background(), first)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	cleanupDelivery(t, pool, stored.ID)

	rebound := first
	rebound.Target.SessionID = uuid.NewString()
	replayed, err := store.Enqueue(context.Background(), rebound)
	if err != nil {
		t.Fatalf("replay after rebind: %v", err)
	}
	if replayed.ID != stored.ID {
		t.Fatalf("replayed id = %s, want frozen original %s", replayed.ID, stored.ID)
	}
	if replayed.Target.SessionID != first.Target.SessionID {
		t.Fatalf("replayed target = %s, want frozen %s", replayed.Target.SessionID, first.Target.SessionID)
	}

	next := first
	next.NotificationKey = "ci:run-42-attempt-2"
	next.Target.SessionID = rebound.Target.SessionID
	second, err := store.Enqueue(context.Background(), next)
	if err != nil {
		t.Fatalf("new notification enqueue: %v", err)
	}
	cleanupDelivery(t, pool, second.ID)
	if second.ID == stored.ID {
		t.Fatal("a new notification reused the earlier delivery id")
	}
	if second.Target.SessionID != rebound.Target.SessionID {
		t.Fatalf("new notification target = %s, want %s", second.Target.SessionID, rebound.Target.SessionID)
	}
}

// TestPostgresEnqueueRejectsNotificationKeyCollision covers the physical
// uniqueness boundary: one (workspace, notification_key) may only answer the
// notification that created it. A producer that reuses a key for another
// recipient or another source event must fail instead of receiving that stored
// row, and the failed attempt must neither rewrite nor duplicate it.
func TestPostgresEnqueueRejectsNotificationKeyCollision(t *testing.T) {
	pool := openDeliveryTestPool(t)
	store := NewPostgres(db.New(pool))
	workspaceID := uuid.NewString()
	recipientID := uuid.NewString()

	first := genericRequest(workspaceID, recipientID)
	first.Target.SessionID = uuid.NewString()
	stored, err := store.Enqueue(context.Background(), first)
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	cleanupDelivery(t, pool, stored.ID)

	tests := []struct {
		name   string
		mutate func(*EnqueueRequest)
	}{
		{name: "different recipient", mutate: func(r *EnqueueRequest) {
			r.Recipient.ID = uuid.NewString()
		}},
		{name: "different recipient type", mutate: func(r *EnqueueRequest) {
			r.Recipient.Type = "squad"
		}},
		{name: "different source type", mutate: func(r *EnqueueRequest) {
			r.Source.Type = "issue_wakeup"
		}},
		{name: "different source key", mutate: func(r *EnqueueRequest) {
			r.Source.Key = "run-42-attempt-2"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := first
			tt.mutate(&req)
			_, err := store.Enqueue(context.Background(), req)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("enqueue with a colliding key = %v, want ErrConflict", err)
			}

			var rows int
			if err := pool.QueryRow(context.Background(),
				`SELECT count(*) FROM agent_mailbox_message WHERE workspace_id = $1 AND notification_key = $2`,
				util.MustParseUUID(workspaceID), stored.NotificationKey).Scan(&rows); err != nil {
				t.Fatalf("count rows for the key: %v", err)
			}
			if rows != 1 {
				t.Fatalf("rows under the notification key = %d, want 1", rows)
			}

			got, err := store.Get(context.Background(), Scope{WorkspaceID: workspaceID}, stored.ID)
			if err != nil {
				t.Fatalf("read original delivery: %v", err)
			}
			if got.Recipient != first.Recipient {
				t.Fatalf("original recipient = %+v, want %+v", got.Recipient, first.Recipient)
			}
			if got.Source != first.Source {
				t.Fatalf("original source = %+v, want %+v", got.Source, first.Source)
			}
		})
	}

	// The control keeps route-only retry semantics: the same notification that
	// now resolves another session still replays the frozen original.
	rebound := first
	rebound.Target.SessionID = uuid.NewString()
	replayed, err := store.Enqueue(context.Background(), rebound)
	if err != nil {
		t.Fatalf("retry after rebind: %v", err)
	}
	if replayed.ID != stored.ID || replayed.Target.SessionID != first.Target.SessionID {
		t.Fatalf("replayed = %s/%s, want frozen %s/%s",
			replayed.ID, replayed.Target.SessionID, stored.ID, first.Target.SessionID)
	}
}

// TestPostgresEnqueueRecoversLegacyCommentID proves rows written before the
// core keep their original mailbox UUID: a replay resolves through the legacy
// (recipient, comment) key instead of inserting a second request.
func TestPostgresEnqueueRecoversLegacyCommentID(t *testing.T) {
	pool := openDeliveryTestPool(t)
	queries := db.New(pool)
	store := NewPostgres(queries)
	workspaceID := uuid.NewString()
	recipientID := uuid.NewString()
	issueID := uuid.NewString()
	commentID := uuid.NewString()

	legacy, err := queries.CreateAgentMailboxMessage(context.Background(), db.CreateAgentMailboxMessageParams{
		WorkspaceID:      util.MustParseUUID(workspaceID),
		IssueID:          util.MustParseUUID(issueID),
		CommentID:        util.MustParseUUID(commentID),
		RecipientAgentID: util.MustParseUUID(recipientID),
		SenderType:       pgtype.Text{String: "member", Valid: true},
		SenderID:         util.MustParseUUID(uuid.NewString()),
	})
	if err != nil {
		t.Fatalf("create legacy row: %v", err)
	}
	cleanupDelivery(t, pool, util.UUIDToString(legacy.ID))

	payload := json.RawMessage(`{"schema":"comment_mention.v1","issue_id":"` + issueID + `","comment_id":"` + commentID + `"}`)
	recovered, err := store.Enqueue(context.Background(), EnqueueRequest{
		Scope:           Scope{WorkspaceID: workspaceID},
		Source:          Source{Type: "comment", Key: commentID, ActorType: "member", ActorID: util.UUIDToString(legacy.SenderID)},
		Target:          Target{SessionID: uuid.NewString()},
		Recipient:       Recipient{Type: "agent", ID: recipientID},
		NotificationKey: "comment:" + commentID + ":agent:" + recipientID,
		Kind:            "comment_mention",
		Payload:         payload,
		Refs:            Refs{IssueID: issueID, CommentID: commentID},
	})
	if err != nil {
		t.Fatalf("enqueue replay: %v", err)
	}
	if recovered.ID != util.UUIDToString(legacy.ID) {
		t.Fatalf("recovered id = %s, want legacy %s", recovered.ID, util.UUIDToString(legacy.ID))
	}
}
