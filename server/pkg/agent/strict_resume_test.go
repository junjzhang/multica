package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func runStrictBackend(t *testing.T, backend Backend, opts ExecOptions) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "strict resume test prompt", opts)
	if err != nil {
		return Result{}, err
	}
	go func() {
		for range session.Messages {
		}
	}()
	select {
	case result, ok := <-session.Result:
		if !ok {
			t.Fatal("result channel closed without a value")
		}
		return result, nil
	case <-time.After(20 * time.Second):
		t.Fatal("timeout waiting for result")
		return Result{}, nil
	}
}

func writeMarkerExecutable(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent")
	writeTestExecutable(t, path, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"))
	return path
}

func readRecordedMethods(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded requests: %v", err)
	}
	var methods []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var frame struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("recorded request is not JSON: %v", err)
		}
		if frame.Method != "" {
			methods = append(methods, frame.Method)
		}
	}
	return methods
}

func recordHasMethod(methods []string, want string) bool {
	for _, method := range methods {
		if method == want {
			return true
		}
	}
	return false
}

// TestRequireResumeMissingSessionIDFailsBeforeLaunch covers every current
// backend, not just the providers that implement strict resume. A missing id
// is a caller error and must never cause a provider process to start.
func TestRequireResumeMissingSessionIDFailsBeforeLaunch(t *testing.T) {
	for _, provider := range SupportedTypes {
		t.Run(provider, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "launched")
			backend, err := New(provider, Config{
				ExecutablePath: writeMarkerExecutable(t, marker),
				Logger:         slog.Default(),
			})
			if err != nil {
				t.Fatalf("New(%s): %v", provider, err)
			}
			_, err = backend.Execute(context.Background(), "prompt", ExecOptions{RequireResume: true})
			if !errors.Is(err, ErrResumeRequired) {
				t.Fatalf("Execute error = %v, want ErrResumeRequired", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("strict run launched the backend without a session id (stat error: %v)", statErr)
			}
		})
	}
}

func TestStrictResumeUnsupportedBackendsFailBeforeLaunch(t *testing.T) {
	unsupported := []string{
		"antigravity",
		"codearts",
		"codebuddy",
		"copilot",
		"cursor",
		"deveco",
		"openclaw",
		"qwen",
	}
	for _, provider := range unsupported {
		t.Run(provider, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "launched")
			backend, err := New(provider, Config{
				ExecutablePath: writeMarkerExecutable(t, marker),
				Logger:         slog.Default(),
			})
			if err != nil {
				t.Fatalf("New(%s): %v", provider, err)
			}
			_, err = backend.Execute(context.Background(), "prompt", ExecOptions{
				RequireResume:   true,
				ResumeSessionID: "ses_requested",
			})
			if !errors.Is(err, ErrResumeRequired) {
				t.Fatalf("Execute error = %v, want ErrResumeRequired", err)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("strict unsupported run launched the backend (stat error: %v)", statErr)
			}
		})
	}
}

const strictACPInitResult = `{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":true,"sse":true},"sessionCapabilities":{"resume":{}}},"sessionCapabilities":{"resume":true},"authMethods":[{"id":"cached_token"}],"agentInfo":{"version":"0.3.10"}}`

func strictACPScript() string {
	return `#!/bin/sh
record="$STRICT_ACP_RECORD"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$record"
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"method":"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" "$STRICT_ACP_INIT"
      ;;
    *'"method":"session/resume"'*|*'"method":"session/load"'*)
      if [ "$STRICT_ACP_ERROR" = "1" ]; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"session not found"}}\n' "$id"
      else
        printf '{"jsonrpc":"2.0","id":%s,"result":%s}\n' "$id" "$STRICT_ACP_RESPONSE"
      fi
      ;;
    *'"method":"session/new"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"sessionId":"ses_new"}}\n' "$id"
      ;;
    *'"method":"session/prompt"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"stopReason":"end_turn"}}\n' "$id"
      exit 0
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
  esac
done
`
}

