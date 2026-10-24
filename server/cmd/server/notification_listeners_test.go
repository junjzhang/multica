package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// notificationTest helpers — reuse the integration test fixtures from TestMain
// (testPool, testUserID, testWorkspaceID are set in integration_test.go).

// createNotificationMember gives delivery controls real workspace membership.
// A user account alone is not an eligible notification recipient.
func createNotificationMember(t *testing.T, email string) string {
	t.Helper()
	id := createTestUser(t, email)
	workspaceFixture(t).Member(t, testWorkspaceID, id, "member")
	return id
}

// inboxItemsForRecipient returns all non-archived inbox items for a given recipient.
func inboxItemsForRecipient(t *testing.T, queries *db.Queries, recipientID string) []db.ListInboxItemsRow {
	t.Helper()
	items, err := queries.ListInboxItems(context.Background(), db.ListInboxItemsParams{
		WorkspaceID:   util.MustParseUUID(testWorkspaceID),
		RecipientType: "member",
		RecipientID:   util.MustParseUUID(recipientID),
	})
	if err != nil {
		t.Fatalf("ListInboxItems: %v", err)
	}
	return items
}

// cleanupInboxForIssue deletes all inbox items related to a given issue.
func cleanupInboxForIssue(t *testing.T, issueID string) {
	t.Helper()
	testPool.Exec(context.Background(), `DELETE FROM inbox_item WHERE issue_id = $1`, issueID)
}

// addTestSubscriber manually inserts a subscriber for an issue.
func addTestSubscriber(t *testing.T, issueID, userType, userID, reason string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(), `
		INSERT INTO issue_subscriber (issue_id, user_type, user_id, reason)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (issue_id, user_type, user_id) DO NOTHING
	`, issueID, userType, userID, reason)
	if err != nil {
		t.Fatalf("addTestSubscriber: %v", err)
	}
}

// assignTestIssue sets the issue's authoritative owner without touching
// subscription reasons, mirroring a reassignment that leaves stale rows.
func assignTestIssue(t *testing.T, issueID, memberID string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`UPDATE issue SET assignee_type = 'member', assignee_id = $2 WHERE id = $1`,
		issueID, memberID)
	if err != nil {
		t.Fatalf("assignTestIssue: %v", err)
	}
}

// createTestSubIssue inserts an issue with parent_issue_id set and returns its UUID.
// Picks the next per-workspace number to avoid colliding with the
// uq_issue_workspace_number unique constraint (parent + sub created in the
// same test would otherwise both default to number=0).
func createTestSubIssue(t *testing.T, workspaceID, creatorID, parentIssueID string) string {
	t.Helper()
	ctx := context.Background()
	var issueID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, position, parent_issue_id, number)
		VALUES ($1, 'sub-issue test', 'todo', 'medium', 'member', $2, 0, $3,
		        (SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1))
		RETURNING id
	`, workspaceID, creatorID, parentIssueID).Scan(&issueID)
	if err != nil {
		t.Fatalf("createTestSubIssue: %v", err)
	}
	return issueID
}

// newNotificationBus creates a bus with subscriber + notification listeners registered.
func newNotificationBus(t *testing.T, queries *db.Queries) *events.Bus {
	t.Helper()
	bus := events.New()
	registerSubscriberListeners(bus, testPool)
	registerNotificationListeners(bus, queries)
	return bus
}

// TestNotification_IssueCreated_AssigneeNotified verifies that when an issue is
// created with an assignee different from the creator, the assignee receives an
// "issue_assigned" inbox notification and the creator receives nothing.
func TestNotification_IssueCreated_AssigneeNotified(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	assigneeEmail := "notif-assignee-created@multica.ai"
	assigneeID := createNotificationMember(t, assigneeEmail)
	t.Cleanup(func() { cleanupTestUser(t, assigneeEmail) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	// Track inbox:new events
	var inboxEvents []events.Event
	bus.Subscribe(protocol.EventInboxNew, func(e events.Event) {
		inboxEvents = append(inboxEvents, e)
	})

	assigneeType := "member"
	bus.Publish(events.Event{
		Type:        protocol.EventIssueCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:           issueID,
				WorkspaceID:  testWorkspaceID,
				Title:        "notif test issue",
				Status:       "todo",
				Priority:     "medium",
				CreatorType:  "member",
				CreatorID:    testUserID,
				AssigneeType: &assigneeType,
				AssigneeID:   &assigneeID,
			},
		},
	})

	// Assignee should have an inbox item
	items := inboxItemsForRecipient(t, queries, assigneeID)
	if len(items) != 1 {
		t.Fatalf("expected 1 inbox item for assignee, got %d", len(items))
	}
	if items[0].Type != "issue_assigned" {
		t.Fatalf("expected type 'issue_assigned', got %q", items[0].Type)
	}
	if items[0].Severity != "action_required" {
		t.Fatalf("expected severity 'action_required', got %q", items[0].Severity)
	}

	// Creator (actor) should NOT have any inbox items
	creatorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(creatorItems) != 0 {
		t.Fatalf("expected 0 inbox items for creator, got %d", len(creatorItems))
	}

	// At least one inbox:new event should have been published
	if len(inboxEvents) < 1 {
		t.Fatal("expected at least 1 inbox:new event")
	}
}

// TestNotification_IssueCreated_SelfAssign verifies that when the creator
// assigns the issue to themselves, no notification is generated.
func TestNotification_IssueCreated_SelfAssign(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	var inboxEvents []events.Event
	bus.Subscribe(protocol.EventInboxNew, func(e events.Event) {
		inboxEvents = append(inboxEvents, e)
	})

	assigneeType := "member"
	assigneeID := testUserID // self-assign
	bus.Publish(events.Event{
		Type:        protocol.EventIssueCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:           issueID,
				WorkspaceID:  testWorkspaceID,
				Title:        "self-assign issue",
				Status:       "todo",
				Priority:     "medium",
				CreatorType:  "member",
				CreatorID:    testUserID,
				AssigneeType: &assigneeType,
				AssigneeID:   &assigneeID,
			},
		},
	})

	items := inboxItemsForRecipient(t, queries, testUserID)
	if len(items) != 0 {
		t.Fatalf("expected 0 inbox items for self-assign, got %d", len(items))
	}
	if len(inboxEvents) != 0 {
		t.Fatalf("expected 0 inbox:new events for self-assign, got %d", len(inboxEvents))
	}
}

// TestNotification_IssueCreated_NoAssignee verifies that when an issue is
// created without an assignee, no notifications are generated.
func TestNotification_IssueCreated_NoAssignee(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	var inboxEvents []events.Event
	bus.Subscribe(protocol.EventInboxNew, func(e events.Event) {
		inboxEvents = append(inboxEvents, e)
	})

	bus.Publish(events.Event{
		Type:        protocol.EventIssueCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "no assignee issue",
				Status:      "todo",
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
			},
		},
	})

	items := inboxItemsForRecipient(t, queries, testUserID)
	if len(items) != 0 {
		t.Fatalf("expected 0 inbox items for no-assignee issue, got %d", len(items))
	}
	if len(inboxEvents) != 0 {
		t.Fatalf("expected 0 inbox:new events, got %d", len(inboxEvents))
	}
}

// TestNotification_StatusChanged verifies that all subscribers except the actor
// receive a "status_changed" notification when an issue status changes.
func TestNotification_StatusChanged(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	// Create two extra users as subscribers
	sub1Email := "notif-sub1-status@multica.ai"
	sub1ID := createNotificationMember(t, sub1Email)
	t.Cleanup(func() { cleanupTestUser(t, sub1Email) })

	sub2Email := "notif-sub2-status@multica.ai"
	sub2ID := createNotificationMember(t, sub2Email)
	t.Cleanup(func() { cleanupTestUser(t, sub2Email) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	// Manually add subscribers before the event fires
	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "member", sub1ID, "assignee")
	addTestSubscriber(t, issueID, "member", sub2ID, "commenter")

	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID, // actor is the creator
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "status test issue",
				Status:      "in_progress",
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
			},
			"assignee_changed": false,
			"status_changed":   true,
			"prev_status":      "todo",
		},
	})

	// Actor (testUserID) should NOT get a notification
	actorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(actorItems) != 0 {
		t.Fatalf("expected 0 inbox items for actor, got %d", len(actorItems))
	}

	// sub1 should get a status_changed notification
	sub1Items := inboxItemsForRecipient(t, queries, sub1ID)
	if len(sub1Items) != 1 {
		t.Fatalf("expected 1 inbox item for sub1, got %d", len(sub1Items))
	}
	if sub1Items[0].Type != "status_changed" {
		t.Fatalf("expected type 'status_changed', got %q", sub1Items[0].Type)
	}
	if sub1Items[0].Severity != "info" {
		t.Fatalf("expected severity 'info', got %q", sub1Items[0].Severity)
	}
	// Title is now just the issue title; details contain from/to
	expectedTitle := "status test issue"
	if sub1Items[0].Title != expectedTitle {
		t.Fatalf("expected title %q, got %q", expectedTitle, sub1Items[0].Title)
	}

	// sub2 should also get a status_changed notification
	sub2Items := inboxItemsForRecipient(t, queries, sub2ID)
	if len(sub2Items) != 1 {
		t.Fatalf("expected 1 inbox item for sub2, got %d", len(sub2Items))
	}
	if sub2Items[0].Type != "status_changed" {
		t.Fatalf("expected type 'status_changed', got %q", sub2Items[0].Type)
	}
}

// TestNotification_StatusChanged_Muted verifies that status_changes="muted"
// prevents a new Inbox row without affecting other subscribers.
func TestNotification_StatusChanged_Muted(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	mutedEmail := "notif-muted-status@multica.ai"
	mutedID := createNotificationMember(t, mutedEmail)
	t.Cleanup(func() { cleanupTestUser(t, mutedEmail) })

	normalEmail := "notif-normal-status@multica.ai"
	normalID := createNotificationMember(t, normalEmail)
	t.Cleanup(func() { cleanupTestUser(t, normalEmail) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO notification_preference (workspace_id, user_id, preferences)
		VALUES ($1, $2, '{"status_changes":"muted"}'::jsonb)
	`, testWorkspaceID, mutedID); err != nil {
		t.Fatalf("seed muted notification preference: %v", err)
	}

	addTestSubscriber(t, issueID, "member", mutedID, "commenter")
	addTestSubscriber(t, issueID, "member", normalID, "commenter")

	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "muted status test issue",
				Status:      "in_progress",
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
			},
			"assignee_changed": false,
			"status_changed":   true,
			"prev_status":      "todo",
		},
	})

	if items := inboxItemsForRecipient(t, queries, mutedID); len(items) != 0 {
		t.Fatalf("expected muted subscriber to receive 0 items, got %d", len(items))
	}
	if items := inboxItemsForRecipient(t, queries, normalID); len(items) != 1 {
		t.Fatalf("expected normal subscriber to receive 1 item, got %d", len(items))
	}
}

