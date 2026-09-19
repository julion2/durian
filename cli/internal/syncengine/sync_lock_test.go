package syncengine

import (
	"bytes"
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

type pageBoundaryBackend struct {
	backend.Backend
	boundary chan struct{}
	resume   chan struct{}
	calls    int
}

func (b *pageBoundaryBackend) FetchMessages(ctx context.Context, folder string, cursor backend.Cursor, limit int) (backend.FetchResult, error) {
	b.calls++
	if b.calls == 2 {
		close(b.boundary)
		select {
		case <-ctx.Done():
			return backend.FetchResult{}, ctx.Err()
		case <-b.resume:
		}
	}
	return b.Backend.FetchMessages(ctx, folder, cursor, limit)
}

type observedFoldersBackend struct {
	backend.Backend
	entered chan struct{}
}

func (b *observedFoldersBackend) FetchFolders(ctx context.Context) ([]backend.Folder, error) {
	close(b.entered)
	return b.Backend.FetchFolders(ctx)
}

func TestConcurrentEnginesCannotInterleaveReplacementPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	kr, err := dbcrypto.NewKeyring(bytes.Repeat([]byte{0x42}, dbcrypto.MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	firstDB, err := store.Open(path, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstDB.Close() })
	if err := firstDB.Init(); err != nil {
		t.Fatal(err)
	}
	secondDB, err := store.Open(path, kr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondDB.Close() })

	folder := backend.Folder{Name: "ALL", Role: backend.RoleAll, Selectable: true}
	message := func(id string) backend.Message {
		return backend.Message{
			StableID: id, MessageID: id + "@example.test",
			Ref: backend.RemoteRef{Folder: folder.Name, ID: id},
			Raw: rawMessage(id+"@example.test", "sender@example.test", testAccount, "Snapshot", "body"),
		}
	}
	firstBase := newFakeBackend([]backend.Folder{folder}, map[string][]backend.FetchResult{
		folder.Name: {
			{Messages: []backend.Message{message("r1")}, Present: []backend.RemoteRef{{Folder: folder.Name, ID: "r1"}}, Cursor: backend.Cursor("page-1"), HasMore: true, FullSnapshot: true},
			{Messages: []backend.Message{message("r2")}, Present: []backend.RemoteRef{{Folder: folder.Name, ID: "r2"}}, Cursor: backend.Cursor("final"), FullSnapshot: true},
		},
	})
	boundary := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	resumeFirst := func() { resumeOnce.Do(func() { close(resume) }) }
	t.Cleanup(resumeFirst)
	firstBackend := &pageBoundaryBackend{Backend: firstBase, boundary: boundary, resume: resume}
	firstCursors := newMemCursorStore()
	if err := firstCursors.Set(testAccount, folder.Name, backend.Cursor("base")); err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() {
		result, err := newTestEngine(firstDB, firstCursors).Sync(t.Context(), firstBackend)
		if err == nil && len(result.Errors) > 0 {
			err = result.Errors[0]
		}
		firstDone <- err
	}()
	select {
	case <-boundary:
	case <-time.After(5 * time.Second):
		t.Fatal("first engine did not reach the staged page boundary")
	}

	secondEntered := make(chan struct{})
	secondBase := newFakeBackend(nil, nil)
	secondBackend := &observedFoldersBackend{Backend: secondBase, entered: secondEntered}
	secondDone := make(chan error, 1)
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		_, err := newTestEngine(secondDB, newMemCursorStore()).Sync(t.Context(), secondBackend)
		secondDone <- err
	}()
	<-secondStarted
	select {
	case <-secondEntered:
		t.Fatal("second engine entered the provider while the first snapshot was between pages")
	case <-time.After(100 * time.Millisecond):
	}

	resumeFirst()
	if err := <-firstDone; err != nil {
		t.Fatalf("first sync: %v", err)
	}
	select {
	case <-secondEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second engine did not enter after the first released its account lock")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second sync: %v", err)
	}
}
