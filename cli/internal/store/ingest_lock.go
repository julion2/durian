package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	ingestLockStripes = 256
	ingestLockTimeout = 30 * time.Second
)

var (
	// ErrMessageIngestLock means enrichment could not safely acquire its stripe.
	// Sync engines must retry without advancing their provider cursor.
	ErrMessageIngestLock = errors.New("message ingest lock unavailable")
	// ErrMessageIngestLockTimeout means another process still owns the stripe.
	ErrMessageIngestLockTimeout = fmt.Errorf("%w: timeout", ErrMessageIngestLock)
)

// ingestLocks serializes the full enrichment lifetime across independent DB
// handles and processes. A fixed stripe count avoids creating one sidecar file
// per message while still allowing unrelated messages to progress in parallel.
type ingestLocks struct {
	dir      string
	memoryMu sync.Mutex
}

func newIngestLocks(dbPath string) *ingestLocks {
	locks := &ingestLocks{}
	if dbPath != ":memory:" {
		if absolute, err := filepath.Abs(dbPath); err == nil {
			dbPath = absolute
		}
		if resolved, err := filepath.EvalSymlinks(dbPath); err == nil {
			dbPath = resolved
		}
		locks.dir = dbPath + ".ingest-locks"
	}
	return locks
}

// AcquireMessageIngest holds one message's enrichment stripe until the returned
// release function is called. Callers acquire it after resolving the canonical
// Message-ID and before the core-row upsert, then defer release until all
// attachments, headers, tags, rules, and completion state are durable.
func (d *DB) AcquireMessageIngest(account, messageID string) (func(), error) {
	if d.ingestLocks == nil {
		return nil, fmt.Errorf("%w: store has no ingest lock manager", ErrMessageIngestLock)
	}
	if d.ingestLocks.dir == "" {
		d.ingestLocks.memoryMu.Lock()
		var once sync.Once
		return func() { once.Do(d.ingestLocks.memoryMu.Unlock) }, nil
	}

	if err := os.MkdirAll(d.ingestLocks.dir, 0700); err != nil {
		return nil, fmt.Errorf("%w: create directory: %w", ErrMessageIngestLock, err)
	}
	if err := os.Chmod(d.ingestLocks.dir, 0700); err != nil {
		return nil, fmt.Errorf("%w: chmod directory: %w", ErrMessageIngestLock, err)
	}
	sum := sha256.Sum256([]byte(account + "\x00" + messageID))
	stripe := int(sum[0]) % ingestLockStripes
	lockFile, err := os.OpenFile(filepath.Join(d.ingestLocks.dir, fmt.Sprintf("%02x.lock", stripe)), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("%w: open stripe: %w", ErrMessageIngestLock, err)
	}
	deadline := time.Now().Add(ingestLockTimeout)
	for {
		err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN && err != syscall.EINTR {
			lockFile.Close()
			return nil, fmt.Errorf("%w: flock stripe: %w", ErrMessageIngestLock, err)
		}
		if !time.Now().Before(deadline) {
			lockFile.Close()
			return nil, fmt.Errorf("%w after %s", ErrMessageIngestLockTimeout, ingestLockTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
				slog.Warn("Could not unlock message ingest stripe", "module", "STORE", "err", err)
			}
			if err := lockFile.Close(); err != nil {
				slog.Warn("Could not close message ingest lock", "module", "STORE", "err", err)
			}
		})
	}, nil
}
