// demo-session is a manual, test-only composition root, never a production
// credential bypass. See README.md before an explicitly authorized live launch.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/contacts"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/demotest"
	"github.com/julion2/durian/cli/internal/handler"
	"github.com/julion2/durian/cli/internal/store"
	"github.com/julion2/durian/cli/internal/syncengine"
)

var version = "dev"
var gitCommit = "unknown"

type options struct {
	outputDir string
	duration  time.Duration
	port      int
	calendars bool
}

type connection struct {
	Backend string `json:"MAIL_BACKEND"`
	Token   string `json:"MAIL_TOKEN"`
	PID     int    `json:"pid"`
	API     int    `json:"api"`
}

func main() {
	// No provider-controlled errors, subjects, addresses or credentials can
	// escape via library/HTTP logs. Only fixed operational statuses go to stdout.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	log.SetOutput(io.Discard)
	var o options
	flag.StringVar(&o.outputDir, "output-dir", "", "required NEW private directory; retained after exit (keys are not)")
	flag.DurationVar(&o.duration, "duration", 10*time.Minute, "total lifetime including startup (1s to 30m)")
	flag.IntVar(&o.port, "port", 0, "loopback port (0 chooses a free port)")
	flag.BoolVar(&o.calendars, "calendars", false, "enable only the two explicitly pinned existing Durian Test calendars")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "Invalid arguments")
		os.Exit(1)
	}
	if err := runSession(ctx, o, newSessionFromEnv, os.Stdout); err != nil {
		// Deliberately never format err: even setup failures may embed PII.
		fmt.Fprintln(os.Stderr, "Session failed (details withheld); inspect private state before any retry")
		os.Exit(1)
	}
}

// Mail-only constructor adapter retained for the offline mail contract tests.
func run(parent context.Context, o options, newMail func(context.Context) (*demotest.Mail, error), status io.Writer) error {
	return runSession(parent, o, func(ctx context.Context, calendars bool) (*demotest.Mail, *calendarSession, error) {
		if calendars {
			return nil, nil, errors.New("mail-only constructor")
		}
		m, err := newMail(ctx)
		return m, nil, err
	}, status)
}

