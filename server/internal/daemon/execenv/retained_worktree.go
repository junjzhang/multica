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
	expected := branchOwner{WorkspaceID: workspaceID, AgentID: agentID, ConversationID: scopeID}
	w, err := readRetainedWorktree(envRoot)
	if err != nil {
		return nil, err
	}
	if !expected.valid() || w.owner != expected {
		return nil, errors.New("retained worktree owner differs from the source scope")
	}
	if !sameRetainedDirectory(w.WorkDir, workDir) || !sameRetainedDirectory(w.Path, filepath.Join(envRoot, "worktree")) {
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
	return w, nil
}

func readRetainedWorktree(envRoot string) (*LocalWorktree, error) {
	data, err := os.ReadFile(filepath.Join(envRoot, retainedWorktreeFile))
	if err != nil {
		return nil, err
	}
	var w LocalWorktree
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// ValidateRemovedWorktree checks the saved finalization and durable repository
// after the server has confirmed removal. Missing checkout data alone is not proof.
func ValidateRemovedWorktree(envRoot, workDir, workspaceID, agentID, scopeType, scopeID, ownerTaskID, durableWorkDir string, logger *slog.Logger) error {
	expected := branchOwner{WorkspaceID: workspaceID, AgentID: agentID, ConversationID: scopeID}
	w, err := readRetainedWorktree(envRoot)
	if err != nil {
		return err
	}
	if err := validateRemovedWorktreeOwner(envRoot, w, expected, scopeType, ownerTaskID); err != nil {
		return err
	}
	if w.finalizedTip == "" || filepath.Base(w.Path) != "worktree" || !sameRetainedDirectory(filepath.Dir(w.Path), envRoot) || filepath.Clean(w.WorkDir) != filepath.Clean(workDir) {
		return errors.New("removed worktree finalization or original path is unproven")
	}
	if _, err := os.Lstat(w.Path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("removed worktree path is present or inaccessible")
	}
	rel, err := filepath.Rel(w.Path, w.WorkDir)
	if err != nil || !filepath.IsLocal(rel) || !sameRetainedDirectory(filepath.Join(w.GitRoot, rel), durableWorkDir) {
		return errors.New("removed worktree durable location differs or is inaccessible")
	}
	unlock, err := lockGitRoot(w.GitRoot, logger)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := runGit(w.GitRoot, "cat-file", "-e", w.finalizedTip+"^{commit}"); err != nil {
		return fmt.Errorf("removed worktree final commit is unavailable in the durable repository: %w", err)
	}
	if w.dropOnRemove {
		return nil
	}
	if w.tracksState {
		if _, ok := branchOwnedBy(w.GitRoot, w.Branch, expected, logger); !ok {
			return errors.New("removed worktree durable branch ownership is unproven")
		}
	}
	if _, err := runGit(w.GitRoot, "merge-base", "--is-ancestor", w.finalizedTip, "refs/heads/"+w.Branch); err != nil {
		return errors.New("removed worktree final commit is no longer on its durable branch")
	}
	return nil
}

func validateRemovedWorktreeOwner(envRoot string, w *LocalWorktree, expected branchOwner, scopeType, ownerTaskID string) error {
	if expected.valid() && w.owner == expected {
		return nil
	}
	if w.owner != (branchOwner{}) || w.tracksState || !expected.valid() {
		return errors.New("removed worktree conversation owner differs")
	}
	creator, err := ReadEnvRootOwner(envRoot)
	if err != nil || creator == nil || creator.TaskID != ownerTaskID || creator.WorkspaceID != expected.WorkspaceID {
		return errors.New("removed task worktree creator cannot be proven")
	}
	prov, err := ReadManagedEnvProvenance(envRoot)
	if err != nil || prov == nil || prov.ManagedBy != ManagedEnvProvenanceManagedBy || prov.WorkspaceID != expected.WorkspaceID || prov.AgentID != expected.AgentID {
		return errors.New("removed task worktree provenance cannot be proven")
	}
	if (scopeType == "task" && prov.ScopeTaskID == ownerTaskID && expected.ConversationID == ownerTaskID) || (scopeType == "autopilot" && prov.AutopilotRunID == expected.ConversationID) {
		return nil
	}
	return errors.New("removed task worktree original scope differs")
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
	if err := w.verifyFinalizedContent(); err != nil {
		return fmt.Errorf("worktree has changes after persisted finalization: %w", err)
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
