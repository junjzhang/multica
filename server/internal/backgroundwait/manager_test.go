package backgroundwait

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestWaitProcess(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "wait-helper" {
		os.Exit(ExecuteHelper(os.Stdin))
	}
}

func openTestManager(t *testing.T, dir string, notify Notify) *Manager {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixtures")
	}
	auth := func(_ context.Context, token string, req protocol.WaitStart) (protocol.WaitAddress, error) {
		ws, session := req.WorkspaceID, req.SessionRef
		if token != "external-test-token" || ws != "workspace" || session != "original-session" {
			return protocol.WaitAddress{}, fmt.Errorf("unauthorized")
		}
		return protocol.WaitAddress{WorkspaceID: ws, AgentID: "agent", SessionID: "frozen-session-row", DaemonID: "daemon"}, nil
	}
	m, err := Open(t.Context(), dir, "local-capability", "daemon", []string{os.Args[0], "-test.run=^TestWaitProcess$", "--", "wait-helper"}, auth, notify, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func waitRequest(t *testing.T, m *Manager, method, path, capability, token string, body any, status int) protocol.WaitStatus {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+capability)
	req.Header.Set("X-Wait-Actor-Token", token)
	w := httptest.NewRecorder()
	m.ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("%s %s = %d: %s", method, path, w.Code, w.Body.String())
	}
	var result protocol.WaitStatus
	if status == 200 && json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatal("invalid response")
	}
	return result
}

func startRequest(t *testing.T, command string) protocol.WaitStart {
	return protocol.WaitStart{ID: uuid.NewString(), WorkspaceID: "workspace", SessionRef: "original-session", Argv: []string{"/bin/sh", "-c", command}, Cwd: t.TempDir(), Deadline: time.Now().UTC().Add(10 * time.Second)}
}

func awaitResult(t *testing.T, results <-chan protocol.WaitResult) protocol.WaitResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(15 * time.Second):
		t.Fatal("no terminal result")
		return protocol.WaitResult{}
	}
}

func TestWaitTerminalAndRegistrationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, command, want string
		code                int
	}{
		{"success", "printf 'private tool output'", "succeeded", 0},
		{"failure", "exit 7", "failed", 7},
		{"timeout", "sleep 20", "timed_out", 125},
		{"cancel", "sleep 20", "cancelled", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := make(chan protocol.WaitResult, 4)
			m := openTestManager(t, t.TempDir(), func(_ context.Context, result protocol.WaitResult) (string, error) {
				results <- result
				return "mailbox-id", nil
			})
			req := startRequest(t, tc.command)
			if tc.want == "timed_out" {
				req.Deadline = time.Now().UTC().Add(300 * time.Millisecond)
			}
			waitRequest(t, m, "POST", "/waits", "", "external-test-token", req, 401)
			waitRequest(t, m, "POST", "/waits", "local-capability", "wrong-token", req, 403)
			registered := waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", req, 200)
			if registered.State != "running" {
				t.Fatalf("registration = %+v", registered)
			}
			waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", req, 200)
			withOffset := req
			withOffset.Deadline = req.Deadline.In(time.FixedZone("", 8*60*60))
			waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", withOffset, 200)
			changed := req
			changed.Argv = []string{"other-command"}
			waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", changed, 409)
			if tc.want == "cancelled" {
				waitRequest(t, m, "DELETE", "/waits/"+req.ID, "local-capability", "", nil, 200)
			}
			result := awaitResult(t, results)
			if result.State != tc.want || result.Address.SessionID != "frozen-session-row" {
				t.Fatalf("result = %+v", result)
			}
			if tc.want == "succeeded" || tc.want == "failed" {
				if result.ExitCode == nil || *result.ExitCode != tc.code {
					t.Fatalf("exit code = %v", result.ExitCode)
				}
			}
			// Terminal cancellation is idempotent and cannot overwrite an exit.
			again := waitRequest(t, m, "DELETE", "/waits/"+req.ID, "local-capability", "", nil, 200)
			if again.State != tc.want {
				t.Fatalf("cancel overwrote terminal state: %+v", again)
			}
			data, err := os.ReadFile(filepath.Join(m.dir, req.ID+".json"))
			if err != nil || !strings.Contains(string(data), `"state":"`+tc.want+`"`) {
				t.Fatalf("terminal not durable: %s %v", data, err)
			}
			payload, _ := json.Marshal(result)
			if strings.Contains(string(payload), "private tool output") || strings.Contains(string(data), "external-test-token") {
				t.Fatal("output or credential escaped into result/journal")
			}
		})
	}
}

