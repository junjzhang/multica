package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/backgroundwait"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestBackgroundWaitListCredentialRouting(t *testing.T) {
	dir := t.TempDir()
	taskID, otherTask, session := uuid.NewString(), uuid.NewString(), uuid.NewString()
	var receipts atomic.Int32
	var batchReads atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/background-waits/scope":
			a := protocol.WaitAddress{WorkspaceID: "ws", DaemonID: "host", AgentID: "agent"}
			switch r.Header.Get("Authorization") {
			case "Bearer mat_current":
				a.TaskID = taskID
			case "Bearer mea_session":
				a.SessionID = session
			default:
				w.WriteHeader(403)
				return
			}
			_ = json.NewEncoder(w).Encode(a)
		case "/api/daemon/background-waits/status":
			batchReads.Add(1)
			var req protocol.WaitDeliveryRequest
			_ = json.NewDecoder(r.Body).Decode(&req)

			response := protocol.WaitDeliveryResponse{Statuses: map[string]protocol.WaitDeliveryStatus{}, ObservedAt: time.Now().UTC()}
			for _, lookup := range req.Waits {
				if lookup.Address.TaskID == otherTask {
					response.Statuses[lookup.ID] = protocol.WaitDeliveryStatus{}
					continue
				}
				response.Statuses[lookup.ID] = protocol.WaitDeliveryStatus{Known: true, Enqueued: true, DeliveryStatus: "delivered", ProcessingStatus: "handled"}
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			receipts.Add(1)
			t.Errorf("list hit mutating or unexpected API %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer api.Close()
	for _, a := range []protocol.WaitAddress{{WorkspaceID: "ws", DaemonID: "host", TaskID: taskID}, {WorkspaceID: "ws", DaemonID: "host", TaskID: otherTask}, {WorkspaceID: "ws", DaemonID: "host", AgentID: "agent", SessionID: session}} {
		id := uuid.NewString()
		row := protocol.WaitStatus{WaitResult: protocol.WaitResult{ID: id, Address: a, State: "succeeded"}, DeliveryID: uuid.NewString()}
		if err := os.MkdirAll(filepath.Join(dir, "waits"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := backgroundwait.WritePrivate(filepath.Join(dir, "waits", id+".json"), row); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{cfg: Config{DaemonID: "host", ServerBaseURL: api.URL}, client: NewClient(api.URL)}
	d.client.SetToken("daemon-token")
	if err := d.openBackgroundWaits(t.Context(), dir, "http://127.0.0.1:1", nil); err != nil {
		t.Fatal(err)
	}
	defer d.backgroundWaits.Close()
	d.registerActiveRepoCheckoutTask("mat_current", activeRepoCheckoutTask{TaskID: taskID, WorkspaceID: "ws"})
	// A registry entry alone does not validate a revoked server credential.
	d.registerActiveRepoCheckoutTask("mat_revoked", activeRepoCheckoutTask{TaskID: taskID, WorkspaceID: "ws"})
	for _, tc := range []struct {
		token, query string
		code, count  int
	}{{"mat_current", "", 200, 1}, {"mat_current", "?session=" + session, 403, 0}, {"mat_current", "?task=" + otherTask, 403, 0}, {"mat_revoked", "", 403, 0}, {"mea_session", "", 200, 1}, {"mea_session", "?task=" + taskID, 403, 0}, {"mea_unregistered", "", 403, 0}} {
		t.Run(tc.token+tc.query, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/waits"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("X-Workspace-ID", "ws")
			w := httptest.NewRecorder()
			d.backgroundWaitHandler()(w, req)
			if w.Code != tc.code {
				t.Fatalf("status=%d %s", w.Code, w.Body.String())
			}
			if tc.code == 200 {
				var out protocol.WaitList
				if json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Items) != tc.count || out.Items[0].Delivery.ProcessingStatus != "handled" {
					t.Fatalf("list %s", w.Body.String())
				}
				if strings.Contains(w.Body.String(), "log_path") {
					t.Fatal("private log leaked")
				}
			}
		})
	}
	var endpoint protocol.WaitEndpoint
	data, err := os.ReadFile(filepath.Join(dir, "wait.endpoint.json"))
	if err != nil || json.Unmarshal(data, &endpoint) != nil {
		t.Fatal("read owner endpoint")
	}
	beforeReads := batchReads.Load()
	ownerReq := httptest.NewRequest("GET", "/waits", nil)
	ownerReq.Header.Set("Authorization", "Bearer "+endpoint.Secret)
	ownerResponse := httptest.NewRecorder()
	d.backgroundWaitHandler()(ownerResponse, ownerReq)
	if batchReads.Load()-beforeReads != 1 {
		t.Fatal("list made one HTTP request per frozen target")
	}
	var ownerList protocol.WaitList
	if ownerResponse.Code != 200 || json.Unmarshal(ownerResponse.Body.Bytes(), &ownerList) != nil || len(ownerList.Items) != 3 {
		t.Fatalf("owner list %s", ownerResponse.Body.String())
	}
	for _, item := range ownerList.Items {
		if item.Address.TaskID == otherTask {
			if item.Delivery.Known {
				t.Fatal("failed scope appeared current")
			}
			continue
		}
		if !item.Delivery.Known || item.Delivery.ProcessingStatus != "handled" {
			t.Fatalf("unavailable scope poisoned healthy receipt %+v", item)
		}
	}
	if receipts.Load() != 0 {
		t.Fatal("list wrote a processing receipt")
	}
}
