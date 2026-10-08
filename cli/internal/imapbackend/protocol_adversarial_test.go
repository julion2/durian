package imapbackend

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap"
	serverbackend "github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/server"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/config"
)

var (
	protocolTestCertificate tls.Certificate
	protocolTestRoots       *x509.CertPool
)

// TestMain installs a process-local test CA because imap.Client intentionally
// has no insecure-TLS mode, and puts fake OS-keychain commands first on PATH.
// This keeps the tests on the real New -> TLS -> AUTHENTICATE path without
// touching a developer keychain or weakening production TLS.
func TestMain(m *testing.M) {
	cert, roots, rootPEM, err := newProtocolTestCertificate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "create protocol test certificate: %v\n", err)
		os.Exit(1)
	}
	protocolTestCertificate = cert
	protocolTestRoots = roots

	tmp, err := os.MkdirTemp("", "durian-imap-wire-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create protocol test directory: %v\n", err)
		os.Exit(1)
	}

	caFile := filepath.Join(tmp, "ca.pem")
	if err := os.WriteFile(caFile, rootPEM, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write protocol test CA: %v\n", err)
		_ = os.RemoveAll(tmp)
		os.Exit(1)
	}
	credentialCommand := []byte("#!/bin/sh\nprintf '%s\\n' password\n")
	for _, name := range []string{"secret-tool", "security"} {
		if err := os.WriteFile(filepath.Join(tmp, name), credentialCommand, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "write fake keychain command: %v\n", err)
			_ = os.RemoveAll(tmp)
			os.Exit(1)
		}
	}

	// SetFallbackRoots plus x509usefallbackroots works on macOS too, where
	// SSL_CERT_FILE alone does not replace the platform verifier.
	debug := os.Getenv("GODEBUG")
	if debug != "" {
		debug += ","
	}
	_ = os.Setenv("GODEBUG", debug+"x509usefallbackroots=1")
	_ = os.Setenv("SSL_CERT_FILE", caFile)
	_ = os.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	x509.SetFallbackRoots(roots)

	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

func TestProtocolCompetingClientFlagChangesRoundTrip(t *testing.T) {
	env := newProtocolTestEnv(t)
	msg, _ := fetchOnlyMessage(t, env.durian, nil)

	wireSelect(t, env.other, "INBOX")
	wireStoreFlags(t, env.other, 6, goimap.SetFlags, []string{goimap.FlaggedFlag})

	flags, err := env.durian.FetchFlags(context.Background(), "INBOX", []backend.RemoteRef{msg.Ref})
	if err != nil {
		t.Fatalf("FetchFlags after competing STORE: %v", err)
	}
	got, ok := flags[msg.Ref.ID]
	if !ok || got.Seen || !got.Flagged {
		t.Fatalf("FetchFlags after competing STORE = (%+v, present=%v), want unread and flagged", got, ok)
	}

	if err := env.durian.ApplyFlags(context.Background(), msg.Ref,
		backend.Flags{Seen: true}, backend.Flags{Flagged: true}); err != nil {
		t.Fatalf("ApplyFlags: %v", err)
	}
	wireSelect(t, env.other, "INBOX")
	gotWire := wireFetchFlags(t, env.other, 6)
	if !containsFlag(gotWire, goimap.SeenFlag) || containsFlag(gotWire, goimap.FlaggedFlag) {
		t.Fatalf("wire flags after ApplyFlags = %v, want \\Seen without \\Flagged", gotWire)
	}
}

