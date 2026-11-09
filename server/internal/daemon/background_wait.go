package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/multica-ai/multica/server/internal/backgroundwait"
	"github.com/multica-ai/multica/server/internal/cli"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

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
	authorize := func(ctx context.Context, token, workspaceID, sessionRef string) (protocol.WaitAddress, error) {
		client := cli.NewAPIClient(d.cfg.ServerBaseURL, workspaceID, token)
		client.ReturnSession = sessionRef
		var address protocol.WaitAddress
		err := client.PostJSON(ctx, "/api/background-waits/authorize", map[string]string{"daemon_id": d.cfg.DaemonID}, &address)
		return address, err
	}
	notify := func(ctx context.Context, result protocol.WaitResult) (string, error) {
		var response struct {
			ID string `json:"delivery_id"`
		}
		err := d.client.postDaemonDeliveryJSON(ctx, result.Address.WorkspaceID, "/api/daemon/background-waits/complete", result, &response)
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
	d.backgroundWaits = manager
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
		d.backgroundWaits.ServeHTTP(w, r)
	}
}
