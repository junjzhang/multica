package execenv

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

const retainedWorktreeFile = ".local_worktree.json"

func (w *LocalWorktree) SaveForReuse(envRoot string) error {
	data, err := json.Marshal(w)
	if err != nil {
		return err
	}
	tmp := filepath.Join(envRoot, retainedWorktreeFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(envRoot, retainedWorktreeFile))
}

// LoadRetainedWorktree verifies the saved preparation state against the actual
// checkout and its conversation owner. Callers hold the env-root reuse lock.
func LoadRetainedWorktree(envRoot, workDir, workspaceID, agentID, scopeID string, logger *slog.Logger) (*LocalWorktree, error) {
	data, err := os.ReadFile(filepath.Join(envRoot, retainedWorktreeFile))
	if err != nil {
		return nil, err
	}
	var w LocalWorktree
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, err
	}
	expected := branchOwner{WorkspaceID: workspaceID, AgentID: agentID, ConversationID: scopeID}
	if !expected.valid() || w.owner != expected || !sameRetainedDirectory(w.WorkDir, workDir) || !sameRetainedDirectory(w.Path, filepath.Join(envRoot, "worktree")) {
		return nil, errors.New("retained worktree ownership differs from the source scope")
	}
	rel, err := filepath.Rel(w.Path, w.WorkDir)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, errors.New("retained workdir escapes the checkout")
	}
	unlock, err := lockGitRoot(w.GitRoot, logger)
	if err != nil {
		return nil, err
	}
	defer unlock()
	top, err := runGitTrimmed(workDir, "rev-parse", "--show-toplevel")
	if err != nil || !sameRetainedDirectory(top, w.Path) {
		return nil, errors.New("retained worktree checkout changed")
	}
	common, err := runGitTrimmed(w.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	expectedCommon, expectedErr := runGitTrimmed(w.GitRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || expectedErr != nil || !sameRetainedDirectory(common, expectedCommon) {
		return nil, errors.New("retained worktree repository changed")
	}
	branch, err := runGitTrimmed(w.Path, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch != w.Branch {
		return nil, errors.New("retained worktree branch changed")
	}
	if w.tracksState {
		if _, ok := branchOwnedBy(w.GitRoot, w.Branch, expected, logger); !ok {
			tip, err := runGitTrimmed(w.Path, "rev-parse", "HEAD")
			if err != nil || !w.createdBranch || tip != w.BaseCommit {
				return nil, fmt.Errorf("retained branch %s no longer belongs to this conversation", w.Branch)
			}
		}
	}
	w.createdBranch = w.dropOnRemove
	w.Continued = true
	return &w, nil
}

// RestoreRemovalProof reconstructs the in-memory identity only from a saved
// successful finalization, while the caller excludes current execution.
func (w *LocalWorktree) RestoreRemovalProof() error {
	if w.finalizedTip == "" {
		return errors.New("worktree has no persisted finalization")
	}
	tip, err := runGitTrimmed(w.Path, "rev-parse", "HEAD")
	if err != nil || tip != w.finalizedTip {
		return errors.New("worktree changed after persisted finalization")
	}
	dirty, err := worktreeIsDirty(w.Path)
	if err != nil || dirty {
		return errors.New("worktree has changes after persisted finalization")
	}
	info, err := os.Lstat(w.Path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("worktree directory changed")
	}
	w.finalizedInfo = info
	return nil
}

func sameRetainedDirectory(a, b string) bool {
	first, err := os.Stat(a)
	if err != nil {
		return false
	}
	second, err := os.Stat(b)
	return err == nil && first.IsDir() && second.IsDir() && os.SameFile(first, second)
}