func TestProtocolFetchBodyUsesPeekAndPreservesUnread(t *testing.T) {
	env := newProtocolTestEnv(t)
	msg, _ := fetchOnlyMessage(t, env.durian, nil)
	if err := env.durian.ApplyFlags(context.Background(), msg.Ref, backend.Flags{}, backend.Flags{Seen: true}); err != nil {
		t.Fatalf("make message unread: %v", err)
	}

	var body bytes.Buffer
	if err := env.durian.FetchBody(context.Background(), msg.Ref, &body); err != nil {
		t.Fatalf("FetchBody: %v", err)
	}
	if !bytes.Contains(body.Bytes(), []byte("Hi there :)")) {
		t.Fatalf("FetchBody returned unexpected message: %q", body.String())
	}
	select {
	case peek := <-env.hooks.bodyRequests:
		if !peek {
			t.Fatal("FetchBody issued BODY[] instead of BODY.PEEK[]")
		}
	case <-time.After(time.Second):
		t.Fatal("server did not observe a BODY section fetch")
	}

	wireSelect(t, env.other, "INBOX")
	if flags := wireFetchFlags(t, env.other, 6); containsFlag(flags, goimap.SeenFlag) {
		t.Fatalf("BODY.PEEK changed unread message flags to %v", flags)
	}
}

func TestProtocolMoveExpungesSourceAndReconcilesBothMailboxes(t *testing.T) {
	env := newProtocolTestEnv(t)
	if err := env.other.Create("Archive"); err != nil {
		t.Fatalf("wire CREATE Archive: %v", err)
	}
	msg, cursor := fetchOnlyMessage(t, env.durian, nil)
	ref := msg.Ref
	ref.MessageID = msg.MessageID

	moved, err := env.durian.Move(context.Background(), ref, "Archive")
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moved.Folder != "Archive" || moved.ID != "" || moved.MessageID != msg.MessageID {
		t.Fatalf("Move returned %+v", moved)
	}

	wireSelect(t, env.other, "INBOX")
	if uids := wireSearchAll(t, env.other); len(uids) != 0 {
		t.Fatalf("source still contains UIDs after MOVE/EXPUNGE: %v", uids)
	}
	wireSelect(t, env.other, "Archive")
	if uids := wireSearchAll(t, env.other); len(uids) != 1 {
		t.Fatalf("destination UIDs after MOVE = %v, want one", uids)
	}

	source, err := env.durian.FetchMessages(context.Background(), "INBOX", cursor, 0)
	if err != nil {
		t.Fatalf("FetchMessages source after move: %v", err)
	}
	if len(source.Deleted) != 1 || source.Deleted[0].MessageID != msg.MessageID {
		t.Fatalf("source deletions after move = %+v, want %q", source.Deleted, msg.MessageID)
	}
	dest, err := env.durian.FetchMessages(context.Background(), "Archive", nil, 0)
	if err != nil {
		t.Fatalf("FetchMessages destination after move: %v", err)
	}
	if len(dest.Messages) != 1 || dest.Messages[0].MessageID != msg.MessageID {
		t.Fatalf("destination messages after move = %+v, want %q", dest.Messages, msg.MessageID)
	}
}

func TestProtocolExpungeBetweenSearchAndFetchDoesNotCreatePhantomState(t *testing.T) {
	env := newProtocolTestEnv(t)
	searched, release := env.hooks.armSearchBarrier()
	type fetchOutcome struct {
		result backend.FetchResult
		err    error
	}
	done := make(chan fetchOutcome, 1)
	go func() {
		result, err := env.durian.FetchMessages(context.Background(), "INBOX", nil, 0)
		done <- fetchOutcome{result: result, err: err}
	}()

	select {
	case <-searched:
	case <-time.After(time.Second):
		t.Fatal("Durian did not reach UID SEARCH")
	}
	wireSelect(t, env.other, "INBOX")
	wireStoreFlags(t, env.other, 6, goimap.AddFlags, []string{goimap.DeletedFlag})
	if err := env.other.Expunge(nil); err != nil {
		t.Fatalf("wire EXPUNGE: %v", err)
	}
	close(release)

	var outcome fetchOutcome
	select {
	case outcome = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FetchMessages hung after a competing EXPUNGE")
	}
	if outcome.err != nil {
		t.Fatalf("FetchMessages after competing EXPUNGE: %v", outcome.err)
	}
	if len(outcome.result.Messages) != 0 || len(outcome.result.Present) != 0 {
		t.Fatalf("expunged UID became present: messages=%d present=%v",
			len(outcome.result.Messages), outcome.result.Present)
	}
	state, _, err := decodeCursor(outcome.result.Cursor)
	if err != nil {
		t.Fatalf("decode resulting cursor: %v", err)
	}
	if len(state.SyncedUIDs) != 0 {
		t.Fatalf("expunged UID was persisted as synced: %v", state.SyncedUIDs)
	}
}