func TestWaitConcurrentRegistrationCancelCompletion(t *testing.T) {
	results := make(chan protocol.WaitResult, 20)
	m := openTestManager(t, t.TempDir(), func(_ context.Context, result protocol.WaitResult) (string, error) {
		results <- result
		return "receipt", nil
	})
	req := startRequest(t, "printf x >> executions; sleep 0.1")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", req, 200)
		}()
	}
	wg.Wait()
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			page := readList(t, m, "/waits", protocol.WaitAddress{}, 200)
			if len(page.Items) != 1 || page.Items[0].ID != req.ID {
				t.Errorf("concurrent list lost registered wait: %+v", page.Items)
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			waitRequest(t, m, "DELETE", "/waits/"+req.ID, "local-capability", "", nil, 200)
		}()
	}
	wg.Wait()
	result := awaitResult(t, results)
	latest := readList(t, m, "/waits", protocol.WaitAddress{}, 200)
	if latest.Items[0].State != result.State {
		t.Fatal("list lost terminal cancellation decision")
	}
	if result.State != "succeeded" && result.State != "cancelled" {
		t.Fatalf("unexpected race result: %+v", result)
	}
	m.Close()
	data, err := os.ReadFile(filepath.Join(req.Cwd, "executions"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(data) > 1 {
		t.Fatalf("duplicate command executed %d times", len(data))
	}
}

func TestWaitRestartLostAndOutboxReplay(t *testing.T) {
	dir := t.TempDir()
	request := startRequest(t, "touch must-not-run")
	address := protocol.WaitAddress{WorkspaceID: "workspace", AgentID: "agent", SessionID: "frozen-session-row", DaemonID: "daemon"}
	row := record{Request: request, WaitStatus: protocol.WaitStatus{WaitResult: protocol.WaitResult{ID: request.ID, Address: address, State: "running", LogPath: filepath.Join(dir, "wait.log")}, Deadline: request.Deadline}}
	if err := WritePrivate(filepath.Join(dir, request.ID+".json"), row); err != nil {
		t.Fatal(err)
	}
	first := make(chan protocol.WaitResult, 1)
	m := openTestManager(t, dir, func(_ context.Context, result protocol.WaitResult) (string, error) {
		first <- result
		return "", fmt.Errorf("response lost after enqueue")
	})
	lost := awaitResult(t, first)
	if lost.State != "lost" {
		t.Fatalf("recovery = %+v", lost)
	}
	m.Close()
	second := make(chan protocol.WaitResult, 1)
	next := openTestManager(t, dir, func(_ context.Context, result protocol.WaitResult) (string, error) {
		second <- result
		return "same-stable-delivery", nil
	})
	replayed := awaitResult(t, second)
	if !replayed.FinishedAt.Equal(lost.FinishedAt) || replayed.ID != lost.ID || replayed.Address != lost.Address {
		t.Fatalf("replay changed result: %+v vs %+v", replayed, lost)
	}
	next.Close()
	if _, err := os.Stat(filepath.Join(request.Cwd, "must-not-run")); !os.IsNotExist(err) {
		t.Fatal("restart reran arbitrary command")
	}
	for _, path := range []string{dir, filepath.Join(dir, request.ID+".json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("not private: %s %v", path, err)
		}
	}
}

func TestWaitLogBound(t *testing.T) {
	results := make(chan protocol.WaitResult, 1)
	m := openTestManager(t, t.TempDir(), func(_ context.Context, result protocol.WaitResult) (string, error) {
		results <- result
		return "receipt", nil
	})
	req := startRequest(t, "head -c 2097152 /dev/zero")
	waitRequest(t, m, http.MethodPost, "/waits", "local-capability", "external-test-token", req, 200)
	result := awaitResult(t, results)
	info, err := os.Stat(result.LogPath)
	if err != nil || info.Size() != MaxLogBytes || result.State != "succeeded" {
		t.Fatalf("bounded log: %v %v %+v", info, err, result)
	}
}

func TestWaitDaemonCrashHarness(t *testing.T) {
	dir := os.Getenv("BACKGROUND_WAIT_TEST_CRASH_DIR")
	if dir == "" {
		return
	}
	m := openTestManager(t, filepath.Join(dir, "journal"), func(context.Context, protocol.WaitResult) (string, error) { return "", fmt.Errorf("offline") })
	req := startRequest(t, "touch started; sleep 1; touch escaped")
	req.Cwd = dir
	waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", req, 200)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			os.Exit(0)
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.Exit(2)
}

func TestWaitDaemonDeathStopsCommandTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWaitDaemonCrashHarness$")
	// The race runtime otherwise delays os.Exit for one second, allowing the
	// fixture to finish before the simulated daemon death actually happens.
	cmd.Env = append(os.Environ(), "BACKGROUND_WAIT_TEST_CRASH_DIR="+dir, "GORACE=atexit_sleep_ms=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash harness: %v %s", err, out)
	}
	// The fixture's command would publish escaped after one second if its
	// process tree survived the daemon's abrupt exit.
	time.Sleep(1400 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(err) {
		t.Fatal("wait command survived daemon death")
	}
	results := make(chan protocol.WaitResult, 1)
	m := openTestManager(t, filepath.Join(dir, "journal"), func(_ context.Context, result protocol.WaitResult) (string, error) {
		results <- result
		return "receipt", nil
	})
	if result := awaitResult(t, results); result.State != "lost" {
		t.Fatalf("recovered state = %+v", result)
	}
	m.Close()
}

func TestWaitLocalRejectionsDoNotRegisterObligations(t *testing.T) {
	m := openTestManager(t, t.TempDir(), func(context.Context, protocol.WaitResult) (string, error) { return "id", nil })
	calls := 0
	m.authorize = func(context.Context, string, protocol.WaitStart) (protocol.WaitAddress, error) {
		calls++
		return protocol.WaitAddress{}, fmt.Errorf("unexpected registration")
	}
	req := startRequest(t, "exit 0")
	m.admit = func() (func(), bool) { return nil, false }
	waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", req, 503)
	m.admit = nil
	expired := req
	expired.Deadline = time.Now().Add(-time.Second)
	waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", expired, 400)
	m.rows[req.ID] = &record{Request: req}
	changed := req
	changed.Argv = []string{"different"}
	waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", changed, 409)
	delete(m.rows, req.ID)
	for i := 0; i < 32; i++ {
		m.running[fmt.Sprint(i)] = func() {}
	}
	waitRequest(t, m, "POST", "/waits", "local-capability", "external-test-token", req, 429)
	if calls != 0 {
		t.Fatalf("local rejection created %d server obligations", calls)
	}
}
