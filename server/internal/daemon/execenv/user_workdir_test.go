package execenv

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUserDirectoryCannotBorrowManagedTaskTree(t *testing.T) {
	managedRoot := t.TempDir()
	source, err := Prepare(PrepareParams{WorkspacesRoot: managedRoot, WorkspaceID: "workspace-one", TaskID: "source-one", Provider: "claude", Task: TaskContextForEnv{AgentID: "agent-one", IssueID: "issue-one"}}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	source.ReleaseLock()
	child := filepath.Join(source.WorkDir, "borrowed")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "borrowed-link")
	if err := os.Symlink(child, alias); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{source.RootDir, source.WorkDir, child, alias} {
		t.Run(filepath.Base(candidate), func(t *testing.T) {
			for _, root := range []string{managedRoot, t.TempDir()} {
				params := PrepareParams{WorkspacesRoot: root, WorkspaceID: "workspace-two", TaskID: "borrower", Provider: "claude", LocalWorkDir: candidate}
				if env, err := Prepare(params, slog.Default()); err == nil {
					env.ReleaseLock()
					t.Fatal("Prepare adopted a daemon-owned directory as user-owned")
				}
				if env := Reuse(ReuseParams{WorkspacesRoot: root, WorkDir: candidate, Provider: "claude", LocalDirectory: true}, slog.Default()); env != nil {
					t.Fatal("Reuse refreshed a daemon-owned directory as user-owned")
				}
			}
		})
	}
	marker, err := os.ReadFile(filepath.Join(source.WorkDir, TaskContextMarkerRelPath))
	if err != nil || !strings.Contains(string(marker), "agent-one") {
		t.Fatalf("source context overwritten: %s, %v", marker, err)
	}
}

func TestUserDirectoryOwnershipAllowsOrdinaryContextAndPrefixSibling(t *testing.T) {
	base := t.TempDir()
	managedRoot := filepath.Join(base, "managed")
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(base, "managed-user-repo")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	params := PrepareParams{WorkspacesRoot: managedRoot, WorkspaceID: "workspace", TaskID: "task", Provider: "claude", LocalWorkDir: userDir, Task: TaskContextForEnv{AgentID: "agent", IssueID: "issue"}}
	env, err := Prepare(params, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	env.ReleaseLock()
	if err := ValidateUserWorkDir(managedRoot, userDir); err != nil {
		t.Fatalf("ordinary context marker became ownership: %v", err)
	}
	if reused := Reuse(ReuseParams{WorkspacesRoot: managedRoot, WorkDir: userDir, Provider: "claude", LocalDirectory: true, Task: params.Task}, slog.Default()); reused == nil {
		t.Fatal("ordinary user directory cannot be reused")
	}
}
