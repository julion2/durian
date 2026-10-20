package calendartest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/julion2/durian/cli/internal/calendar"
	"github.com/julion2/durian/cli/internal/calendarsync"
)

// SyncOccurrences exercises the production plan/apply/state/cache/read path in
// an isolated vdir. Only the already pinned calendar is planned. This read-back
// step must never upload local changes or send invitations.
func SyncOccurrences(ctx context.Context, p calendarsync.CalendarProvider, id, dir string, from, to time.Time) ([]calendar.Event, error) {
	store := calendarsync.NewFileStateStore(dir)
	state, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("load integration sync state")
	}
	state.Mailbox = p.Owner()
	cal := calendarsync.Calendar{ID: id, Name: "Durian Test"}
	calDir := filepath.Join(dir, "Durian Test")
	plan, err := calendarsync.Plan(ctx, p, cal, calDir, state.Calendars[id])
	if err != nil {
		return nil, fmt.Errorf("plan integration read-back")
	}
	for _, action := range plan.Actions {
		switch action.Kind {
		case calendarsync.ActionDownloadNew, calendarsync.ActionDownloadUpdate, calendarsync.ActionPruneLocal:
		default:
			return nil, fmt.Errorf("integration read-back unexpectedly planned a non-download action")
		}
	}
	plans := []calendarsync.CalendarPlan{plan}
	if _, err := calendarsync.ApplyAll(ctx, p, state, plans, calendarsync.SyncOptions{}); err != nil {
		return nil, fmt.Errorf("apply integration read-back")
	}
	if err := store.Save(state); err != nil {
		return nil, fmt.Errorf("save integration sync state")
	}
	// A fixed clock makes live and offline issue exactly the same cache query.
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if _, err := calendarsync.RefreshOccurrences(ctx, p, dir, state, plans, now); err != nil {
		return nil, fmt.Errorf("refresh integration occurrences")
	}
	cals, err := calendar.ReadCollections([]calendar.Collection{{Name: cal.Name, Dir: calDir, Account: "demo", Owner: p.Owner()}})
	if err != nil || len(cals) != 1 {
		return nil, fmt.Errorf("read integration collection")
	}
	return calendarsync.ReadOccurrences(cals[0], from, to), nil
}

// UploadAuthoredAppointment exercises the real ICS → planner → provider →
// settled read-back path, with no mail recipients. key binds the planner's
// random transaction key to the live guard's run identity for repeatable
// recordings. The returned ID permits exact cleanup even if persistence fails.
func UploadAuthoredAppointment(ctx context.Context, p calendarsync.CalendarProvider, id, dir, key string, ev calendar.Event) (string, error) {
	if ev.ICalUID == "" || len(ev.Attendees) != 0 || ev.Organizer != nil || ev.RequestOnlineMeeting {
		return "", fmt.Errorf("authored integration case requires a UID and an attendee-less appointment")
	}
	calDir := filepath.Join(dir, "Durian Test")
	if err := os.MkdirAll(calDir, 0700); err != nil {
		return "", fmt.Errorf("create integration calendar directory")
	}
	data, err := calendar.EventToICal(ev)
	if err != nil {
		return "", fmt.Errorf("encode authored integration event")
	}
	if err := calendarsync.WriteFileAtomic(filepath.Join(calDir, calendar.EventFileName(ev.ICalUID)), data, 0600); err != nil {
		return "", fmt.Errorf("write authored integration event")
	}
	store := calendarsync.NewFileStateStore(dir)
	state, err := store.Load()
	if err != nil {
		return "", fmt.Errorf("load authored integration state")
	}
	state.Mailbox = p.Owner()
	plan, err := calendarsync.Plan(ctx, p, calendarsync.Calendar{ID: id, Name: "Durian Test"}, calDir, state.Calendars[id])
	if err != nil {
		return "", fmt.Errorf("plan authored integration event")
	}
	plans, found := calendarsync.FilterEvent([]calendarsync.CalendarPlan{plan}, "Durian Test", ev.ICalUID)
	if !found || len(plans) != 1 || len(plans[0].Actions) != 1 || plans[0].Actions[0].Kind != calendarsync.ActionUploadCreate {
		return "", fmt.Errorf("authored integration case did not plan exactly one create")
	}
	observed := &authoredCreate{CalendarProvider: p, key: key}
	stats, applyErr := calendarsync.ApplyAll(ctx, observed, state, plans, calendarsync.SyncOptions{})
	if err := store.Save(state); err != nil {
		return observed.id, fmt.Errorf("persist authored integration state")
	}
	if applyErr != nil || stats.Uploaded != 1 || stats.Skipped != 0 || stats.Failed != 0 {
		return observed.id, fmt.Errorf("authored integration create did not converge")
	}
	return observed.id, nil
}

type authoredCreate struct {
	calendarsync.CalendarProvider
	key, id string
}

func (p *authoredCreate) CreateEvent(ctx context.Context, cal string, ev calendar.Event, opts calendarsync.CreateOptions) (calendar.Event, error) {
	if opts.IncludeAttendees || opts.NotifyAttendees || opts.RequestOnlineMeeting || len(ev.Attendees) != 0 {
		return calendar.Event{}, fmt.Errorf("authored integration case unexpectedly requested invitations")
	}
	opts.IdempotencyKey = p.key
	created, err := p.CalendarProvider.CreateEvent(ctx, cal, ev, opts)
	p.id = created.ID
	return created, err
}