// TestNotification_CommentCreated verifies that all subscribers except the
// commenter receive a "new_comment" notification.
func TestNotification_CommentCreated(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	commenterEmail := "notif-commenter@multica.ai"
	commenterID := createNotificationMember(t, commenterEmail)
	t.Cleanup(func() { cleanupTestUser(t, commenterEmail) })

	sub1Email := "notif-sub1-comment@multica.ai"
	sub1ID := createNotificationMember(t, sub1Email)
	t.Cleanup(func() { cleanupTestUser(t, sub1Email) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	// Pre-add subscribers: creator and sub1. The commenter will also be added
	// by subscriber_listeners when the event fires.
	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "member", sub1ID, "assignee")

	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     commenterID, // commenter is the actor
		Payload: map[string]any{
			"comment": handler.CommentResponse{
				ID:         "00000000-0000-0000-0000-000000000000",
				IssueID:    issueID,
				AuthorType: "member",
				AuthorID:   commenterID,
				Content:    "test comment content",
				Type:       "comment",
			},
			"issue_title":  "comment test issue",
			"issue_status": "todo",
		},
	})

	// Creator should get a new_comment notification
	creatorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(creatorItems) != 1 {
		t.Fatalf("expected 1 inbox item for creator, got %d", len(creatorItems))
	}
	if creatorItems[0].Type != "new_comment" {
		t.Fatalf("expected type 'new_comment', got %q", creatorItems[0].Type)
	}
	if creatorItems[0].Severity != "info" {
		t.Fatalf("expected severity 'info', got %q", creatorItems[0].Severity)
	}

	// sub1 should also get a new_comment notification
	sub1Items := inboxItemsForRecipient(t, queries, sub1ID)
	if len(sub1Items) != 1 {
		t.Fatalf("expected 1 inbox item for sub1, got %d", len(sub1Items))
	}
	if sub1Items[0].Type != "new_comment" {
		t.Fatalf("expected type 'new_comment', got %q", sub1Items[0].Type)
	}

	// Commenter (actor) should NOT get a notification
	commenterItems := inboxItemsForRecipient(t, queries, commenterID)
	if len(commenterItems) != 0 {
		t.Fatalf("expected 0 inbox items for commenter, got %d", len(commenterItems))
	}
}