func TestProtocolCancelledFetchStopsWhileServerIsStalled(t *testing.T) {
	env := newProtocolTestEnv(t)
	searched, release := env.hooks.armSearchBarrier()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := env.durian.FetchMessages(ctx, "INBOX", nil, 0)
		done <- err
	}()

	select {
	case <-searched:
	case <-time.After(2 * time.Second):
		t.Fatal("Durian did not reach the server-side search barrier")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled FetchMessages returned %v, want context.Canceled", err)
		}
		unblock()
		// The cancelled operation must not reconnect and complete anyway. A
		// later independent request may reconnect and use a fresh transport.
		fetchOnlyMessage(t, env.durian, nil)
		return
	case <-time.After(500 * time.Millisecond):
		t.Error("FetchMessages ignored cancellation while the live server withheld its response")
	}

	// Always release the synthetic server and join the call, even on failure.
	// This reproduces the lack of cancellation without waiting for the real
	// transport timeout or leaving a blocked test goroutine behind.
	unblock()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("FetchMessages completed after cancellation with err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FetchMessages did not finish after the server was released")
	}
}

func TestProtocolCompletedContextDoesNotCloseReusedConnection(t *testing.T) {
	env := newProtocolTestEnv(t)
	// Disallow transparent reconnect so a stray cancellation callback cannot
	// hide behind withReconnect's retry on the next operation.
	env.durian.ownsClient = false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := env.durian.FetchMessages(ctx, "INBOX", nil, 0); err != nil {
		t.Fatal(err)
	}
	cancel()
	fetchOnlyMessage(t, env.durian, nil)
}

func TestProtocolCancelledAppendDoesNotMutateMailbox(t *testing.T) {
	env := newProtocolTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.durian.Append(ctx, "INBOX", backend.Flags{}, []byte("Subject: no upload\r\n\r\nbody")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Append error = %v, want canceled", err)
	}
	wireSelect(t, env.other, "INBOX")
	if uids := wireSearchAll(t, env.other); len(uids) != 1 || uids[0] != 6 {
		t.Fatalf("cancelled Append mutated mailbox: %v", uids)
	}
}

func TestProtocolReconnectDoesNotExpungeDeletedMessages(t *testing.T) {
	env := newProtocolTestEnv(t)
	fetchOnlyMessage(t, env.durian, nil)
	wireSelect(t, env.other, "INBOX")
	wireStoreFlags(t, env.other, 6, goimap.AddFlags, []string{goimap.DeletedFlag})
	if err := env.durian.client.ReconnectContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	wireSelect(t, env.other, "INBOX")
	if uids := wireSearchAll(t, env.other); len(uids) != 1 || uids[0] != 6 {
		t.Fatalf("reconnect expunged messages marked deleted by another client: %v", uids)
	}
}

func TestProtocolCancellationAndDeadConnectionReturnBoundedly(t *testing.T) {
	t.Run("cancel IDLE", func(t *testing.T) {
		env := newProtocolTestEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan error, 1)
		go func() { done <- env.durian.Watch(ctx, "INBOX", func() {}) }()

		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Watch after cancellation = %v, want context.Canceled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("cancelled Watch did not return within 2s")
		}
	})

	t.Run("dead connection", func(t *testing.T) {
		env := newProtocolTestEnv(t)
		if err := env.server.Close(); err != nil {
			t.Fatalf("close server: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := env.durian.FetchFlags(context.Background(), "INBOX", []backend.RemoteRef{{Folder: "INBOX", ID: "6"}})
			done <- err
		}()

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("FetchFlags on a dead connection unexpectedly succeeded")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("FetchFlags on a dead connection did not return within 2s")
		}
	})
}

type protocolTestEnv struct {
	durian *Backend
	other  *client.Client
	server *server.Server
	hooks  *protocolHooks
}