func runSession(parent context.Context, o options, setup func(context.Context, bool) (*demotest.Mail, *calendarSession, error), status io.Writer) error {
	if os.Getenv("DURIAN_MAIL_LIVE_TEST") != "1" {
		return errors.New("explicit mail live opt-in required")
	}
	if o.calendars && os.Getenv("DURIAN_CALENDAR_LIVE_TEST") != "1" {
		return errors.New("explicit calendar live opt-in required")
	}
	if o.outputDir == "" || strings.ContainsAny(o.outputDir, "$~") || o.port < 0 || o.port > 65535 || o.duration < time.Second || o.duration > 30*time.Minute {
		return errors.New("invalid private directory, port or lifetime")
	}
	root, err := filepath.Abs(o.outputDir)
	if err != nil {
		return errors.New("resolve private directory")
	}
	// Exclusive creation rejects existing roots/symlinks and stale queues. Never
	// resume with new keys, overwrite a config, or silently reuse prior state.
	if err := os.Mkdir(root, 0700); err != nil {
		return errors.New("private directory must be new")
	}
	ctx, cancel := context.WithTimeout(parent, o.duration)
	defer cancel()
	m, calendars, err := setup(ctx, o.calendars)
	if err != nil {
		return errors.New("verify demo identities")
	}
	cfg := sessionConfig(root, o.calendars)
	if o.calendars {
		if calendars == nil {
			return errors.New("guarded calendars unavailable")
		}
		if err := writePrivateJSON(filepath.Join(root, "calendar-session.json"), map[string]string{"prefix": calendars.prefix}); err != nil {
			return err
		}
	}
	if err := writePrivateJSON(filepath.Join(root, "config.json"), cfg); err != nil {
		return err
	}
	profiles := map[string]any{"profiles": []config.ProfileConfig{{Name: "Demo mail", Default: true,
		Accounts: []string{"gmail-demo", "outlook-demo"}, Folders: []config.FolderConfig{
			{Name: "Inbox", Icon: "tray", Query: "tag:inbox"},
			{Name: "Sent", Icon: "paperplane", Query: "tag:sent"},
			{Name: "All mail", Icon: "archivebox", Query: "*"},
		}}}}
	if err := writePrivateJSON(filepath.Join(root, "profiles.json"), profiles); err != nil {
		return err
	}
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		return errors.New("generate ephemeral key")
	}
	kr, err := dbcrypto.NewKeyring(master)
	clear(master)
	if err != nil {
		return errors.New("derive ephemeral keyring")
	}
	db, err := store.Open(filepath.Join(root, "email.db"), kr)
	if err != nil {
		return errors.New("open isolated mail store")
	}
	defer db.Close()
	if err := db.Init(); err != nil {
		return errors.New("initialize isolated mail store")
	}
	aliases := make(map[string]string)
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		for _, ref := range []string{a.Name, a.Alias, a.Email} {
			aliases[strings.ToLower(ref)] = a.AccountIdentifier()
		}
	}
	db.SetAccountAliases(aliases)
	// The production contacts schema is deliberately plaintext. Keep contacts
	// exclusively in RAM instead of claiming it supports disk encryption.
	cdb, err := contacts.OpenMemory()
	if err != nil {
		return errors.New("open memory contacts")
	}
	defer cdb.Close()
	if err := cdb.Init(); err != nil {
		return errors.New("initialize memory contacts")
	}
	for _, a := range cfg.Accounts {
		if err := cdb.Add(a.Email, a.DisplayName, "demo"); err != nil {
			return errors.New("seed demo contacts")
		}
	}
	h := handler.New(db, cdb)
	h.SetConfig(cfg)
	h.SetConfigPath(filepath.Join(root, "config.json"))
	h.SetProfilesPath(filepath.Join(root, "profiles.json"))
	h.SetKeymapsPath(filepath.Join(root, "keymaps.json")) // absent = client defaults, never user's file
	h.SetBackendFactory(m.BackendFor)
	if calendars != nil {
		h.SetCalendarEventSyncer(handler.NewCalendarEventSyncer(cfg, calendars.providerFor))
	}
	hub := handler.NewEventHub()
	worker := handler.NewOutboxWorker(db, cfg, hub)
	worker.SetSenderFactory(m.SenderFor)

	// Complete one bounded, guarded download for BOTH accounts before READY.
	var engines []*syncengine.Engine
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		folders := []string{"INBOX", "junk", "sent"}
		if a.UsesGmailBackend() {
			folders = []string{"ALL"}
		}
		engines = append(engines, syncengine.New(syncengine.Options{Store: db,
			Cursors: &memoryCursors{states: make(map[string]syncengine.FolderState)},
			Account: a.AccountIdentifier(), Ingest: syncengine.IngestOptions{Account: a.AccountIdentifier()},
			Mode: syncengine.DownloadOnly, Folders: folders, BatchLimit: 20, MaxPerFolder: 20,
			Timeout: 30 * time.Second, RecoveryTimeout: 30 * time.Second}))
	}
	syncAll := func() error {
		var result error
		for i, engine := range engines {
			result = errors.Join(result, syncAccount(ctx, engine, m, &cfg.Accounts[i]))
		}
		if calendars != nil {
			for i := range cfg.Accounts {
				result = errors.Join(result, calendars.download(ctx, cfg, &cfg.Accounts[i], hub))
			}
		}
		return result
	}
	if err := syncAll(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", o.port))
	if err != nil {
		return errors.New("bind loopback listener")
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return errors.New("generate session token")
	}
	router := handler.NewRouter(h, hub, version, gitCommit)
	router.Use(handler.LoopbackAuth(port, hex.EncodeToString(tokenBytes)))
	if calendars != nil {
		router.Use(calendars.policy(cfg))
	}
	server := &http.Server{Handler: router, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx }}
	var wg sync.WaitGroup
	wg.Add(3)
	serveErr := make(chan error, 1)
	go func() { defer wg.Done(); serveErr <- server.Serve(listener) }()
	go func() { defer wg.Done(); worker.Start(ctx) }()
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := syncAll(); err != nil && ctx.Err() == nil {
					fmt.Fprintln(status, "SYNC_FAILED")
				}
			}
		}
	}()
	defer func() {
		cancel() // cancels SSE, provider requests, sync and in-flight sends
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if server.Shutdown(shutdown) != nil {
			server.Close()
		}
		wg.Wait() // no store close while worker/sync still access it
	}()
	connPath := filepath.Join(root, "connection.json")
	if err := writePrivateJSON(connPath, connection{Backend: fmt.Sprintf("http://127.0.0.1:%d", port),
		Token: hex.EncodeToString(tokenBytes), PID: os.Getpid(), API: 1}); err != nil {
		return err
	}
	defer os.Remove(connPath)     // not a stale readiness signal after exit
	fmt.Fprintln(status, "READY") // deliberately not production's token-bearing handshake
	select {
	case <-ctx.Done():
		return nil
	case <-serveErr:
		return errors.New("session listener stopped")
	}
}