// TestNotification_SystemCommentSkipsInboxAndMentions guards the MUL-2538
// must-fix: a comment with author_type='system' (the platform-generated
// child-done parent notify) must NOT create any inbox rows for parent
// subscribers and must NOT spawn mention-inbox rows even if the body string
// contains markdown mentions. The reviewer's concern was that a child title
// containing `mention://member/<uuid>` would silently light up that member's
// inbox once the title was transcluded into the system comment body —
// because the generic comment:created listener treated all comments
// identically. The fix is to gate at author_type='system'.
func TestNotification_SystemCommentSkipsInboxAndMentions(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	// Subscriber on the issue who would normally receive new_comment.
	subEmail := "notif-system-comment-sub@multica.ai"
	subID := createNotificationMember(t, subEmail)
	t.Cleanup(func() { cleanupTestUser(t, subEmail) })

	// A second member whose UUID we will smuggle into the system-comment
	// body as a fake mention to prove the listener does not parse it.
	targetEmail := "notif-system-comment-target@multica.ai"
	targetID := createNotificationMember(t, targetEmail)
	t.Cleanup(func() { cleanupTestUser(t, targetEmail) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	addTestSubscriber(t, issueID, "member", subID, "manual")

	// Publish a system-authored comment that transcludes a member mention
	// in the body — the exact attack vector the reviewer flagged. If the
	// generic listener path runs, the new_comment row will fire for `sub`
	// and the mention path will fire for `target`.
	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"comment": handler.CommentResponse{
				ID:         "00000000-0000-0000-0000-000000000000",
				IssueID:    issueID,
				AuthorType: "system",
				AuthorID:   "00000000-0000-0000-0000-000000000000",
				Content:    "Sub-issue done — see [@Target](mention://member/" + targetID + ").",
				Type:       "system",
			},
			"issue_title":  "system comment isolation",
			"issue_status": "in_progress",
		},
	})

	if items := inboxItemsForRecipient(t, queries, subID); len(items) != 0 {
		t.Errorf("expected 0 inbox rows for issue subscriber, got %d", len(items))
	}
	if items := inboxItemsForRecipient(t, queries, targetID); len(items) != 0 {
		t.Errorf("expected 0 inbox rows for smuggled @mention target, got %d", len(items))
	}
}

// TestSubscriberSystemCommentDoesNotSubscribe guards the same boundary on
// the subscriber listener: a system-authored comment must NOT be treated as
// "a commenter joined the conversation." The CHECK constraint on
// issue_subscriber.user_type only permits ('member','agent'); without the
// author_type='system' early-return, AddIssueSubscriber would log a noisy
// constraint violation on every child-done event.
func TestSubscriberSystemCommentDoesNotSubscribe(t *testing.T) {
	queries := db.New(testPool)
	bus := events.New()
	registerSubscriberListeners(bus, testPool)

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() { cleanupTestIssue(t, issueID) })

	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"comment": handler.CommentResponse{
				ID:         "00000000-0000-0000-0000-000000000000",
				IssueID:    issueID,
				AuthorType: "system",
				AuthorID:   "00000000-0000-0000-0000-000000000000",
				Content:    "platform notify",
				Type:       "system",
			},
		},
	})

	if count := subscriberCount(t, queries, issueID); count != 0 {
		t.Fatalf("expected 0 subscribers after system comment, got %d", count)
	}
}

// TestNotification_AssigneeChanged verifies the full assignee change flow:
// - New assignee gets "issue_assigned" (Direct)
// - Old assignee gets "unassigned" (Direct)
// - Other subscribers get "assignee_changed" (Subscriber), excluding actor + old + new
// - Actor gets nothing
func TestNotification_AssigneeChanged(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	oldAssigneeEmail := "notif-old-assignee@multica.ai"
	oldAssigneeID := createNotificationMember(t, oldAssigneeEmail)
	t.Cleanup(func() { cleanupTestUser(t, oldAssigneeEmail) })

	newAssigneeEmail := "notif-new-assignee@multica.ai"
	newAssigneeID := createNotificationMember(t, newAssigneeEmail)
	t.Cleanup(func() { cleanupTestUser(t, newAssigneeEmail) })

	bystanderEmail := "notif-bystander@multica.ai"
	bystanderID := createNotificationMember(t, bystanderEmail)
	t.Cleanup(func() { cleanupTestUser(t, bystanderEmail) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	// Pre-add subscribers: creator, old assignee, bystander
	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "member", oldAssigneeID, "assignee")
	addTestSubscriber(t, issueID, "member", bystanderID, "commenter")

	newAssigneeType := "member"
	oldAssigneeType := "member"
	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID, // actor is the creator
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:           issueID,
				WorkspaceID:  testWorkspaceID,
				Title:        "assignee change issue",
				Status:       "todo",
				Priority:     "medium",
				CreatorType:  "member",
				CreatorID:    testUserID,
				AssigneeType: &newAssigneeType,
				AssigneeID:   &newAssigneeID,
			},
			"assignee_changed":   true,
			"status_changed":     false,
			"prev_assignee_type": &oldAssigneeType,
			"prev_assignee_id":   &oldAssigneeID,
		},
	})

	// New assignee should get "issue_assigned"
	newItems := inboxItemsForRecipient(t, queries, newAssigneeID)
	if len(newItems) != 1 {
		t.Fatalf("expected 1 inbox item for new assignee, got %d", len(newItems))
	}
	if newItems[0].Type != "issue_assigned" {
		t.Fatalf("expected type 'issue_assigned', got %q", newItems[0].Type)
	}
	if newItems[0].Severity != "action_required" {
		t.Fatalf("expected severity 'action_required', got %q", newItems[0].Severity)
	}

	// Old assignee should get "unassigned"
	oldItems := inboxItemsForRecipient(t, queries, oldAssigneeID)
	if len(oldItems) != 1 {
		t.Fatalf("expected 1 inbox item for old assignee, got %d", len(oldItems))
	}
	if oldItems[0].Type != "unassigned" {
		t.Fatalf("expected type 'unassigned', got %q", oldItems[0].Type)
	}
	if oldItems[0].Severity != "info" {
		t.Fatalf("expected severity 'info', got %q", oldItems[0].Severity)
	}

	// Bystander should get "assignee_changed"
	bystanderItems := inboxItemsForRecipient(t, queries, bystanderID)
	if len(bystanderItems) != 1 {
		t.Fatalf("expected 1 inbox item for bystander, got %d", len(bystanderItems))
	}
	if bystanderItems[0].Type != "assignee_changed" {
		t.Fatalf("expected type 'assignee_changed', got %q", bystanderItems[0].Type)
	}
	if bystanderItems[0].Severity != "info" {
		t.Fatalf("expected severity 'info', got %q", bystanderItems[0].Severity)
	}

	// Actor (testUserID / creator) should NOT get any notification
	actorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(actorItems) != 0 {
		t.Fatalf("expected 0 inbox items for actor, got %d", len(actorItems))
	}
}

// TestNotification_TaskCompleted verifies that task:completed events do NOT
// create inbox notifications (completion is visible from the status change).
func TestNotification_TaskCompleted(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	// The agent ID (acting as system actor)
	agentID := "00000000-0000-0000-0000-aaaaaaaaaaaa"

	// Pre-add subscribers: creator and the agent
	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "agent", agentID, "assignee")

	bus.Publish(events.Event{
		Type:        protocol.EventTaskCompleted,
		WorkspaceID: testWorkspaceID,
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"task_id":  "00000000-0000-0000-0000-bbbbbbbbbbbb",
			"agent_id": agentID,
			"issue_id": issueID,
			"status":   "completed",
		},
	})

	// No inbox notification should be created for task:completed
	creatorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(creatorItems) != 0 {
		t.Fatalf("expected 0 inbox items for creator on task:completed, got %d", len(creatorItems))
	}
}