func newProtocolTestEnv(t *testing.T) *protocolTestEnv {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hooks := &protocolHooks{bodyRequests: make(chan bool, 4)}
	srv := server.New(newProtocolServerBackend(hooks))
	srv.ErrorLog = log.New(io.Discard, "", 0)
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{protocolTestCertificate},
		MinVersion:   tls.VersionTLS12,
	})
	go func() { _ = srv.Serve(tlsListener) }()

	host, portString, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	account := &config.AccountConfig{
		Name:  "wire-test",
		Email: "username",
		Auth:  &config.AuthConfig{Username: "username"},
		IMAP: config.IMAPConfig{
			Host: host,
			Port: port,
			Auth: "password",
		},
	}
	durian, err := New(account)
	if err != nil {
		_ = srv.Close()
		t.Fatalf("create Durian IMAP backend: %v", err)
	}
	other, err := client.DialTLS(listener.Addr().String(), &tls.Config{
		RootCAs:    protocolTestRoots,
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		_ = durian.Close()
		_ = srv.Close()
		t.Fatalf("dial competing wire client: %v", err)
	}
	if err := other.Login("username", "password"); err != nil {
		_ = other.Close()
		_ = durian.Close()
		_ = srv.Close()
		t.Fatalf("login competing wire client: %v", err)
	}

	env := &protocolTestEnv{durian: durian, other: other, server: srv, hooks: hooks}
	t.Cleanup(func() {
		_ = other.Close()
		_ = durian.Close()
		_ = srv.Close()
	})
	return env
}

// protocolServerBackend is a deliberately small disposable mailbox store. It
// implements only the RFC operations exercised below; all command parsing,
// dispatch, response generation and client behavior still cross real TLS IMAP
// sockets through go-imap's server and two independent clients.
type protocolServerBackend struct {
	user *protocolUser
}

func newProtocolServerBackend(hooks *protocolHooks) *protocolServerBackend {
	const raw = "From: sender@example.test\r\n" +
		"To: recipient@example.test\r\n" +
		"Subject: wire test\r\n" +
		"Message-ID: <wire-message@example.test>\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"Hi there :)"
	user := &protocolUser{
		mailboxes: make(map[string]*protocolMailbox),
		hooks:     hooks,
	}
	user.mailboxes["INBOX"] = &protocolMailbox{
		name: "INBOX",
		user: user,
		messages: []protocolMessage{{
			uid:   6,
			date:  time.Unix(1_700_000_000, 0).UTC(),
			flags: []string{goimap.SeenFlag},
			body:  []byte(raw),
		}},
	}
	return &protocolServerBackend{user: user}
}

func (b *protocolServerBackend) Login(_ *goimap.ConnInfo, username, password string) (serverbackend.User, error) {
	if username != "username" || password != "password" {
		return nil, serverbackend.ErrInvalidCredentials
	}
	return b.user, nil
}

type protocolUser struct {
	mu        sync.Mutex
	mailboxes map[string]*protocolMailbox
	hooks     *protocolHooks
}

func (u *protocolUser) Username() string { return "username" }

func (u *protocolUser) ListMailboxes(subscribed bool) ([]serverbackend.Mailbox, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	mailboxes := make([]serverbackend.Mailbox, 0, len(u.mailboxes))
	for _, mailbox := range u.mailboxes {
		if !subscribed || mailbox.subscribed {
			mailboxes = append(mailboxes, mailbox)
		}
	}
	return mailboxes, nil
}

func (u *protocolUser) GetMailbox(name string) (serverbackend.Mailbox, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	mailbox, ok := u.mailboxes[name]
	if !ok {
		return nil, serverbackend.ErrNoSuchMailbox
	}
	return mailbox, nil
}

func (u *protocolUser) CreateMailbox(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, exists := u.mailboxes[name]; exists {
		return serverbackend.ErrMailboxAlreadyExists
	}
	u.mailboxes[name] = &protocolMailbox{name: name, user: u}
	return nil
}

