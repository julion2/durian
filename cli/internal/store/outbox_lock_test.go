package store

import (
	"errors"
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
