package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/multica-ai/multica/server/internal/delivery"
	"github.com/multica-ai/multica/server/internal/issueleader"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/squadbriefing"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// AssignmentTarget is the single executor selected by an agent or squad
// assignment. Resolve it inside the write transaction to stabilize the leader.
type AssignmentTarget struct {
	Agent db.Agent
	Squad *db.Squad
}

func ResolveAssignmentTarget(ctx context.Context, q *db.Queries, issue db.Issue) (AssignmentTarget, error) {
	id := issue.AssigneeID
	var squad *db.Squad
	switch issue.AssigneeType.String {
	case "agent":
	case "squad":
		row, err := q.LockSquadForAutopilotAssignment(ctx, db.LockSquadForAutopilotAssignmentParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if err != nil || row.ArchivedAt.Valid {
			return AssignmentTarget{}, &RecipientSessionError{Message: "squad is unavailable"}
		}
		squad = &row
		leader, err := issueleader.ResolveIssueLeader(ctx, q, issue, row)
		if err != nil {
			return AssignmentTarget{}, &RecipientSessionError{Message: err.Error()}
		}
		id = leader.Agent.ID
	default:
		return AssignmentTarget{}, nil
	}
	agent, err := q.LockAgentForAutopilotAssignment(ctx, db.LockAgentForAutopilotAssignmentParams{ID: id, WorkspaceID: issue.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return AssignmentTarget{}, &RecipientSessionError{Message: "assignment agent is unavailable"}
	}
	return AssignmentTarget{Agent: agent, Squad: squad}, nil
}

type RecipientSessionError struct {
	Message   string
	Conflict  bool
	Forbidden bool
}

func (e *RecipientSessionError) Error() string { return e.Message }

// ExternalAssignment persists the recipient binding and launch obligation in
// the issue transaction. Parked assignments may record a validated binding but
// never require one. No managed task is created for an external target.
func ExternalAssignment(ctx context.Context, q *db.Queries, issue db.Issue, target AssignmentTarget, sessionID *string, dispatch bool, actorType, actorID, createdBy string) error {
	if target.Agent.ExecutionKind != "external" {
		if sessionID != nil {
			return &RecipientSessionError{Message: "recipient_session_id applies only to an external agent or external squad leader"}
		}
		return nil
	}
	if sessionID != nil {
		parsed, err := util.ParseUUID(*sessionID)
		if err != nil {
			return &RecipientSessionError{Message: "recipient_session_id must be a registered session UUID"}
		}
		normalized := util.UUIDToString(parsed)
		sessionID = &normalized
	}
	if !dispatch && sessionID == nil {
		return nil
	}
	if target.Squad != nil {
		leader, err := issueleader.ResolveIssueLeader(ctx, q, issue, *target.Squad)
		if err != nil {
			return err
		}
		if leader.SessionID.Valid {
			id := util.UUIDToString(leader.SessionID)
			if sessionID != nil && *sessionID != id {
				return &RecipientSessionError{Message: "recipient session differs from task leader", Conflict: true}
			}
			sessionID = &id
		}
	}
	workspaceID, issueID, agentID := util.UUIDToString(issue.WorkspaceID), util.UUIDToString(issue.ID), util.UUIDToString(target.Agent.ID)
	var binding sessionroute.Binding
	var err error
	if sessionID != nil {
		binding, err = sessionroute.Ensure(ctx, q, sessionroute.BindRequest{WorkspaceID: workspaceID, IssueID: issueID, AgentID: agentID, SessionID: *sessionID, CreatedBy: createdBy})
		if err == nil && binding.SessionID != *sessionID {
			return &RecipientSessionError{Message: "recipient_session_id conflicts with the existing issue binding; use explicit session takeover", Conflict: true}
		}
	} else {
		binding, err = sessionroute.Resolve(ctx, q, workspaceID, agentID, issueID)
	}
	if errors.Is(err, sessionroute.ErrNotBound) {
		return &RecipientSessionError{Message: "recipient_session_id is required for this external assignment; select a registered session or bind this agent on the issue"}
	}
	if errors.Is(err, sessionroute.ErrInvalidBinding) {
		return &RecipientSessionError{Message: err.Error()}
	}
	if err != nil {
		return err
	}
	if err := q.AdoptIssueSessionSemantics(ctx,db.AdoptIssueSessionSemanticsParams{ID:issue.ID,WorkspaceID:issue.WorkspaceID}); err != nil {return err}
	if !dispatch {
		return nil
	}
	// Revision identifies this assignment event. Re-observing it is idempotent;
	// assigning the same agent again after another owner is a new event.
	key := fmt.Sprintf("assignment:%s:%d:agent:%s", issueID, issue.Revision, agentID)
	payload := protocol.ExternalIssueRequest{Schema: "issue_assignment.v1", IssueID: issueID}
	if target.Squad != nil {
		payload.SquadID = util.UUIDToString(target.Squad.ID)
		payload.Briefing = squadbriefing.BuildForIssue(ctx, q, issue, *target.Squad, true)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = delivery.NewPostgres(q).Enqueue(ctx, delivery.EnqueueRequest{
		Scope:           delivery.Scope{WorkspaceID: workspaceID},
		Source:          delivery.Source{Type: "issue_assignment", Key: key, ActorType: actorType, ActorID: actorID},
		Target:          delivery.Target{SessionID: binding.SessionID},
		Recipient:       delivery.Recipient{Type: "agent", ID: agentID},
		NotificationKey: key, Kind: "issue_assignment", Payload: body, Refs: delivery.Refs{IssueID: issueID},
	})
	return err
}

// ValidateRecipientSessionID validates even parked requests, before business writes.
func ValidateRecipientSessionID(id *string) error {
	if id == nil {
		return nil
	}
	parsed, err := util.ParseUUID(*id)
	if err != nil || !parsed.Valid {
		return &RecipientSessionError{Message: "recipient_session_id must be a registered session UUID"}
	}
	return nil
}
