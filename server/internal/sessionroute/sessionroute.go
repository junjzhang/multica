// Package sessionroute is the application boundary between a business
// producer and the transport-neutral delivery core.
//
// It answers one question before an enqueue: which authorized host session
// serves this agent on this issue? The answer comes from an explicit
// (workspace, agent, issue) binding, never from the agent's "current" or
// "default" session, so one agent may own several sessions and one session may
// serve several issues. The delivery core then freezes the returned session and
// never performs this lookup again on claim, retry or receipt paths.
package sessionroute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ErrNotBound reports that the exact (workspace, agent, issue) key has no
// active binding. A producer must fail closed instead of picking another
// session of the same agent.
var ErrNotBound = errors.New("no active session binding for this agent on this issue")

// ErrInvalidBinding reports a requested target that cannot be bound: the
// session or issue is outside the workspace, the session belongs to another
// agent, or the agent is not an executable external agent.
var ErrInvalidBinding = errors.New("session binding target is invalid")

// Binding is one resolved target, including everything the transport adapter
// needs to reach it. DaemonID qualifies which worker may deliver; it is not
// part of the frozen message identity.
type Binding struct {
	ID            string
	WorkspaceID   string
	AgentID       string
	IssueID       string
	SessionID     string
	OwnerUserID   string
	Provider      string
	HostID        string
	HostSessionID string
	DaemonID      string
	CreatedAt     time.Time
}

// BindRequest records the target an explicit registration, takeover or future
// in-transaction delegation chose for one exact key.
type BindRequest struct {
	WorkspaceID string
	AgentID     string
	IssueID     string
	SessionID   string
	CreatedBy   string
}

// Resolve returns the active binding for this exact key. q may be a
// transaction handle; the FOR SHARE lock then serializes the caller's enqueue
// with a concurrent rebind.
func Resolve(ctx context.Context, q *db.Queries, workspaceID, agentID, issueID string) (Binding, error) {
	ws, err := util.ParseUUID(workspaceID)
	if err != nil {
		return Binding{}, fmt.Errorf("%w: workspace_id must be a UUID", ErrInvalidBinding)
	}
	agent, err := util.ParseUUID(agentID)
	if err != nil {
		return Binding{}, fmt.Errorf("%w: agent id must be a UUID", ErrInvalidBinding)
	}
	issue, err := util.ParseUUID(issueID)
	if err != nil {
		return Binding{}, fmt.Errorf("%w: issue_id must be a UUID", ErrInvalidBinding)
	}
	row, err := q.GetActiveAgentSessionBinding(ctx, db.GetActiveAgentSessionBindingParams{
		WorkspaceID: ws,
		AgentID:     agent,
		IssueID:     issue,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, fmt.Errorf("%w: agent %s, issue %s", ErrNotBound, agentID, issueID)
	}
	if err != nil {
		return Binding{}, fmt.Errorf("sessionroute: resolve binding: %w", err)
	}
	return Binding{
		ID:            util.UUIDToString(row.ID),
		WorkspaceID:   util.UUIDToString(row.WorkspaceID),
		AgentID:       util.UUIDToString(row.AgentID),
		IssueID:       util.UUIDToString(row.IssueID),
		SessionID:     util.UUIDToString(row.SessionID),
		OwnerUserID:   util.UUIDToString(row.OwnerUserID),
		Provider:      row.Provider,
		HostID:        row.HostID,
		HostSessionID: row.HostSessionID,
		DaemonID:      row.DaemonID,
	}, nil
}

// Ensure records the target for one exact key only when that key has no active
// binding, and returns the accepted target otherwise. A delegation uses this
// instead of Bind: the first accepted return address is fixed, so an ordinary
// dispatch (a later assignment, rerun, or mention) cannot silently retarget
// someone else's result. Only the explicit takeover endpoint calls Bind.
func Ensure(ctx context.Context, q *db.Queries, req BindRequest) (Binding, error) {
	ws, agentID, issueID, sessionID, createdBy, err := parseBindRequest(req)
	if err != nil {
		return Binding{}, err
	}

	existing, err := Resolve(ctx, q, util.UUIDToString(ws), util.UUIDToString(agentID), util.UUIDToString(issueID))
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrNotBound) {
		return Binding{}, err
	}

	session, err := validateTarget(ctx, q, ws, agentID, issueID, sessionID)
	if err != nil {
		return Binding{}, err
	}
	created, err := q.CreateAgentSessionBinding(ctx, db.CreateAgentSessionBindingParams{
		WorkspaceID: ws,
		AgentID:     agentID,
		IssueID:     issueID,
		SessionID:   sessionID,
		CreatedBy:   createdBy,
	})
	if err != nil {
		// A concurrent Ensure or an explicit Bind won the key between the read
		// and this insert. The unique active-binding index is the arbiter; the
		// accepted row is now the one to return.
		if errors.Is(err, pgx.ErrNoRows) || isUniqueViolation(err) {
			return Resolve(ctx, q, util.UUIDToString(ws), util.UUIDToString(agentID), util.UUIDToString(issueID))
		}
		return Binding{}, fmt.Errorf("sessionroute: create binding: %w", err)
	}
	return bindingFromRow(created, session), nil
}

