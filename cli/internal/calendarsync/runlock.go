// Run-level cross-process lock for one account's calendar sync cycle.
//
// FileStateStore's flock only guards each individual Load/Save, not the whole
// Load -> Plan -> Apply -> Save span: two concurrent runs (e.g. the serve
// autosync loop and a manual `durian calendar sync`) could both plan from the
// same baseline and then double-execute an upload or clobber each other's
// saved state. AcquireRunLock closes that window with one non-blocking flock
// held for the entire cycle; the loser skips (autosync) or reports the
// concurrent run (CLI) instead of waiting.

package calendarsync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// runLockName is the lock file inside the account's vdir directory. The
// hidden dotfile sits next to .durian-calsync-state.json, above the
// per-calendar collection subdirs, so khal/vdirsyncer ignore it.
const runLockName = ".durian-calsync-run.lock"

// AcquireRunLock takes a NON-blocking exclusive flock on
// <accountDir>/.durian-calsync-run.lock, guarding a whole sync cycle for the
// account across processes. ok=false (with a nil release and nil error) means
// another holder — process or goroutine — currently runs a sync for this
// directory. On ok=true the caller must call release on every exit path
// (defer); release unlocks and closes the lock file. The lock file itself is
// left in place — flock locks die with the descriptor, so a crashed holder
// never leaves a stale lock.
func AcquireRunLock(accountDir string) (release func() error, ok bool, err error) {
	if err := os.MkdirAll(accountDir, 0o700); err != nil {
		return nil, false, fmt.Errorf("failed to create account calendar dir: %w", err)
	}

	lockFile, err := os.OpenFile(filepath.Join(accountDir, runLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("failed to open calendar run lock file: %w", err)
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to flock calendar run lock: %w", err)
	}

	release = func() error {
		syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		return lockFile.Close()
	}
	return release, true, nil
}