// TestStrictACPResumeAcceptsRealEmptyResult runs strict resume against each
// ACP fake whose session/load or session/resume success shape was captured
// from a real runtime. ACP's load/resume result is allowed to be empty: the
// client supplies the id, a missing one is an RPC error, and no response field
// names the served session. Empty must therefore confirm the requested id, not
// fail it. The fakes here are the same ones the ordinary resume tests use.
func TestStrictACPResumeAcceptsRealEmptyResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	cases := []struct {
		name     string
		resumeID string
		build    func(t *testing.T) (Backend, string)
	}{
		{
			name:     "dim",
			resumeID: "ses_prior",
			build: func(t *testing.T) (Backend, string) {
				record := filepath.Join(t.TempDir(), "requests.jsonl")
				return newDimTestBackend(t, record), record
			},
		},
		{
			name:     "kiro",
			resumeID: "ses_existing",
			build: func(t *testing.T) (Backend, string) {
				record := filepath.Join(t.TempDir(), "requests.jsonl")
				exe := filepath.Join(t.TempDir(), "kiro-cli")
				writeTestExecutable(t, exe, []byte(fakeKiroACPScript()))
				backend, err := New("kiro", Config{
					ExecutablePath: exe,
					Logger:         slog.Default(),
					Env:            map[string]string{"KIRO_REQUESTS_FILE": record},
				})
				if err != nil {
					t.Fatalf("New(kiro): %v", err)
				}
				return backend, record
			},
		},
		{
			name:     "grok",
			resumeID: "ses_existing",
			build: func(t *testing.T) (Backend, string) {
				record := filepath.Join(t.TempDir(), "requests.jsonl")
				exe := filepath.Join(t.TempDir(), "grok")
				writeTestExecutable(t, exe, []byte(fakeGrokACPScript()))
				backend, err := New("grok", Config{
					ExecutablePath: exe,
					Logger:         slog.Default(),
					Env:            map[string]string{"GROK_REQUESTS_FILE": record},
				})
				if err != nil {
					t.Fatalf("New(grok): %v", err)
				}
				return backend, record
			},
		},
		{
			name:     "qwenpaw",
			resumeID: "ses_existing",
			build: func(t *testing.T) (Backend, string) {
				record := filepath.Join(t.TempDir(), "requests.jsonl")
				exe := writeFakeQwenpawScript(t, fakeQwenpawACPScript())
				backend, err := New("qwenpaw", Config{
					ExecutablePath: exe,
					Logger:         slog.Default(),
					Env:            map[string]string{"QWENPAW_REQUESTS_FILE": record},
				})
				if err != nil {
					t.Fatalf("New(qwenpaw): %v", err)
				}
				return backend, record
			},
		},
		{
			name:     "traecli",
			resumeID: "ses_existing",
			build: func(t *testing.T) (Backend, string) {
				record := filepath.Join(t.TempDir(), "requests.jsonl")
				exe := filepath.Join(t.TempDir(), "traecli")
				writeTestExecutable(t, exe, []byte(fakeTraecliACPScript()))
				backend, err := New("traecli", Config{
					ExecutablePath: exe,
					Logger:         slog.Default(),
					Env:            map[string]string{"TRAECLI_REQUESTS_FILE": record},
				})
				if err != nil {
					t.Fatalf("New(traecli): %v", err)
				}
				return backend, record
			},
		},
		{
			name:     "mcode",
			resumeID: "ses_existing",
			build: func(t *testing.T) (Backend, string) {
				exe, record, _ := writeFakeMcodeACP(t, true)
				backend, err := New("mcode", Config{ExecutablePath: exe, Logger: slog.Default()})
				if err != nil {
					t.Fatalf("New(mcode): %v", err)
				}
				return backend, record
			},
		},
		{
			name:     "zeroclaw",
			resumeID: "ses_existing",
			build: func(t *testing.T) (Backend, string) {
				record := filepath.Join(t.TempDir(), "requests.jsonl")
				exe := writeFakeZeroclawScript(t, fakeZeroclawACPScript())
				backend, err := New("zeroclaw", Config{
					ExecutablePath: exe,
					Logger:         slog.Default(),
					Env:            map[string]string{"ZEROCLAW_REQUESTS_FILE": record},
				})
				if err != nil {
					t.Fatalf("New(zeroclaw): %v", err)
				}
				return backend, record
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend, record := tc.build(t)
			result, err := runStrictBackend(t, backend, ExecOptions{
				RequireResume:   true,
				ResumeSessionID: tc.resumeID,
				Timeout:         10 * time.Second,
				Cwd:             t.TempDir(),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Status != "completed" {
				t.Fatalf("a genuine resume of the real empty-result shape was rejected: %+v", result)
			}
			if result.SessionID != tc.resumeID {
				t.Fatalf("SessionID = %q, want %q", result.SessionID, tc.resumeID)
			}
			if result.StrictResumeUnmet {
				t.Fatalf("confirmed strict resume marked unmet: %+v", result)
			}
			methods := readRecordedMethods(t, record)
			if recordHasMethod(methods, "session/new") {
				t.Fatalf("strict resume created a new session: %v", methods)
			}
			if !recordHasMethod(methods, "session/load") && !recordHasMethod(methods, "session/resume") {
				t.Fatalf("no resume RPC recorded: %v", methods)
			}
			if !recordHasMethod(methods, "session/prompt") {
				t.Fatalf("resumed session never received the task prompt: %v", methods)
			}
		})
	}
}

// TestStrictACPResumeEchoContract keeps both non-empty response shapes honest:
// an exact echo confirms, and a different id is a silent rebind that strict
// mode must refuse before the prompt. The load and resume RPCs share this
// check, so one provider of each kind is enough here; the empty-result matrix
// above covers the real per-provider shapes.
func TestStrictACPResumeEchoContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	cases := []struct {
		name       string
		provider   string
		response   string
		wantStatus string
		wantPrompt bool
	}{
		{name: "load exact echo", provider: "dim", response: `{"sessionId":"ses_requested"}`, wantStatus: "completed", wantPrompt: true},
		{name: "load different id", provider: "dim", response: `{"sessionId":"ses_other"}`, wantStatus: "failed"},
		{name: "resume exact echo kimi", provider: "kimi", response: `{"sessionId":"ses_requested"}`, wantStatus: "completed", wantPrompt: true},
		{name: "resume different id kimi", provider: "kimi", response: `{"sessionId":"ses_other"}`, wantStatus: "failed"},
		{name: "resume exact echo qoder", provider: "qoder", response: `{"sessionId":"ses_requested"}`, wantStatus: "completed", wantPrompt: true},
		{name: "resume different id qoder", provider: "qoder", response: `{"sessionId":"ses_other"}`, wantStatus: "failed"},
		{name: "resume exact echo reasonix", provider: "reasonix", response: `{"sessionId":"ses_requested"}`, wantStatus: "completed", wantPrompt: true},
		{name: "resume different id reasonix", provider: "reasonix", response: `{"sessionId":"ses_other"}`, wantStatus: "failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "requests.jsonl")
			exe := filepath.Join(t.TempDir(), tc.provider)
			writeTestExecutable(t, exe, []byte(strictACPScript()))
			backend, err := New(tc.provider, Config{
				ExecutablePath: exe,
				Logger:         slog.Default(),
				Env: map[string]string{
					"STRICT_ACP_RECORD":   record,
					"STRICT_ACP_INIT":     strictACPInitResult,
					"STRICT_ACP_RESPONSE": tc.response,
				},
			})
			if err != nil {
				t.Fatalf("New(%s): %v", tc.provider, err)
			}
			result, err := runStrictBackend(t, backend, ExecOptions{
				RequireResume:   true,
				ResumeSessionID: "ses_requested",
				Timeout:         10 * time.Second,
				Cwd:             t.TempDir(),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (error %q)", result.Status, tc.wantStatus, result.Error)
			}
			methods := readRecordedMethods(t, record)
			if recordHasMethod(methods, "session/new") {
				t.Fatalf("strict resume created a new session: %v", methods)
			}
			if gotPrompt := recordHasMethod(methods, "session/prompt"); gotPrompt != tc.wantPrompt {
				t.Fatalf("session/prompt present = %v, want %v; methods: %v", gotPrompt, tc.wantPrompt, methods)
			}
			if tc.wantPrompt {
				if result.SessionID != "ses_requested" || result.StrictResumeUnmet {
					t.Fatalf("exact resume result = %+v", result)
				}
			} else if !result.ResumeRejected || !result.StrictResumeUnmet || result.SessionID != "" {
				t.Fatalf("strict failure = %+v, want ResumeRejected, StrictResumeUnmet, no session", result)
			}
		})
	}
}

