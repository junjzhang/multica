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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestGCReleaseHoldsExecutionLockAndRechecksCurrentOwner(t *testing.T) {
	var releaseCalls atomic.Int32
	entered, allow := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/resources/release" {
			releaseCalls.Add(1)
			close(entered)
			<-allow
			w.Write([]byte(`{"admission_closed":true,"resource_control_required":true,"release_allowed":true,"protected_resources":[]}`))
			return
		}
		w.Write([]byte(`{"status":"completed"}`))
	}))
	defer srv.Close()
	d := New(Config{WorkspacesRoot: t.TempDir()}, slog.Default())
	d.client = NewClient(srv.URL)
	meta := &execenv.GCMeta{Kind: execenv.GCKindQuickCreate, TaskID: "owner", WorkspaceID: "workspace", CompletedAt: time.Now().Add(-time.Hour)}
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "workspace", "owner", meta)
	resource := resourceForTask(Task{ID: "owner", RuntimeID: "runtime", AgentID: "agent"}, "claude", root, filepath.Join(root, "workdir"), "session")
	if err := writeManagedResource(root, resource); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- d.applyGCActionWithChatMeta(context.Background(), root, gcActionClean, &gcStats{}, meta, gcRemovalDetails{}) }()
	<-entered
	ws, err := os.OpenRoot(d.cfg.WorkspacesRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	claim, _, lockErr := execenv.LockEnvRootForReuse(ws, filepath.Join("workspace", "owner"), root)
	if claim != nil {
		claim.Release()
	}
	if lockErr == nil {
		t.Fatal("GC release query did not exclude execution")
	}
	close(allow)
	if got := <-done; got != 1 {
		t.Fatalf("authorized GC removed %d roots", got)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("root remains: %v", err)
	}
	if releaseCalls.Load() != 1 {
		t.Fatal(releaseCalls.Load())
	}

	// The stale scan selected an old completed task. The root now belongs to
	// another task's active metadata, and must not use that old decision.
	active := *meta
	active.TaskID = "new-owner"
	active.CompletedAt = time.Now()
	root = createTaskDir(t, d.cfg.WorkspacesRoot, "workspace", "owner", &active)
	creator, _ := json.Marshal(execenv.EnvRootOwner{WorkspaceID: "workspace", TaskID: "owner"})
	if err := os.WriteFile(filepath.Join(root, ".task_owner"), creator, 0o600); err != nil {
		t.Fatal(err)
	}
	resource.OwnerTaskID = "new-owner"
	if err := writeManagedResource(root, resource); err != nil {
		t.Fatal(err)
	}
	unknown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/resources/release" {
			t.Error("stale action reached release after current owner became active")
		}
		w.Write([]byte(`{"status":"running"}`))
	}))
	defer unknown.Close()
	d.client = NewClient(unknown.URL)
	if got := d.applyGCActionWithChatMeta(context.Background(), root, gcActionClean, &gcStats{}, meta, gcRemovalDetails{}); got != 0 {
		t.Fatal("stale GC deleted new owner")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
}