func sessionConfig(root string, calendars bool) *config.Config {
	disabled := false
	var include []string
	if calendars {
		include = []string{"Durian Test"}
	}
	return &config.Config{
		Settings:   &config.SettingsConfig{Theme: "system"},
		Calendar:   config.CalendarConfig{VdirPath: filepath.Join(root, "calendars"), Autosync: &disabled},
		Signatures: map[string]string{"demo": "Demo session signature"},
		Accounts: []config.AccountConfig{
			{Name: "Gmail", Alias: "gmail-demo", DisplayName: "Gmail Demo", Default: true,
				Email: os.Getenv("DURIAN_GOOGLE_TEST_EMAIL"), OAuth: &config.OAuthConfig{Provider: "google"},
				DefaultSignature: "demo", Calendar: &config.AccountCalendarConfig{Enabled: &calendars, Include: include}},
			{Name: "Outlook", Alias: "outlook-demo", DisplayName: "Outlook Demo",
				Email: os.Getenv("DURIAN_MS_TEST_EMAIL"), OAuth: &config.OAuthConfig{Provider: "microsoft"},
				DefaultSignature: "demo", Calendar: &config.AccountCalendarConfig{Enabled: &calendars, Include: include}},
		},
	}
}

func syncAccount(parent context.Context, engine *syncengine.Engine, m *demotest.Mail, a *config.AccountConfig) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second) // hard outer recovery bound too
	defer cancel()
	b, err := m.BackendFor(a)
	if err != nil {
		return errors.New("guarded backend unavailable")
	}
	defer b.Close()
	result, err := engine.Sync(ctx, b)
	if err != nil || result == nil || len(result.Errors) != 0 {
		return errors.New("bounded download sync failed")
	}
	return nil
}

func writePrivateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("encode private session file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("create private session file")
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(path)
		return errors.New("write private session file")
	}
	return nil
}

// Each engine has its own RAM-only cursors. No default XDG cache, provider
// cursor or token can leak to disk; a new session always starts from scratch.
type memoryCursors struct {
	states map[string]syncengine.FolderState
}

func (m *memoryCursors) GetState(_, folder string) (syncengine.FolderState, error) {
	return m.states[folder], nil
}

func (m *memoryCursors) Commit(_, folder string, cursor backend.Cursor, pending syncengine.PendingFlags) error {
	m.states[folder] = syncengine.FolderState{Cursor: cursor, PendingFlags: pending}
	return nil
}
