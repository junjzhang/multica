package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/util"
)

func validateDelegationResultTask(task Task, provider string) error {
	if task.DelegationResult == nil {
		if task.RequireResume {
			return errors.New("strict result return is missing its accepted address")
		}
		return nil
	}
	if _, err := uuid.Parse(task.SessionPinGeneration); err != nil {
		return errors.New("result return has no current session pin generation")
	}
	r := task.DelegationResult
	if r.ScopeTaskID != "" && (task.IssueID != "" || task.ChatSessionID != "" || task.AutopilotRunID != "") {
		return errors.New("result return has conflicting source scopes")
	}
	if !task.RequireResume {
		return errors.New("result return requires strict resume")
	}
	for _, value := range []string{r.ResultTaskID, r.DelegationReturnID, r.SourceTaskID, r.DelegatedIssueID, r.Outcome, r.RuntimeID, r.Provider, r.SessionID, r.WorkDir} {
		if strings.TrimSpace(value) == "" {
			return errors.New("result return has an incomplete accepted address")
		}
	}
	if task.RuntimeID != r.RuntimeID || provider != r.Provider || task.PriorSessionID != r.SessionID || task.PriorWorkDir != r.WorkDir {
		return errors.New("result return claim differs from the accepted address")
	}
	if task.Agent == nil {
		return errors.New("result return agent is unavailable")
	}
	if task.IssueID == "" && task.ChatSessionID == "" && task.AutopilotRunID == "" && r.ScopeTaskID == "" {
		return fmt.Errorf("result return source scope is unavailable: %s", r.SourceTaskID)
	}
	return nil
}

func managedRootForWorkDir(root, workDir string) string {
	root, err := util.ResolveSymlinks(root)
	if err != nil {
		return ""
	}
	workDir, err = util.ResolveSymlinks(workDir)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(root, workDir)
	if err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 {
		return ""
	}
	return filepath.Join(root, parts[0], parts[1])
}

func relativeManagedRoot(workspacesRoot, envRoot string) (string, error) {
	root, err := util.ResolveSymlinks(workspacesRoot)
	if err != nil {
		return "", err
	}
	target, err := util.ResolveSymlinks(envRoot)
	if err != nil {
		return "", err
	}
	return filepath.Rel(root, target)
}

func originalTaskScopeID(task Task) string {
	if task.IssueID != "" || task.ChatSessionID != "" || task.AutopilotRunID != "" {
		return ""
	}
	if task.DelegationResult != nil {
		return task.DelegationResult.ScopeTaskID
	}
	return task.ID
}
