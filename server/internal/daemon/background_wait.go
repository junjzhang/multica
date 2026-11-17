package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/multica-ai/multica/server/internal/backgroundwait"
	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type waitTaskContextKey struct{}
type taskTurnContextKey struct{}

func (report terminalTaskReport) queueID() string {
	if report.generation == "" {
		return report.taskID
	}
	return report.taskID + ":" + report.generation
}

func addTaskTurn(ctx context.Context, body map[string]any) {
	if turn, ok := ctx.Value(taskTurnContextKey{}).(protocol.TaskTurn); ok {
		body["turn_generation"] = turn.Generation
		if turn.KnownWaitIDs != nil {
			body["known_wait_ids"] = turn.KnownWaitIDs
		}
	}
}

func (d *Daemon) startBackgroundWaits(ctx context.Context, healthAddr net.Addr) error {
	dir, err := cli.ProfileDir(d.cfg.Profile)
	if err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	return d.openBackgroundWaits(ctx, dir, "http://"+healthAddr.String(), []string{binary, "internal-wait-exec"})
}

func (d *Daemon) openBackgroundWaits(ctx context.Context, dir, localURL string, helper []string) error {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	endpoint := protocol.WaitEndpoint{URL: localURL, Secret: hex.EncodeToString(secret), DaemonID: d.cfg.DaemonID}
	authorize := func(ctx context.Context, token string, req protocol.WaitStart) (protocol.WaitAddress, error) {
		if req.TaskID != "" {
			task, ok := ctx.Value(waitTaskContextKey{}).(activeRepoCheckoutTask)
			if !ok || task.TaskID != req.TaskID || task.WorkspaceID != req.WorkspaceID {
				return protocol.WaitAddress{}, fmt.Errorf("active task credential required")
			}
			cwd, err := authorizeRepoCheckoutWorkDir(task.WorkDir, req.Cwd)
			if err != nil || cwd != req.Cwd {
				return protocol.WaitAddress{}, fmt.Errorf("wait cwd must be inside the active task directory")
			}
			if task.LocalWorktree != nil {
				if err := task.LocalWorktree.SaveForReuse(task.EnvRoot); err != nil {
					return protocol.WaitAddress{}, err
				}
				resource, err := readManagedResource(task.EnvRoot)
				if err != nil {
					return protocol.WaitAddress{}, err
				}
				resource.FinalizationPending = true
				resource.DurableWorkDir = durableWorktreePath(task.LocalWorktree)
				if err := writeManagedResource(task.EnvRoot, resource); err != nil {
					return protocol.WaitAddress{}, err
				}
			}
		}
		client := cli.NewAPIClient(d.cfg.ServerBaseURL, req.WorkspaceID, token)
		client.ReturnSession = req.SessionRef
		data, err := json.Marshal(req)
		if err != nil {
			return protocol.WaitAddress{}, err
		}
		hash := sha256.Sum256(data)
		var address protocol.WaitAddress
		err = client.PostJSON(ctx, "/api/background-waits/authorize", protocol.WaitAuthorization{DaemonID: d.cfg.DaemonID, ID: req.ID, RequestHash: hex.EncodeToString(hash[:]), Deadline: req.Deadline, IssueRef: req.IssueRef}, &address)
		return address, err
	}
	notify := func(ctx context.Context, result protocol.WaitResult) (string, error) {
		var response struct {
			ID string `json:"delivery_id"`
		}
		err := d.client.postDaemonDeliveryJSON(ctx, result.Address.WorkspaceID, "/api/daemon/background-waits/complete", result, &response)
		if err == nil && response.ID != "" && result.Address.TaskID != "" {
			d.reconcileRetainedWorktree(ctx, managedRootForWorkDir(d.cfg.WorkspacesRoot, result.Address.WorkDir))
		}
		return response.ID, err
	}
	admit := func() (func(), bool) {
		// Lock order is claimMu then the manager's journal mutex, including
		// Active() in the update barriers. No network call holds claimMu.
		d.claimMu.Lock()
		if d.pauseClaims {
			d.claimMu.Unlock()
			return nil, false
		}
		return d.claimMu.Unlock, true
	}
	manager, err := backgroundwait.Open(ctx, filepath.Join(dir, "waits"), endpoint.Secret, d.cfg.DaemonID, helper, authorize, notify, admit)
	if err != nil {
		return fmt.Errorf("open background waits: %w", err)
	}
	if err := backgroundwait.WritePrivate(filepath.Join(dir, "wait.endpoint.json"), endpoint); err != nil {
		manager.Close()
		return err
	}
	manager.ConfigureManaged(func(ctx context.Context, token string, result protocol.WaitResult) error {
		// Materialize first when get races the asynchronous outbox, then consume
		// using only the requesting task's credential.
		if _, err := notify(ctx, result); err != nil {
			return err
		}
		client := cli.NewAPIClient(d.cfg.ServerBaseURL, result.Address.WorkspaceID, token)
		return client.PostJSON(ctx, "/api/background-waits/"+result.ID+"/read", map[string]any{}, nil)
	}, func(ctx context.Context, address protocol.WaitAddress) (bool, error) {
		state, err := d.client.GetTaskStatus(ctx, address.TaskID)
		if err != nil {
			code := mailboxStatus(err)
			if code == 404 || code == 403 {
				return true, nil
			}
			return false, err
		}
		return state == "cancelled" || state == "failed" || state == "completed", nil
	})
	manager.ReauthorizeWith(func(ctx context.Context, token string, req protocol.WaitStart) (protocol.WaitAddress, error) {
		client := cli.NewAPIClient(d.cfg.ServerBaseURL, req.WorkspaceID, token)
		var status protocol.WaitStatus
		err := client.PostJSON(ctx, "/api/background-waits/"+req.ID+"/access", map[string]any{}, &status)
		return status.Address, err
	})
	manager.ConfigureList(func(ctx context.Context, lookups []protocol.WaitLookup) (protocol.WaitDeliveryResponse, error) {
		out := protocol.WaitDeliveryResponse{Statuses: map[string]protocol.WaitDeliveryStatus{}}
		// The batch endpoint isolates unavailable targets within a workspace.
		// Keep independent workspace observations when a whole request fails.
		groups := map[string][]protocol.WaitLookup{}
		var lastError error
		for _, lookup := range lookups {
			groups[lookup.Address.WorkspaceID] = append(groups[lookup.Address.WorkspaceID], lookup)
		}
		for workspace, group := range groups {
			for start := 0; start < len(group); start += 200 {
				var part protocol.WaitDeliveryResponse
				err := d.client.postDaemonDeliveryJSON(ctx, workspace, "/api/daemon/background-waits/status", protocol.WaitDeliveryRequest{Waits: group[start:min(start+200, len(group))]}, &part)
				if err != nil {
					lastError = err
					continue
				}
				for id, status := range part.Statuses {
					out.Statuses[id] = status
				}
				if out.ObservedAt.IsZero() || part.ObservedAt.Before(out.ObservedAt) {
					out.ObservedAt = part.ObservedAt
				}
			}
		}
		if out.ObservedAt.IsZero() {
			return out, lastError
		}
		return out, nil
	})
	d.backgroundWaits = manager
	d.client.backgroundWaitIDs = manager.KnownIDs
	return nil
}

