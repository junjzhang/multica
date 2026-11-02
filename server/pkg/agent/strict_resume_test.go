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
		"dsh",
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

func TestStrictACPResumeIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	providers := []struct {
		name       string
		provenance bool
	}{
		{name: "hermes", provenance: true},
		{name: "kimi"},
		{name: "reasonix"},
		{name: "dim"},
		{name: "kiro"},
		{name: "qoder"},
		{name: "qoderclicn"},
		{name: "traecli"},
		{name: "grok"},
		{name: "qwenpaw"},
		{name: "zeroclaw"},
		{name: "mcode"},
	}
	for _, tc := range providers {
		t.Run(tc.name, func(t *testing.T) {
			exact := `{"sessionId":"ses_requested"}`
			changed := `{"sessionId":"ses_other"}`
			if tc.provenance {
				exact = `{"_meta":{"hermes":{"sessionProvenance":{"acpSessionId":"ses_requested"}}}}`
				changed = `{"_meta":{"hermes":{"sessionProvenance":{"acpSessionId":"ses_other"}}}}`
			}
			cases := []struct {
				name       string
				response   string
				wantStatus string
				wantPrompt bool
			}{
				{name: "exact", response: exact, wantStatus: "completed", wantPrompt: true},
				{name: "changed", response: changed, wantStatus: "failed", wantPrompt: false},
				{name: "missing confirmation", response: `{}`, wantStatus: "failed", wantPrompt: false},
			}
			for _, runCase := range cases {
				t.Run(runCase.name, func(t *testing.T) {
					record := filepath.Join(t.TempDir(), "requests.jsonl")
					exe := filepath.Join(t.TempDir(), tc.name)
					writeTestExecutable(t, exe, []byte(strictACPScript()))
					backend, err := New(tc.name, Config{
						ExecutablePath: exe,
						Logger:         slog.Default(),
						Env: map[string]string{
							"STRICT_ACP_RECORD":   record,
							"STRICT_ACP_INIT":     strictACPInitResult,
							"STRICT_ACP_RESPONSE": runCase.response,
						},
					})
					if err != nil {
						t.Fatalf("New(%s): %v", tc.name, err)
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
					if result.Status != runCase.wantStatus {
						t.Fatalf("status = %q, want %q (error %q)", result.Status, runCase.wantStatus, result.Error)
					}
					methods := readRecordedMethods(t, record)
					if recordHasMethod(methods, "session/new") {
						t.Fatalf("strict resume created a new session: %v", methods)
					}
					if gotPrompt := recordHasMethod(methods, "session/prompt"); gotPrompt != runCase.wantPrompt {
						t.Fatalf("session/prompt present = %v, want %v; methods: %v", gotPrompt, runCase.wantPrompt, methods)
					}
					if !recordHasMethod(methods, "session/resume") && !recordHasMethod(methods, "session/load") {
						t.Fatalf("no resume RPC recorded: %v", methods)
					}
					if runCase.wantPrompt {
						if result.ResumeRejected {
							t.Fatal("a successful exact resume must not be marked ResumeRejected")
						}
					} else if !result.ResumeRejected {
						t.Fatalf("strict failure = %+v, want ResumeRejected", result)
					}
				})
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
	if result.Status != "failed" || !result.ResumeRejected {
		t.Fatalf("result = %+v, want failed ResumeRejected", result)
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
				if result.SessionID != "thr_requested" {
					t.Fatalf("SessionID = %q, want thr_requested", result.SessionID)
				}
			} else if !result.ResumeRejected {
				t.Fatalf("strict failure = %+v, want ResumeRejected", result)
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
