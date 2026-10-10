package store

import (
	"container/list"
	"database/sql"
	"sync"
)

// stmtCacheSize bounds the prepared statements kept per store. A query shape
// – the SQL text, with values bound as parameters – repeats across searches
// (the same folder, the same filter with another thread id), so a few dozen
// cover a session.
const stmtCacheSize = 128

// stmtCache keeps prepared statements for SQL text that is run again and
// again. Compiling a search query costs SQLite ~45 µs, often more than
// running it on a selective search. The zero value is ready to use; least
// recently used statements are closed past stmtCacheSize. database/sql keeps
// a closed statement alive until rows read from it are closed. Callers hold
// mu from lookup until Query/QueryRow returns, so eviction cannot close a
// statement before execution has acquired its rows.
type stmtCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List // front: most recently used
}

type stmtEntry struct {
	query string
	stmt  *sql.Stmt
}

// get returns the prepared statement for query, preparing it on first use.
// The caller must hold mu through the start of statement execution.
func (c *stmtCache) get(db *sql.DB, query string) (*sql.Stmt, error) {
	if e, ok := c.entries[query]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*stmtEntry).stmt, nil
	}
	stmt, err := db.Prepare(query)
	if err != nil {
		return nil, err
	}
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	c.entries[query] = c.order.PushFront(&stmtEntry{query: query, stmt: stmt})
	for c.order.Len() > stmtCacheSize {
		oldest := c.order.Back()
		entry := c.order.Remove(oldest).(*stmtEntry)
		delete(c.entries, entry.query)
		_ = entry.stmt.Close()
	}
	return stmt, nil
}

// close closes every cached statement (with the store).
func (c *stmtCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := c.order.Front(); e != nil; e = e.Next() {
		_ = e.Value.(*stmtEntry).stmt.Close()
	}
	c.entries = nil
	c.order.Init()
}

// query runs a cached statement.
func (d *DB) query(q string, args ...any) (*sql.Rows, error) {
	d.stmts.mu.Lock()
	defer d.stmts.mu.Unlock()
	stmt, err := d.stmts.get(d.db, q)
	if err != nil {
		return nil, err
	}
	return stmt.Query(args...)
}

// queryRow runs a cached statement for a single row.
func (d *DB) queryRow(q string, args ...any) *sql.Row {
	d.stmts.mu.Lock()
	defer d.stmts.mu.Unlock()
	stmt, err := d.stmts.get(d.db, q)
	if err != nil {
		// a failed prepare reports itself through the row, like db.QueryRow
		return d.db.QueryRow(q, args...)
	}
	return stmt.QueryRow(args...)
}