func (d *Daemon) activeBackgroundWaits() int {
	if d.backgroundWaits == nil {
		return 0
	}
	return d.backgroundWaits.Active()
}

func (d *Daemon) backgroundWaitHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.backgroundWaits == nil {
			http.Error(w, "background waits unavailable", 503)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/waits" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer mea_") {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			client := cli.NewAPIClient(d.cfg.ServerBaseURL, r.Header.Get("X-Workspace-ID"), token)
			var address protocol.WaitAddress
			if err := client.GetJSON(r.Context(), "/api/background-waits/scope?daemon_id="+url.QueryEscape(d.cfg.DaemonID), &address); err != nil {
				http.Error(w, "external session scope unavailable", 403)
				return
			}
			d.backgroundWaits.ServeSessionList(w, r, address)
			return
		}
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer mat_") {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			d.repoCheckoutTasksMu.RLock()
			task, ok := d.repoCheckoutTasks[token]
			if !ok || task.waitGate == nil {
				d.repoCheckoutTasksMu.RUnlock()
				http.Error(w, "active task credential required", 403)
				return
			}
			task.waitGate.RLock()
			d.repoCheckoutTasksMu.RUnlock()
			defer task.waitGate.RUnlock()
			r.Header.Set("X-Wait-Actor-Token", token)
			if r.Method == http.MethodGet && r.URL.Path == "/waits" {
				client := cli.NewAPIClient(d.cfg.ServerBaseURL, task.WorkspaceID, token)
				var address protocol.WaitAddress
				if err := client.GetJSON(r.Context(), "/api/background-waits/scope?daemon_id="+url.QueryEscape(d.cfg.DaemonID), &address); err != nil || address.TaskID != task.TaskID || address.WorkspaceID != task.WorkspaceID {
					http.Error(w, "current task scope unavailable", 403)
					return
				}
			}
			if r.Method != http.MethodPost && !(r.Method == http.MethodGet && r.URL.Path == "/waits") {
				id := strings.TrimPrefix(r.URL.Path, "/waits/")
				if _, err := util.ParseUUID(id); err != nil {
					http.Error(w, "invalid wait id", 400)
					return
				}
				client := cli.NewAPIClient(d.cfg.ServerBaseURL, task.WorkspaceID, token)
				if err := client.PostJSON(r.Context(), "/api/background-waits/"+id+"/access", map[string]any{}, nil); err != nil {
					http.Error(w, "current task credential or wait scope is unavailable", 403)
					return
				}
			}
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/waits/") && !d.backgroundWaits.HasID(strings.TrimPrefix(r.URL.Path, "/waits/")) {
				client := cli.NewAPIClient(d.cfg.ServerBaseURL, task.WorkspaceID, token)
				var status protocol.WaitStatus
				id := strings.TrimPrefix(r.URL.Path, "/waits/")
				if err := client.PostJSON(r.Context(), "/api/background-waits/"+id+"/read", map[string]any{}, &status); err != nil {
					http.Error(w, "wait not found in current task", 404)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(status)
				return
			}
			d.backgroundWaits.ServeManagedHTTP(w, r.WithContext(context.WithValue(r.Context(), waitTaskContextKey{}, task)), task.TaskID)
			return
		}
		d.backgroundWaits.ServeHTTP(w, r)
	}
}

func (d *Daemon) reconcileWaitReads(ctx context.Context, task Task, token string) {
	if d.backgroundWaits == nil {
		return
	}
	client := cli.NewAPIClient(d.cfg.ServerBaseURL, task.WorkspaceID, token)
	for _, id := range d.backgroundWaits.KnownTaskIDs(task.ID) {
		var status protocol.WaitStatus
		if err := client.PostJSON(ctx, "/api/background-waits/"+id+"/access", map[string]any{}, &status); err == nil && status.Consumed {
			_ = d.backgroundWaits.MarkHandled(task.ID, id)
		}
	}
}