// TestNotification_TaskFailed verifies that subscribers get a "task_failed"
// notification when a task fails, excluding the agent.
func TestNotification_TaskFailed(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	agentID := "00000000-0000-0000-0000-aaaaaaaaaaaa"

	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "agent", agentID, "assignee")

	bus.Publish(events.Event{
		Type:        protocol.EventTaskFailed,
		WorkspaceID: testWorkspaceID,
		ActorType:   "system",
		ActorID:     "",
		Payload: map[string]any{
			"task_id":  "00000000-0000-0000-0000-bbbbbbbbbbbb",
			"agent_id": agentID,
			"issue_id": issueID,
			"status":   "failed",
		},
	})

	creatorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(creatorItems) != 1 {
		t.Fatalf("expected 1 inbox item for creator, got %d", len(creatorItems))
	}
	if creatorItems[0].Type != "task_failed" {
		t.Fatalf("expected type 'task_failed', got %q", creatorItems[0].Type)
	}
	if creatorItems[0].Severity != "action_required" {
		t.Fatalf("expected severity 'action_required', got %q", creatorItems[0].Severity)
	}
}

// TestNotification_PriorityChanged verifies that all subscribers except the actor
// receive a "priority_changed" notification when an issue priority changes.
func TestNotification_PriorityChanged(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	sub1Email := "notif-sub1-priority@multica.ai"
	sub1ID := createNotificationMember(t, sub1Email)
	t.Cleanup(func() { cleanupTestUser(t, sub1Email) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "member", sub1ID, "assignee")

	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "priority test issue",
				Status:      "todo",
				Priority:    "high",
				CreatorType: "member",
				CreatorID:   testUserID,
			},
			"assignee_changed": false,
			"status_changed":   false,
			"priority_changed": true,
			"prev_priority":    "medium",
		},
	})

	// Actor should NOT get a notification
	actorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(actorItems) != 0 {
		t.Fatalf("expected 0 inbox items for actor, got %d", len(actorItems))
	}

	// sub1 should get a priority_changed notification
	sub1Items := inboxItemsForRecipient(t, queries, sub1ID)
	if len(sub1Items) != 1 {
		t.Fatalf("expected 1 inbox item for sub1, got %d", len(sub1Items))
	}
	if sub1Items[0].Type != "priority_changed" {
		t.Fatalf("expected type 'priority_changed', got %q", sub1Items[0].Type)
	}
	if sub1Items[0].Severity != "info" {
		t.Fatalf("expected severity 'info', got %q", sub1Items[0].Severity)
	}
	// Title is now just the issue title; details contain from/to
	expectedTitle := "priority test issue"
	if sub1Items[0].Title != expectedTitle {
		t.Fatalf("expected title %q, got %q", expectedTitle, sub1Items[0].Title)
	}
}

// TestNotification_DueDateChanged verifies that all subscribers except the actor
// receive a "due_date_changed" notification when an issue due date changes.
func TestNotification_DueDateChanged(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	sub1Email := "notif-sub1-duedate@multica.ai"
	sub1ID := createNotificationMember(t, sub1Email)
	t.Cleanup(func() { cleanupTestUser(t, sub1Email) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "member", sub1ID, "assignee")

	dueDate := "2026-04-15"
	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "due date test issue",
				Status:      "todo",
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
				DueDate:     &dueDate,
			},
			"assignee_changed": false,
			"status_changed":   false,
			"due_date_changed": true,
		},
	})

	// Actor should NOT get a notification
	actorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(actorItems) != 0 {
		t.Fatalf("expected 0 inbox items for actor, got %d", len(actorItems))
	}

	// sub1 should get a due_date_changed notification
	sub1Items := inboxItemsForRecipient(t, queries, sub1ID)
	if len(sub1Items) != 1 {
		t.Fatalf("expected 1 inbox item for sub1, got %d", len(sub1Items))
	}
	if sub1Items[0].Type != "due_date_changed" {
		t.Fatalf("expected type 'due_date_changed', got %q", sub1Items[0].Type)
	}
	if sub1Items[0].Severity != "info" {
		t.Fatalf("expected severity 'info', got %q", sub1Items[0].Severity)
	}
}

// TestNotification_StartDateChanged verifies that subscribers (except the actor)
// receive a "start_date_changed" notification when an issue start date changes.
func TestNotification_StartDateChanged(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	sub1Email := "notif-sub1-startdate@multica.ai"
	sub1ID := createNotificationMember(t, sub1Email)
	t.Cleanup(func() { cleanupTestUser(t, sub1Email) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "member", sub1ID, "assignee")

	startDate := "2026-04-01"
	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "start date test issue",
				Status:      "todo",
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
				StartDate:   &startDate,
			},
			"assignee_changed":   false,
			"status_changed":     false,
			"start_date_changed": true,
		},
	})

	// Actor should NOT get a notification
	actorItems := inboxItemsForRecipient(t, queries, testUserID)
	if len(actorItems) != 0 {
		t.Fatalf("expected 0 inbox items for actor, got %d", len(actorItems))
	}

	sub1Items := inboxItemsForRecipient(t, queries, sub1ID)
	if len(sub1Items) != 1 {
		t.Fatalf("expected 1 inbox item for sub1, got %d", len(sub1Items))
	}
	if sub1Items[0].Type != "start_date_changed" {
		t.Fatalf("expected type 'start_date_changed', got %q", sub1Items[0].Type)
	}
	if sub1Items[0].Severity != "info" {
		t.Fatalf("expected severity 'info', got %q", sub1Items[0].Severity)
	}
}

// TestNotification_ParentBubble_StatusChanged verifies that a HANDOFF status
// change on a sub-issue bubbles to subscribers of the parent issue. Routine
// child progress is covered by
// TestNotification_ParentManualSubscriptionDoesNotInheritRoutineChildActivity.
func TestNotification_ParentBubble_StatusChanged(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	parentSubEmail := "notif-parent-sub-status@multica.ai"
	parentSubID := createNotificationMember(t, parentSubEmail)
	t.Cleanup(func() { cleanupTestUser(t, parentSubEmail) })

	parentID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, parentID)
		cleanupTestIssue(t, parentID)
	})
	subID := createTestSubIssue(t, testWorkspaceID, testUserID, parentID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, subID)
		cleanupTestIssue(t, subID)
	})

	// Subscribe a watcher to the parent only — they should hear about
	// handoff status changes on the sub-issue.
	addTestSubscriber(t, parentID, "member", parentSubID, "manual")

	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          subID,
				WorkspaceID: testWorkspaceID,
				Title:       "sub-issue status bubble",
				Status:      "blocked",
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
			},
			"assignee_changed": false,
			"status_changed":   true,
			"prev_status":      "in_progress",
		},
	})

	items := inboxItemsForRecipient(t, queries, parentSubID)
	if len(items) != 1 {
		t.Fatalf("expected 1 inbox item bubbled to parent subscriber, got %d", len(items))
	}
	if items[0].Type != "status_changed" {
		t.Fatalf("expected type 'status_changed', got %q", items[0].Type)
	}
	// The inbox item should point to the sub-issue, not the parent.
	if util.UUIDToString(items[0].IssueID) != subID {
		t.Fatalf("expected inbox item issue_id=%s (sub-issue), got %s",
			subID, util.UUIDToString(items[0].IssueID))
	}
}