func (u *protocolUser) DeleteMailbox(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if name == "INBOX" {
		return fmt.Errorf("cannot delete INBOX")
	}
	if _, exists := u.mailboxes[name]; !exists {
		return serverbackend.ErrNoSuchMailbox
	}
	delete(u.mailboxes, name)
	return nil
}

func (u *protocolUser) RenameMailbox(existingName, newName string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	mailbox, exists := u.mailboxes[existingName]
	if !exists {
		return serverbackend.ErrNoSuchMailbox
	}
	if _, exists := u.mailboxes[newName]; exists {
		return serverbackend.ErrMailboxAlreadyExists
	}
	delete(u.mailboxes, existingName)
	mailbox.name = newName
	u.mailboxes[newName] = mailbox
	return nil
}

func (u *protocolUser) Logout() error { return nil }

type protocolMessage struct {
	uid   uint32
	date  time.Time
	flags []string
	body  []byte
}

type protocolMailbox struct {
	name       string
	user       *protocolUser
	subscribed bool
	messages   []protocolMessage
}

func (m *protocolMailbox) Name() string { return m.name }

func (m *protocolMailbox) Info() (*goimap.MailboxInfo, error) {
	return &goimap.MailboxInfo{Name: m.name, Delimiter: "/"}, nil
}

func (m *protocolMailbox) Status(items []goimap.StatusItem) (*goimap.MailboxStatus, error) {
	m.user.mu.Lock()
	defer m.user.mu.Unlock()
	status := goimap.NewMailboxStatus(m.name, items)
	status.PermanentFlags = []string{"\\*"}
	status.Flags = []string{goimap.SeenFlag, goimap.FlaggedFlag, goimap.DeletedFlag}
	for _, item := range items {
		switch item {
		case goimap.StatusMessages:
			status.Messages = uint32(len(m.messages))
		case goimap.StatusUidNext:
			status.UidNext = m.nextUIDLocked()
		case goimap.StatusUidValidity:
			status.UidValidity = 1
		}
	}
	return status, nil
}

func (m *protocolMailbox) SetSubscribed(subscribed bool) error {
	m.user.mu.Lock()
	defer m.user.mu.Unlock()
	m.subscribed = subscribed
	return nil
}

func (m *protocolMailbox) Check() error { return nil }

func (m *protocolMailbox) ListMessages(uid bool, seqSet *goimap.SeqSet, items []goimap.FetchItem, ch chan<- *goimap.Message) error {
	defer close(ch)
	for _, item := range items {
		if !strings.HasPrefix(strings.ToUpper(string(item)), "BODY") {
			continue
		}
		if section, err := goimap.ParseBodySectionName(item); err == nil {
			m.user.hooks.bodyRequests <- section.Peek
		}
	}

	m.user.mu.Lock()
	messages := make([]*goimap.Message, 0, len(m.messages))
	for i := range m.messages {
		message := &m.messages[i]
		seqNum := uint32(i + 1)
		id := seqNum
		if uid {
			id = message.uid
		}
		if !seqSet.Contains(id) {
			continue
		}
		fetched := goimap.NewMessage(seqNum, items)
		for _, item := range items {
			switch item {
			case goimap.FetchUid:
				fetched.Uid = message.uid
			case goimap.FetchFlags:
				fetched.Flags = append([]string(nil), message.flags...)
			case goimap.FetchInternalDate:
				fetched.InternalDate = message.date
			case goimap.FetchRFC822Size:
				fetched.Size = uint32(len(message.body))
			default:
				section, err := goimap.ParseBodySectionName(item)
				if err == nil {
					fetched.Body[section] = bytes.NewReader(append([]byte(nil), message.body...))
				}
			}
		}
		messages = append(messages, fetched)
	}
	m.user.mu.Unlock()
	for _, message := range messages {
		ch <- message
	}
	return nil
}

func (m *protocolMailbox) SearchMessages(uid bool, criteria *goimap.SearchCriteria) ([]uint32, error) {
	m.user.mu.Lock()
	var ids []uint32
	for i := range m.messages {
		message := &m.messages[i]
		seqNum := uint32(i + 1)
		id := seqNum
		if uid {
			id = message.uid
		}
		if messageMatches(message, seqNum, criteria) {
			ids = append(ids, id)
		}
	}
	m.user.mu.Unlock()
	if searched, release := m.user.hooks.takeSearchBarrier(); searched != nil {
		close(searched)
		<-release
	}
	return ids, nil
}