func TestProviderStoreGCWaitsForWriterAndRetainsUnknownControl(t *testing.T) {
	for _, provider := range []string{"codex", "hermes"} {
		t.Run(provider, func(t *testing.T) {
			d := New(Config{WorkspacesRoot: t.TempDir(), Profile: "store-gc-" + provider}, slog.Default())
			store := execenv.CodexSessionStorePath(d.cfg.Profile, execenv.TaskContextForEnv{AgentID: "agent", IssueID: "issue"})
			if provider == "hermes" {
				store = execenv.HermesSessionStorePath(d.cfg.Profile, "agent", t.TempDir(), execenv.TaskContextForEnv{IssueID: "issue"})
			}
			if err := os.MkdirAll(store, 0o700); err != nil {
				t.Fatal(err)
			}
			resource := resourceForTask(Task{ID: "source", RuntimeID: "runtime", AgentID: "agent", IssueID: "issue"}, provider, store, "/original/workdir", "session")
			resource.Address.Kind = "provider_store"
			if err := writeManagedResource(store, resource); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(filepath.Join(store, managedResourceFile), old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(store, old, old); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); http.Error(w, "unknown", 503) }))
			defer srv.Close()
			d.client = NewClient(srv.URL)
			writer, err := execenv.AcquireProviderStoreWriterLock(context.Background(), provider, store, nil)
			if err != nil {
				t.Fatal(err)
			}
			reserve := d.reserveProviderStoreForDeletion(context.Background(), provider, time.Hour)
			if release, ok := reserve(store); ok {
				release()
				t.Fatal("GC acquired live provider store")
			}
			if calls.Load() != 0 {
				t.Fatal("queried release before writer exclusion")
			}
			writer.Release()
			if release, ok := reserve(store); ok {
				release()
				t.Fatal("GC released store after unknown control response")
			}
			if calls.Load() != 1 {
				t.Fatalf("release calls = %d", calls.Load())
			}
			if _, err := os.Stat(store); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResourceRecordRejectsMovedRoot(t *testing.T) {
	root := t.TempDir()
	resource := resourceForTask(Task{ID: "source", RuntimeID: "runtime"}, "claude", root, "/frozen/cwd", "session")
	data, _ := json.Marshal(resource)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, managedResourceFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readManagedResource(other); err == nil {
		t.Fatal("accepted resource record copied to another root")
	}
}

func TestHermesConcurrentMemoryUsersBothExcludeGC(t *testing.T) {
	store := t.TempDir()
	first, err := execenv.AcquireHermesMemoryUseLock(context.Background(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second, err := execenv.AcquireHermesMemoryUseLock(ctx, store, nil)
	if err != nil {
		t.Fatalf("independent memory user serialized: %v", err)
	}
	defer second.Release()
	if gc, err := execenv.TryProviderStoreWriterLock("hermes", store); err == nil {
		gc.Release()
		t.Fatal("GC entered while both users held memory")
	}
	first.Release()
	if gc, err := execenv.TryProviderStoreWriterLock("hermes", store); err == nil {
		gc.Release()
		t.Fatal("GC entered while one memory user remained")
	}
	second.Release()
	gc, err := execenv.TryProviderStoreWriterLock("hermes", store)
	if err != nil {
		t.Fatal(err)
	}
	gc.Release()
}

func TestSourceTerminalPreservesReturnWorktreeUntilDependencyCloses(t *testing.T) {
	for _, terminalMode := range []string{"complete", "cancel-ack"} {
		t.Run(terminalMode, func(t *testing.T) {

			d, _, cleanup := newLeaderReuseTestDaemon(t)
			defer cleanup()
			d.cfg.DaemonID = "daemon-result"
			d.localPathLocks = NewLocalPathLocker()

			repo := t.TempDir()
			gitOK(t, repo, "init", "-q", "-b", "main")
			gitOK(t, repo, "config", "user.name", "Test")
			gitOK(t, repo, "config", "user.email", "test@example.com")
			if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("source"), 0o600); err != nil {
				t.Fatal(err)
			}
			gitOK(t, repo, "add", "-A")
			gitOK(t, repo, "commit", "-qm", "initial")
			task := leaderReuseTestTask("source-retention")
			task.IsLeaderTask = false
			ref, _ := json.Marshal(localDirectoryRef{LocalPath: repo, DaemonID: d.cfg.DaemonID, ExecutionMode: localDirectoryModeWorktree})
			task.ProjectResources = []ProjectResourceData{{ID: "directory", ResourceType: localDirectoryResourceType, ResourceRef: ref}}
			var terminal, dependencyClosed, providerFinished, finishing atomic.Bool
			var releaseCalls, receipts atomic.Int32
			var workdir string
			d.runner = taskRunnerFunc(func(ctx context.Context, task Task, provider string, slot int, logger *slog.Logger) (TaskResult, error) {
				result, err := d.runTask(ctx, task, provider, slot, logger)
				workdir = result.WorkDir
				providerFinished.Store(true)
				return result, err
			})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/complete"):
					if !providerFinished.Load() {
						t.Error("completion preceded provider exit")
					}
					var body struct {
						WorkDir        string `json:"work_dir"`
						DurableWorkDir string `json:"durable_work_dir"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					workdir = body.WorkDir
					if _, err := os.Stat(workdir); err != nil {
						t.Errorf("worktree removed before authoritative terminal: %v", err)
					}
					if body.DurableWorkDir != "" {
						t.Error("claimed removal before it happened")
					}
					terminal.Store(true)
					w.Write([]byte(`{}`))
				case strings.HasSuffix(r.URL.Path, "/cancel-ack"):
					if !providerFinished.Load() {
						t.Error("cancel ack preceded provider exit")
					}
					if _, err := os.Stat(workdir); err != nil {
						t.Errorf("cancelled worktree removed before ack: %v", err)
					}
					terminal.Store(true)
					w.Write([]byte(`{}`))
				case strings.HasSuffix(r.URL.Path, "/progress"):
					var progress struct {
						Summary string `json:"summary"`
					}
					json.NewDecoder(r.Body).Decode(&progress)
					if progress.Summary == "Finishing task" {
						finishing.Store(true)
					}
					w.Write([]byte(`{}`))
				case r.URL.Path == "/api/daemon/resources/release":
					releaseCalls.Add(1)
					if !terminal.Load() {
						t.Error("release checked before terminal was confirmed")
					}
					json.NewEncoder(w).Encode(map[string]any{"resource_control_required": true, "admission_closed": terminal.Load(), "release_allowed": dependencyClosed.Load(), "protected_resources": []protocol.ManagedResourceAddress{}})
				case strings.HasSuffix(r.URL.Path, "/resources-released"):
					var receipt protocol.ResourceReleaseReceipt
					if err := json.NewDecoder(r.Body).Decode(&receipt); err != nil {
						t.Error(err)
					}
					if receipt.DurableWorkDir != repo {
						t.Errorf("wrong durable directory: %+v", receipt)
					}
					if _, err := os.Stat(receipt.ResourcePath); !os.IsNotExist(err) {
						t.Errorf("receipt sent before deletion: %v", err)
					}
					receipts.Add(1)
					w.Write([]byte(`{"recorded":true}`))
				case strings.HasSuffix(r.URL.Path, "/status"):
					if terminalMode == "cancel-ack" && finishing.Load() {
						w.Write([]byte(`{"status":"cancelled"}`))
						return
					}
					w.Write([]byte(`{"status":"running"}`))
				default:
					w.Write([]byte(`{}`))
				}
			}))
			defer srv.Close()
			d.client = NewClient(srv.URL)
			d.handleTask(context.Background(), task, 0)
			if !terminal.Load() || releaseCalls.Load() != 1 {
				t.Fatalf("terminal/release sequence incomplete: %v %d", terminal.Load(), releaseCalls.Load())
			}
			if _, err := os.Stat(workdir); err != nil {
				t.Fatalf("unfinished delegation lost its source: %v", err)
			}
			root := managedRootForWorkDir(d.cfg.WorkspacesRoot, workdir)
			meta, err := execenv.ReadGCMeta(root)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			// A restarted daemon can use the persisted owner, finalization checkpoint,
			// and current dependency decision; no in-memory finalizer is required.
			restarted := New(Config{WorkspacesRoot: d.cfg.WorkspacesRoot, GCOrphanTTL: time.Nanosecond}, slog.Default())
			restarted.client = d.client
			dependencyClosed.Store(true)
			if got := restarted.applyGCActionWithChatMeta(context.Background(), root, gcActionClean, &gcStats{}, meta, gcRemovalDetails{}); got != 1 {
				t.Fatalf("closed dependency did not release persisted root: %d", got)
			}
			if _, err := os.Stat(workdir); !os.IsNotExist(err) {
				t.Fatalf("released worktree remains: %v", err)
			}
			if _, err := os.Stat(repo); err != nil {
				t.Fatalf("user repository was removed: %v", err)
			}
			if receipts.Load() != 1 {
				t.Fatalf("removal metadata receipts=%d", receipts.Load())
			}

		})
	}
}

func TestGCIncompletePermissionCannotDeleteOwnedRoot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"release_allowed":true}`))
	}))
	defer srv.Close()
	d := New(Config{WorkspacesRoot: t.TempDir()}, slog.Default())
	d.client = NewClient(srv.URL)
	meta := &execenv.GCMeta{Kind: execenv.GCKindQuickCreate, WorkspaceID: "workspace", TaskID: "source", CompletedAt: time.Now().Add(-time.Hour)}
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "workspace", "source", meta)
	if got := d.applyGCActionWithChatMeta(context.Background(), root, gcActionClean, &gcStats{}, meta, gcRemovalDetails{}); got != 0 {
		t.Fatal("incomplete permission deleted the resource")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
}

type resourceWaitLog struct {
	once    sync.Once
	waiting chan struct{}
}

func (w *resourceWaitLog) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "waiting for") {
		w.once.Do(func() { close(w.waiting) })
	}
	return len(data), nil
}

