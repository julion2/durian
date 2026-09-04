package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const syncLockPollInterval = 50 * time.Millisecond

// accountSyncLocks serializes complete provider syncs for one local account.
// Snapshot staging and cursor files live in separate durable stores, so their
// multi-step protocol must have exactly one owner across processes.
type accountSyncLocks struct {
	dir      string
	memoryMu sync.Mutex
	memory   map[string]*sync.Mutex
}

func newAccountSyncLocks(dbPath string) *accountSyncLocks {
	locks := &accountSyncLocks{memory: make(map[string]*sync.Mutex)}
	if dbPath != ":memory:" {
		if absolute, err := filepath.Abs(dbPath); err == nil {
			dbPath = absolute
		}
		if resolved, err := filepath.EvalSymlinks(dbPath); err == nil {
			dbPath = resolved
		}
		locks.dir = dbPath + ".sync-locks"
	}
	return locks
}

// AcquireAccountSync holds the account's lock until release is called. Waiting
// is context-aware so a canceled CLI or daemon pass does not remain blocked
// behind another process. The account-only scope is intentionally stricter
// than account+backend: backend retargets share local rows and must not overlap.
func (d *DB) AcquireAccountSync(ctx context.Context, account string) (func(), error) {
	if d.syncLocks == nil {
		return nil, syncLockErrorf("store has no sync lock manager")
	}
	if d.syncLocks.dir == "" {
		d.syncLocks.memoryMu.Lock()
		lock := d.syncLocks.memory[account]
		if lock == nil {
			lock = &sync.Mutex{}
			d.syncLocks.memory[account] = lock
		}
		d.syncLocks.memoryMu.Unlock()
		for !lock.TryLock() {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("acquire account sync lock: %w", ctx.Err())
			case <-time.After(syncLockPollInterval):
			}
		}
		var once sync.Once
		return func() { once.Do(lock.Unlock) }, nil
	}

	if err := os.MkdirAll(d.syncLocks.dir, 0700); err != nil {
		return nil, syncLockErrorf("create directory: %v", err)
	}
	if err := os.Chmod(d.syncLocks.dir, 0700); err != nil {
		return nil, syncLockErrorf("chmod directory: %v", err)
	}
	sum := sha256.Sum256([]byte(account))
	lockFile, err := os.OpenFile(filepath.Join(d.syncLocks.dir, fmt.Sprintf("%x.lock", sum)), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, syncLockErrorf("open account lock: %v", err)
	}
	for {
		err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			_ = lockFile.Close()
			return nil, syncLockErrorf("flock account: %v", err)
		}
		select {
		case <-ctx.Done():
			_ = lockFile.Close()
			return nil, fmt.Errorf("acquire account sync lock: %w", ctx.Err())
		case <-time.After(syncLockPollInterval):
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
				slog.Warn("Could not unlock account sync", "module", "STORE", "err", err)
			}
			if err := lockFile.Close(); err != nil {
				slog.Warn("Could not close account sync lock", "module", "STORE", "err", err)
			}
		})
	}, nil
}

func syncLockErrorf(format string, args ...any) error {
	return fmt.Errorf("account sync lock: "+format, args...)
}