// TestNotification_ParentBubble_NewCommentSuppressed verifies that comments
// on a sub-issue do NOT bubble to subscribers of the parent issue. Comments
// are the loudest signal and we explicitly want to keep them off the parent
// watcher's inbox.
func TestNotification_ParentBubble_NewCommentSuppressed(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	commenterEmail := "notif-parent-bubble-commenter@multica.ai"
	commenterID := createNotificationMember(t, commenterEmail)
	t.Cleanup(func() { cleanupTestUser(t, commenterEmail) })

	parentSubEmail := "notif-parent-sub-comment@multica.ai"
	parentSubID := createNotificationMember(t, parentSubEmail)
	t.Cleanup(func() { cleanupTestUser(t, parentSubEmail) })

	parentID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, parentID)
		cleanupTestIssue(t, parentID)
	})
	subID := createTestSubIssue(t, testWorkspaceID, testUserID, parentID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, subID)
		cleanupTestIssue(t, subID)
	})

	addTestSubscriber(t, parentID, "member", parentSubID, "manual")

	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     commenterID,
		Payload: map[string]any{
			"comment": handler.CommentResponse{
				ID:         "00000000-0000-0000-0000-000000000000",
				IssueID:    subID,
				AuthorType: "member",
				AuthorID:   commenterID,
				Content:    "comment on sub-issue",
				Type:       "comment",
			},
			"issue_title":  "sub-issue comment bubble",
			"issue_status": "todo",
		},
	})

	items := inboxItemsForRecipient(t, queries, parentSubID)
	if len(items) != 0 {
		t.Fatalf("expected 0 inbox items bubbled to parent subscriber for new_comment, got %d", len(items))
	}
}

// TestNotification_ParentBubble_PriorityChangeSuppressed verifies that a
// priority change on a sub-issue does NOT bubble to parent subscribers.
func TestNotification_ParentBubble_PriorityChangeSuppressed(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	parentSubEmail := "notif-parent-sub-priority@multica.ai"
	parentSubID := createNotificationMember(t, parentSubEmail)
	t.Cleanup(func() { cleanupTestUser(t, parentSubEmail) })

	parentID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, parentID)
		cleanupTestIssue(t, parentID)
	})
	subID := createTestSubIssue(t, testWorkspaceID, testUserID, parentID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, subID)
		cleanupTestIssue(t, subID)
	})

	addTestSubscriber(t, parentID, "member", parentSubID, "manual")

	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          subID,
				WorkspaceID: testWorkspaceID,
				Title:       "sub-issue priority bubble",
				Status:      "todo",
				Priority:    "high",
				CreatorType: "member",
				CreatorID:   testUserID,
			},
			"assignee_changed": false,
			"status_changed":   false,
			"priority_changed": true,
			"prev_priority":    "medium",
		},
	})

	items := inboxItemsForRecipient(t, queries, parentSubID)
	if len(items) != 0 {
		t.Fatalf("expected 0 inbox items bubbled to parent subscriber for priority_changed, got %d", len(items))
	}
}

// countInboxByTypeForRecipient counts inbox rows of a given type for a
// recipient, including archived rows. Used to distinguish "row never created"
// from "row archived."
func countInboxByTypeForRecipient(t *testing.T, recipientID, notifType string) (active, archived int) {
	t.Helper()
	rows, err := testPool.Query(context.Background(), `
		SELECT archived FROM inbox_item
		WHERE workspace_id = $1 AND recipient_type = 'member' AND recipient_id = $2 AND type = $3
	`, testWorkspaceID, recipientID, notifType)
	if err != nil {
		t.Fatalf("countInboxByTypeForRecipient: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var isArchived bool
		if err := rows.Scan(&isArchived); err != nil {
			t.Fatalf("countInboxByTypeForRecipient scan: %v", err)
		}
		if isArchived {
			archived++
		} else {
			active++
		}
	}
	return active, archived
}

// publishStatusChange is a small helper to publish the issue:updated event
// shape used by the notification listener for status-only transitions.
func publishStatusChange(bus *events.Bus, issueID, newStatus, prevStatus string) {
	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "task_failed dismiss test",
				Status:      newStatus,
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
			},
			"assignee_changed": false,
			"status_changed":   true,
			"prev_status":      prevStatus,
		},
	})
}

// TestNotification_StatusChange_ArchivesStaleTaskFailed verifies that when an
// issue transitions into a terminal status (in_review/done/cancelled), any
// existing task_failed inbox rows for that issue are archived for every
// affected member recipient, an inbox:batch-archived event fires per
// recipient, and sibling notifications on the same issue are untouched.
func TestNotification_StatusChange_ArchivesStaleTaskFailed(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	subEmail := "notif-archive-task-failed-sub@multica.ai"
	subID := createNotificationMember(t, subEmail)
	t.Cleanup(func() { cleanupTestUser(t, subEmail) })

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	addTestSubscriber(t, issueID, "member", testUserID, "creator")
	addTestSubscriber(t, issueID, "member", subID, "assignee")

	agentID := "00000000-0000-0000-0000-aaaaaaaaaaaa"

	// Two failed runs land before the status flip.
	for i := 0; i < 2; i++ {
		bus.Publish(events.Event{
			Type:        protocol.EventTaskFailed,
			WorkspaceID: testWorkspaceID,
			ActorType:   "system",
			Payload: map[string]any{
				"task_id":  "00000000-0000-0000-0000-bbbbbbbbbbbb",
				"agent_id": agentID,
				"issue_id": issueID,
			},
		})
	}

	// A separate non-task notification on the same issue, so we can prove
	// the archive scope is narrow. Use a comment-like notification by
	// directly inserting a row of a different type.
	_, err := testPool.Exec(context.Background(), `
		INSERT INTO inbox_item (workspace_id, recipient_type, recipient_id, type, severity, issue_id, title, details)
		VALUES ($1, 'member', $2, 'new_comment', 'info', $3, 'sibling notification', '{}')
	`, testWorkspaceID, testUserID, issueID)
	if err != nil {
		t.Fatalf("seed sibling notification: %v", err)
	}

	if active, _ := countInboxByTypeForRecipient(t, testUserID, "task_failed"); active != 2 {
		t.Fatalf("precondition: expected 2 active task_failed rows for creator, got %d", active)
	}
	if active, _ := countInboxByTypeForRecipient(t, subID, "task_failed"); active != 2 {
		t.Fatalf("precondition: expected 2 active task_failed rows for sub, got %d", active)
	}

	// Track the batch-archived events fired during the status change.
	var batchArchived []events.Event
	bus.Subscribe(protocol.EventInboxBatchArchived, func(e events.Event) {
		batchArchived = append(batchArchived, e)
	})

	publishStatusChange(bus, issueID, "in_review", "in_progress")

	// task_failed rows are archived for both recipients.
	for _, recipient := range []string{testUserID, subID} {
		active, archived := countInboxByTypeForRecipient(t, recipient, "task_failed")
		if active != 0 {
			t.Fatalf("recipient %s: expected 0 active task_failed rows after terminal status, got %d", recipient, active)
		}
		if archived != 2 {
			t.Fatalf("recipient %s: expected 2 archived task_failed rows after terminal status, got %d", recipient, archived)
		}
	}

	// Sibling notification on the same issue is untouched.
	if active, _ := countInboxByTypeForRecipient(t, testUserID, "new_comment"); active != 1 {
		t.Fatalf("expected sibling new_comment row to remain active, got %d active", active)
	}

	// One inbox:batch-archived event per affected recipient.
	if len(batchArchived) != 2 {
		t.Fatalf("expected 2 inbox:batch-archived events (one per recipient), got %d", len(batchArchived))
	}
	seenRecipients := map[string]bool{}
	for _, e := range batchArchived {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			t.Fatalf("inbox:batch-archived: unexpected payload type %T", e.Payload)
		}
		recipientID, _ := payload["recipient_id"].(string)
		if recipientID == "" {
			t.Fatalf("inbox:batch-archived: missing recipient_id in payload %+v", payload)
		}
		if payload["issue_id"] != issueID {
			t.Fatalf("inbox:batch-archived: expected issue_id %q, got %v", issueID, payload["issue_id"])
		}
		if payload["reason"] != "issue_status_terminal" {
			t.Fatalf("inbox:batch-archived: expected reason 'issue_status_terminal', got %v", payload["reason"])
		}
		if count, _ := payload["count"].(int64); count != 2 {
			t.Fatalf("inbox:batch-archived: expected count=2 for recipient %s, got %v", recipientID, payload["count"])
		}
		seenRecipients[recipientID] = true
	}
	if !seenRecipients[testUserID] || !seenRecipients[subID] {
		t.Fatalf("expected batch-archived events for both creator and sub, got %v", seenRecipients)
	}
}