// Bind records (or replaces) the active target for one exact key. The caller
// owns the transaction when this must commit with its business write; a
// replacement revokes the previous row first, so there is never a window with
// two active targets.
func Bind(ctx context.Context, q *db.Queries, req BindRequest) (Binding, error) {
	ws, agentID, issueID, sessionID, createdBy, err := parseBindRequest(req)
	if err != nil {
		return Binding{}, err
	}
	session, err := validateTarget(ctx, q, ws, agentID, issueID, sessionID)
	if err != nil {
		return Binding{}, err
	}

	if _, err := q.RevokeActiveAgentSessionBindings(ctx, db.RevokeActiveAgentSessionBindingsParams{
		WorkspaceID: ws,
		AgentID:     agentID,
		IssueID:     issueID,
	}); err != nil {
		return Binding{}, fmt.Errorf("sessionroute: revoke previous binding: %w", err)
	}
	created, err := q.CreateAgentSessionBinding(ctx, db.CreateAgentSessionBindingParams{
		WorkspaceID: ws,
		AgentID:     agentID,
		IssueID:     issueID,
		SessionID:   sessionID,
		CreatedBy:   createdBy,
	})
	if err != nil {
		return Binding{}, fmt.Errorf("sessionroute: create binding: %w", err)
	}
	return bindingFromRow(created, session), nil
}

// parseBindRequest validates the address shape every binding write shares.
func parseBindRequest(req BindRequest) (ws, agentID, issueID, sessionID pgtype.UUID, createdBy pgtype.UUID, err error) {
	ws, err = util.ParseUUID(strings.TrimSpace(req.WorkspaceID))
	if err != nil {
		return ws, agentID, issueID, sessionID, createdBy, fmt.Errorf("%w: workspace_id must be a UUID", ErrInvalidBinding)
	}
	agentID, err = util.ParseUUID(strings.TrimSpace(req.AgentID))
	if err != nil {
		return ws, agentID, issueID, sessionID, createdBy, fmt.Errorf("%w: agent id must be a UUID", ErrInvalidBinding)
	}
	issueID, err = util.ParseUUID(strings.TrimSpace(req.IssueID))
	if err != nil {
		return ws, agentID, issueID, sessionID, createdBy, fmt.Errorf("%w: issue_id must be a UUID", ErrInvalidBinding)
	}
	sessionID, err = util.ParseUUID(strings.TrimSpace(req.SessionID))
	if err != nil {
		return ws, agentID, issueID, sessionID, createdBy, fmt.Errorf("%w: session_id must be a UUID", ErrInvalidBinding)
	}
	if req.CreatedBy != "" {
		createdBy, err = util.ParseUUID(req.CreatedBy)
		if err != nil {
			return ws, agentID, issueID, sessionID, createdBy, fmt.Errorf("sessionroute: created by: %w", err)
		}
	}
	return ws, agentID, issueID, sessionID, createdBy, nil
}

// validateTarget proves the session, issue, agent, and owner all belong to one
// workspace and that the agent is still an executable external identity. Every
// caller resolves an address before an enqueue, so a foreign or retired target
// fails closed here instead of producing an undeliverable route.
func validateTarget(ctx context.Context, q *db.Queries, ws, agentID, issueID, sessionID pgtype.UUID) (db.ExternalAgentSession, error) {
	if _, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: ws}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ExternalAgentSession{}, fmt.Errorf("%w: issue is not in this workspace", ErrInvalidBinding)
		}
		return db.ExternalAgentSession{}, fmt.Errorf("sessionroute: load issue: %w", err)
	}
	session, err := q.GetExternalAgentSessionByID(ctx, db.GetExternalAgentSessionByIDParams{ID: sessionID, WorkspaceID: ws})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.ExternalAgentSession{}, fmt.Errorf("%w: session is not in this workspace", ErrInvalidBinding)
	}
	if err != nil {
		return db.ExternalAgentSession{}, fmt.Errorf("sessionroute: load session: %w", err)
	}
	if session.RevokedAt.Valid {
		return db.ExternalAgentSession{}, fmt.Errorf("%w: session is revoked", ErrInvalidBinding)
	}
	if util.UUIDToString(session.AgentID) != util.UUIDToString(agentID) {
		return db.ExternalAgentSession{}, fmt.Errorf("%w: session belongs to another agent", ErrInvalidBinding)
	}
	agent, err := q.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: ws})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ExternalAgentSession{}, fmt.Errorf("%w: agent is not in this workspace", ErrInvalidBinding)
		}
		return db.ExternalAgentSession{}, fmt.Errorf("sessionroute: load agent: %w", err)
	}
	if agent.ExecutionKind != "external" || agent.ArchivedAt.Valid {
		return db.ExternalAgentSession{}, fmt.Errorf("%w: agent is not an active external agent", ErrInvalidBinding)
	}
	if _, err := q.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: session.OwnerUserID, WorkspaceID: ws}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ExternalAgentSession{}, fmt.Errorf("%w: session owner is not a member of this workspace", ErrInvalidBinding)
		}
		return db.ExternalAgentSession{}, fmt.Errorf("sessionroute: load session owner: %w", err)
	}
	return session, nil
}

func bindingFromRow(created db.ExternalAgentSessionBinding, session db.ExternalAgentSession) Binding {
	return Binding{
		ID:            util.UUIDToString(created.ID),
		WorkspaceID:   util.UUIDToString(created.WorkspaceID),
		AgentID:       util.UUIDToString(created.AgentID),
		IssueID:       util.UUIDToString(created.IssueID),
		SessionID:     util.UUIDToString(created.SessionID),
		OwnerUserID:   util.UUIDToString(session.OwnerUserID),
		Provider:      session.Provider,
		HostID:        session.HostID,
		HostSessionID: session.SessionID,
		DaemonID:      session.DaemonID,
		CreatedAt:     created.CreatedAt.Time,
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
