package execenv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFinalizePersistsWorkBeforeResourceRelease(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)
	writeFile(t, filepath.Join(wt.Path, "accepted-work.txt"), "durable work\n")
	outcome, err := wt.Finalize(worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Branch != wt.Branch || !outcome.AutoCommitted {
		t.Fatalf("delivery = %+v", outcome)
	}
	if _, err := os.Stat(wt.WorkDir); err != nil {
		t.Fatalf("workdir removed before terminal/release authority: %v", err)
	}
	if got := gitRun(t, repo, "show", wt.Branch+":accepted-work.txt"); got != "durable work" {
		t.Fatalf("saved work = %q", got)
	}
}

func TestWorktreeRemovalRejectsChangesAfterFinalization(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)
	if _, err := wt.Finalize(worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "later-turn.txt"), "later owner work")
	if err := wt.Remove(worktreeTestLogger()); err == nil {
		t.Fatal("removed later work after old finalization")
	}
	if got := readFile(t, filepath.Join(wt.Path, "later-turn.txt")); got != "later owner work" {
		t.Fatal(got)
	}
}

func TestWorktreeRemovalRequiresPersistedWork(t *testing.T) {
	repo := newTestRepo(t)
	wt := prepareForTest(t, repo)
	if err := wt.Remove(worktreeTestLogger()); err == nil {
		t.Fatal("removed unfinalized worktree")
	}
	if _, err := wt.Finalize(worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	if err := wt.Remove(worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("authorized clean worktree remains: %v", err)
	}
}

func TestRetainedWorktreeRestoresThroughDirectoryAlias(t *testing.T) {
	repo := newTestRepo(t)
	root := t.TempDir()
	wt, err := PrepareLocalWorktree(LocalWorktreeParams{LocalPath: repo, EnvRoot: root, TaskID: "source", WorkspaceID: "workspace", AgentID: "agent", ConversationID: "issue", ConversationKey: "issue"}, worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Finalize(worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	if err := wt.SaveForReuse(root); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "original-root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	restored, err := LoadRetainedWorktree(alias, filepath.Join(alias, "worktree"), "workspace", "agent", "issue", worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(wt.Path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(restored.Path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("restoration chose another checkout: %v", err)
	}
	if err := restored.RestoreRemovalProof(); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedEmptyWorktreeDoesNotBecomeDeliveredWork(t *testing.T) {
	repo := newTestRepo(t)
	root := t.TempDir()
	wt, err := PrepareLocalWorktree(LocalWorktreeParams{LocalPath: repo, EnvRoot: root, TaskID: "source", WorkspaceID: "workspace", AgentID: "agent", ConversationID: "issue", ConversationKey: "issue"}, worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Finalize(worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	if err := wt.SaveForReuse(root); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadRetainedWorktree(root, wt.WorkDir, "workspace", "agent", "issue", worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := restored.Finalize(worktreeTestLogger())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Branch != "" {
		t.Fatalf("read-only return manufactured delivered work: %+v", outcome)
	}
	if err := restored.Remove(worktreeTestLogger()); err != nil {
		t.Fatal(err)
	}
	if _, err := gitTry(t, repo, "show-ref", "--verify", "refs/heads/"+wt.Branch); err == nil {
		t.Fatal("empty retained branch survived resource release")
	}
}