// TestStrictHermesResumeRequiresProvenance pins the one ACP backend whose
// successful resume response is untrustworthy on its own: Hermes silently
// creates a new session when the requested one is missing. The served ACP id
// is only visible on _meta.hermes.sessionProvenance.acpSessionId, so strict
// mode requires it to match exactly; no provenance stays fail-closed even if a
// top-level sessionId happens to echo the request.
func TestStrictHermesResumeRequiresProvenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	cases := []struct {
		name       string
		response   string
		wantStatus string
		wantPrompt bool
	}{
		{
			name:       "provenance exact",
			response:   `{"_meta":{"hermes":{"sessionProvenance":{"acpSessionId":"ses_requested"}}}}`,
			wantStatus: "completed",
			wantPrompt: true,
		},
		{
			name:       "provenance different",
			response:   `{"_meta":{"hermes":{"sessionProvenance":{"acpSessionId":"ses_other"}}}}`,
			wantStatus: "failed",
		},
		{
			name:       "no provenance",
			response:   `{}`,
			wantStatus: "failed",
		},
		{
			name:       "top-level echo without provenance",
			response:   `{"sessionId":"ses_requested"}`,
			wantStatus: "failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := filepath.Join(t.TempDir(), "requests.jsonl")
			exe := filepath.Join(t.TempDir(), "hermes")
			writeTestExecutable(t, exe, []byte(strictACPScript()))
			backend, err := New("hermes", Config{
				ExecutablePath: exe,
				Logger:         slog.Default(),
				Env: map[string]string{
					"STRICT_ACP_RECORD":   record,
					"STRICT_ACP_INIT":     strictACPInitResult,
					"STRICT_ACP_RESPONSE": tc.response,
				},
			})
			if err != nil {
				t.Fatalf("New(hermes): %v", err)
			}
			result, err := runStrictBackend(t, backend, ExecOptions{
				RequireResume:   true,
				ResumeSessionID: "ses_requested",
				Timeout:         10 * time.Second,
				Cwd:             t.TempDir(),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (error %q)", result.Status, tc.wantStatus, result.Error)
			}
			methods := readRecordedMethods(t, record)
			if recordHasMethod(methods, "session/new") {
				t.Fatalf("strict resume created a new session: %v", methods)
			}
			if gotPrompt := recordHasMethod(methods, "session/prompt"); gotPrompt != tc.wantPrompt {
				t.Fatalf("session/prompt present = %v, want %v; methods: %v", gotPrompt, tc.wantPrompt, methods)
			}
			if tc.wantPrompt {
				if result.SessionID != "ses_requested" || result.StrictResumeUnmet {
					t.Fatalf("confirmed hermes resume result = %+v", result)
				}
			} else if !result.ResumeRejected || !result.StrictResumeUnmet || result.SessionID != "" {
				t.Fatalf("strict hermes failure = %+v, want ResumeRejected, StrictResumeUnmet, no session", result)
			}
		})
	}
}