// TestNotification_StatusChange_NonTerminalKeepsTaskFailed verifies that a
// transition to a non-terminal status (e.g. in_progress) does NOT archive
// existing task_failed inbox rows.
func TestNotification_StatusChange_NonTerminalKeepsTaskFailed(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	addTestSubscriber(t, issueID, "member", testUserID, "creator")

	bus.Publish(events.Event{
		Type:        protocol.EventTaskFailed,
		WorkspaceID: testWorkspaceID,
		ActorType:   "system",
		Payload: map[string]any{
			"task_id":  "00000000-0000-0000-0000-bbbbbbbbbbbb",
			"agent_id": "00000000-0000-0000-0000-aaaaaaaaaaaa",
			"issue_id": issueID,
		},
	})

	if active, _ := countInboxByTypeForRecipient(t, testUserID, "task_failed"); active != 1 {
		t.Fatalf("precondition: expected 1 active task_failed row, got %d", active)
	}

	publishStatusChange(bus, issueID, "in_progress", "todo")

	// task_failed row stays active because in_progress is not terminal.
	active, archived := countInboxByTypeForRecipient(t, testUserID, "task_failed")
	if active != 1 || archived != 0 {
		t.Fatalf("expected task_failed row to remain active after non-terminal transition, got active=%d archived=%d", active, archived)
	}
}

// TestNotification_StatusChange_ReopenSurfacesNewTaskFailed verifies that
// after a terminal-status auto-archive, a status flip back to in_progress
// followed by a new task failure produces a fresh, visible task_failed row.
// This guards the "reopen and rerun" path described in the design.
func TestNotification_StatusChange_ReopenSurfacesNewTaskFailed(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, issueID)
		cleanupTestIssue(t, issueID)
	})

	addTestSubscriber(t, issueID, "member", testUserID, "creator")

	agentID := "00000000-0000-0000-0000-aaaaaaaaaaaa"

	bus.Publish(events.Event{
		Type:        protocol.EventTaskFailed,
		WorkspaceID: testWorkspaceID,
		ActorType:   "system",
		Payload: map[string]any{
			"task_id":  "00000000-0000-0000-0000-bbbbbbbbbbbb",
			"agent_id": agentID,
			"issue_id": issueID,
		},
	})

	// First terminal transition archives the original failure.
	publishStatusChange(bus, issueID, "in_review", "in_progress")
	if active, archived := countInboxByTypeForRecipient(t, testUserID, "task_failed"); active != 0 || archived != 1 {
		t.Fatalf("after terminal transition: expected active=0 archived=1, got active=%d archived=%d", active, archived)
	}

	// Reviewer kicks the issue back; a rerun fails again.
	publishStatusChange(bus, issueID, "in_progress", "in_review")
	bus.Publish(events.Event{
		Type:        protocol.EventTaskFailed,
		WorkspaceID: testWorkspaceID,
		ActorType:   "system",
		Payload: map[string]any{
			"task_id":  "00000000-0000-0000-0000-cccccccccccc",
			"agent_id": agentID,
			"issue_id": issueID,
		},
	})

	// The new failure is visible; the old archived row stays archived.
	active, archived := countInboxByTypeForRecipient(t, testUserID, "task_failed")
	if active != 1 {
		t.Fatalf("expected 1 active task_failed row after reopen+fail, got %d", active)
	}
	if archived != 1 {
		t.Fatalf("expected 1 archived task_failed row preserved from prior cycle, got %d", archived)
	}
}

// --- Child-issue Inbox quieting -------------------------------------------

// createNotificationWatcher creates a workspace member with a unique email so
// each test starts with an empty Inbox.
func createNotificationWatcher(t *testing.T, tag string) string {
	t.Helper()
	email := fmt.Sprintf("notif-%s-%d@multica.ai", tag, time.Now().UnixNano())
	id := createNotificationMember(t, email)
	t.Cleanup(func() { cleanupTestUser(t, email) })
	return id
}

// createNotificationChild creates a parent and a child issue and returns both.
func createNotificationChild(t *testing.T) (parentID, childID string) {
	t.Helper()
	parentID = createTestIssue(t, testWorkspaceID, testUserID)
	childID = createTestSubIssue(t, testWorkspaceID, testUserID, parentID)
	t.Cleanup(func() {
		cleanupInboxForIssue(t, childID)
		cleanupInboxForIssue(t, parentID)
		cleanupTestIssue(t, childID)
		cleanupTestIssue(t, parentID)
	})
	return parentID, childID
}

