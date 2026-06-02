// Durian Tag Sync Server
//
// Lightweight HTTP server that synchronizes email tags across machines.
// Only stores (message_id, account, tag, action, timestamp) tuples —
// no email content, no attachments, no bodies.
//
// Usage:
//
//	durian-sync --db /data/sync.db --port 8724 --api-key "your-secret"
//	docker run -v /data:/data -p 8724:8724 durian/sync --api-key "your-secret"
package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

// TagChange represents a single tag add/remove event.
type TagChange struct {
	MessageID string `json:"message_id"`
	Account   string `json:"account"`
	Tag       string `json:"tag"`
	Action    string `json:"action"` // "add" or "remove"
	Timestamp int64  `json:"timestamp"`
	ClientID  string `json:"client_id"`
}

// SyncRequest is the payload for POST /sync.
type SyncRequest struct {
	Changes  []TagChange `json:"changes"`
	ClientID string      `json:"client_id"`
}

// SyncResponse is the response for GET /sync.
type SyncResponse struct {
	Changes []TagChange `json:"changes"`
	SyncAt  int64       `json:"sync_at"`
}

var (
	db     *sql.DB
	apiKey string
)

func main() {
	dbPath := flag.String("db", "sync.db", "SQLite database path")
	port := flag.Int("port", 8724, "HTTP port")
	flag.StringVar(&apiKey, "api-key", os.Getenv("DURIAN_SYNC_API_KEY"), "API key for auth (or DURIAN_SYNC_API_KEY env)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	if apiKey == "" {
		slog.Error("API key required", "module", "SYNC",
			"hint", "use --api-key flag or DURIAN_SYNC_API_KEY env")
		os.Exit(1)
	}

	var err error
	db, err = openDB(*dbPath)
	if err != nil {
		slog.Error("Failed to open database", "module", "SYNC", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := initDB(); err != nil {
		slog.Error("Failed to init database", "module", "SYNC", "err", err)
		os.Exit(1)
	}

	http.HandleFunc("/v1/sync", authMiddleware(handleSync))
	http.HandleFunc("/health", handleHealth)

	addr := fmt.Sprintf(":%d", *port)
	slog.Info("Sync server listening", "module", "SYNC", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		slog.Error("Server exited", "module", "SYNC", "err", err)
		os.Exit(1)
	}
}

func openDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path+"?_pragma=journal_mode(wal)")
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func initDB() error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tag_changes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id TEXT NOT NULL,
			account TEXT NOT NULL,
			tag TEXT NOT NULL,
			action TEXT NOT NULL CHECK(action IN ('add', 'remove')),
			timestamp INTEGER NOT NULL,
			client_id TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
		);
		CREATE INDEX IF NOT EXISTS idx_changes_timestamp ON tag_changes(timestamp);
		CREATE INDEX IF NOT EXISTS idx_changes_message ON tag_changes(message_id, account);
	`)
	return err
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(apiKey)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func handleSync(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		handleGetSync(w, r)
	case "POST":
		handlePostSync(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// GET /sync?since=<timestamp> — pull changes from other clients
func handleGetSync(w http.ResponseWriter, r *http.Request) {
	sinceStr := r.URL.Query().Get("since")
	since, _ := strconv.ParseInt(sinceStr, 10, 64)

	clientID := r.URL.Query().Get("client_id")

	// Get latest effective state per (message_id, account, tag) since timestamp.
	// Only return the last action for each unique (message_id, account, tag) combo,
	// excluding changes from the requesting client.
	q := `
		SELECT message_id, account, tag, action, timestamp, client_id
		FROM tag_changes
		WHERE timestamp > ? AND id IN (
			SELECT MAX(id) FROM tag_changes
			WHERE timestamp > ?
			GROUP BY message_id, account, tag
		)
	`
	params := []interface{}{since, since}
	if clientID != "" {
		q += " AND client_id != ?"
		params = append(params, clientID)
	}
	q += " ORDER BY timestamp ASC"

	rows, err := db.Query(q, params...)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var changes []TagChange
	for rows.Next() {
		var c TagChange
		if err := rows.Scan(&c.MessageID, &c.Account, &c.Tag, &c.Action, &c.Timestamp, &c.ClientID); err != nil {
			continue
		}
		changes = append(changes, c)
	}

	if changes == nil {
		changes = []TagChange{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(SyncResponse{
		Changes: changes,
		SyncAt:  time.Now().Unix(),
	})
}

// POST /sync — push local tag changes
func handlePostSync(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 10MB max
	var req SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	if len(req.Changes) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": 0})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO tag_changes (message_id, account, tag, action, timestamp, client_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}
	defer stmt.Close()

	count := 0
	for _, c := range req.Changes {
		if c.MessageID == "" || c.Tag == "" || (c.Action != "add" && c.Action != "remove") {
			continue
		}
		clientID := c.ClientID
		if clientID == "" {
			clientID = req.ClientID
		}
		if c.Timestamp == 0 {
			c.Timestamp = time.Now().Unix()
		}
		if _, err := stmt.Exec(c.MessageID, c.Account, c.Tag, c.Action, c.Timestamp, clientID); err != nil {
			continue
		}
		count++
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, "Database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "count": count})
}
