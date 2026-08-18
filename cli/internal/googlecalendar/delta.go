// Incremental download for Google Calendar: events.list driven by a syncToken
// instead of a full re-read every run.
//
// The protocol has one rule that makes or breaks it: an incremental call must
// repeat EXACTLY the parameters of the full call that produced the token, and
// must not add the ones Google forbids alongside syncToken (timeMin, timeMax,
// updatedMin, orderBy, q, iCalUID, and the extended-property filters). A
// mismatch is answered with 400 Bad Request, not the 410 that means "resync",
// so it reads as a client bug rather than an expired token. Both the full and
// the incremental call are therefore built from the same one place —
// baseQuery — and the shape is pinned by DeltaParamFingerprint, which the
// engine compares against the fingerprint stored with the token.

package googlecalendar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/julion2/durian/cli/internal/calendarsync"
)

// Client satisfies the optional incremental seam of the sync engine.
var _ calendarsync.DeltaCalendarProvider = (*Client)(nil)

// deltaParamFingerprint pins the query shape the sync tokens are bound to.
// Bump it whenever baseQuery changes: a token minted under the old shape would
// otherwise be replayed against the new one, which Google rejects with a 400.
const deltaParamFingerprint = "google/v1;singleEvents=false;showDeleted=true;maxResults=250"

// baseQuery is the parameter set shared by the full and the incremental round.
// Nothing may be added to it per-call except pageToken and syncToken.
func baseQuery() url.Values {
	q := url.Values{}
	q.Set("maxResults", strconv.Itoa(pageSize))
	q.Set("singleEvents", "false")
	q.Set("showDeleted", "true")
	return q
}

// DeltaParamFingerprint implements the engine seam.
func (c *Client) DeltaParamFingerprint() string { return deltaParamFingerprint }

// syncPage is one page of a sync-token driven events list.
type syncPage struct {
	Items         []googleEvent `json:"items"`
	NextPageToken string        `json:"nextPageToken"`
	NextSyncToken string        `json:"nextSyncToken"`
}

// FetchMasterEventsDelta returns the changes since cursor, or a complete round
// when cursor is empty.
//
// An expired or invalidated token comes back as 410 with reason
// fullSyncRequired. Google invalidates tokens for reasons a client cannot
// avoid — plain expiry, but also any ACL change on the calendar, which makes
// it routine on shared and subscribed calendars — so it is handled here rather
// than surfaced: the token is dropped and one full round runs in its place,
// reported as Reset so the engine replaces the mirror instead of merging into
// it.
func (c *Client) FetchMasterEventsDelta(ctx context.Context, calendarID, cursor string) (calendarsync.DeltaResult, error) {
	result, err := c.fetchDeltaRound(ctx, calendarID, cursor)
	if err == nil {
		return result, nil
	}
	if cursor == "" || !isSyncTokenExpired(err) {
		return calendarsync.DeltaResult{}, err
	}

	slog.Info("Sync token no longer valid, running a full round", "module", "GOOGLECAL",
		"calendar", calendarID)
	return c.fetchDeltaRound(ctx, calendarID, "")
}

// fetchDeltaRound runs one paged round and classifies what came back. A round
// with no cursor is a full one, whose items are the complete remote set.
func (c *Client) fetchDeltaRound(ctx context.Context, calendarID, cursor string) (calendarsync.DeltaResult, error) {
	base := c.baseURL + "/calendars/" + url.PathEscape(calendarID) + "/events"
	q := baseQuery()
	if cursor != "" {
		q.Set("syncToken", cursor)
	}

	result := calendarsync.DeltaResult{
		ParamFingerprint: deltaParamFingerprint,
		Reset:            cursor == "",
	}
	// masterIDs of the masters this round delivered, so an instance can be
	// folded into its master directly when both arrived together, and reported
	// as an occurrence change when only the instance moved.
	inRound := make(map[string]int)

	for {
		var page syncPage
		if err := c.doJSON(ctx, base+"?"+q.Encode(), nil, &page); err != nil {
			return calendarsync.DeltaResult{}, fmt.Errorf("failed to fetch calendar changes for %s: %w", calendarID, err)
		}

		for _, g := range page.Items {
			switch {
			case g.RecurringEventID != "":
				c.classifyInstance(&result, inRound, g)
			case g.Status == "cancelled":
				// A cancelled master is the tombstone the whole incremental
				// path depends on: without it a deletion would be invisible,
				// since a change feed never mentions what is simply absent.
				result.RemovedIDs = append(result.RemovedIDs, g.ID)
			default:
				if ev, ok := c.eventFromGoogle(g); ok {
					inRound[g.ID] = len(result.ChangedMasters)
					result.ChangedMasters = append(result.ChangedMasters, ev)
				}
			}
		}

		// The token appears on the LAST page only. Paging all the way to it is
		// not optional: stopping early would leave the round unsettled, and
		// recording no cursor is the only honest outcome then — the next run
		// repeats the round rather than skipping the unreported changes.
		if page.NextPageToken == "" {
			result.Cursor = page.NextSyncToken
			break
		}
		q.Set("pageToken", page.NextPageToken)
	}

	// Occurrence changes whose master DID arrive in this round belong on that
	// master directly; only the rest travel as OverrideChange for the mirror
	// to apply to the master it already holds.
	result = foldInRoundOverrides(result, inRound)

	slog.Debug("Fetched calendar changes", "module", "GOOGLECAL", "calendar", calendarID,
		"masters", len(result.ChangedMasters), "occurrences", len(result.ChangedOverrides),
		"removed", len(result.RemovedIDs), "full", result.Reset, "settled", result.Cursor != "")
	return result, nil
}

