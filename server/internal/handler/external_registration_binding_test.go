package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestExternalRegistrationKeepsSessionAndDaemonBinding(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	ownedDaemon := "owned-" + suffix
	foreignDaemon := "foreign-" + suffix
	foreignOwner := dbfx.User(t, "Other owner", "other-"+suffix+"@test.local")
	dbfx.Member(t, testWorkspaceID, foreignOwner, "member")
	dbfx.Runtime(t, "Owned host", testutil.Cols{"daemon_id": ownedDaemon})
	dbfx.Runtime(t, "Foreign host", testutil.Cols{"daemon_id": foreignDaemon, "owner_id": foreignOwner})
	for _, tc := range []struct {
		name, daemon, link string
	}{
		{"foreign-daemon", foreignDaemon, "paseo://h/host-1/agent/session-1"},
		{"wrong-session-url", ownedDaemon, "paseo://h/host-2/agent/session-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRequest(http.MethodPost, "/api/agents/external", map[string]any{
				"name": tc.name + suffix,
				"external_session": map[string]any{
					"provider": "paseo", "host_id": "host-1", "session_id": "session-1",
					"display_name": "Session", "url": tc.link, "daemon_id": tc.daemon,
				},
			})
			w := httptest.NewRecorder()
			testHandler.CreateExternalAgent(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid session binding accepted: status=%d", w.Code)
			}
		})
	}
}
