// Package backgroundwait owns local command execution and a replayable result
// outbox. Mailbox leases, retries and host receipts remain in delivery.
package backgroundwait

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const MaxDuration = 7 * 24 * time.Hour
const MaxLogBytes = 1 << 20

type Authorize func(context.Context, string, protocol.WaitStart) (protocol.WaitAddress, error)
type Notify func(context.Context, protocol.WaitResult) (string, error)

// Admission reserves execution against a daemon restart barrier. Release must
// remain held until the running entry is published under the journal mutex.
type Admission func() (release func(), allowed bool)

type record struct {
	protocol.WaitStatus
	Request         protocol.WaitStart    `json:"request"`
	CancelRequested bool                  `json:"cancel_requested,omitempty"`
	Handled         bool                  `json:"handled,omitempty"`
	Suspension      *protocol.WaitSuspend `json:"suspension,omitempty"`
	Suspended       bool                  `json:"suspended,omitempty"`
}

type Manager struct {
	mu                    sync.Mutex
	dir, secret, daemonID string
	helper                []string
	authorize             Authorize
	notify                Notify
	admit                 Admission
	ctx                   context.Context
	cancel                context.CancelFunc
	wg                    sync.WaitGroup
	rows                  map[string]*record
	running               map[string]context.CancelFunc
	wake                  chan struct{}
	consume               func(context.Context, string, protocol.WaitResult) error
	checkTask             func(context.Context, protocol.WaitAddress) (bool, error)
	suspend               func(context.Context, string, protocol.WaitSuspend) error
	finished              map[string]chan struct{}
}

// Open must be called once under the daemon profile's existing process lock.
// No command is ever replayed: an unfinished journal becomes lost on startup.
func Open(parent context.Context, dir, secret, daemonID string, helper []string, authorize Authorize, notify Notify, admit Admission) (*Manager, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	m := &Manager{dir: dir, secret: secret, daemonID: daemonID, helper: helper, authorize: authorize, notify: notify, admit: admit, ctx: ctx, cancel: cancel, rows: map[string]*record{}, running: map[string]context.CancelFunc{}, finished: map[string]chan struct{}{}, wake: make(chan struct{}, 1)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		cancel()
		return nil, err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		var row record
		if readErr != nil || json.Unmarshal(data, &row) != nil || entry.Name() != row.ID+".json" || row.Address.DaemonID != daemonID {
			cancel()
			return nil, fmt.Errorf("invalid wait journal %s", entry.Name())
		}
		if !protocol.WaitTerminal(row.State) {
			row.State, row.FinishedAt = "lost", time.Now().UTC()
			if err := m.save(&row); err != nil {
				cancel()
				return nil, err
			}
		}
		m.rows[row.ID] = &row
	}
	m.wg.Add(1)
	go m.deliver()
	m.signal()
	return m, nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) Active() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.running)
}

func (m *Manager) ConfigureManaged(consume func(context.Context, string, protocol.WaitResult) error, check func(context.Context, protocol.WaitAddress) (bool, error), suspend func(context.Context, string, protocol.WaitSuspend) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consume, m.checkTask, m.suspend = consume, check, suspend
}

func (m *Manager) SuspendTask(taskID string, req protocol.WaitSuspend) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.Address.TaskID == taskID && !row.Handled {
			row.Suspension = &req
			err := m.save(row)
			m.signal()
			return true, err
		}
	}
	return false, nil
}

func (m *Manager) CancelTask(taskID string) {
	m.mu.Lock()
	var finished []chan struct{}
	for id, cancel := range m.running {
		if m.rows[id].Address.TaskID == taskID {
			m.rows[id].CancelRequested = true
			cancel()
			finished = append(finished, m.finished[id])
		}
	}
	m.mu.Unlock()
	for _, done := range finished {
		<-done
	}
}

func (m *Manager) HasPendingTask(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range m.rows {
		if row.Address.TaskID == taskID && !row.Handled {
			return true
		}
	}
	return false
}

func (m *Manager) MarkHandled(taskID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[id]
	if row == nil {
		return nil
	}
	if row.Address.TaskID != taskID {
		return fmt.Errorf("wait belongs to another task")
	}
	row.Handled = true
	return m.save(row)
}

func (m *Manager) KnownIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.rows))
	for id := range m.rows {
		ids = append(ids, id)
	}
	return ids
}

func (m *Manager) HasID(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[id] != nil
}

// WritePrivate atomically publishes durable owner-only state.
func WritePrivate(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".wait-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func (m *Manager) save(row *record) error {
	return WritePrivate(filepath.Join(m.dir, row.ID+".json"), row)
}
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// A private on-disk capability grants the same OS user's local execution
	// authority. Remote session credentials do not grant this authority.
	if !validSecret(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), m.secret) {
		http.Error(w, "local daemon capability required", http.StatusUnauthorized)
		return
	}
	m.serve(w, r, "")
}

