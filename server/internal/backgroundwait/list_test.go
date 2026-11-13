package backgroundwait

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func listFixture(t *testing.T) *Manager {
	t.Helper()
	return &Manager{dir: t.TempDir(), daemonID: "daemon", secret: "owner", rows: map[string]*record{}}
}
func addListRow(t *testing.T, m *Manager, a protocol.WaitAddress) string {
	t.Helper()
	id := uuid.NewString()
	a.DaemonID = m.daemonID
	m.rows[id] = &record{WaitStatus: protocol.WaitStatus{WaitResult: protocol.WaitResult{ID: id, Address: a, State: "succeeded"}, Deadline: time.Now().UTC().Add(time.Hour)}}
	if err := m.save(m.rows[id]); err != nil {
		t.Fatal(err)
	}
	return id
}
func readList(t *testing.T, m *Manager, path string, scope protocol.WaitAddress, code int) protocol.WaitList {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+m.secret)
	w := httptest.NewRecorder()
	if scope.SessionID != "" {
		m.ServeSessionList(w, req, scope)
	} else if scope.TaskID != "" {
		m.ServeManagedHTTP(w, req, scope.TaskID)
	} else {
		m.ServeHTTP(w, req)
	}
	if w.Code != code {
		t.Fatalf("%s = %d: %s", path, w.Code, w.Body.String())
	}
	var out protocol.WaitList
	if code == 200 && json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("invalid list")
	}
	return out
}
func TestListIsolationAndNoConsumption(t *testing.T) {
	m := listFixture(t)
	own := protocol.WaitAddress{WorkspaceID: "ws", AgentID: "agent", SessionID: "session", DaemonID: "daemon"}
	ownID := addListRow(t, m, own)
	addListRow(t, m, protocol.WaitAddress{WorkspaceID: "ws", AgentID: "agent", SessionID: "other-session"})
	addListRow(t, m, protocol.WaitAddress{WorkspaceID: "other-ws", AgentID: "agent", SessionID: "session"})
	taskID := addListRow(t, m, protocol.WaitAddress{WorkspaceID: "ws", TaskID: "task", SessionID: "provider-pin"})
	addListRow(t, m, protocol.WaitAddress{WorkspaceID: "ws", TaskID: "other-task", SessionID: "provider-pin"})
	foreign := addListRow(t, m, own)
	m.rows[foreign].Address.DaemonID = "other-host"
	m.consume = func(context.Context, string, protocol.WaitResult) error {
		t.Fatal("list consumed a result")
		return nil
	}
	before, _ := os.ReadFile(filepath.Join(m.dir, taskID+".json"))
	out := readList(t, m, "/waits", own, 200)
	if len(out.Items) != 1 || out.Items[0].ID != ownID {
		t.Fatalf("session leaked: %+v", out.Items)
	}
	managed := readList(t, m, "/waits", protocol.WaitAddress{TaskID: "task"}, 200)
	if len(managed.Items) != 1 || managed.Items[0].ID != taskID {
		t.Fatalf("task leaked: %+v", managed.Items)
	}
	after, _ := os.ReadFile(filepath.Join(m.dir, taskID+".json"))
	if !reflect.DeepEqual(before, after) || m.rows[taskID].Handled {
		t.Fatal("list mutated journal")
	}
	readList(t, m, "/waits?session=other-session", own, 403)
	readList(t, m, "/waits?session=provider-pin", protocol.WaitAddress{TaskID: "task"}, 403)
	owner := readList(t, m, "/waits", protocol.WaitAddress{}, 200)
	if len(owner.Items) != 5 {
		t.Fatalf("owner host scope count=%d", len(owner.Items))
	}
	req := httptest.NewRequest("GET", "/waits", nil)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("unauthenticated owner list")
	}
}
func TestListPaginationSnapshotAndReceiptFilters(t *testing.T) {
	m := listFixture(t)
	for i := 0; i < 7; i++ {
		addListRow(t, m, protocol.WaitAddress{WorkspaceID: "ws"})
	}
	seen := map[string]bool{}
	path := "/waits?limit=2"
	version := ""
	firstCursor := ""
	for {
		page := readList(t, m, path, protocol.WaitAddress{}, 200)
		if version != "" && version != page.SnapshotVersion {
			t.Fatal("unstable unchanged snapshot")
		}
		version = page.SnapshotVersion
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatal("duplicate row")
			}
			seen[item.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		if firstCursor == "" {
			firstCursor = page.NextCursor
		}
		path = "/waits?limit=2&cursor=" + url.QueryEscape(page.NextCursor)
	}
	if len(seen) != 7 {
		t.Fatalf("missed rows: %d", len(seen))
	}
	addListRow(t, m, protocol.WaitAddress{WorkspaceID: "ws"})
	readList(t, m, "/waits?limit=2&cursor="+url.QueryEscape(firstCursor), protocol.WaitAddress{}, 409)
	m.ConfigureList(func(_ context.Context, ls []protocol.WaitLookup) (protocol.WaitDeliveryResponse, error) {
		out := protocol.WaitDeliveryResponse{Statuses: map[string]protocol.WaitDeliveryStatus{}, ObservedAt: time.Now().UTC()}
		for i, l := range ls {
			status := protocol.WaitDeliveryStatus{Known: true, Enqueued: true, DeliveryStatus: "delivered", ProcessingStatus: "unread"}
			if i%2 == 0 {
				status.ProcessingStatus = "handled"
			}
			out.Statuses[l.ID] = status
		}
		return out, nil
	})
	handled := readList(t, m, "/waits?processing_status=handled", protocol.WaitAddress{}, 200)
	if len(handled.Items) != 4 || handled.DeliveryObservedAt == nil {
		t.Fatalf("handled filter %+v", handled)
	}
	m.ConfigureList(func(context.Context, []protocol.WaitLookup) (protocol.WaitDeliveryResponse, error) {
		return protocol.WaitDeliveryResponse{}, fmt.Errorf("offline")
	})
	unknown := readList(t, m, "/waits?delivery_status=unknown", protocol.WaitAddress{}, 200)
	if len(unknown.Items) != 8 || unknown.DeliveryObservedAt != nil {
		t.Fatal("offline receipts appeared current")
	}
	readList(t, m, "/waits?state=typo", protocol.WaitAddress{}, 400)
}
func TestListRestartPreservesStartAndLegacyMissingMetadata(t *testing.T) {
	dir := t.TempDir()
	started := time.Now().UTC().Add(-time.Hour)
	old := record{WaitStatus: protocol.WaitStatus{WaitResult: protocol.WaitResult{ID: uuid.NewString(), Address: protocol.WaitAddress{DaemonID: "daemon"}, State: "running"}, StartedAt: &started, Description: "Build"}}
	legacy := record{WaitStatus: protocol.WaitStatus{WaitResult: protocol.WaitResult{ID: uuid.NewString(), Address: protocol.WaitAddress{DaemonID: "daemon"}, State: "succeeded"}, DeliveryID: "old-delivery"}}
	for _, row := range []record{old, legacy} {
		if err := WritePrivate(filepath.Join(dir, row.ID+".json"), row); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Open(t.Context(), dir, "owner", "daemon", nil, nil, func(context.Context, protocol.WaitResult) (string, error) { return "", fmt.Errorf("offline") }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	out := readList(t, m, "/waits", protocol.WaitAddress{}, 200)
	for _, item := range out.Items {
		if item.ID == old.ID && (item.State != "lost" || item.StartedAt == nil || !item.StartedAt.Equal(started) || item.Description != "Build") {
			t.Fatalf("restart metadata %+v", item)
		}
		if item.ID == legacy.ID && (item.StartedAt != nil || item.Description != "" || item.Address.IssueID != "") {
			t.Fatalf("invented legacy metadata %+v", item)
		}
	}
}