func TestStoreGCExcludesProviderExecutionAndPin(t *testing.T) {
	for _, provider := range []string{"codex", "hermes"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			var pins atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/session") {
					pins.Add(1)
				}
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			promptFile := filepath.Join(t.TempDir(), "provider-executed")
			fake := filepath.Join(t.TempDir(), provider)
			writeTestExecutable(t, fake, []byte("#!/bin/sh\nif [ \"$1\" = '--version' ]; then echo '0.144.0'; exit 0; fi\necho prompt > '"+promptFile+"'\n"))
			waiting := &resourceWaitLog{waiting: make(chan struct{})}
			logger := slog.New(slog.NewTextHandler(waiting, nil))
			d := New(Config{WorkspacesRoot: t.TempDir(), Agents: map[string]AgentEntry{provider: {Path: fake}}, ServerBaseURL: srv.URL}, logger)
			d.client = NewClient(srv.URL)
			task := leaderReuseTestTask("waiting-source")
			home := t.TempDir()
			task.Agent.CustomEnv = map[string]string{"HERMES_HOME": home}
			contextForStore := execenv.TaskContextForEnv{AgentID: task.AgentID, IssueID: task.IssueID}
			store := execenv.CodexSessionStorePath(d.cfg.Profile, contextForStore)
			if provider == "hermes" {
				store = execenv.HermesMemoryStorePath(d.cfg.Profile, task.AgentID, home)
			}
			gc, err := execenv.TryProviderStoreWriterLock(provider, store)
			if err != nil {
				t.Fatal(err)
			}
			defer gc.Release()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); d.runTask(ctx, task, provider, 0, logger) }()
			select {
			case <-waiting.waiting:
			case <-time.After(5 * time.Second):
				t.Fatal("source never waited for the GC store lock")
			}
			if _, err := os.Stat(promptFile); !os.IsNotExist(err) {
				t.Fatalf("provider executed while GC held its resource: %v", err)
			}
			if pins.Load() != 0 {
				t.Fatal("task pinned a session before acquiring its resource")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("source did not stop waiting on cancellation")
			}
		})
	}
}

