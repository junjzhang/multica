package service

import (
	"context"
	"path"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TaskResourceAddress(task db.AgentTaskQueue, rt db.AgentRuntime) protocol.ManagedResourceAddress {
	a := protocol.ManagedResourceAddress{SourceTaskID: util.UUIDToString(task.ID), WorkspaceID: util.UUIDToString(rt.WorkspaceID), AgentID: util.UUIDToString(task.AgentID), RuntimeID: util.UUIDToString(rt.ID), Provider: rt.Provider, SessionID: task.SessionID.String, WorkDir: task.WorkDir.String}
	switch {
	case task.IssueID.Valid:
		a.ScopeType, a.ScopeID = "issue", util.UUIDToString(task.IssueID)
	case task.ChatSessionID.Valid:
		a.ScopeType, a.ScopeID = "chat", util.UUIDToString(task.ChatSessionID)
	case task.AutopilotRunID.Valid:
		a.ScopeType, a.ScopeID = "autopilot", util.UUIDToString(task.AutopilotRunID)
	default:
		a.ScopeType, a.ScopeID = "task", util.UUIDToString(task.ID)
	}
	return a
}

func resourcePathsOverlap(a, b string) bool {
	a = path.Clean(strings.ReplaceAll(a, "\\", "/"))
	b = path.Clean(strings.ReplaceAll(b, "\\", "/"))
	if a == "." || b == "." || a == "" || b == "" {
		return false
	}
	// Windows filesystems commonly ignore case. Keeping an extra directory is
	// safer than deleting a case alias of a protected session.
	if strings.Contains(a, ":") || strings.Contains(b, ":") {
		a = strings.ToLower(a)
		b = strings.ToLower(b)
	}
	return a == b || strings.HasPrefix(a, strings.TrimRight(b, "/")+"/") || strings.HasPrefix(b, strings.TrimRight(a, "/")+"/")
}

func resourceMatches(req protocol.ResourceReleaseRequest, a protocol.ManagedResourceAddress) bool {
	switch req.Kind {
	case "task":
		root := req.ResourcePath
		if root == "" {
			root = req.WorkDir
		}
		return req.SourceTaskID == a.SourceTaskID || resourcePathsOverlap(root, a.WorkDir)
	case "workdir":
		return resourcePathsOverlap(req.ResourcePath, a.WorkDir)
	case "provider_store":
		return req.Provider == a.Provider && req.AgentID == a.AgentID && (req.ScopeType == "agent" || (req.ScopeType == a.ScopeType && req.ScopeID == a.ScopeID))
	}
	return false
}

// ResourceRelease checks the authoritative dependencies while the daemon holds
// the same local resource lock used by execution. A new source must acquire that
// lock before execution; the terminal source cannot admit new returns.
func (s *TaskService) ResourceRelease(ctx context.Context, workspace pgtype.UUID, daemonID string, req protocol.ResourceReleaseRequest) (protocol.ResourceReleaseDecision, error) {
	out := protocol.ResourceReleaseDecision{ProtectedResources: []protocol.ManagedResourceAddress{}}
	sourceID, _ := util.ParseUUID(req.SourceTaskID)
	tasks, err := s.Queries.ListDaemonResourceTasks(ctx, db.ListDaemonResourceTasksParams{WorkspaceID: workspace, DaemonID: pgtype.Text{String: daemonID, Valid: true}, SourceTaskID: sourceID})
	if err != nil {
		return out, err
	}
	if req.Kind == "task" && req.WorkDir == "" && req.ResourcePath == "" {
		for _, row := range tasks {
			if util.UUIDToString(row.TaskID) == req.SourceTaskID {
				req.WorkDir = row.WorkDir.String
				break
			}
		}
	}
	byID := make(map[string]db.ListDaemonResourceTasksRow, len(tasks))
	block := func(reason string, a protocol.ManagedResourceAddress) {
		matches := resourceMatches(req, a) || (req.Kind == "workdir" && a.WorkDir == "")
		if matches && out.Reason == "" {
			out.Reason = reason
		}
		if matches || req.Kind == "workdir" || req.Kind == "task" {
			out.ProtectedResources = append(out.ProtectedResources, a)
		}
	}
	for _, row := range tasks {
		a := resourceTaskAddress(row)
		byID[a.SourceTaskID] = row
		if a.SourceTaskID == req.SourceTaskID {
			out.AdmissionClosed = terminalResourceTask(row.Status)
			out.ControlRequired = row.ResourceControlRequired
		}
		matches := resourceMatches(req, a)
		if req.Kind == "workdir" && a.WorkDir == "" && !terminalResourceTask(row.Status) {
			matches = true
		}
		if !matches && req.Kind != "workdir" && req.Kind != "task" {
			continue
		}
		if !terminalResourceTask(row.Status) {
			block("execution_active", a)
			continue
		}
		if row.ResourceControlRequired && !row.TerminalReportedAt.Valid {
			block("execution_exit_unknown", a)
		}
	}
	refs, err := s.Queries.ListDaemonResourceReturns(ctx, db.ListDaemonResourceReturnsParams{WorkspaceID: workspace, DaemonID: pgtype.Text{String: daemonID, Valid: true}})
	if err != nil {
		return out, err
	}
	for _, ref := range refs {
		source, found := byID[util.UUIDToString(ref.SourceTaskID)]
		a := resourceTaskAddress(source)
		a.SourceTaskID, a.WorkspaceID, a.AgentID = util.UUIDToString(ref.SourceTaskID), util.UUIDToString(ref.WorkspaceID), util.UUIDToString(ref.DispatcherAgentID)
		a.RuntimeID, a.Provider, a.SessionID, a.WorkDir = util.UUIDToString(ref.RuntimeID), ref.Provider.String, ref.SessionID.String, ref.WorkDir.String
		if !found && req.Kind == "provider_store" && req.AgentID == a.AgentID && req.Provider == a.Provider {
			block("source_scope_unknown", a)
			continue
		}
		if !resourceMatches(req, a) && req.Kind != "workdir" && req.Kind != "task" {
			continue
		}
		if !found {
			block("source_execution_unknown", a)
			continue
		}
		if ref.ScopeTaskID.Valid {
			a.ScopeType, a.ScopeID = "task", util.UUIDToString(ref.ScopeTaskID)
			if root, ok := byID[a.ScopeID]; !ok || root.AgentID != ref.DispatcherAgentID {
				block("source_scope_unknown", a)
				continue
			}
		}
		reason, err := delegationResourceRetention(ctx, s.Queries, ref)
		if err != nil {
			return out, err
		}
		if reason != "" {
			block(reason, a)
		}
	}
	if out.Reason == "" {
		out.ReleaseAllowed = true
		out.Reason = "dependencies_closed"
	}
	return out, nil
}

func resourceTaskAddress(row db.ListDaemonResourceTasksRow) protocol.ManagedResourceAddress {
	return TaskResourceAddress(db.AgentTaskQueue{ID: row.TaskID, AgentID: row.AgentID,
		RuntimeID: row.RuntimeID, IssueID: row.IssueID, ChatSessionID: row.ChatSessionID,
		AutopilotRunID: row.AutopilotRunID, SessionID: row.SessionID, WorkDir: row.WorkDir},
		db.AgentRuntime{ID: row.RuntimeID, WorkspaceID: row.WorkspaceID, Provider: row.Provider})
}
