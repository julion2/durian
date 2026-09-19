package store

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

var ErrOutboxLifecycleLocked = errors.New("outbox is owned by another Durian process")

// AcquireOutboxLifecycle prevents a daemon's worker and an operator's direct
// store reconciliation from running concurrently. The daemon holds this lock
// for its complete lifetime; reconciliation acquires it nonblocking before it
// opens the database.
func AcquireOutboxLifecycle(dbPath string) (func(), error) {
	if dbPath == ":memory:" {
		return nil, errors.New("outbox lifecycle lock requires a file-backed store")
	}
	if strings.HasPrefix(dbPath, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve outbox lock home directory: %w", err)
		}
		dbPath = filepath.Join(home, dbPath[2:])
	}
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve outbox lock path: %w", err)
	}
	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create outbox lock directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("chmod outbox lock directory: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(dir); resolveErr == nil {
		dir = resolved
	}
	lockFile, err := os.OpenFile(filepath.Join(dir, filepath.Base(absPath)+".outbox.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open outbox lifecycle lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, ErrOutboxLifecycleLocked
		}
		return nil, fmt.Errorf("acquire outbox lifecycle lock: %w", err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
				slog.Warn("Could not unlock outbox lifecycle", "module", "STORE", "err", err)
			}
			if err := lockFile.Close(); err != nil {
				slog.Warn("Could not close outbox lifecycle lock", "module", "STORE", "err", err)
			}
		})
	}, nil
}
