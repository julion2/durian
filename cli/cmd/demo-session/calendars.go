package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/calendartest"
	"github.com/julion2/durian/cli/internal/config"
	"github.com/julion2/durian/cli/internal/demotest"
	"github.com/julion2/durian/cli/internal/googlecalendar"
	"github.com/julion2/durian/cli/internal/graphcalendar"
	"github.com/julion2/durian/cli/internal/handler"
)

type calendarSession struct {
	prefix            string
	google, microsoft *pinnedProvider
}

// One source per provider for the ENTIRE session: mail, metadata verification,
// sync and writes all share the same serialized Microsoft refresh/persistence.
func newSessionFromEnv(ctx context.Context, calendars bool) (*demotest.Mail, *calendarSession, error) {
	if calendars && (os.Getenv("DURIAN_CALENDAR_LIVE_TEST") != "1" || os.Getenv("DURIAN_GOOGLE_TEST_CALENDAR_ID") == "" || os.Getenv("DURIAN_MS_TEST_CALENDAR_ID") == "") {
		return nil, nil, errors.New("calendar opt-in and two exact existing calendar IDs required")
	}
	g, err := calendartest.NewMailTokenSource(calendartest.Google)
	if err != nil {
		return nil, nil, err
	}
	m, err := calendartest.NewMailTokenSource(calendartest.Microsoft)
	if err != nil {
		return nil, nil, err
	}
	mail, err := demotest.New(ctx, demotest.Options{LiveOptIn: os.Getenv("DURIAN_MAIL_LIVE_TEST") == "1",
		GoogleEmail: os.Getenv("DURIAN_GOOGLE_TEST_EMAIL"), MicrosoftEmail: os.Getenv("DURIAN_MS_TEST_EMAIL"),
		GoogleToken: g.Token, MicrosoftToken: m.Token})
	if err != nil || !calendars {
		return mail, nil, err
	}
	c, err := newCalendarSession(ctx, g, m, os.Getenv("DURIAN_GOOGLE_TEST_CALENDAR_ID"), os.Getenv("DURIAN_MS_TEST_CALENDAR_ID"))
	return mail, c, err
}

func newCalendarSession(ctx context.Context, g, m *calendartest.TokenSource, googleID, graphID string) (*calendarSession, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// Never call Ensure*: the session is authorized for EXISTING calendars only.
	if err := calendartest.ExistingGoogleCalendar(ctx, g, googleID); err != nil {
		return nil, err
	}
	pin, err := calendartest.ExistingGraphCalendar(ctx, m, graphID)
	if err != nil {
		return nil, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, errors.New("generate calendar run identity")
	}
	prefix := "Durian Test " + hex.EncodeToString(nonce[:]) + " "
	gg, err := calendartest.NewGoogleGuard(googleID, prefix, g)
	if err != nil {
		return nil, err
	}
	mg, err := calendartest.NewGraphGuard(pin, prefix)
	if err != nil {
		return nil, err
	}
	// Providers receive no credential; guards obtain fresh tokens from the
	// shared sources and replace Authorization only after validating requests.
	gp := googlecalendar.NewWithToken(os.Getenv("DURIAN_GOOGLE_TEST_EMAIL"), calendartest.GoogleBase, "guard-supplied", gg.HTTPClient())
	mp := graphcalendar.NewWithToken(os.Getenv("DURIAN_MS_TEST_EMAIL"), calendartest.GraphBase, "guard-supplied", mg.HTTPClient())
	return &calendarSession{prefix: prefix,
		google:    &pinnedProvider{CalendarProvider: gp, id: googleID, prefix: prefix},
		microsoft: &pinnedProvider{CalendarProvider: mp, id: graphID, prefix: prefix, graph: true}}, nil
}

