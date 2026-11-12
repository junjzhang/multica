package execenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/multica-ai/multica/server/internal/cli"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	codexSessionWriterLockRoot = "multica-session-writer-locks"
	codexSessionWriterLockPoll = 25 * time.Millisecond
)

// ProviderStoreWriterLock serializes writers and deletion of a persistent provider
// store. Each local_directory task gets a fresh CODEX_HOME, so Codex's own
// writer lock (which lives under CODEX_HOME/thread-writer-locks) cannot see a
// sibling task that mounts the same persistent sessions directory. Without a
// store-scoped lock, a replacement task can append to the same rollout while
// the cancelled task is still unwinding and corrupt its ordinal sequence.
type ProviderStoreWriterLock struct {
	file *os.File
	once sync.Once
}

// AcquireCodexSessionWriterLock waits until no other Multica task is writing
// store. The lock is cross-process and crash-safe: the OS releases it when the
// daemon exits even if Release is not called.
//
// The lock file deliberately lives outside the session store. Store GC may
// remove and recreate the store; putting the lock inside it would let a waiter
// lock a new inode while the old writer still held the unlinked one. Lock files
// are stable coordination objects and must not be deleted as stale markers.
// onWait is called at most once, when another writer is first observed.
func AcquireCodexSessionWriterLock(ctx context.Context, store string, onWait func()) (*ProviderStoreWriterLock, error) {
	if store == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return acquireStoreWriterLock(ctx, store, codexSessionWriterLockPath(store), false, onWait)
}

func acquireStoreWriterLock(ctx context.Context, store, path string, shared bool, onWait func()) (*ProviderStoreWriterLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create provider store writer lock directory: %w", err)
	}
	f, err := openLockFile(path)
	if err != nil {
		return nil, fmt.Errorf("open provider store writer lock %s: %w", path, err)
	}

	announced := false
	ticker := time.NewTicker(codexSessionWriterLockPoll)
	defer ticker.Stop()
	for {
		lock := lockFileExclusiveNonBlocking
		if shared {
			lock = lockFileSharedNonBlocking
		}
		locked, lockErr := lock(f)
		if lockErr != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock provider store %s: %w", store, lockErr)
		}
		if locked {
			return &ProviderStoreWriterLock{file: f}, nil
		}
		if !announced {
			announced = true
			if onWait != nil {
				onWait()
			}
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("wait for provider store writer lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Release drops the store writer lock. It is safe to call more than once.
func (l *ProviderStoreWriterLock) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		releaseLockFile(l.file)
		l.file = nil
	})
}

func codexSessionWriterLockPath(store string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(store)))
	return filepath.Join(resolveSharedCodexHome(), codexSessionWriterLockRoot, hex.EncodeToString(sum[:])+".lock")
}

// AcquireProviderStoreWriterLock uses Codex's existing lock identity for Codex
// and a stable Multica-owned lock directory for other persistent stores.
func AcquireProviderStoreWriterLock(ctx context.Context, provider, store string, onWait func()) (*ProviderStoreWriterLock, error) {
	if provider == "codex" {
		return AcquireCodexSessionWriterLock(ctx, store, onWait)
	}
	path, err := providerStoreLockPath(store)
	if err != nil {
		return nil, err
	}
	return acquireStoreWriterLock(ctx, store, path, false, onWait)
}

func providerStoreLockPath(store string) (string, error) {
	root, err := cli.ProfileDir("")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(store)))
	return filepath.Join(root, "resource-writer-locks", hex.EncodeToString(sum[:])+".lock"), nil
}

// TryProviderStoreWriterLock never waits behind a live task during GC.
func TryProviderStoreWriterLock(provider, store string) (*ProviderStoreWriterLock, error) {
	path := codexSessionWriterLockPath(store)
	if provider != "codex" {
		var err error
		path, err = providerStoreLockPath(store)
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	ok, err := lockFileExclusiveNonBlocking(f)
	if err != nil || !ok {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrEnvRootBusy
	}
	return &ProviderStoreWriterLock{file: f}, nil
}

// AcquireHermesMemoryUseLock preserves existing concurrent memory writers while
// excluding GC. Session transcript stores still require an exclusive writer.
func AcquireHermesMemoryUseLock(ctx context.Context, store string, onWait func()) (*ProviderStoreWriterLock, error) {
	path, err := providerStoreLockPath(store)
	if err != nil {
		return nil, err
	}
	return acquireStoreWriterLock(ctx, store, path, true, onWait)
}