// ServeManagedHTTP is called only after the daemon has resolved its active
// task-token registry. A caller-supplied header never selects this scope.
func (m *Manager) ServeManagedHTTP(w http.ResponseWriter, r *http.Request, taskID string) {
	if taskID == "" {
		http.Error(w, "task scope required", 403)
		return
	}
	m.serve(w, r, taskID)
}

func (m *Manager) serve(w http.ResponseWriter, r *http.Request, scope string) {
	if r.Method == http.MethodPost && r.URL.Path == "/waits" {
		m.start(w, r, scope)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/waits/")
	if _, err := util.ParseUUID(id); err != nil {
		http.Error(w, "invalid wait id", 400)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	m.mu.Lock()
	row := m.rows[id]
	if row == nil {
		m.mu.Unlock()
		http.Error(w, "wait not found", 404)
		return
	}
	if scope != "" && row.Address.TaskID != scope {
		m.mu.Unlock()
		http.Error(w, "wait not found", 404)
		return
	}
	if r.Method == http.MethodDelete && !protocol.WaitTerminal(row.State) {
		// Persist cancellation before touching the process. Completion takes
		// the same mutex; the first durable terminal decision wins.
		next := *row
		next.State, next.CancelRequested = "cancelling", true
		if err := m.save(&next); err != nil {
			m.mu.Unlock()
			http.Error(w, "cannot persist cancellation", 500)
			return
		}
		*row = next
		if cancel := m.running[id]; cancel != nil {
			cancel()
		}
		m.signal()
	}
	status, consume, handled := row.WaitStatus, m.consume, row.Handled
	m.mu.Unlock()
	if scope != "" && r.Method == http.MethodGet && protocol.WaitTerminal(status.State) && !handled {
		if consume == nil || consume(r.Context(), r.Header.Get("X-Wait-Actor-Token"), status.WaitResult) != nil {
			http.Error(w, "cannot durably consume wait result; retry", 503)
			return
		}
		if err := m.MarkHandled(scope, id); err != nil {
			http.Error(w, "cannot persist result receipt; retry", 503)
			return
		}
	}
	writeJSON(w, status)
}

func (m *Manager) start(w http.ResponseWriter, r *http.Request, scope string) {
	var req protocol.WaitStart
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if _, err := util.ParseUUID(req.ID); err != nil {
		http.Error(w, "request id must be a UUID", 400)
		return
	}
	req.Deadline = req.Deadline.UTC()
	if scope != "" {
		req.TaskID = scope
	}
	if len(req.Argv) == 0 || req.Argv[0] == "" || !filepath.IsAbs(req.Cwd) || (req.SessionRef == "" && scope == "") || req.WorkspaceID == "" {
		http.Error(w, "argv, absolute cwd, workspace and session required", 400)
		return
	}
	// Authorization is deliberately outside the journal mutex and repeated
	// on duplicate registrations; an expired or revoked token cannot reuse it.
	address, err := m.authorize(r.Context(), r.Header.Get("X-Wait-Actor-Token"), req)
	if err != nil || address.DaemonID != m.daemonID || address.WorkspaceID != req.WorkspaceID || address.TaskID != scope {
		http.Error(w, "session authorization failed", 403)
		return
	}
	if m.admit != nil {
		release, allowed := m.admit()
		if !allowed {
			http.Error(w, "daemon restart in progress; retry this request", http.StatusServiceUnavailable)
			return
		}
		defer release()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		http.Error(w, "daemon stopping", 503)
		return
	}
	if old := m.rows[req.ID]; old != nil {
		if !reflect.DeepEqual(old.Request, req) || old.Address != address {
			http.Error(w, "request id already used", 409)
			return
		}
		writeJSON(w, old.WaitStatus)
		return
	}
	if !req.Deadline.After(time.Now()) || req.Deadline.After(time.Now().Add(MaxDuration)) {
		http.Error(w, "deadline must be within seven days", 400)
		return
	}
	if len(m.running) >= 32 {
		http.Error(w, "too many active waits", 429)
		return
	}
	row := &record{Request: req, WaitStatus: protocol.WaitStatus{WaitResult: protocol.WaitResult{ID: req.ID, Address: address, State: "running", LogPath: filepath.Join(m.dir, req.ID+".log")}, Deadline: req.Deadline}}
	if err := m.save(row); err != nil {
		if scope != "" {
			row.State, row.FinishedAt = "lost", time.Now().UTC()
			m.rows[row.ID] = row
			m.signal()
		}
		http.Error(w, "cannot persist wait", 500)
		return
	}
	ctx, cancel := context.WithDeadline(m.ctx, req.Deadline)
	m.running[row.ID], m.rows[row.ID] = cancel, row
	m.finished[row.ID] = make(chan struct{})
	m.wg.Add(1)
	go m.execute(ctx, *row, cancel)
	writeJSON(w, row.WaitStatus)
}

func (m *Manager) execute(ctx context.Context, row record, cancel context.CancelFunc) {
	defer m.wg.Done()
	defer cancel()
	exitCode, err := runHelper(ctx, m.helper, row.Request, row.LogPath)
	m.mu.Lock()
	defer m.mu.Unlock()
	defer close(m.finished[row.ID])
	delete(m.running, row.ID)
	current := m.rows[row.ID]
	if protocol.WaitTerminal(current.State) {
		return
	}
	row.Suspension, row.Suspended = current.Suspension, current.Suspended
	row.ExitCode, row.FinishedAt, row.State = exitCode, time.Now().UTC(), "succeeded"
	switch {
	case current.CancelRequested:
		row.State = "cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		row.State = "timed_out"
	case m.ctx.Err() != nil:
		row.State = "lost"
	case err != nil:
		row.State = "failed"
	}
	// A failed write leaves the durable running journal in place. Retrying
	// the save never reruns the command and never enqueues an unsaved result.
	*current = row
	_ = m.save(current)
	m.signal()
}

func (m *Manager) deliver() {
	defer m.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
		m.flush()
	}
}

func (m *Manager) flush() {
	m.mu.Lock()
	var pending []record
	var active []record
	check := m.checkTask
	suspend := m.suspend
	var suspensions []record
	for _, row := range m.rows {
		if row.Suspension != nil && !row.Suspended && !row.Handled {
			if m.save(row) == nil {
				suspensions = append(suspensions, *row)
			}
		}
		if row.Address.TaskID != "" && !protocol.WaitTerminal(row.State) {
			active = append(active, *row)
		}
		if protocol.WaitTerminal(row.State) && row.DeliveryID == "" {
			if m.save(row) == nil {
				pending = append(pending, *row)
			}
		}
	}
	m.mu.Unlock()
	if suspend != nil {
		for _, row := range suspensions {
			ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
			err := suspend(ctx, row.Address.TaskID, *row.Suspension)
			cancel()
			if err == nil {
				m.mu.Lock()
				m.rows[row.ID].Suspended = true
				_ = m.save(m.rows[row.ID])
				m.mu.Unlock()
			}
		}
	}
	if check != nil {
		for _, row := range active {
			ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
			stop, err := check(ctx, row.Address)
			cancel()
			if err == nil && stop {
				m.mu.Lock()
				if current := m.rows[row.ID]; !protocol.WaitTerminal(current.State) {
					current.CancelRequested = true
					if cancel := m.running[row.ID]; cancel != nil {
						cancel()
					}
				}
				m.mu.Unlock()
			}
		}
	}
	for _, row := range pending {
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		id, err := m.notify(ctx, row.WaitResult)
		cancel()
		m.mu.Lock()
		current := m.rows[row.ID]
		if err == nil && id != "" {
			current.DeliveryID, current.NotifyError = id, ""
		} else {
			current.NotifyError = "result not yet accepted; will retry"
		}
		_ = m.save(current)
		m.mu.Unlock()
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type limitedLog struct {
	mu        sync.Mutex
	file      *os.File
	remaining int
}

func (l *limitedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	if len(p) > l.remaining {
		p = p[:l.remaining]
	}
	if len(p) != 0 {
		if _, err := l.file.Write(p); err != nil {
			return 0, err
		}
		l.remaining -= len(p)
	}
	return n, nil
}

func runHelper(ctx context.Context, helper []string, req protocol.WaitStart, logPath string) (*int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if len(helper) == 0 {
		return nil, errors.New("missing wait helper")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	defer writer.Close()
	cmd := exec.Command(helper[0], helper[1:]...)
	cmd.Stdin = reader
	log := &limitedLog{file: file, remaining: MaxLogBytes}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Env = helperEnvironment()
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	if err = json.NewEncoder(writer).Encode(req); err != nil {
		_ = writer.Close()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		// EOF also arrives when the daemon is killed, allowing the helper to
		// reap the entire command tree before it exits, without PID guessing.
		_ = writer.Close()
		err = <-done
	}
	code := cmd.ProcessState.ExitCode()
	return &code, err
}

func helperEnvironment() []string {
	var out []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "MULTICA_") {
			out = append(out, entry)
		}
	}
	return out
}

// ExecuteHelper runs in the daemon's own binary. The pipe's lifetime belongs
// only to the daemon; commands receive no copy of its write end or credentials.
func ExecuteHelper(input io.Reader) int {
	decoder := json.NewDecoder(input)
	var req protocol.WaitStart
	if decoder.Decode(&req) != nil || len(req.Argv) == 0 {
		return 125
	}
	ctx, cancel := context.WithDeadline(context.Background(), req.Deadline)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, input); cancel() }()
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = req.Cwd, os.Stdout, os.Stderr
	return executeCommand(ctx, cmd)
}
