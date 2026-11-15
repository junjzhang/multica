package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const managedResourceFile = ".managed_resource.json"

func logRetainedRuntimePaths(logger *slog.Logger, taskID string, err error) {
	var retained *execenv.RetainedRuntimePathsError
	if logger != nil && errors.As(err, &retained) {
		logger.Warn("preserved_runtime_paths", "task_id", taskID, "worktree", retained.Worktree, "paths", fmt.Sprintf("%q", retained.Paths))
	}
}

type managedResource struct {
	FinalizationPending bool                            `json:"finalization_pending,omitempty"`
	DurableWorkDir      string                          `json:"durable_work_dir,omitempty"`
	OwnerTaskID         string                          `json:"owner_task_id"`
	Address             protocol.ResourceReleaseRequest `json:"address"`
}

type completedWorktree struct {
	tree           *execenv.LocalWorktree
	resource       managedResource
	root           string
	durableWorkDir string
}

func resourceForTask(task Task, provider, root, workDir, session string) managedResource {
	source := task.ID
	if task.DelegationResult != nil {
		source = task.DelegationResult.SourceTaskID
	}
	scopeType, scopeID := "issue", task.IssueID
	if scopeID == "" {
		scopeType, scopeID = "chat", task.ChatSessionID
	}
	if scopeID == "" {
		scopeType, scopeID = "autopilot", task.AutopilotRunID
	}
	if scopeID == "" {
		scopeType, scopeID = "task", originalTaskScopeID(task)
	}
	return managedResource{OwnerTaskID: task.ID, Address: protocol.ResourceReleaseRequest{
		Kind: "workdir", WorkspaceID: task.WorkspaceID, SourceTaskID: source, RuntimeID: task.RuntimeID, Provider: provider,
		AgentID: task.AgentID, ScopeType: scopeType, ScopeID: scopeID, SessionID: session,
		WorkDir: workDir, ResourcePath: root,
	}}
}

