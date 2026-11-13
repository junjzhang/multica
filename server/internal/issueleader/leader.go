// Package issueleader resolves a squad's task owner independently of its default.
package issueleader

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/sessionroute"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var ErrUnavailable = errors.New("task leader is unavailable; explicit handoff is required")
var ErrConflict = errors.New("task leadership changed; reload before claiming or handing off")
var ErrDefaultExternal = errors.New("default_leader_must_be_managed: select a managed default leader")

type Leader struct {
	Agent     db.Agent
	RootID    pgtype.UUID
	SessionID pgtype.UUID
	Revision  int64
}

// ResolveIssueLeader never falls back when this squad has a recorded root.
// A different squad is a guest invocation and does not acquire the owning root.
func ResolveIssueLeader(ctx context.Context, q *db.Queries, issue db.Issue, squad db.Squad) (Leader, error) {
	if squad.ArchivedAt.Valid || squad.WorkspaceID != issue.WorkspaceID {
		return Leader{}, ErrUnavailable
	}
	root := db.Issue{}
	var err error
	if issue.ID.Valid {
		root, err = q.GetIssueLeadershipRoot(ctx, db.GetIssueLeadershipRootParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return Leader{}, fmt.Errorf("load leadership root: %w", err)
		}
	}
	id := squad.LeaderID
	result := Leader{}
	if root.LeadershipSquadID == squad.ID {
		id = root.LeaderAgentID
		result.RootID, result.SessionID, result.Revision = root.ID, root.LeaderExternalSessionID, root.LeadershipRevision
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return Leader{}, ErrUnavailable
	}
	member, err := q.IsSquadMember(ctx, db.IsSquadMemberParams{SquadID: squad.ID, MemberType: "agent", MemberID: agent.ID})
	if err != nil {
		return Leader{}, err
	}
	if !member {
		return Leader{}, ErrUnavailable
	}
	if agent.ExecutionKind == "external" {
		if !result.RootID.Valid {
			return Leader{}, ErrDefaultExternal
		}
		session, err := q.GetExternalAgentSessionByID(ctx, db.GetExternalAgentSessionByIDParams{ID: result.SessionID, WorkspaceID: issue.WorkspaceID})
		if err != nil || session.AgentID != agent.ID || session.RevokedAt.Valid {
			return Leader{}, ErrUnavailable
		}
		if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: session.OwnerUserID, WorkspaceID: issue.WorkspaceID}); err != nil {
			return Leader{}, ErrUnavailable
		}
	}
	result.Agent = agent
	return result, nil
}

// Claim establishes a root in the same transaction as assignment and dispatch.
// Explicit claims use CAS; ordinary producers only reuse a previously bound root.
func Claim(ctx context.Context, q *db.Queries, issue db.Issue, squad db.Squad, agentID, sessionID pgtype.UUID, expected int64, createdBy string) (Leader, error) {
	if squad.ArchivedAt.Valid || squad.WorkspaceID != issue.WorkspaceID {
		return Leader{}, ErrUnavailable
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: issue.WorkspaceID})
	if err != nil || agent.ArchivedAt.Valid {
		return Leader{}, ErrUnavailable
	}
	member, err := q.IsSquadMember(ctx, db.IsSquadMemberParams{SquadID: squad.ID, MemberType: "agent", MemberID: agentID})
	if err != nil {
		return Leader{}, err
	}
	if !member {
		return Leader{}, ErrUnavailable
	}
	if agent.ExecutionKind == "external" {
		if !sessionID.Valid {
			return Leader{}, ErrUnavailable
		}
		binding, err := sessionroute.Ensure(ctx, q, sessionroute.BindRequest{WorkspaceID: util.UUIDToString(issue.WorkspaceID), IssueID: util.UUIDToString(issue.ID), AgentID: util.UUIDToString(agentID), SessionID: util.UUIDToString(sessionID), CreatedBy: createdBy})
		if err != nil {
			return Leader{}, err
		}
		if binding.SessionID != util.UUIDToString(sessionID) {
			return Leader{}, ErrConflict
		}
	} else if sessionID.Valid {
		return Leader{}, ErrUnavailable
	}
	_, err = q.ClaimIssueLeadership(ctx, db.ClaimIssueLeadershipParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID, SquadID: squad.ID, AgentID: agentID, SessionID: sessionID, ExpectedRevision: expected})
	if errors.Is(err, pgx.ErrNoRows) {
		return Leader{}, ErrConflict
	}
	if err != nil {
		return Leader{}, err
	}
	return ResolveIssueLeader(ctx, q, issue, squad)
}

func Ensure(ctx context.Context, q *db.Queries, issue db.Issue, squad db.Squad, actorAgent, actorSession pgtype.UUID, createdBy string) (Leader, error) {
	root, err := q.GetIssueLeadershipRoot(ctx, db.GetIssueLeadershipRootParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return Leader{}, err
	}
	if root.LeadershipSquadID == squad.ID {
		return ResolveIssueLeader(ctx, q, issue, squad)
	}
	if root.ID != issue.ID && root.LeadershipSquadID.Valid {
		return Leader{}, fmt.Errorf("%w: child still references leadership root %s", ErrConflict, util.UUIDToString(root.ID))
	}
	if root.ID == issue.ID && root.LeadershipSquadID.Valid {
		children, err := q.ListLeadershipReferencingIssues(ctx, db.ListLeadershipReferencingIssuesParams{WorkspaceID: issue.WorkspaceID, RootID: issue.ID})
		if err != nil {
			return Leader{}, err
		}
		if len(children) > 0 {
			ids := make([]string, 0, len(children))
			for _, id := range children {
				ids = append(ids, util.UUIDToString(id))
			}
			return Leader{}, fmt.Errorf("%w: handle referencing child task chain first: %s", ErrConflict, strings.Join(ids, ", "))
		}
	}
	if root.ID == issue.ID && root.LeadershipSquadID.Valid {
		if err := RetireExecution(ctx, q, issue, root.LeaderAgentID); err != nil {
			return Leader{}, err
		}
	}
	// A parent relationship alone never joins leadership chains. A newly
	// assigned squad establishes its own root, including squad sub-issues.
	id, session := squad.LeaderID, pgtype.UUID{}
	if actorSession.Valid {
		id, session = actorAgent, actorSession
	} else {
		a, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: issue.WorkspaceID})
		if err != nil {
			return Leader{}, err
		}
		if a.ExecutionKind != "managed" {
			return Leader{}, ErrDefaultExternal
		}
	}
	return Claim(ctx, q, issue, squad, id, session, issue.LeadershipRevision, createdBy)
}

// RetireExecution withdraws only execution obligations; accepted returns and
// ambiguous receipts keep their immutable targets and history.
func RetireExecution(ctx context.Context, q *db.Queries, issue db.Issue, agentID pgtype.UUID) error {
	if _, err := q.RevokeActiveAgentSessionBindings(ctx, db.RevokeActiveAgentSessionBindingsParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: agentID}); err != nil {
		return err
	}
	if err := q.WithdrawLeadershipDeliveries(ctx, db.WithdrawLeadershipDeliveriesParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID, AgentID: agentID}); err != nil {
		return err
	}
	if err := q.CancelLeadershipQueuedTasks(ctx, db.CancelLeadershipQueuedTasksParams{IssueID: issue.ID, AgentID: agentID}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "40P01") {
			return fmt.Errorf("%w: executor is being claimed; retry handoff", ErrConflict)
		}
		return err
	}
	return nil
}