func TestLegacyProviderStoresUseExistingScopeForRelease(t *testing.T) {
	for _, kind := range []string{"codex", "hermes-memory", "hermes-session"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			profile := "legacy-" + kind
			task := execenv.TaskContextForEnv{AgentID: "legacy-agent", IssueID: "legacy-issue"}
			provider, scopeType := kind, "issue"
			store := execenv.CodexSessionStorePath(profile, task)
			var prune func(func(string) (func(), bool)) (int, int64)
			now := time.Now()
			switch kind {
			case "codex":
				prune = func(reserve func(string) (func(), bool)) (int, int64) {
					return execenv.PruneCodexSessionStores(profile, time.Hour, now, reserve, slog.Default())
				}
			case "hermes-memory":
				provider, scopeType = "hermes", "agent"
				store = execenv.HermesMemoryStorePath(profile, task.AgentID, t.TempDir())
				prune = func(reserve func(string) (func(), bool)) (int, int64) {
					return execenv.PruneHermesMemoryStores(profile, time.Hour, now, reserve, slog.Default())
				}
			case "hermes-session":
				provider = "hermes"
				store = execenv.HermesSessionStorePath(profile, task.AgentID, t.TempDir(), task)
				prune = func(reserve func(string) (func(), bool)) (int, int64) {
					return execenv.PruneHermesSessionStores(profile, time.Hour, now, reserve, slog.Default())
				}
			}
			if err := os.MkdirAll(store, 0o700); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(store, "history.txt")
			if err := os.WriteFile(filename, []byte("legacy state"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := now.Add(-48 * time.Hour)
			if err := os.Chtimes(filename, old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(store, old, old); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request protocol.ResourceReleaseRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.DaemonID != "fixture-daemon" || request.Provider != provider || request.AgentID != task.AgentID || request.ScopeType != scopeType || request.RuntimeID != "" || request.SessionID != "" {
					t.Errorf("legacy ownership was guessed: %+v", request)
				}
				calls.Add(1)
				w.Write([]byte(`{"release_allowed":true,"resource_control_required":false,"admission_closed":false,"protected_resources":[]}`))
			}))
			defer srv.Close()
			d := New(Config{WorkspacesRoot: t.TempDir(), Profile: profile, DaemonID: "fixture-daemon", ServerBaseURL: srv.URL}, slog.Default())
			removed, _ := prune(d.reserveProviderStoreForDeletion(context.Background(), provider, time.Hour))
			if removed != 1 || calls.Load() != 1 {
				t.Fatalf("legacy TTL stopped: removed=%d calls=%d", removed, calls.Load())
			}
			if _, err := os.Stat(store); !os.IsNotExist(err) {
				t.Fatalf("legacy store remains: %v", err)
			}
		})
	}
}

