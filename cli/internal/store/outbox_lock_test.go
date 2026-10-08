package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOutboxLifecycleLockRejectsConcurrentOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "email.db")
	release, err := AcquireOutboxLifecycle(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if secondRelease, err := AcquireOutboxLifecycle(path); !errors.Is(err, ErrOutboxLifecycleLocked) {
		if secondRelease != nil {
			secondRelease()
		}
		t.Fatalf("second owner error = %v, want lifecycle lock conflict", err)
	}
	release()
	thirdRelease, err := AcquireOutboxLifecycle(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	thirdRelease()
}

func TestOutboxLifecycleAliasFirstLocksTargetAndReleases(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "email.db")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.db")
	if err := os.Symlink("email.db", alias); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireOutboxLifecycle(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if other, err := AcquireOutboxLifecycle(target); !errors.Is(err, ErrOutboxLifecycleLocked) {
		if other != nil {
			other()
		}
		t.Fatalf("target acquired while alias owns it: %v", err)
	}
	release()
	other, err := AcquireOutboxLifecycle(target)
	if err != nil {
		t.Fatalf("target still locked after alias released: %v", err)
	}
	other()
}

func TestOutboxLifecycleRejectsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "alias.db")
	if err := os.Symlink("not-created.db", alias); err != nil {
		t.Fatal(err)
	}
	release, err := AcquireOutboxLifecycle(alias)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("dangling alias acquired an independent lifecycle lock")
	}
	if _, err := os.Stat(alias + ".outbox.lock"); !os.IsNotExist(err) {
		t.Fatalf("dangling alias created a lock: %v", err)
	}
}
