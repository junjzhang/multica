package execenv

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyOwnerCannotBeUserDirectory(t *testing.T) {
	root := t.TempDir()
	params := PrepareParams{WorkspacesRoot: root, WorkspaceID: "workspace-one", TaskID: "source-one", Provider: "claude", Task: TaskContextForEnv{AgentID: "agent-one", IssueID: "issue-one"}}
	source, err := Prepare(params, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	source.ReleaseLock()
	if err := os.WriteFile(filepath.Join(source.RootDir, envRootOwnerFile), []byte("source-one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source.RootDir, gcMetaFile)); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	adopted, err := findOwnedTaskRoot(RootDirParams{WorkspacesRoot: root, WorkspaceID: params.WorkspaceID, TaskID: params.TaskID})
	if err != nil || adopted != source.RootDir {
		t.Fatalf("not a supported owned legacy root: %s %v", adopted, err)
	}
	borrower, err := Prepare(PrepareParams{WorkspacesRoot: t.TempDir(), WorkspaceID: "workspace-two", TaskID: "borrower", Provider: "claude", LocalWorkDir: source.WorkDir}, slog.Default())
	if err == nil {
		borrower.ReleaseLock()
		t.Fatalf("supported legacy root %s adopted as another profile user directory %s", adopted, borrower.WorkDir)
	}
}