func (m *protocolMailbox) CreateMessage(flags []string, date time.Time, body goimap.Literal) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if date.IsZero() {
		date = time.Now()
	}
	m.user.mu.Lock()
	defer m.user.mu.Unlock()
	m.messages = append(m.messages, protocolMessage{
		uid: m.nextUIDLocked(), date: date, flags: append([]string(nil), flags...), body: data,
	})
	return nil
}

func (m *protocolMailbox) UpdateMessagesFlags(uid bool, seqSet *goimap.SeqSet, op goimap.FlagsOp, flags []string) error {
	m.user.mu.Lock()
	defer m.user.mu.Unlock()
	m.updateFlagsLocked(uid, seqSet, op, flags)
	return nil
}

func (m *protocolMailbox) CopyMessages(uid bool, seqSet *goimap.SeqSet, dest string) error {
	m.user.mu.Lock()
	defer m.user.mu.Unlock()
	return m.copyLocked(uid, seqSet, dest)
}

func (m *protocolMailbox) Expunge() error {
	m.user.mu.Lock()
	defer m.user.mu.Unlock()
	m.expungeLocked()
	return nil
}

func (m *protocolMailbox) MoveMessages(uid bool, seqSet *goimap.SeqSet, dest string) error {
	m.user.mu.Lock()
	defer m.user.mu.Unlock()
	if err := m.copyLocked(uid, seqSet, dest); err != nil {
		return err
	}
	m.updateFlagsLocked(uid, seqSet, goimap.AddFlags, []string{goimap.DeletedFlag})
	m.expungeLocked()
	return nil
}

func (m *protocolMailbox) nextUIDLocked() uint32 {
	var highest uint32
	for i := range m.messages {
		if m.messages[i].uid > highest {
			highest = m.messages[i].uid
		}
	}
	return highest + 1
}

func (m *protocolMailbox) updateFlagsLocked(uid bool, seqSet *goimap.SeqSet, op goimap.FlagsOp, flags []string) {
	for i := range m.messages {
		id := uint32(i + 1)
		if uid {
			id = m.messages[i].uid
		}
		if !seqSet.Contains(id) {
			continue
		}
		switch op {
		case goimap.SetFlags:
			m.messages[i].flags = append([]string(nil), flags...)
		case goimap.AddFlags:
			for _, flag := range flags {
				if !containsFlag(m.messages[i].flags, flag) {
					m.messages[i].flags = append(m.messages[i].flags, flag)
				}
			}
		case goimap.RemoveFlags:
			kept := m.messages[i].flags[:0]
			for _, existing := range m.messages[i].flags {
				if !containsFlag(flags, existing) {
					kept = append(kept, existing)
				}
			}
			m.messages[i].flags = kept
		}
	}
}

func (m *protocolMailbox) copyLocked(uid bool, seqSet *goimap.SeqSet, dest string) error {
	destination, ok := m.user.mailboxes[dest]
	if !ok {
		return serverbackend.ErrNoSuchMailbox
	}
	for i := range m.messages {
		id := uint32(i + 1)
		if uid {
			id = m.messages[i].uid
		}
		if !seqSet.Contains(id) {
			continue
		}
		copied := m.messages[i]
		copied.uid = destination.nextUIDLocked()
		copied.flags = append([]string(nil), copied.flags...)
		copied.body = append([]byte(nil), copied.body...)
		destination.messages = append(destination.messages, copied)
	}
	return nil
}

func (m *protocolMailbox) expungeLocked() {
	kept := m.messages[:0]
	for _, message := range m.messages {
		if !containsFlag(message.flags, goimap.DeletedFlag) {
			kept = append(kept, message)
		}
	}
	m.messages = kept
}