func (s *calendarSession) providerFor(a *config.AccountConfig) (calendarsync.CalendarProvider, error) {
	if a != nil && a.OAuth != nil && !a.IsDelegatedMailbox() {
		if a.OAuth.Provider == "google" && a.Email == s.google.Owner() {
			return s.google, nil
		}
		if a.OAuth.Provider == "microsoft" && a.Email == s.microsoft.Owner() {
			return s.microsoft, nil
		}
	}
	return nil, errors.New("calendar session refuses unverified account")
}

// Deliberately does NOT expose the optional delta interface: guarded snapshots
// are full reads of only the pinned collection. No account-wide discovery.
type pinnedProvider struct {
	calendarsync.CalendarProvider
	id, prefix string
	graph      bool
}

func (p *pinnedProvider) ListCalendars(context.Context) ([]calendarsync.Calendar, error) {
	return []calendarsync.Calendar{{ID: p.id, Name: "Durian Test"}}, nil
}

func (p *pinnedProvider) writable(id string, ev calendar.Event) error {
	if id != p.id || !strings.HasPrefix(ev.Subject, p.prefix) || len(ev.Attendees) != 0 || ev.RequestOnlineMeeting || ev.IsOnlineMeeting || ev.OnlineMeetingURL != "" {
		return errors.New("session permits only run-prefixed appointments in its pinned calendar; attendees and online meetings unsupported")
	}
	return nil
}

func (p *pinnedProvider) CreateEvent(ctx context.Context, id string, ev calendar.Event, opts calendarsync.CreateOptions) (calendar.Event, error) {
	if err := p.writable(id, ev); err != nil {
		return calendar.Event{}, err
	}
	if opts.NotifyAttendees || opts.RequestOnlineMeeting || opts.IncludeAttendees {
		return calendar.Event{}, errors.New("session refuses invitations")
	}
	// Existing guards bind idempotency to the full run-prefixed subject. Keep
	// the production planner/operation policy intact, adapting only this seam.
	opts.IdempotencyKey = ev.Subject
	if p.graph {
		opts.IdempotencyKey = calendartest.GraphTransactionID(ev.Subject)
	}
	return p.CalendarProvider.CreateEvent(ctx, id, ev, opts)
}

func (p *pinnedProvider) UpdateEvent(ctx context.Context, id, eventID string, spec calendarsync.UpdateSpec) error {
	if err := p.writable(id, spec.Event); err != nil {
		return err
	}
	if spec.NotifyAttendees || spec.IncludeAttendees || spec.AttendeesOnly {
		return errors.New("session refuses attendee changes")
	}
	return p.CalendarProvider.UpdateEvent(ctx, id, eventID, spec)
}

func (p *pinnedProvider) DeleteEvent(ctx context.Context, id, eventID, etag string, notify bool) error {
	if id != p.id || notify {
		return errors.New("session refuses unscoped or notifying delete")
	}
	return p.CalendarProvider.DeleteEvent(ctx, id, eventID, etag, false)
}

func (p *pinnedProvider) RespondToEvent(context.Context, string, string, calendar.OwnerResp, bool, string) error {
	return errors.New("session does not support RSVP or invitation changes")
}

// policy rejects unsupported UI actions BEFORE a local-first handler could
// return success (including attendee lists the normal update route preserves).
// The provider guards remain the final independent boundary for all writes.
func (s *calendarSession) policy(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return s.checkAction(cfg, next) }
}

func (s *calendarSession) localWriteAllowed(cfg *config.Config, account, uid string, create bool) bool {
	a, err := cfg.GetAccountByIdentifier(account)
	if err != nil {
		return false
	}
	p, err := s.providerFor(a)
	if err != nil {
		return false
	}
	if uid == "" {
		return create
	}
	dir := filepath.Join(config.CalendarBaseDir(cfg, ""), a.CalendarDir())
	cols, err := calendar.CollectionsUnderFor(dir, account, a.Email)
	if err != nil {
		return false
	}
	_, ev, _, err := calendar.ResolveUIDIn(cols, uid, "Durian Test")
	if errors.Is(err, calendar.ErrEventNotFound) {
		return create
	}
	if err != nil {
		return false
	}
	pinned := p.(*pinnedProvider) // providerFor returns only this session's two pins
	return pinned.writable(pinned.id, ev) == nil
}

