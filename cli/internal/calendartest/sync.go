package calendartest

import (
	"context"
	"fmt"
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