// classifyInstance turns one series instance into an occurrence change. The
// original start is the instance's identity within its series and survives the
// occurrence being moved, so it — never the current start — is the key.
func (c *Client) classifyInstance(result *calendarsync.DeltaResult, inRound map[string]int, g googleEvent) {
	original, _, err := parseGoogleTime(g.OriginalStartTime)
	if err != nil {
		slog.Warn("Dropping series instance without a usable original start",
			"module", "GOOGLECAL", "id", g.ID, "err", err)
		return
	}

	change := calendarsync.OverrideChange{
		MasterID:     g.RecurringEventID,
		RecurrenceID: original,
	}
	if g.Status == "cancelled" {
		change.Cancelled = true
		result.ChangedOverrides = append(result.ChangedOverrides, change)
		return
	}

	ev, ok := c.eventFromGoogle(g)
	if !ok {
		return
	}
	change.Event = ev
	result.ChangedOverrides = append(result.ChangedOverrides, change)
}

// foldInRoundOverrides moves every occurrence change whose master is part of
// the same round onto that master, and returns the result with only the
// remaining ones.
//
// Both destinations end up in the same place, but the distinction matters on a
// FULL round: there the ChangedMasters ARE the complete remote set, so an
// occurrence left outside them would be applied to a mirror the engine is
// about to replace, and silently lost.
func foldInRoundOverrides(result calendarsync.DeltaResult, inRound map[string]int) calendarsync.DeltaResult {
	remaining := result.ChangedOverrides[:0:0]
	for _, oc := range result.ChangedOverrides {
		idx, together := inRound[oc.MasterID]
		if !together {
			remaining = append(remaining, oc)
			continue
		}
		master := &result.ChangedMasters[idx]
		if oc.Cancelled {
			master.ExceptionDates = append(master.ExceptionDates, oc.RecurrenceID.UTC())
			continue
		}
		override := oc.Event
		override.RecurrenceID = oc.RecurrenceID.UTC()
		override.Recurrence = nil
		override.ExceptionDates = nil
		master.Overrides = append(master.Overrides, override)
	}
	result.ChangedOverrides = remaining
	return result
}

// isSyncTokenExpired reports whether err is Google's "your token is no longer
// usable, start over" answer: HTTP 410, reason fullSyncRequired.
//
// A 400 is deliberately NOT folded in here. It means the parameters did not
// match the ones the token was minted with — a client bug that a silent
// full-sync retry would hide, and that would then repeat on every single run.
func isSyncTokenExpired(err error) bool {
	var se *statusError
	if !errors.As(err, &se) {
		return false
	}
	// The STATUS is what decides. Matching the reason string on its own would
	// swallow a 400 whose message happens to mention the sync token — exactly
	// the client bug this must not hide, and one that would then trigger a
	// silent full download on every single run instead of failing once.
	if se.status != http.StatusGone {
		return false
	}
	// A 410 can also mean the calendar itself is gone. Retrying that as a full
	// round fails again and reports the real error, so the reason is only used
	// to keep the log honest, not to gate the recovery.
	if !strings.Contains(se.body, "fullSyncRequired") {
		slog.Debug("Got 410 without fullSyncRequired, retrying as a full round anyway",
			"module", "GOOGLECAL", "body", se.body)
	}
	return true
}