func writeManagedResource(root string, resource managedResource) error {
	data, err := json.Marshal(resource)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(root, ".managed-resource-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(root, managedResourceFile))
}

func readManagedResource(root string) (managedResource, error) {
	var resource managedResource
	data, err := os.ReadFile(filepath.Join(root, managedResourceFile))
	if err != nil {
		return resource, err
	}
	if err = json.Unmarshal(data, &resource); err != nil {
		return resource, err
	}
	if resource.OwnerTaskID == "" || resource.Address.RuntimeID == "" || resource.Address.Provider == "" || !sameResourceRoot(resource.Address.ResourcePath, root) {
		return resource, errors.New("resource identity is incomplete or moved")
	}
	return resource, nil
}

func (c *Client) ReleaseResource(ctx context.Context, request protocol.ResourceReleaseRequest) (protocol.ResourceReleaseDecision, error) {
	request.DaemonID = c.mailboxDaemonID
	var response struct {
		AdmissionClosed    *bool                              `json:"admission_closed"`
		ControlRequired    *bool                              `json:"resource_control_required"`
		ReleaseAllowed     *bool                              `json:"release_allowed"`
		Reason             string                             `json:"reason"`
		ProtectedResources *[]protocol.ManagedResourceAddress `json:"protected_resources"`
	}
	if err := c.postJSON(ctx, "/api/daemon/resources/release", request, &response); err != nil {
		return protocol.ResourceReleaseDecision{}, err
	}
	if response.AdmissionClosed == nil || response.ControlRequired == nil || response.ReleaseAllowed == nil || response.ProtectedResources == nil {
		return protocol.ResourceReleaseDecision{}, errors.New("resource release response is incomplete")
	}
	return protocol.ResourceReleaseDecision{AdmissionClosed: *response.AdmissionClosed, ControlRequired: *response.ControlRequired, ReleaseAllowed: *response.ReleaseAllowed, Reason: response.Reason, ProtectedResources: *response.ProtectedResources}, nil
}

func (d *Daemon) releaseCompletedWorktree(ctx context.Context, completed *completedWorktree) {
	if completed == nil {
		return
	}
	ws, err := os.OpenRoot(d.cfg.WorkspacesRoot)
	if err != nil {
		return
	}
	defer ws.Close()
	rel, err := relativeManagedRoot(d.cfg.WorkspacesRoot, completed.root)
	if err != nil {
		return
	}
	claim, info, err := execenv.LockEnvRootForReuse(ws, rel, completed.root)
	if err != nil || claim == nil {
		return
	}
	defer claim.Release()
	current, err := readManagedResource(completed.root)
	if err != nil || current != completed.resource {
		return
	}
	actual, err := os.Stat(completed.root)
	if err != nil || !os.SameFile(info, actual) {
		return
	}
	request := current.Address
	request.ResourcePath = filepath.Join(completed.root, "worktree")
	if completed.tree != nil {
		request.ResourcePath = completed.tree.Path
	}
	decision, err := d.client.ReleaseResource(ctx, request)
	if err != nil {
		d.logger.Warn("retaining worktree: release permission unavailable", "resource", request.ResourcePath, "error", err)
		return
	}
	if !d.permitResourceRelease(request, decision) {
		return
	}
	if completed.tree == nil && current.FinalizationPending {
		if _, err := os.Lstat(request.ResourcePath); errors.Is(err, os.ErrNotExist) {
			if err := execenv.ValidateRemovedWorktree(completed.root, current.Address.WorkDir, current.Address.WorkspaceID, current.Address.AgentID, current.Address.ScopeType, current.Address.ScopeID, current.OwnerTaskID, current.DurableWorkDir, d.logger); err != nil {
				return
			}
			if d.acknowledgeResourceRemoval(ctx, current.OwnerTaskID, request, current.DurableWorkDir) {
				current.FinalizationPending = false
				_ = writeManagedResource(completed.root, current)
			}
			return
		}
		completed.tree, err = execenv.LoadRetainedWorktree(completed.root, current.Address.WorkDir, current.Address.WorkspaceID, current.Address.AgentID, current.Address.ScopeID, d.logger)
		if err != nil {
			return
		}
	}
	if current.FinalizationPending {
		if _, err := completed.tree.Finalize(d.logger); err != nil {
			logRetainedRuntimePaths(d.logger, current.OwnerTaskID, err)
			d.logger.Warn("retaining unfinished worktree", "path", completed.tree.Path, "error", err)
			return
		}
		if err := completed.tree.SaveForReuse(completed.root); err != nil {
			return
		}
	}
	if err := completed.tree.Remove(d.logger); err != nil {
		logRetainedRuntimePaths(d.logger, current.OwnerTaskID, err)
		d.logger.Warn("retaining finalized worktree", "error", err)
		return
	}
	durable := completed.durableWorkDir
	if durable == "" {
		durable = current.DurableWorkDir
	}
	if d.acknowledgeResourceRemoval(ctx, current.OwnerTaskID, request, durable) && current.FinalizationPending {
		current.FinalizationPending = false
		_ = writeManagedResource(completed.root, current)
	}
}

func (d *Daemon) reserveProviderStoreForDeletion(ctx context.Context, provider string, retention time.Duration) func(string) (func(), bool) {
	return func(store string) (func(), bool) {
		scanned, err := os.Stat(store)
		if err != nil {
			return nil, false
		}
		localRelease, ok := d.reserveStoreForDeletion(store)
		if !ok {
			return nil, false
		}
		lock, err := execenv.TryProviderStoreWriterLock(provider, store)
		if err != nil {
			localRelease()
			return nil, false
		}
		release := func() { lock.Release(); localRelease() }
		current, err := os.Stat(store)
		if err != nil || !os.SameFile(scanned, current) {
			release()
			return nil, false
		}
		agentID, scopeType, scopeID, proven := execenv.ProviderStoreScope(d.cfg.Profile, provider, store)
		if !proven {
			release()
			return nil, false
		}
		resource, err := readManagedResource(store)
		if errors.Is(err, os.ErrNotExist) {
			resource.Address = protocol.ResourceReleaseRequest{Kind: "provider_store", Provider: provider, AgentID: agentID, ScopeType: scopeType, ScopeID: scopeID, ResourcePath: store}
			err = nil
		}
		if err != nil || resource.Address.Kind != "provider_store" || resource.Address.Provider != provider || resource.Address.AgentID != agentID || resource.Address.ScopeType != scopeType || resource.Address.ScopeID != scopeID {
			release()
			return nil, false
		}
		newest := time.Time{}
		err = filepath.WalkDir(store, func(_ string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
		if err != nil || newest.IsZero() || time.Since(newest) <= retention {
			release()
			return nil, false
		}
		decision, err := d.client.ReleaseResource(ctx, resource.Address)
		if err != nil {
			d.logger.Warn("retaining provider store: release permission unavailable", "store", store, "error", err)
			release()
			return nil, false
		}
		if !d.permitResourceRelease(resource.Address, decision) {
			release()
			return nil, false
		}
		current, err = os.Stat(store)
		if err != nil || !os.SameFile(scanned, current) {
			release()
			return nil, false
		}
		return release, true
	}
}

func (d *Daemon) resourceForGC(root string) (managedResource, error) {
	resource, err := readManagedResource(root)
	if err == nil {
		if meta, metaErr := execenv.ReadGCMeta(root); metaErr == nil && meta.TaskID != "" && meta.TaskID != resource.OwnerTaskID {
			return resource, errors.New("resource owner differs from current GC metadata")
		}
		return resource, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return resource, err
	}
	owner, err := d.gcTaskDirOwner(root)
	if err != nil {
		return resource, err
	}
	taskID := owner.TaskID
	if meta, metaErr := execenv.ReadGCMeta(root); metaErr == nil && meta.TaskID != "" {
		// Older completion metadata omits the workspace; the owner marker has
		// already proven it. Only a recorded, different workspace is a conflict.
		if meta.WorkspaceID != "" && meta.WorkspaceID != owner.WorkspaceID {
			return resource, errors.New("GC workspace differs from directory owner")
		}
		taskID = meta.TaskID
	}
	resource.OwnerTaskID = taskID
	resource.Address = protocol.ResourceReleaseRequest{Kind: "workdir", WorkspaceID: owner.WorkspaceID, SourceTaskID: taskID, ResourcePath: root}
	return resource, nil
}

func sameResourceRoot(recorded, current string) bool {
	if recorded == current {
		return true
	}
	a, err := os.Stat(recorded)
	if err != nil {
		return false
	}
	b, err := os.Stat(current)
	return err == nil && a.IsDir() && b.IsDir() && os.SameFile(a, b)
}

func durableWorktreePath(tree *execenv.LocalWorktree) string {
	rel, err := filepath.Rel(tree.Path, tree.WorkDir)
	if err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	return filepath.Join(tree.GitRoot, rel)
}

func (d *Daemon) acknowledgeResourceRemoval(ctx context.Context, taskID string, request protocol.ResourceReleaseRequest, durableWorkDir string) bool {
	receipt := protocol.ResourceReleaseReceipt{ResourceReleaseRequest: request, DurableWorkDir: durableWorkDir}
	var acknowledgement protocol.ResourceReleaseAcknowledgement
	if err := d.client.postJSON(ctx, "/api/daemon/tasks/"+taskID+"/resources-released", receipt, &acknowledgement); err != nil {
		d.logger.Warn("worktree removal metadata not acknowledged", "task", taskID, "error", err)
		return false
	}
	return acknowledgement.Recorded
}

func (d *Daemon) confirmRemovedWorktree(ctx context.Context, root string, resource managedResource) bool {
	status, err := d.client.GetTaskGCCheck(ctx, resource.OwnerTaskID)
	if err != nil || status == nil || !isAgentTaskTerminal(status.Status) || !status.ResourceControlRequired || status.TerminalReportedAt == nil || status.DurableWorkDir == nil || strings.TrimSpace(*status.DurableWorkDir) == "" || status.ResourcesRetained == nil || *status.ResourcesRetained || status.ResourceIdentity == nil {
		d.logger.Info("gc: retaining missing worktree without confirmed removal", "dir", root, "error", err)
		return false
	}
	identity, address := status.ResourceIdentity, resource.Address
	if identity.SourceTaskID != resource.OwnerTaskID || identity.WorkspaceID != address.WorkspaceID || identity.AgentID != address.AgentID || identity.RuntimeID != address.RuntimeID || identity.Provider != address.Provider || identity.SessionID != address.SessionID || identity.WorkDir != address.WorkDir {
		d.logger.Info("gc: retaining missing worktree whose owner or address changed", "dir", root)
		return false
	}
	if err := execenv.ValidateRemovedWorktree(root, address.WorkDir, address.WorkspaceID, address.AgentID, address.ScopeType, address.ScopeID, resource.OwnerTaskID, *status.DurableWorkDir, d.logger); err != nil {
		d.logger.Info("gc: retaining missing worktree without durable finalization proof", "dir", root, "error", err)
		return false
	}
	return true
}

// The resource record owns delayed finalization until removal is acknowledged.
// GC replays it after a crash even when the wait/result outbox is already acked.
func (d *Daemon) reconcileRetainedWorktree(ctx context.Context, root string) {
	resource, err := readManagedResource(root)
	if err != nil || !resource.FinalizationPending {
		return
	}
	if _, err := d.gcTaskDirOwner(root); err != nil {
		return
	}
	status, err := d.client.GetTaskGCCheck(ctx, resource.OwnerTaskID)
	if err != nil || status == nil || !isAgentTaskTerminal(status.Status) || status.TerminalReportedAt == nil || status.ResourceIdentity == nil {
		return
	}
	identity := status.ResourceIdentity
	address := resource.Address
	if identity.SourceTaskID != resource.OwnerTaskID || identity.WorkspaceID != address.WorkspaceID || identity.AgentID != address.AgentID || identity.RuntimeID != address.RuntimeID || identity.Provider != address.Provider || identity.WorkDir != address.WorkDir || identity.SessionID != address.SessionID {
		return
	}
	release, ok := d.reserveEnvRootForGC(root)
	if !ok {
		return
	}
	defer release()
	d.releaseCompletedWorktree(ctx, &completedWorktree{root: root, resource: resource, durableWorkDir: resource.DurableWorkDir})
}
