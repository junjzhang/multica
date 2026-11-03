package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

// This child is a fake provider process. Its HTTP barriers model provider
// tools independently of the daemon's asynchronous session-pin request.
func TestSessionPinProviderProcess(t *testing.T) {
	url := os.Getenv("RESULT_PIN_HELPER_URL")
	if url == "" {
		return
	}
	resume := false
	for _, arg := range os.Args {
		if arg == "--resume" {
			resume = true
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	call := func(path string) {
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Get(url + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("provider barrier %s returned %d", path, response.StatusCode)
		}
	}
	if resume {
		fmt.Println(`{"type":"system","session_id":"session-leader-reuse"}`)
		call("/await-first-pin")
		fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"No conversation found with session ID: session-leader-reuse"}`)
		return
	}
	call("/fresh-input")
	fmt.Println(`{"type":"system","session_id":"new-session-B"}`)
	call("/await-new-pin")
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"session_id":"new-session-B","result":"done"}`)
}

func TestFreshRetryRequiresAcknowledgedPinGeneration(t *testing.T) {
	for _, mode := range []string{"confirmed-old-pin", "late-old-pin", "unknown-invalidation"} {
		t.Run(mode, func(t *testing.T) {
			d, _, cleanup := newLeaderReuseTestDaemon(t)
			defer cleanup()
			source := leaderReuseTestTask("source")
			first, err := d.runTask(context.Background(), source, "claude", 0, d.logger)
			if err != nil {
				t.Fatal(err)
			}
			const initial = "22222222-2222-4222-8222-222222222222"
			var mu sync.Mutex
			generation := initial
			confirmed := false
			oldEntered, oldFinished, newFinished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseOld := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(releaseOld) })
			var freshInputs, oldPins, rejectedOld atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/session"):
					var request protocol.SessionPinRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						http.Error(w, "decode", 400)
						return
					}
					if request.NextGeneration != "" {
						mu.Lock()
						if request.Generation != generation {
							mu.Unlock()
							http.Error(w, "generation", 409)
							return
						}
						generation = request.NextGeneration
						confirmed = false
						mu.Unlock()
						if mode == "unknown-invalidation" {
							http.Error(w, "ack lost", 503)
							return
						}
						json.NewEncoder(w).Encode(protocol.SessionPinAcknowledgement{Generation: request.NextGeneration})
						return
					}
					if request.SessionID == "session-leader-reuse" {
						oldPins.Add(1)
						close(oldEntered)
						if mode == "late-old-pin" {
							select {
							case <-releaseOld:
							case <-r.Context().Done():
								return
							}
						}
						mu.Lock()
						stale := request.Generation != generation
						if !stale {
							confirmed = true
						}
						mu.Unlock()
						if stale {
							rejectedOld.Add(1)
							http.Error(w, "stale attempt", 409)
						} else {
							w.WriteHeader(204)
						}
						close(oldFinished)
						return
					}
					if request.SessionID == "new-session-B" {
						mu.Lock()
						if request.Generation == initial || request.Generation != generation {
							t.Error("new provider pin used the previous attempt generation")
						}
						confirmed = true
						mu.Unlock()
						w.WriteHeader(204)
						close(newFinished)
						return
					}
				case r.URL.Path == "/await-first-pin":
					barrier := oldFinished
					if mode == "late-old-pin" {
						barrier = oldEntered
					}
					select {
					case <-barrier:
						w.Write([]byte(`{}`))
					case <-r.Context().Done():
						return
					}
					return
				case r.URL.Path == "/fresh-input":
					freshInputs.Add(1)
					if mode == "late-old-pin" {
						releaseOnce.Do(func() { close(releaseOld) })
						select {
						case <-oldFinished:
						case <-r.Context().Done():
							return
						}
					}
					mu.Lock()
					if generation == initial || confirmed {
						t.Error("fresh input reached provider while previous attempt remained confirmed")
					}
					mu.Unlock()
					w.Write([]byte(`{}`))
					return
				case r.URL.Path == "/await-new-pin":
					select {
					case <-newFinished:
						w.Write([]byte(`{}`))
					case <-r.Context().Done():
						return
					}
					return
				}
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			d.client = NewClient(srv.URL)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
			script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then echo '2.1.0'; exit 0; fi\nexec " + quoted + " -test.run='^TestSessionPinProviderProcess$' -- \"$@\"\n"
			writeTestExecutable(t, d.cfg.Agents["claude"].Path, []byte(script))
			task := source
			task.ID = "retry-task"
			task.PriorWorkDir = first.WorkDir
			task.PriorSessionID = first.SessionID
			task.SessionPinGeneration = initial
			task.Agent.CustomEnv = map[string]string{"RESULT_PIN_HELPER_URL": srv.URL}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := d.runTask(ctx, task, "claude", 0, d.logger)
			if mode == "unknown-invalidation" {
				if err == nil || freshInputs.Load() != 0 {
					t.Fatalf("unknown reset launched fresh provider: %+v %v fresh=%d", result, err, freshInputs.Load())
				}
				return
			}
			if err != nil || result.Status != "completed" || freshInputs.Load() != 1 {
				t.Fatalf("retry: %+v %v fresh=%d", result, err, freshInputs.Load())
			}
			if oldPins.Load() != 1 {
				t.Fatalf("old pin was retried: %d", oldPins.Load())
			}
			if mode == "late-old-pin" && rejectedOld.Load() != 1 {
				t.Fatal("late old pin did not retain its old generation")
			}
		})
	}
}