func (s *calendarSession) checkAction(cfg *config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/calendars") || r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodDelete {
			q := r.URL.Query()
			if q.Get("calendar") != "Durian Test" || !s.localWriteAllowed(cfg, q.Get("account"), q.Get("uid"), false) {
				http.Error(w, "session permits deleting only its own run-prefixed Durian Test appointments by UID", 400)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		if err != nil || len(data) > 1<<20 {
			http.Error(w, "invalid session calendar action", 400)
			return
		}
		r.Body.Close()
		var req struct {
			Account, Calendar, UID, Subject, Operation string
			Attendees                                  []string
			Online                                     bool `json:"request_online_meeting"`
		}
		// GPUI always sends replace_attendees:true, attendees:[] even for
		// appointments. That is a no-op ONLY because localWriteAllowed also
		// rejects any stored attendees. Never accept an actual add/remove.
		if json.Unmarshal(data, &req) != nil || req.Calendar != "Durian Test" || len(req.Attendees) > 0 || req.Online || req.Operation == "rsvp" || r.URL.Path == "/api/v1/calendars/rsvp" || (r.Method == http.MethodPut && !strings.HasPrefix(req.Subject, s.prefix)) {
			http.Error(w, "session supports only run-prefixed Durian Test appointments; no attendees, RSVP or online meetings", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPut && !s.localWriteAllowed(cfg, req.Account, req.UID, true) {
			http.Error(w, "session cannot edit existing events from another run", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		next.ServeHTTP(w, r)
	})
}

// The periodic/initial path NEVER sees remote mutations, even if a GUI save
// was interrupted between its local PUT and its explicit sync request.
func (s *calendarSession) download(parent context.Context, cfg *config.Config, a *config.AccountConfig, hub *handler.EventHub) error {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	p, err := s.providerFor(a)
	if err != nil {
		return err
	}
	dir := filepath.Join(config.CalendarBaseDir(cfg, ""), a.CalendarDir())
	release, ok, err := calendarsync.AcquireRunLock(dir)
	if err != nil {
		return err
	}
	if !ok {
		return handler.ErrCalendarSyncBusy
	}
	defer release()
	store := calendarsync.NewFileStateStore(dir)
	state, err := store.Load()
	if err != nil {
		return err
	}
	state, _, err = calendarsync.BindMailbox(dir, state, a.Email, false, true)
	if err != nil {
		return err
	}
	mirrors := calendarsync.NewFileMirrorStore(dir)
	mirror, err := mirrors.Load()
	if err != nil {
		return err
	}
	plans, err := calendarsync.PlanAll(ctx, p, dir, a.CalendarInclude(), state, mirror)
	if err != nil {
		return err
	}
	filtered, _ := calendarsync.FilterDownloadOnly(plans)
	stats, applyErr := calendarsync.ApplyAll(ctx, p, state, filtered, calendarsync.SyncOptions{})
	if err := store.Save(state); err != nil {
		return errors.Join(applyErr, err)
	}
	// Do not advance a partially applied cursor.
	if applyErr != nil {
		return applyErr
	}
	changed, cacheErr := calendarsync.RefreshOccurrences(ctx, p, dir, state, filtered, time.Now())
	if err := mirrors.Save(mirror); err != nil {
		return errors.Join(cacheErr, err)
	}
	if stats.Failed > 0 || stats.Skipped > 0 {
		return errors.New("calendar download incomplete")
	}
	if changed || stats.Downloaded+stats.Pruned > 0 {
		hub.BroadcastCalendar(handler.CalendarUpdatedEvent{Account: a.GetAliasOrName(), Downloaded: stats.Downloaded, Pruned: stats.Pruned})
	}
	return cacheErr
}