func TestStrictMcodeWithoutLoadSessionFailsBeforePrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	record := filepath.Join(t.TempDir(), "requests.jsonl")
	exe := filepath.Join(t.TempDir(), "mcode")
	writeTestExecutable(t, exe, []byte(strictACPScript()))
	backend, err := New("mcode", Config{
		ExecutablePath: exe,
		Logger:         slog.Default(),
		Env: map[string]string{
			"STRICT_ACP_RECORD":   record,
			"STRICT_ACP_INIT":     `{"protocolVersion":1,"agentCapabilities":{"loadSession":false}}`,
			"STRICT_ACP_RESPONSE": `{"sessionId":"ses_requested"}`,
		},
	})
	if err != nil {
		t.Fatalf("New(mcode): %v", err)
	}
	result, err := runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: "ses_requested",
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != "failed" || !result.ResumeRejected || !result.StrictResumeUnmet {
		t.Fatalf("result = %+v, want failed ResumeRejected StrictResumeUnmet", result)
	}
	if !strings.Contains(result.Error, ErrResumeRequired.Error()) {
		t.Fatalf("error = %q, want strict-resume diagnostic", result.Error)
	}
	methods := readRecordedMethods(t, record)
	for _, forbidden := range []string{"session/load", "session/new", "session/prompt"} {
		if recordHasMethod(methods, forbidden) {
			t.Fatalf("mcode without loadSession still called %s: %v", forbidden, methods)
		}
	}
}

func TestStrictACPResumeRPCErrorDoesNotPrompt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	record := filepath.Join(t.TempDir(), "requests.jsonl")
	exe := filepath.Join(t.TempDir(), "kimi")
	writeTestExecutable(t, exe, []byte(strictACPScript()))
	backend, err := New("kimi", Config{
		ExecutablePath: exe,
		Logger:         slog.Default(),
		Env: map[string]string{
			"STRICT_ACP_RECORD":   record,
			"STRICT_ACP_INIT":     strictACPInitResult,
			"STRICT_ACP_RESPONSE": `{}`,
			"STRICT_ACP_ERROR":    "1",
		},
	})
	if err != nil {
		t.Fatalf("New(kimi): %v", err)
	}
	result, err := runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: "ses_requested",
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !result.StrictResumeUnmet {
		t.Fatalf("strict resume RPC error is not marked StrictResumeUnmet: %+v", result)
	}
	methods := readRecordedMethods(t, record)
	if recordHasMethod(methods, "session/new") || recordHasMethod(methods, "session/prompt") {
		t.Fatalf("strict resume RPC error still created/prompted a session: %v", methods)
	}
}

func writeFakeCodexStrictServer(t *testing.T) string {
	t.Helper()
	body := `DIR="$(dirname "$0")"
LOG="$DIR/rpc.log"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$LOG"
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  case "$method" in
    initialize)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    thread/resume)
      if [ "$CODEX_STRICT_RESUME_ERROR" = "1" ]; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"unknown thread"}}\n' "$id"
      else
        printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"%s"}}}\n' "$id" "$CODEX_STRICT_THREAD"
      fi
      ;;
    thread/start)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"thr_new"}}}\n' "$id"
      ;;
    turn/start)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"%s","turn":{"id":"turn-1"}}}\n' "$CODEX_STRICT_THREAD"
      printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"%s","item":{"type":"agentMessage","id":"msg-1","text":"OK"}}}\n' "$CODEX_STRICT_THREAD"
      printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"%s","turn":{"id":"turn-1","status":"completed"}}}\n' "$CODEX_STRICT_THREAD"
      exit 0
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
  esac
done
`
	return writeFakeCodexAppServer(t, body)
}

func newStrictCodexBackend(t *testing.T, exe, threadID string, resumeError bool) Backend {
	t.Helper()
	mode := ""
	if resumeError {
		mode = "1"
	}
	backend, err := New("codex", Config{
		ExecutablePath: exe,
		Logger:         slog.Default(),
		Env: map[string]string{
			"CODEX_STRICT_THREAD":       threadID,
			"CODEX_STRICT_RESUME_ERROR": mode,
		},
	})
	if err != nil {
		t.Fatalf("New(codex): %v", err)
	}
	return backend
}

func TestStrictCodexResumeIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	cases := []struct {
		name        string
		threadID    string
		resumeError bool
		wantStatus  string
		wantPrompt  bool
	}{
		{name: "exact", threadID: "thr_requested", wantStatus: "completed", wantPrompt: true},
		{name: "changed", threadID: "thr_other", wantStatus: "failed", wantPrompt: false},
		{name: "missing confirmation", threadID: "", wantStatus: "failed", wantPrompt: false},
		{name: "resume rpc error", threadID: "thr_requested", resumeError: true, wantStatus: "failed", wantPrompt: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exe := writeFakeCodexStrictServer(t)
			backend := newStrictCodexBackend(t, exe, tc.threadID, tc.resumeError)
			result, err := runStrictBackend(t, backend, ExecOptions{
				RequireResume:   true,
				ResumeSessionID: "thr_requested",
				Timeout:         10 * time.Second,
				Cwd:             t.TempDir(),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (error %q)", result.Status, tc.wantStatus, result.Error)
			}
			logPath := filepath.Join(filepath.Dir(exe), "rpc.log")
			methods := readRecordedMethods(t, logPath)
			if recordHasMethod(methods, "thread/start") {
				t.Fatalf("strict resume started a new thread: %v", methods)
			}
			if gotPrompt := recordHasMethod(methods, "turn/start"); gotPrompt != tc.wantPrompt {
				t.Fatalf("turn/start present = %v, want %v; methods: %v", gotPrompt, tc.wantPrompt, methods)
			}
			if tc.wantPrompt {
				if result.SessionID != "thr_requested" || result.StrictResumeUnmet {
					t.Fatalf("strict success result = %+v, want thr_requested and no unmet marker", result)
				}
			} else if !result.ResumeRejected || !result.StrictResumeUnmet {
				t.Fatalf("strict failure = %+v, want ResumeRejected and StrictResumeUnmet", result)
			}
		})
	}
}