func TestGCProtectsHistoricalSessionThroughPhysicalAlias(t *testing.T) {
	d := New(Config{WorkspacesRoot: t.TempDir()}, slog.Default())
	meta := &execenv.GCMeta{Kind: execenv.GCKindQuickCreate, WorkspaceID: "workspace", TaskID: "current-B", CompletedAt: time.Now().Add(-time.Hour)}
	root := createTaskDir(t, d.cfg.WorkspacesRoot, "workspace", "current-B", meta)
	workdir := filepath.Join(root, "workdir")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	resource := resourceForTask(Task{ID: "current-B", WorkspaceID: "workspace", RuntimeID: "runtime", AgentID: "agent"}, "claude", root, workdir, "session-B")
	if err := writeManagedResource(root, resource); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "historical-address-A")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	var closed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protected := []protocol.ManagedResourceAddress{}
		if !closed.Load() {
			protected = append(protected, protocol.ManagedResourceAddress{SourceTaskID: "source-A", WorkspaceID: "workspace", AgentID: "agent", RuntimeID: "runtime", Provider: "claude", SessionID: "session-A", WorkDir: filepath.Join(alias, "workdir")})
		}
		json.NewEncoder(w).Encode(protocol.ResourceReleaseDecision{ReleaseAllowed: true, ControlRequired: true, AdmissionClosed: true, ProtectedResources: protected})
	}))
	defer srv.Close()
	d.client = NewClient(srv.URL)
	if got := d.applyGCActionWithChatMeta(context.Background(), root, gcActionClean, &gcStats{}, meta, gcRemovalDetails{}); got != 0 {
		t.Fatal("string-only permission deleted another session's physical resource")
	}
	if _, err := os.Stat(workdir); err != nil {
		t.Fatal(err)
	}
	closed.Store(true)
	if got := d.applyGCActionWithChatMeta(context.Background(), root, gcActionClean, &gcStats{}, meta, gcRemovalDetails{}); got != 1 {
		t.Fatal("closed dependency kept owned root indefinitely")
	}
	if _, err := os.Lstat(alias); err != nil {
		t.Fatalf("GC deleted a user-owned alias: %v", err)
	}
}