// createNotificationCustomStatus creates a workspace status in the given
// lifecycle category, for the custom handoff cases.
func createNotificationCustomStatus(t *testing.T, category string) string {
	t.Helper()
	key := fmt.Sprintf("g%d", time.Now().UnixNano())
	entry, err := db.New(testPool).CreateIssueStatusEntry(context.Background(), db.CreateIssueStatusEntryParams{
		WorkspaceID: util.MustParseUUID(testWorkspaceID),
		Key:         key,
		Name:        key,
		Description: "",
		Category:    category,
		Color:       "#123456",
	})
	if err != nil {
		t.Fatalf("create custom status %q in %q: %v", key, category, err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM issue_status WHERE id = $1`, entry.ID)
	})
	return entry.Key
}

func publishNotificationComment(bus *events.Bus, issueID, authorID, content string) {
	bus.Publish(events.Event{
		Type:        protocol.EventCommentCreated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     authorID,
		Payload: map[string]any{
			"comment": handler.CommentResponse{
				ID:         "00000000-0000-0000-0000-00000000c0de",
				IssueID:    issueID,
				AuthorType: "member",
				AuthorID:   authorID,
				Content:    content,
				Type:       "comment",
			},
			"issue_title":  "child routine activity",
			"issue_status": "todo",
		},
	})
}

func publishNotificationPriorityChange(bus *events.Bus, issueID, status string) {
	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "child routine activity",
				Status:      status,
				Priority:    "high",
				CreatorType: "member",
				CreatorID:   testUserID,
			},
			"priority_changed": true,
			"prev_priority":    "medium",
		},
	})
}

func publishNotificationDueDateChange(bus *events.Bus, issueID, status string) {
	due := "2026-11-01"
	bus.Publish(events.Event{
		Type:        protocol.EventIssueUpdated,
		WorkspaceID: testWorkspaceID,
		ActorType:   "member",
		ActorID:     testUserID,
		Payload: map[string]any{
			"issue": handler.IssueResponse{
				ID:          issueID,
				WorkspaceID: testWorkspaceID,
				Title:       "child routine activity",
				Status:      status,
				Priority:    "medium",
				CreatorType: "member",
				CreatorID:   testUserID,
				DueDate:     &due,
			},
			"due_date_changed": true,
		},
	})
}

func publishNotificationTaskFailed(bus *events.Bus, issueID, agentID string, retryPending bool) {
	bus.Publish(events.Event{
		Type:        protocol.EventTaskFailed,
		WorkspaceID: testWorkspaceID,
		ActorType:   "system",
		Payload: map[string]any{
			"task_id":       "00000000-0000-0000-0000-00000000f411",
			"agent_id":      agentID,
			"issue_id":      issueID,
			"status":        "failed",
			"retry_pending": retryPending,
		},
	})
}

// TestNotification_ChildRoutineActivityStaysQuietForAutomaticWatchers pins the
// Inbox default for sub-issue activity: creator, commenter and delegated
// subscriptions keep seeing the GUI/timeline, but routine progress, review,
// success, comments and field updates create no Inbox row.
func TestNotification_ChildRoutineActivityStaysQuietForAutomaticWatchers(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	_, childID := createNotificationChild(t)

	creatorID := createNotificationWatcher(t, "child-creator")
	commenterID := createNotificationWatcher(t, "child-commenter")
	delegateID := createNotificationWatcher(t, "child-delegate")
	addTestSubscriber(t, childID, "member", creatorID, "creator")
	addTestSubscriber(t, childID, "member", commenterID, "commenter")
	addTestSubscriber(t, childID, "member", delegateID, "delegated")

	inboxEvents := 0
	bus.Subscribe(protocol.EventInboxNew, func(events.Event) { inboxEvents++ })

	publishStatusChange(bus, childID, "in_progress", "todo")
	publishStatusChange(bus, childID, "in_review", "in_progress")
	publishStatusChange(bus, childID, "done", "in_review")
	publishNotificationComment(bus, childID, testUserID, "Routine progress.")
	publishNotificationPriorityChange(bus, childID, "done")
	publishNotificationDueDateChange(bus, childID, "done")

	for reason, recipientID := range map[string]string{
		"creator":   creatorID,
		"commenter": commenterID,
		"delegated": delegateID,
	} {
		if items := inboxItemsForRecipient(t, queries, recipientID); len(items) != 0 {
			t.Fatalf("%s received %d routine child inbox row(s), want 0", reason, len(items))
		}
	}
	if inboxEvents != 0 {
		t.Fatalf("published %d inbox:new event(s) for routine child activity, want 0", inboxEvents)
	}
}

// TestNotification_ChildManualSubscriptionStillDeliversRoutineActivity: a
// manual subscription to the exact child is an explicit human opt-in, so it
// survives the quiet default.
func TestNotification_ChildManualSubscriptionStillDeliversRoutineActivity(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	_, childID := createNotificationChild(t)

	manualID := createNotificationWatcher(t, "child-manual")
	addTestSubscriber(t, childID, "member", manualID, "manual")

	publishStatusChange(bus, childID, "in_review", "in_progress")
	publishNotificationComment(bus, childID, testUserID, "Routine progress.")
	publishNotificationPriorityChange(bus, childID, "in_review")

	items := inboxItemsForRecipient(t, queries, manualID)
	if len(items) != 3 {
		t.Fatalf("manual child subscriber got %d inbox rows, want 3", len(items))
	}
	got := map[string]int{}
	for _, item := range items {
		got[item.Type]++
	}
	for _, want := range []string{"status_changed", "new_comment", "priority_changed"} {
		if got[want] != 1 {
			t.Fatalf("manual child subscriber got %d %q row(s), want 1 (all: %v)", got[want], want, got)
		}
	}
}

// TestNotification_ChildExplicitAssigneeStillReceivesRoutineActivity: the
// child's current human assignee is a direct owner, not a passive watcher.
func TestNotification_ChildExplicitAssigneeStillReceivesRoutineActivity(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	_, childID := createNotificationChild(t)

	assigneeID := createNotificationWatcher(t, "child-assignee")
	addTestSubscriber(t, childID, "member", assigneeID, "assignee")
	assignTestIssue(t, childID, assigneeID)

	publishStatusChange(bus, childID, "in_progress", "todo")
	publishNotificationComment(bus, childID, testUserID, "Routine progress.")

	items := inboxItemsForRecipient(t, queries, assigneeID)
	if len(items) != 2 {
		t.Fatalf("current child assignee got %d inbox rows, want 2", len(items))
	}
	got := map[string]int{}
	for _, item := range items {
		got[item.Type]++
	}
	if got["status_changed"] != 1 || got["new_comment"] != 1 {
		t.Fatalf("current child assignee types = %v, want one status_changed and one new_comment", got)
	}
}

// TestNotification_ChildRoutineActivityReachesExistingWatcherAfterAssignment:
// assignment is authoritative even when the subscription reason predates it
// and AddIssueSubscriber leaves that reason unchanged.
func TestNotification_ChildRoutineActivityReachesExistingWatcherAfterAssignment(t *testing.T) {
	for _, reason := range []string{"creator", "commenter"} {
		t.Run(reason, func(t *testing.T) {
			queries := db.New(testPool)
			bus := newNotificationBus(t, queries)
			_, childID := createNotificationChild(t)

			assigneeID := createNotificationWatcher(t, "child-assigned-"+reason)
			addTestSubscriber(t, childID, "member", assigneeID, reason)
			assignTestIssue(t, childID, assigneeID)
			if _, err := queries.AddIssueSubscriber(context.Background(), db.AddIssueSubscriberParams{
				IssueID:  util.MustParseUUID(childID),
				UserType: "member",
				UserID:   util.MustParseUUID(assigneeID),
				Reason:   "assignee",
			}); err != nil {
				t.Fatalf("add assignee subscriber: %v", err)
			}
			var storedReason string
			if err := testPool.QueryRow(context.Background(),
				`SELECT reason FROM issue_subscriber WHERE issue_id = $1 AND user_id = $2`,
				childID, assigneeID).Scan(&storedReason); err != nil {
				t.Fatalf("read subscriber reason: %v", err)
			}
			if storedReason != reason {
				t.Fatalf("subscriber reason = %q after assignment, want stale %q", storedReason, reason)
			}

			publishStatusChange(bus, childID, "in_progress", "todo")
			publishNotificationComment(bus, childID, testUserID, "Routine progress.")

			items := inboxItemsForRecipient(t, queries, assigneeID)
			if len(items) != 2 {
				t.Fatalf("%s subscriber assigned later got %d inbox rows, want 2", reason, len(items))
			}
			got := map[string]int{}
			for _, item := range items {
				got[item.Type]++
			}
			if got["status_changed"] != 1 || got["new_comment"] != 1 {
				t.Fatalf("%s subscriber assigned later types = %v, want one status_changed and one new_comment", reason, got)
			}
		})
	}
}

// TestNotification_ChildRoutineActivitySkipsFormerAssignee: reassignment
// leaves the old assignee's subscription reason stale; only the current owner
// receives routine activity.
func TestNotification_ChildRoutineActivitySkipsFormerAssignee(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	_, childID := createNotificationChild(t)

	formerID := createNotificationWatcher(t, "child-former-assignee")
	addTestSubscriber(t, childID, "member", formerID, "assignee")
	assignTestIssue(t, childID, formerID)

	currentID := createNotificationWatcher(t, "child-current-assignee")
	addTestSubscriber(t, childID, "member", currentID, "commenter")
	assignTestIssue(t, childID, currentID)

	publishStatusChange(bus, childID, "in_progress", "todo")
	publishNotificationComment(bus, childID, testUserID, "Routine progress.")
	publishNotificationPriorityChange(bus, childID, "in_progress")

	if items := inboxItemsForRecipient(t, queries, formerID); len(items) != 0 {
		t.Fatalf("former assignee got %d routine child inbox rows, want 0", len(items))
	}
	if items := inboxItemsForRecipient(t, queries, currentID); len(items) != 3 {
		t.Fatalf("current assignee with stale commenter reason got %d inbox rows, want 3", len(items))
	}
}

// TestNotification_ParentManualSubscriptionDoesNotInheritRoutineChildActivity:
// a manual subscription to the parent is not opt-in to every child. Progress,
// review and success stay out of the parent watcher's Inbox.
func TestNotification_ParentManualSubscriptionDoesNotInheritRoutineChildActivity(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	parentID, childID := createNotificationChild(t)

	parentWatcherID := createNotificationWatcher(t, "parent-manual")
	addTestSubscriber(t, parentID, "member", parentWatcherID, "manual")

	publishStatusChange(bus, childID, "in_review", "in_progress")
	publishStatusChange(bus, childID, "done", "in_review")

	if items := inboxItemsForRecipient(t, queries, parentWatcherID); len(items) != 0 {
		t.Fatalf("parent manual watcher inherited %d routine child row(s), want 0", len(items))
	}
}

// TestNotification_ChildHumanHandoffsStayLoud: blocked and custom started
// statuses are handoffs (including possible approval requests) and keep
// notifying; a custom done status is routine success and stays quiet.
func TestNotification_ChildHumanHandoffsStayLoud(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	_, childID := createNotificationChild(t)

	creatorID := createNotificationWatcher(t, "child-handoff-creator")
	addTestSubscriber(t, childID, "member", creatorID, "creator")

	customGate := createNotificationCustomStatus(t, "started")
	customShipped := createNotificationCustomStatus(t, "done")

	publishStatusChange(bus, childID, "blocked", "in_progress")
	publishStatusChange(bus, childID, customGate, "blocked")
	publishStatusChange(bus, childID, customShipped, "in_progress")

	items := inboxItemsForRecipient(t, queries, creatorID)
	if len(items) != 2 {
		t.Fatalf("creator got %d handoff rows, want 2 (blocked + custom started)", len(items))
	}
	for _, item := range items {
		if item.Type != "status_changed" {
			t.Fatalf("handoff row type = %q, want status_changed", item.Type)
		}
	}
}

// TestNotification_ChildExplicitMentionStillDelivers: a comment on a child is
// routine for its watchers, but the human addressed by name is not.
func TestNotification_ChildExplicitMentionStillDelivers(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	_, childID := createNotificationChild(t)

	creatorID := createNotificationWatcher(t, "child-mention-creator")
	mentionedID := createNotificationWatcher(t, "child-mentioned")
	addTestSubscriber(t, childID, "member", creatorID, "creator")

	publishNotificationComment(bus, childID, testUserID,
		"[@Mentioned](mention://member/"+mentionedID+") please look")

	if items := inboxItemsForRecipient(t, queries, creatorID); len(items) != 0 {
		t.Fatalf("routine child comment created %d row(s) for the creator, want 0", len(items))
	}
	items := inboxItemsForRecipient(t, queries, mentionedID)
	if len(items) != 1 {
		t.Fatalf("mentioned human got %d inbox rows, want 1", len(items))
	}
	if items[0].Type != "mentioned" {
		t.Fatalf("mentioned human row type = %q, want mentioned", items[0].Type)
	}
}

// TestNotification_ChildFinalFailureDeliversWhileRetryPendingStaysQuiet: an
// intermediate failure with a retry queued still reaches GUI/event consumers
// but not the human Inbox; the terminal attempt does both.
func TestNotification_ChildFinalFailureDeliversWhileRetryPendingStaysQuiet(t *testing.T) {
	queries := db.New(testPool)
	bus := newNotificationBus(t, queries)
	_, childID := createNotificationChild(t)

	creatorID := createNotificationWatcher(t, "child-failure-creator")
	addTestSubscriber(t, childID, "member", creatorID, "creator")

	// Forward through the real WS listener so the assertion covers the frame
	// clients actually receive, not just the in-process event payload.
	fb := &fakeBroadcaster{}
	registerListeners(bus, fb)
	forwarded := func() []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, call := range fb.workspaceCalls {
			var frame struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			}
			if err := json.Unmarshal(call.msg, &frame); err != nil {
				t.Fatalf("decode workspace frame: %v", err)
			}
			if frame.Type == protocol.EventTaskFailed {
				out = append(out, frame.Payload)
			}
		}
		return out
	}

	agentID := "00000000-0000-0000-0000-aaaaaaaaaaaa"

	publishNotificationTaskFailed(bus, childID, agentID, true)
	if items := inboxItemsForRecipient(t, queries, creatorID); len(items) != 0 {
		t.Fatalf("retry-pending failure created %d Inbox row(s), want 0", len(items))
	}
	first := forwarded()
	if len(first) != 1 {
		t.Fatalf("retry-pending failure forwarded to %d event consumers, want 1", len(first))
	}
	if retryPending, _ := first[0]["retry_pending"].(bool); !retryPending {
		t.Fatalf("forwarded payload lost retry_pending=true: %v", first[0])
	}

	publishNotificationTaskFailed(bus, childID, agentID, false)
	items := inboxItemsForRecipient(t, queries, creatorID)
	if len(items) != 1 {
		t.Fatalf("exhausted child failure created %d Inbox rows, want 1", len(items))
	}
	if items[0].Type != "task_failed" || items[0].Severity != "action_required" {
		t.Fatalf("exhausted failure row = type %q severity %q, want task_failed/action_required",
			items[0].Type, items[0].Severity)
	}
	if got := forwarded(); len(got) != 2 {
		t.Fatalf("exhausted failure forwarded to %d event consumers, want 2", len(got))
	}
}