// TestStrictCodexCatalogRetryKeepsResumeIdentity covers the one Codex-internal
// retry that ordinary runs turn into a fresh thread: a strict run must instead
// retry the same requested thread and never call thread/start.
func TestStrictCodexCatalogRetryKeepsResumeIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	codexGracefulShutdownTimeoutNanos.Store(int64(100 * time.Millisecond))
	t.Cleanup(func() { codexGracefulShutdownTimeoutNanos.Store(0) })

	body := `DIR="$(dirname "$0")"
ATTEMPT=$(cat "$DIR/attempts" 2>/dev/null || echo 0)
ATTEMPT=$((ATTEMPT+1))
echo "$ATTEMPT" > "$DIR/attempts"
LOG="$DIR/rpc-$ATTEMPT.log"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$LOG"
  id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  method=$(printf '%s' "$line" | sed -n 's/.*"method":"\([^"]*\)".*/\1/p')
  case "$method" in
    initialize)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
    thread/resume)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"thr_prior"}}}\n' "$id"
      ;;
    thread/start)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"thread":{"id":"thr_new"}}}\n' "$id"
      ;;
    turn/start)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      printf '{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"thr_prior","turn":{"id":"turn-1"}}}\n'
      if [ "$ATTEMPT" = "1" ]; then
        echo 'ERROR codex_models_manager::manager: failed to refresh available models: timeout waiting for child process to exit' >&2
        sleep 2
      else
        printf '{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thr_prior","item":{"type":"agentMessage","id":"msg-1","text":"Recovered"}}}\n'
        printf '{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thr_prior","turn":{"id":"turn-1","status":"completed"}}}\n'
        exit 0
      fi
      ;;
    *)
      printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id"
      ;;
  esac
done
`
	exe := writeFakeCodexAppServer(t, body)
	backend := newStrictCodexBackend(t, exe, "thr_prior", false)
	result, err := runStrictBackend(t, backend, ExecOptions{
		RequireResume:             true,
		ResumeSessionID:           "thr_prior",
		Timeout:                   20 * time.Second,
		SemanticInactivityTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != "completed" || result.SessionID != "thr_prior" {
		t.Fatalf("result = %+v, want completed on thr_prior", result)
	}
	second := filepath.Join(filepath.Dir(exe), "rpc-2.log")
	methods := readRecordedMethods(t, second)
	if !recordHasMethod(methods, "thread/resume") {
		t.Fatalf("strict catalog retry did not resume the original thread: %v", methods)
	}
	if recordHasMethod(methods, "thread/start") {
		t.Fatalf("strict catalog retry started a fresh thread: %v", methods)
	}
}

func TestStrictClaudeResumePreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	configDir := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	exe := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
printf '%s\n' "$*" > "$CLAUDE_STRICT_ARGS"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess_strict"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"sess_strict","result":"ok"}'
`
	writeTestExecutable(t, exe, []byte(script))

	backend, err := New("claude", Config{
		ExecutablePath: exe,
		Logger:         slog.Default(),
		Env: map[string]string{
			"CLAUDE_CONFIG_DIR":  configDir,
			"CLAUDE_STRICT_ARGS": argsFile,
		},
	})
	if err != nil {
		t.Fatalf("New(claude): %v", err)
	}
	_, err = runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: "sess_strict",
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if !errors.Is(err, ErrResumeRequired) {
		t.Fatalf("missing transcript error = %v, want ErrResumeRequired", err)
	}
	if _, statErr := os.Stat(argsFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("strict claude launched without a local transcript (stat error: %v)", statErr)
	}

	projectDir := filepath.Join(configDir, "projects", "strict-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create claude project dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "sess_strict.jsonl"), nil, 0o644); err != nil {
		t.Fatalf("create claude transcript: %v", err)
	}
	result, err := runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: "sess_strict",
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Execute with transcript: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed (error %q)", result.Status, result.Error)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read claude args: %v", err)
	}
	if !strings.Contains(string(args), "--resume sess_strict") {
		t.Fatalf("claude args did not carry the exact resume id: %s", args)
	}
}

func TestStrictPiResumePreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	marker := filepath.Join(t.TempDir(), "launched")
	exe := filepath.Join(t.TempDir(), "pi")
	script := `#!/bin/sh
touch "$PI_STRICT_MARKER"
cat >/dev/null
printf '%s\n' '{"type":"agent_start"}'
printf '%s\n' '{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":1,"output":1}}}'
`
	writeTestExecutable(t, exe, []byte(script))

	backend, err := New("pi", Config{
		ExecutablePath: exe,
		Logger:         slog.Default(),
		Env:            map[string]string{"PI_STRICT_MARKER": marker},
	})
	if err != nil {
		t.Fatalf("New(pi): %v", err)
	}
	missingPath := filepath.Join(t.TempDir(), "missing.jsonl")
	_, err = runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: missingPath,
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if !errors.Is(err, ErrResumeRequired) {
		t.Fatalf("missing transcript error = %v, want ErrResumeRequired", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("strict pi launched without a transcript (stat error: %v)", statErr)
	}

	sessionPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(sessionPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("create pi transcript: %v", err)
	}
	result, err := runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: sessionPath,
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Execute with transcript: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed (error %q)", result.Status, result.Error)
	}
}

func TestStrictOpenCodeResumePreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	runMarker := filepath.Join(t.TempDir(), "run")
	exe := filepath.Join(t.TempDir(), "opencode")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo "1.0.0"; exit 0; fi
if [ "$1" = "export" ]; then
  if [ "$OPENCODE_STRICT_EXPORT_FAIL" = "1" ]; then exit 1; fi
  printf '%s\n' '{"info":{"id":"ses_strict","time":{"created":1,"updated":1}},"messages":[]}'
  exit 0
fi
if [ "$1" = "run" ]; then
  touch "$OPENCODE_STRICT_RUN_MARKER"
  cat >/dev/null
  printf '%s\n' '{"type":"text","sessionID":"ses_strict","part":{"type":"text","text":"ok"}}'
  printf '%s\n' '{"type":"step_finish","sessionID":"ses_strict","part":{"type":"step-finish","reason":"stop","tokens":{"input":1,"output":1}}}'
  exit 0
fi
exit 1
`
	writeTestExecutable(t, exe, []byte(script))

	backend, err := New("opencode", Config{
		ExecutablePath: exe,
		CLIVersion:     "1.0.0",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
		Env: map[string]string{
			"OPENCODE_STRICT_EXPORT_FAIL": "1",
			"OPENCODE_STRICT_RUN_MARKER":  runMarker,
		},
	})
	if err != nil {
		t.Fatalf("New(opencode): %v", err)
	}
	_, err = runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: "ses_strict",
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if !errors.Is(err, ErrResumeRequired) {
		t.Fatalf("failed export error = %v, want ErrResumeRequired", err)
	}
	if _, statErr := os.Stat(runMarker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("strict opencode ran without a verified export (stat error: %v)", statErr)
	}

	backend, err = New("opencode", Config{
		ExecutablePath: exe,
		CLIVersion:     "1.0.0",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
		Env:            map[string]string{"OPENCODE_STRICT_RUN_MARKER": runMarker},
	})
	if err != nil {
		t.Fatalf("New(opencode): %v", err)
	}
	result, err := runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: "ses_strict",
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Execute with verified export: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed (error %q)", result.Status, result.Error)
	}
}

func TestStrictMuseResumeIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	bin, requestsPath, _ := writeFakeMuseServe(t, false)
	backend := newFakeMuseBackend(t, bin)

	result, err := runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: fakeMuseSessionID,
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Exact Execute: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("exact status = %q, want completed (error %q)", result.Status, result.Error)
	}

	changedID := "01a14870-5a2f-7791-b847-2385a5d47a00"
	bin, requestsPath, _ = writeFakeMuseServe(t, false)
	backend = newFakeMuseBackend(t, bin)
	result, err = runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: changedID,
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Changed Execute: %v", err)
	}
	if result.Status != "failed" || !result.ResumeRejected {
		t.Fatalf("changed result = %+v, want failed ResumeRejected", result)
	}
	requests := readMuseRequests(t, requestsPath)
	if museRequestByMethod(requests, "turn/start") != nil {
		t.Fatalf("changed muse resume still started a turn: %v", requests)
	}
	if museRequestByMethod(requests, "session/start") != nil {
		t.Fatalf("changed muse resume started a new session: %v", requests)
	}
}

// TestStrictMuseResumeErrorVetoesFreshRetry covers the MSP resume-error path:
// Muse reports a host-side session-not-found as ResumeRejected, which is the
// ordinary fresh-retry signal. A strict run must still mark the result so the
// daemon cannot turn that same signal into the fresh session the caller
// forbade.
func TestStrictMuseResumeErrorVetoesFreshRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	bin, requestsPath, _ := writeFakeMuseServe(t, true)
	backend := newFakeMuseBackend(t, bin)
	result, err := runStrictBackend(t, backend, ExecOptions{
		RequireResume:   true,
		ResumeSessionID: fakeMuseSessionID,
		Timeout:         5 * time.Second,
		Cwd:             t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != "failed" || !result.ResumeRejected || !result.StrictResumeUnmet {
		t.Fatalf("strict muse resume error result = %+v, want failed ResumeRejected StrictResumeUnmet", result)
	}
	requests := readMuseRequests(t, requestsPath)
	if museRequestByMethod(requests, "turn/start") != nil {
		t.Fatalf("strict muse resume error still started a turn: %v", requests)
	}
}

// TestStrictDshResumeIdentity pins the DSH protocol contract: the execute
// frame carries the prompt and resume_session_id together, and strict mode
// accepts the run only after the runtime reports session{resumed:true} for the
// exact requested id before the prompt. A terminal result's session id is not
// evidence on its own, and no failure path may adopt a fresh session.
func TestStrictDshResumeIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	cases := []struct {
		name         string
		mode         string
		wantStatus   string
		wantRejected bool
	}{
		{name: "exact", mode: "exact", wantStatus: "completed"},
		{name: "changed id", mode: "changed", wantStatus: "failed", wantRejected: true},
		{name: "fresh session", mode: "fresh", wantStatus: "failed", wantRejected: true},
		{name: "missing session", mode: "missing", wantStatus: "failed", wantRejected: true},
		{name: "result id only", mode: "result-only", wantStatus: "failed", wantRejected: true},
		{name: "confirmed then different result id", mode: "confirmed-changed", wantStatus: "failed", wantRejected: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			record := filepath.Join(dir, "execute.log")
			bin := writeDshFixture(t, `
if [ "$1" != "--profile" ] || [ "$2" != "multica" ] || [ "$3" != "--stdio" ]; then exit 9; fi
printf '%s\n' '{"v":1,"type":"ready","runtime":"dsh","plugin_version":"test","capabilities":{}}'
IFS= read -r execute
printf '%s\n' "$execute" > "$DSH_STRICT_RECORD"
case "$DSH_STRICT_MODE" in
  exact)
    printf '%s\n' '{"v":1,"type":"session","request_id":"task-1","session_id":"ses_requested","resumed":true}'
    printf '%s\n' '{"v":1,"type":"text","request_id":"task-1","content":"OK"}'
    printf '%s\n' '{"v":1,"type":"result","request_id":"task-1","status":"completed","session_id":"ses_requested","output":"OK","resume_rejected":false}'
    ;;
  changed)
    printf '%s\n' '{"v":1,"type":"session","request_id":"task-1","session_id":"ses_other","resumed":true}'
    printf '%s\n' '{"v":1,"type":"result","request_id":"task-1","status":"completed","session_id":"ses_other","output":"wrong","resume_rejected":false}'
    ;;
  fresh)
    printf '%s\n' '{"v":1,"type":"session","request_id":"task-1","session_id":"ses_new","resumed":false}'
    printf '%s\n' '{"v":1,"type":"result","request_id":"task-1","status":"completed","session_id":"ses_new","output":"wrong","resume_rejected":false}'
    ;;
  missing)
    printf '%s\n' '{"v":1,"type":"result","request_id":"task-1","status":"failed","output":"","resume_rejected":true,"error":{"code":"DSH_RESUME_REJECTED","message":"could not resume DSH session ses_requested"}}'
    ;;
  result-only)
    printf '%s\n' '{"v":1,"type":"result","request_id":"task-1","status":"completed","session_id":"ses_requested","output":"no session frame","resume_rejected":false}'
    ;;
  confirmed-changed)
    printf '%s\n' '{"v":1,"type":"session","request_id":"task-1","session_id":"ses_requested","resumed":true}'
    printf '%s\n' '{"v":1,"type":"result","request_id":"task-1","status":"completed","session_id":"ses_other","output":"wrong","resume_rejected":false}'
    ;;
esac
`)
			backend, err := New("dsh", Config{
				ExecutablePath: bin,
				TaskID:         "task-1",
				Logger:         slog.Default(),
				BuiltinRuntime: true,
				Env: map[string]string{
					"DSH_STRICT_MODE":   tc.mode,
					"DSH_STRICT_RECORD": record,
				},
			})
			if err != nil {
				t.Fatalf("New(dsh): %v", err)
			}
			result, err := runStrictBackend(t, backend, ExecOptions{
				RequireResume:   true,
				ResumeSessionID: "ses_requested",
				Timeout:         5 * time.Second,
				Cwd:             t.TempDir(),
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			raw, readErr := os.ReadFile(record)
			if readErr != nil {
				t.Fatalf("read execute frame: %v", readErr)
			}
			var command dshExecuteCommand
			if err := json.Unmarshal(raw, &command); err != nil {
				t.Fatalf("execute frame is not JSON: %v", err)
			}
			if command.ResumeSessionID != "ses_requested" {
				t.Fatalf("execute resume_session_id = %q, want ses_requested", command.ResumeSessionID)
			}
			if command.Prompt != "strict resume test prompt" {
				t.Fatalf("execute prompt = %q, want the task prompt", command.Prompt)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (error %q)", result.Status, tc.wantStatus, result.Error)
			}
			if tc.wantRejected {
				if !result.ResumeRejected || !result.StrictResumeUnmet || result.SessionID != "" {
					t.Fatalf("strict failure = %+v, want ResumeRejected, StrictResumeUnmet, and no adopted session", result)
				}
				if !strings.Contains(result.Error, "ses_requested") {
					t.Fatalf("strict failure error %q does not name the requested session", result.Error)
				}
				return
			}
			if result.SessionID != "ses_requested" || result.ResumeRejected {
				t.Fatalf("strict success result = %+v, want exact requested session", result)
			}
		})
	}
}

// TestStrictDshUnconfirmedTerminalStatesDoNotLeakSession pins the reviewer's
// cancellation probe and its timeout sibling. A strict run that never saw
// session{resumed:true, session_id==requested} must hand back no session
// pointer, even when the host reports a fresh id in its cancelled/timeout
// terminal frame. The real status is preserved: cancel and timeout are
// outcomes, not resume verdicts, so they do not become a definite rejection.
// No status message may carry the unconfirmed id either, because the daemon
// pins a resume pointer as soon as it sees one.
func TestStrictDshUnconfirmedTerminalStatesDoNotLeakSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	for _, tc := range []struct {
		name       string
		status     string
		wantStatus string
	}{
		{name: "cancelled", status: "cancelled", wantStatus: "cancelled"},
		{name: "timeout", status: "timeout", wantStatus: "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeDshFixture(t, `
if [ "$1" != "--profile" ] || [ "$2" != "multica" ] || [ "$3" != "--stdio" ]; then exit 9; fi
printf '%s\n' '{"v":1,"type":"ready","runtime":"dsh","plugin_version":"test","capabilities":{}}'
IFS= read -r execute
printf '%s\n' '{"v":1,"type":"session","request_id":"task-cancel","session_id":"session-brand-new","resumed":false}'
IFS= read -r cancel
printf '%s\n' '{"v":1,"type":"result","request_id":"task-cancel","status":"'"$DSH_STRICT_TERMINAL"'","session_id":"session-brand-new","output":"","resume_rejected":false}'
`)
			backend, err := New("dsh", Config{
				ExecutablePath: bin,
				TaskID:         "task-cancel",
				Logger:         slog.Default(),
				BuiltinRuntime: true,
				Env:            map[string]string{"DSH_STRICT_TERMINAL": tc.status},
			})
			if err != nil {
				t.Fatalf("New(dsh): %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			session, err := backend.Execute(ctx, "wait", ExecOptions{
				Cwd:             t.TempDir(),
				Timeout:         5 * time.Second,
				RequireResume:   true,
				ResumeSessionID: "session-requested-original",
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			var messages []Message
			done := make(chan struct{})
			go func() {
				defer close(done)
				for msg := range session.Messages {
					messages = append(messages, msg)
				}
			}()
			// The fixture answers the protocol cancel frame, so any cancel is
			// enough; the terminal frame itself decides the status under test.
			time.Sleep(200 * time.Millisecond)
			cancel()
			result := <-session.Result
			<-done
			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (error %q)", result.Status, tc.wantStatus, result.Error)
			}
			if result.SessionID != "" {
				t.Fatalf("strict run never confirmed the requested session yet Result.SessionID=%q", result.SessionID)
			}
			if !result.StrictResumeUnmet {
				t.Fatalf("strict run without confirmation is not marked StrictResumeUnmet: %+v", result)
			}
			if result.ResumeRejected {
				t.Fatalf("cancel/timeout must not be reported as a definite resume rejection: %+v", result)
			}
			for _, msg := range messages {
				if msg.SessionID == "session-brand-new" {
					t.Fatalf("unconfirmed session id was broadcast to the daemon: %+v", msg)
				}
			}
		})
	}
}

// TestStrictDshCustomRuntimeProfileFailsBeforeLaunch keeps the strict
// guarantee scoped to the built-in Multica DSH runtime whose protocol-v1
// execute path was verified. An arbitrary custom runtime profile that merely
// declares the dsh protocol family has no such evidence, so strict mode must
// fail closed before starting it.
func TestStrictDshCustomRuntimeProfileFailsBeforeLaunch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	backend, err := New("dsh", Config{
		ExecutablePath: writeMarkerExecutable(t, marker),
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("New(dsh): %v", err)
	}
	_, err = backend.Execute(context.Background(), "prompt", ExecOptions{
		RequireResume:   true,
		ResumeSessionID: "ses_requested",
	})
	if !errors.Is(err, ErrResumeRequired) {
		t.Fatalf("Execute error = %v, want ErrResumeRequired", err)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("strict custom-profile dsh was launched (stat error: %v)", statErr)
	}
}

func TestRequireExactResumedSessionID(t *testing.T) {
	t.Parallel()
	strict := ExecOptions{RequireResume: true, ResumeSessionID: "ses_requested"}
	if err := requireExactResumedSessionID(strict, "", "test"); !errors.Is(err, ErrResumeRequired) {
		t.Fatalf("empty confirmation error = %v, want ErrResumeRequired", err)
	}
	if err := requireExactResumedSessionID(strict, "ses_other", "test"); !errors.Is(err, ErrResumeRequired) {
		t.Fatalf("changed confirmation error = %v, want ErrResumeRequired", err)
	}
	if err := requireExactResumedSessionID(strict, "ses_requested", "test"); err != nil {
		t.Fatalf("exact confirmation error = %v", err)
	}
	if err := requireExactResumedSessionID(ExecOptions{ResumeSessionID: "ses_requested"}, "", "test"); err != nil {
		t.Fatalf("non-strict empty confirmation error = %v, want nil", err)
	}
}
