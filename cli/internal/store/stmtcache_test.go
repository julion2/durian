package store

import (
	"fmt"
	"sync"
	"testing"
)

func TestStmtCache_ReusesAndBounds(t *testing.T) {
	db := newTestDB(t)
	var c stmtCache
	first, err := c.get(db.db, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.get(db.db, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Error("the same query was prepared twice")
	}
	for i := 0; i < stmtCacheSize+10; i++ {
		if _, err := c.get(db.db, fmt.Sprintf("SELECT %d", i+2)); err != nil {
			t.Fatal(err)
		}
	}
	if c.order.Len() != stmtCacheSize || len(c.entries) != stmtCacheSize {
		t.Errorf("cache holds %d/%d statements, want %d", c.order.Len(), len(c.entries), stmtCacheSize)
	}
	if _, ok := c.entries["SELECT 1"]; ok {
		t.Error("the least recently used statement wasn't evicted")
	}
	// an evicted statement is closed: using it fails
	if _, err := first.Query(); err == nil {
		t.Error("evicted statement still usable")
	}
	c.close()
	if c.order.Len() != 0 || c.entries != nil {
		t.Error("close left statements behind")
	}
	// usable again after close
	if _, err := c.get(db.db, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	c.close()
}

func TestStmtCache_BadQueryIsNotCached(t *testing.T) {
	db := newTestDB(t)
	var c stmtCache
	if _, err := c.get(db.db, "SELEKT"); err == nil {
		t.Fatal("bad SQL prepared")
	}
	if len(c.entries) != 0 {
		t.Error("a failed prepare was cached")
	}
	if err := db.queryRow("SELEKT").Scan(new(int)); err == nil {
		t.Error("queryRow hid the prepare error")
	}
}

// Searches run concurrently from the HTTP handlers.
func TestStmtCache_ConcurrentSearches(t *testing.T) {
	db := seedSearchDB(t)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for _, q := range []string{"tag:inbox", "tag:unread", "from:alice", "tag:inbox AND NOT tag:unread"} {
				if _, err := db.Search(q, 10+i); err != nil {
					errs <- err
				}
				if _, err := db.SearchCount(q); err != nil {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
