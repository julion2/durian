package store

import (
	"fmt"
	"strings"
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

// Distinct query shapes exceed the cache capacity while other HTTP searches
// are starting. Eviction must not close a statement before its query starts.
func TestStmtCache_ConcurrentSearches(t *testing.T) {
	db := seedSearchDB(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 240 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			q := strings.Repeat("NOT ", i) + "from:alice"
			want := 3 // Alice appears in three threads; Bob/Charlie in two.
			if i%2 != 0 {
				want = 2
			}
			for range 20 {
				count, err := db.SearchCount(q)
				if err != nil {
					t.Errorf("SearchCount with %d negations: %v", i, err)
				} else if count != want {
					t.Errorf("SearchCount with %d negations: %d threads, want %d", i, count, want)
				}
			}
			results, err := db.Search(q, 50)
			if err != nil {
				t.Errorf("Search with %d negations: %v", i, err)
			} else if len(results) != want {
				t.Errorf("Search with %d negations: %d threads, want %d", i, len(results), want)
			}
		}(i)
	}
	close(start)
	wg.Wait()
}
