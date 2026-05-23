package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/spf13/cobra"

	"github.com/durian-dev/durian/cli/internal/config"
	"github.com/durian-dev/durian/cli/internal/contacts"
	"github.com/durian-dev/durian/cli/internal/handler"
	"github.com/durian-dev/durian/cli/internal/redact"
	"github.com/durian-dev/durian/cli/internal/store"
	"github.com/durian-dev/durian/cli/internal/tagsync"
)

var servePort int
var serveDB string
var serveContactsDB string
var serveNoAuth bool

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start OpenAPI HTTP server (for GUI integration)",
	Long: "Start the HTTP server that provides a RESTful API for the GUI.",
	Run: runServe,
}

func init() {
	serveCmd.Flags().IntVar(&servePort, "port", 9723, "port to listen on")
	serveCmd.Flags().StringVar(&serveDB, "db", "", "path to email database (default: ~/.local/share/durian/email.db)")
	serveCmd.Flags().StringVar(&serveContactsDB, "contacts-db", "", "path to contacts database (default: ~/.local/share/durian/contacts.db)")
	serveCmd.Flags().BoolVar(&serveNoAuth, "no-auth", false, "disable bearer-token auth (loopback-only; for experimental clients)")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, args []string) {
	// Override default logger: write to serve.log (truncated on each start)
	level := slog.LevelInfo
	if debugMode {
		level = slog.LevelDebug
	}
	stateDir := config.DefaultStateDir()
	os.MkdirAll(stateDir, 0o700)
	os.Chmod(stateDir, 0o700)
	logPath := filepath.Join(stateDir, "serve.log")
	if f, err := os.Create(logPath); err == nil {
		defer f.Close()
		slog.SetDefault(slog.New(redact.Wrap(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level}))))
	}

	// Open contacts database (non-fatal if missing)
	var contactsDB *contacts.DB
	contactsDBPath := serveContactsDB
	if contactsDBPath == "" {
		contactsDBPath = contacts.DefaultDBPath()
	}
	if cdb, err := contacts.Open(contactsDBPath); err != nil {
		slog.Warn("Could not open contacts database", "module", "SERVE", "path", contactsDBPath, "err", err)
	} else {
		contactsDB = cdb
		defer contactsDB.Close()
		slog.Info("Opened contacts database", "module", "SERVE", "path", contactsDBPath)
	}

	// Open email store (required for reads)
	dbPath := serveDB
	if dbPath == "" {
		dbPath = store.DefaultDBPath()
	}
	emailDB, err := store.Open(dbPath)
	if err != nil {
		slog.Error("Email store required but unavailable", "module", "SERVE", "err", err)
		fmt.Fprintln(os.Stderr, "Error: email store unavailable:", err)
		os.Exit(1)
	}
	if err := emailDB.Init(); err != nil {
		emailDB.Close()
		slog.Error("Email store init failed", "module", "SERVE", "err", err)
		fmt.Fprintln(os.Stderr, "Error: email store init failed:", err)
		os.Exit(1)
	}
	defer emailDB.Close()
	slog.Info("Opened email store", "module", "SERVE", "path", dbPath) // encgrep:allow message text, no PII attr

	// ADR-0001 step 4: bootstrap the at-rest master key. This is the
	// canonical first-run path — generates a fresh key if missing, loads
	// the existing one otherwise. The key is not retained in memory here;
	// step 5 will wire it into a keyring once encrypted columns exist.
	ensureMasterKey()

	h := handler.New(emailDB, contactsDB)
	eventHub := handler.NewEventHub()

	// Generate auth token for this session (unless --no-auth)
	var authToken string
	var expectedHeader []byte
	if !serveNoAuth {
		tokenBytes := make([]byte, 32)
		if _, err := rand.Read(tokenBytes); err != nil {
			slog.Error("Failed to generate auth token", "module", "SERVE", "err", err)
			os.Exit(1)
		}
		authToken = hex.EncodeToString(tokenBytes)
		expectedHeader = []byte("Bearer " + authToken)
	} else {
		slog.Warn("Auth disabled via --no-auth (loopback-only access still enforced)", "module", "SERVE")
		fmt.Fprintln(os.Stderr, "Warning: --no-auth disables bearer-token authentication. Loopback-only access is still enforced, but any local process can hit the API.")
	}

	r := mux.NewRouter()
	addr := fmt.Sprintf("127.0.0.1:%d", servePort)
	allowedHost := fmt.Sprintf("localhost:%d", servePort)
	allowedHostIP := fmt.Sprintf("127.0.0.1:%d", servePort)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			host := req.Host
			if host != allowedHost && host != allowedHostIP &&
				host != "localhost" && host != "127.0.0.1" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if !serveNoAuth {
				auth := []byte(req.Header.Get("Authorization"))
				if subtle.ConstantTimeCompare(auth, expectedHeader) != 1 {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next.ServeHTTP(w, req)
		})
	})
	r.HandleFunc("/api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"version": version,
			"commit":  gitCommit,
		})
	}).Methods("GET")
	r.HandleFunc("/api/v1/search", h.SearchHandler).Methods("GET")
	r.HandleFunc("/api/v1/search/count", h.SearchCountHandler).Methods("GET")
	r.HandleFunc("/api/v1/tags", h.ListTagsHandler).Methods("GET")
	r.HandleFunc("/api/v1/threads/{thread_id}", h.ShowThreadHandler).Methods("GET")
	r.HandleFunc("/api/v1/threads/{thread_id}/tags", h.TagThreadHandler).Methods("POST")
	r.HandleFunc("/api/v1/message/body", h.ShowMessageBodyHandler).Methods("GET")
	r.HandleFunc("/api/v1/messages/{message_id}/attachments/{part_id}", h.DownloadAttachmentHandler).Methods("GET")
	r.HandleFunc("/api/v1/groups", h.ListGroupsHandler).Methods("GET")
	r.HandleFunc("/api/v1/contacts/search", h.SearchContactsHandler).Methods("GET")
	r.HandleFunc("/api/v1/contacts/usage", h.IncrementContactUsageHandler).Methods("POST")
	r.HandleFunc("/api/v1/contacts", h.ListContactsHandler).Methods("GET")
	r.Handle("/api/v1/events", eventHub).Methods("GET")

	// Outbox routes
	r.HandleFunc("/api/v1/outbox/send", h.EnqueueOutboxHandler).Methods("POST")
	r.HandleFunc("/api/v1/outbox", h.ListOutboxHandler).Methods("GET")
	r.HandleFunc("/api/v1/outbox/{id}", h.DeleteOutboxHandler).Methods("DELETE")

	// Local draft routes (crash-recovery, not IMAP)
	r.HandleFunc("/api/v1/local-drafts", h.ListLocalDraftsHandler).Methods("GET")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.SaveLocalDraftHandler).Methods("PUT")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.GetLocalDraftHandler).Methods("GET")
	r.HandleFunc("/api/v1/local-drafts/{id}", h.DeleteLocalDraftHandler).Methods("DELETE")

	// Start IMAP IDLE watchers if accounts are configured
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	defer watcherCancel()

	// Load filter rules (non-fatal if missing)
	rules, rulesErr := config.LoadRules("")
	if rulesErr != nil {
		slog.Warn("Could not load filter rules", "module", "SERVE", "err", rulesErr)
	} else if len(rules) > 0 {
		slog.Info("Loaded filter rules", "module", "SERVE", "count", len(rules))
	}

	// Load contact groups (non-fatal if missing)
	groups, groupsErr := config.LoadGroups("")
	if groupsErr != nil {
		slog.Warn("Could not load contact groups", "module", "SERVE", "err", groupsErr)
	} else if len(groups) > 0 {
		h.SetGroups(groups)
		slog.Info("Loaded contact groups", "module", "SERVE", "count", len(groups))
	}

	cfg, err := config.Load(cfgFile)
	if err != nil {
		slog.Warn("Could not load config", "module", "SERVE", "err", err)
	} else {
		h.SetConfig(cfg)

		// Optional: set up remote tag sync client
		if cfg.Sync.TagSync != nil && cfg.Sync.TagSync.URL != "" && cfg.Sync.TagSync.APIKey != "" {
			tagSyncClient := tagsync.NewClient(cfg.Sync.TagSync.URL, cfg.Sync.TagSync.APIKey)
			tagSyncClient.SetStore(emailDB)
			h.SetTagSync(tagSyncClient)
			// Pull remote tag changes periodically
			go tagSyncPollLoop(watcherCtx, tagSyncClient, emailDB)
			slog.Info("Tag sync enabled", "module", "SERVE", "url", cfg.Sync.TagSync.URL)
		}

		accounts := cfg.GetAccountsWithIMAP()
		if len(accounts) == 0 {
			slog.Info("No IMAP accounts configured, skipping watchers", "module", "SERVE")
		} else {
			watcher := handler.NewWatcherManager(eventHub, emailDB, rules, groups)
			h.SetFetcher(watcher)
			h.SetSyncTrigger(watcher)
			go watcher.Start(watcherCtx, accounts)
			slog.Info("Started IDLE watchers", "module", "SERVE", "accounts", len(accounts))
		}

		// Start outbox background worker
		outboxWorker := handler.NewOutboxWorker(emailDB, cfg, eventHub)
		go outboxWorker.Start(watcherCtx)
	}

	server := &http.Server{
		Handler:        r,
		ReadTimeout:    30 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	// Bind first, then signal readiness with auth token on stdout
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("Could not listen", "module", "SERVE", "addr", addr, "err", err)
		fmt.Fprintln(os.Stderr, "Error: could not listen on", addr, err)
		os.Exit(1)
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("Server error", "module", "SERVE", "err", err)
			os.Exit(1)
		}
	}()

	// Machine-readable ready line — GUI parses this from stdout pipe
	if serveNoAuth {
		fmt.Printf("READY token= addr=%s\n", addr)
	} else {
		fmt.Printf("READY token=%s addr=%s\n", authToken, addr)
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	fmt.Println("Server is shutting down...")

	// Stop watchers before server shutdown
	watcherCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		slog.Error("Server forced to shutdown", "module", "SERVE", "err", err)
		os.Exit(1)
	}

	fmt.Println("Server exiting")
}