func messageMatches(message *protocolMessage, seqNum uint32, criteria *goimap.SearchCriteria) bool {
	if criteria.SeqNum != nil && !criteria.SeqNum.Contains(seqNum) {
		return false
	}
	if criteria.Uid != nil && !criteria.Uid.Contains(message.uid) {
		return false
	}
	for _, flag := range criteria.WithFlags {
		if !containsFlag(message.flags, flag) {
			return false
		}
	}
	for _, flag := range criteria.WithoutFlags {
		if containsFlag(message.flags, flag) {
			return false
		}
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(message.body))
	if err != nil {
		return false
	}
	for name, expectedValues := range criteria.Header {
		actual := parsed.Header.Get(name)
		for _, expected := range expectedValues {
			if !strings.Contains(strings.ToLower(actual), strings.ToLower(expected)) {
				return false
			}
		}
	}
	return true
}

type protocolHooks struct {
	mu            sync.Mutex
	searched      chan struct{}
	releaseSearch chan struct{}
	bodyRequests  chan bool
}

func (h *protocolHooks) armSearchBarrier() (<-chan struct{}, chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.searched = make(chan struct{})
	h.releaseSearch = make(chan struct{})
	return h.searched, h.releaseSearch
}

func (h *protocolHooks) takeSearchBarrier() (chan struct{}, <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	searched, release := h.searched, h.releaseSearch
	h.searched = nil
	h.releaseSearch = nil
	return searched, release
}

func fetchOnlyMessage(t *testing.T, b *Backend, cursor backend.Cursor) (backend.Message, backend.Cursor) {
	t.Helper()
	result, err := b.FetchMessages(context.Background(), "INBOX", cursor, 0)
	if err != nil {
		t.Fatalf("FetchMessages: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("FetchMessages returned %d messages, want one", len(result.Messages))
	}
	return result.Messages[0], result.Cursor
}

func wireSelect(t *testing.T, c *client.Client, mailbox string) {
	t.Helper()
	if _, err := c.Select(mailbox, false); err != nil {
		t.Fatalf("wire SELECT %s: %v", mailbox, err)
	}
}

func wireStoreFlags(t *testing.T, c *client.Client, uid uint32, op goimap.FlagsOp, flags []string) {
	t.Helper()
	seqSet := new(goimap.SeqSet)
	seqSet.AddNum(uid)
	values := make([]interface{}, len(flags))
	for i, flag := range flags {
		values[i] = flag
	}
	messages := make(chan *goimap.Message, 1)
	if err := c.UidStore(seqSet, goimap.FormatFlagsOp(op, true), values, messages); err != nil {
		t.Fatalf("wire UID STORE %d: %v", uid, err)
	}
	for range messages {
	}
}

func wireFetchFlags(t *testing.T, c *client.Client, uid uint32) []string {
	t.Helper()
	seqSet := new(goimap.SeqSet)
	seqSet.AddNum(uid)
	messages := make(chan *goimap.Message, 1)
	if err := c.UidFetch(seqSet, []goimap.FetchItem{goimap.FetchUid, goimap.FetchFlags}, messages); err != nil {
		t.Fatalf("wire UID FETCH FLAGS %d: %v", uid, err)
	}
	for msg := range messages {
		return msg.Flags
	}
	t.Fatalf("wire UID FETCH FLAGS %d returned no message", uid)
	return nil
}

func wireSearchAll(t *testing.T, c *client.Client) []uint32 {
	t.Helper()
	uids, err := c.UidSearch(goimap.NewSearchCriteria())
	if err != nil {
		t.Fatalf("wire UID SEARCH ALL: %v", err)
	}
	return uids
}

func containsFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if strings.EqualFold(flag, want) {
			return true
		}
	}
	return false
}

func newProtocolTestCertificate() (tls.Certificate, *x509.CertPool, []byte, error) {
	now := time.Now()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	root := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Durian protocol test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "Durian protocol test IMAP"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, rootCert, &serverKey.PublicKey, rootKey)
	if err != nil {
		return tls.Certificate{}, nil, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(rootCert)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	return tls.Certificate{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: serverKey}, pool, pemBytes, nil
}
