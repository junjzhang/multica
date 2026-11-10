package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
)

func TestExistingLocalDirectoryBindingCannotBorrowManagedTree(t *testing.T) {
	root := t.TempDir()
	source, err := execenv.Prepare(execenv.PrepareParams{WorkspacesRoot: root, WorkspaceID: "workspace-one", TaskID: "source-one", Provider: "claude"}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	source.ReleaseLock()
	borrowed := filepath.Join(source.WorkDir, "borrowed")
	if err := os.MkdirAll(borrowed, 0o700); err != nil {
		t.Fatal(err)
	}
	var failed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/fail") {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		failed = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	ref, _ := json.Marshal(localDirectoryRef{LocalPath: borrowed, DaemonID: "daemon", ExecutionMode: localDirectoryModeInPlace})
	task := Task{ID: "borrower", WorkspaceID: "workspace-two", ProjectResources: []ProjectResourceData{{ResourceType: localDirectoryResourceType, ResourceRef: ref}}}
	d := New(Config{WorkspacesRoot: root, DaemonID: "daemon", ServerBaseURL: srv.URL}, slog.Default())
	release, aborted := d.acquireLocalDirectoryLockIfNeeded(context.Background(), task, d.logger)
	if release != nil {
		release()
	}
	if !aborted || !failed {
		t.Fatalf("existing binding admitted: aborted=%v reported=%v", aborted, failed)
	}
}