// tagSyncPollLoop periodically pulls remote tag changes.
func tagSyncPollLoop(ctx context.Context, client *tagsync.Client, db *store.DB) {
	// Initial pull
	pullRemoteTags(client, db)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pullRemoteTags(client, db)
		}
	}
}

// pullRemoteTags fetches tag changes from the sync server and applies them locally.
func pullRemoteTags(client *tagsync.Client, db *store.DB) {
	since := client.LoadLastSync()
	changes, syncAt, err := client.Pull(since)
	if err != nil {
		slog.Warn("Tag sync pull failed", "module", "TAGSYNC", "err", err)
		return
	}

	if len(changes) == 0 {
		return
	}

	applied := 0
	for _, c := range changes {
		switch c.Action {
		case "add":
			if err := db.ModifyTagsByMessageIDAndAccount(c.MessageID, c.Account, []string{c.Tag}, nil); err == nil {
				applied++
			}
		case "remove":
			if err := db.ModifyTagsByMessageIDAndAccount(c.MessageID, c.Account, nil, []string{c.Tag}); err == nil {
				applied++
			}
		}
	}

	client.SaveLastSync(syncAt)
	if applied > 0 {
		slog.Info("Applied remote tag changes", "module", "TAGSYNC", "applied", applied, "total", len(changes))
	}
}
