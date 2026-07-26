// Two-way calendar sync engine (vdirsyncer model), split into two phases so
// the CLI command can show the full plan and ask for confirmation before any
// remote mutation happens:
//
//   - Plan / PlanAll enumerate both sides of a calendar — remote master
//     events via FetchMasterEvents (keyed by UID = iCalUID, identity =
//     eventContentHash of the meaningful fields; the Graph changeKey is NOT a
//     usable etag, it churns without content changes) and local .ics files in
//     the calendar dir (keyed by their parsed UID, identity = SHA-256 of the
//     file bytes) — and diff both against the persisted CalendarStatus to
//     classify every UID into an Action. Planning mutates NOTHING: no file
//     writes, no remote writes, no status changes.
//   - Apply / ApplyAll execute a plan: local writes (download, prune, adopt),
//     remote writes (create, update, delete) and conflict resolution,
//     updating the status as they go. Apply performs exactly what the plan
//     says — the confirmation gate lives in the command, before Apply is
//     called (or is skipped with --yes). With SyncOptions.DryRun, Apply only
//     counts what it would do.
//
// Decision matrix ("changed" is content-hash based; the owner's own RSVP is
// excluded from content, see below):
//
//	not tracked, remote only            -> DownloadNew
//	not tracked, local only             -> UploadCreate
//	not tracked, both present, equal    -> Adopt (record status, no write)
//	not tracked, both present, differ   -> DownloadUpdate (remote wins on first sight)
//	tracked, only remote changed        -> DownloadUpdate
//	tracked, only local changed         -> UploadUpdate
//	tracked, both changed               -> Conflict
//	tracked, remote deleted, local same -> PruneLocal
//	tracked, local deleted, remote same -> DeleteRemote (routed: organizer
//	  cancels via DELETE, a mere attendee declines via POST /decline, a plain
//	  appointment is deleted silently)
//	tracked, deleted one side + changed
//	  on the other                      -> Conflict
//	tracked, deleted both sides         -> DropStatus
//
// Owner-RSVP sub-matrix (only for TRACKED pairs whose content is unchanged on
// both sides; L = owner response parsed from the local file, B = tracked
// baseline, R = remote owner response; a local edit that only touches the
// owner's own PARTSTAT — or pure formatting — does not count as a content
// change):
//
//	owner is organizer                  -> no RSVP action ever (rebaseline only)
//	L == B, R == B                      -> no action
//	L != B, L == R                      -> Rsvp rebaseline (idempotency guard,
//	                                       NO Graph call)
//	L != B, L != R, L != None           -> Rsvp (POST accept/decline/
//	                                       tentativelyAccept, sendResponse per
//	                                       SilentRSVP)
//	L != B, L != R, L == None           -> no Graph action (a file cannot
//	                                       express "un-respond"; also covers
//	                                       files that lack the owner's
//	                                       ATTENDEE line entirely)
//	L == B, R != B                      -> DownloadUpdate (responded elsewhere:
//	                                       refresh local rendering + baseline)
//
// First-sight (!tracked) NEVER yields a notifying action or an Rsvp — only
// Download/Adopt/UploadCreate. "Remote wins on first sight" is a deliberate
// first-sync convergence choice: when an untracked UID exists on both sides
// with differing content there is no baseline to diff against, so the local
// file is overwritten with the remote rendering and the pair is tracked from
// there.
//
// Scheduling safety rails (a bug here emails real people):
//
//   - Creates carry a client-generated transactionId, so a retried POST can
//     never produce a duplicate event or a second invitation wave.
//   - Updates/deletes send the planned changeKey as If-Match; a 412 (remote
//     changed since planning) skips that action — counted in Stats.Skipped —
//     and the next run re-plans from fresh state instead of clobbering.
//   - Attendees are only uploaded for meetings the OWNER organizes (role
//     gate); for meetings the owner merely attends, attendee changes are
//     never pushed and a local deletion is routed as a decline.
//   - A conflict must never cancel+re-invite: the "local wins, remote
//     deleted" resolution refuses to re-create an event that has attendees
//     (logged + counted as skipped) instead of blasting a new invite wave.
//   - When only the attendee set changed, the PATCH contains just the
//     attendees, so Graph notifies only the added/removed attendees.
//
// Conflicts are resolved by SyncOptions.Conflict ("remote" when empty):
//
//	"remote" — the remote version wins. A conflicting local file is FIRST
//	  backed up to <file>.conflict-<unixts> (never silently lost), then
//	  overwritten with the remote rendering. For a "remote deleted, local
//	  changed" conflict, remote-wins moves the local file to the backup and
//	  untracks the pair.
//	"local"  — the local version wins: the remote event is PATCHed, or
//	  re-created when the remote side was deleted, or deleted remotely when
//	  the local side was deleted.
//	"newer"  — the side modified later wins: remote lastModifiedDateTime vs
//	  the local LAST-MODIFIED (file mtime when absent); ties go to remote. A
//	  deleted side carries no timestamp, so the surviving side's content
//	  wins. Local overwrites are still backed up first.
//
// Upload-create caveat: Graph assigns its own immutable iCalUId on POST and
// ignores any client UID, so the created event's UID will not match the local
// file's UID. Apply therefore rewrites the local .ics from the created event
// (remote UID inside, UID-derived filename) and keys the status by the remote
// iCalUId; the original local file is removed. The event content is preserved
// — only its UID/filename change.
//
// Filenames: the two-way sync names files <sanitized UID>.ics — the UID is
// the one identifier stable across both sides. This differs from the one-way
// Export (which names expanded instances by their per-occurrence Graph event
// id); the two commands own separate directory trees, so the schemes never
// mix. A pre-existing local file whose name differs from the UID scheme keeps
// its name on updates (the UID inside, not the filename, is the identity).

package graphcalendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MARK: - Plan types

// ActionKind classifies one planned sync action.
type ActionKind string

const (
	// ActionDownloadNew writes a new remote event to a local .ics file.
	ActionDownloadNew ActionKind = "download-new"
	// ActionDownloadUpdate overwrites the local .ics with the remote event
	// (remote changed, or remote wins on first sight).
	ActionDownloadUpdate ActionKind = "download-update"
	// ActionAdopt records status for an untracked pair whose local file
	// already matches the remote rendering; no file or remote write.
	ActionAdopt ActionKind = "adopt"
	// ActionPruneLocal removes the local file of a remotely deleted event.
	ActionPruneLocal ActionKind = "prune-local"
	// ActionDropStatus forgets a pair deleted on both sides; no writes.
	ActionDropStatus ActionKind = "drop-status"
	// ActionUploadCreate POSTs a new local event to Outlook.
	ActionUploadCreate ActionKind = "upload-create"
	// ActionUploadUpdate PATCHes a locally edited event to Outlook.
	ActionUploadUpdate ActionKind = "upload-update"
	// ActionDeleteRemote DELETEs the remote event of a locally deleted file.
	ActionDeleteRemote ActionKind = "delete-remote"
	// ActionConflict resolves an item changed (or deleted) on both sides per
	// the configured conflict policy.
	ActionConflict ActionKind = "conflict"
	// ActionRsvp records (and, with RsvpCall, sends) an owner RSVP change —
	// only ever emitted for tracked, content-unchanged, non-organizer pairs
	// (see the owner-RSVP sub-matrix in the file header).
	ActionRsvp ActionKind = "rsvp"
)

// Action is one planned sync operation for one UID, carrying everything
// Apply needs to execute it.
type Action struct {
	Kind ActionKind
	UID  string

	// Remote is the current remote event; valid when RemoteExists (downloads,
	// adopt, delete-remote, conflicts with a surviving remote side).
	Remote       Event
	RemoteExists bool

	// LocalPath/LocalEvent/LocalHash/LocalMtime describe the current local
	// file; valid when LocalExists (uploads, prune, conflicts with a surviving
	// local side, downloads over an existing file).
	LocalPath   string
	LocalEvent  Event
	LocalHash   string
	LocalMtime  time.Time
	LocalExists bool

	// GraphID is the tracked Graph event id (updates, deletes, conflicts).
	GraphID string

	// Prior is the tracked ItemStatus baseline; only meaningful when Tracked.
	Prior   ItemStatus
	Tracked bool

	// OwnerIsOrganizer reports whether the account owner organizes this event
	// (from the remote event when present, else from the local file's
	// ORGANIZER). It role-gates every attendee upload and the delete routing,
	// and is precomputed here so the plan preview and Apply agree.
	OwnerIsOrganizer bool
	// Recipients is the number of attendees other than the owner — the
	// recipient count of any invitation/update/cancellation this action may
	// trigger.
	Recipients int

	// Rsvp is the owner response ActionRsvp records; sent to Graph only when
	// RsvpCall is set (otherwise the action just rebaselines the status).
	Rsvp     OwnerResp
	RsvpCall bool

	// Summary is a short human-readable description ("subject" date) for the
	// command's plan listing.
	Summary string
}

// RemoteMutation reports whether applying this action may write to Outlook.
// Conflicts always count — even under the "remote" policy they are gated
// behind the confirmation, since they overwrite one side. An ActionRsvp
// counts only when it actually calls Graph (a rebaseline is status-only).
func (a Action) RemoteMutation() bool {
	switch a.Kind {
	case ActionUploadCreate, ActionUploadUpdate, ActionDeleteRemote, ActionConflict:
		return true
	case ActionRsvp:
		return a.RsvpCall
	}
	return false
}

// CalendarPlan is the ordered action list for one calendar.
type CalendarPlan struct {
	Calendar Calendar
	// Dir is the local collection directory of this calendar.
	Dir     string
	Actions []Action
}

// RemoteMutations returns the actions of the plan that may write to Outlook.
func (p *CalendarPlan) RemoteMutations() []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.RemoteMutation() {
			out = append(out, a)
		}
	}
	return out
}

// MARK: - Stats and options

// SyncStats reports what one Apply run did (or, in dry-run mode, would do).
type SyncStats struct {
	// Downloaded counts remote items written to local .ics files.
	Downloaded int
	// Pruned counts local files removed because the remote item disappeared.
	Pruned int
	// Uploaded counts local creations/edits pushed to Outlook.
	Uploaded int
	// DeletedRemote counts local deletions propagated to Outlook.
	DeletedRemote int
	// Conflicts counts items changed on both sides resolved per the policy.
	Conflicts int
	// Rsvps counts owner RSVP responses sent to Graph.
	Rsvps int
	// Skipped counts actions aborted gracefully instead of risking a wrong
	// notification: 412 precondition failures (remote changed since planning)
	// and refused meeting re-creates. They re-plan on the next run.
	Skipped int
}

// add accumulates another calendar's stats into s.
func (s *SyncStats) add(o SyncStats) {
	s.Downloaded += o.Downloaded
	s.Pruned += o.Pruned
	s.Uploaded += o.Uploaded
	s.DeletedRemote += o.DeletedRemote
	s.Conflicts += o.Conflicts
	s.Rsvps += o.Rsvps
	s.Skipped += o.Skipped
}

// SyncOptions tunes one Apply run.
type SyncOptions struct {
	// DryRun makes Apply a no-op that only counts what it would do; callers
	// must also skip persisting the (unchanged) status.
	DryRun bool
	// Conflict is the conflict resolution policy: "remote" (default when
	// empty), "local" or "newer". See the file header.
	Conflict string
	// SilentRSVP suppresses the response email to the organizer on RSVPs and
	// decline-routed deletes (Graph sendResponse=false). The zero value sends
	// the response — the standard scheduling-client behavior.
	SilentRSVP bool
}

// conflictPolicy returns the effective conflict policy.
func (o SyncOptions) conflictPolicy() string {
	if o.Conflict == "" {
		return "remote"
	}
	return o.Conflict
}

// MARK: - Local scan

// localItem is one local .ics file found in the calendar dir.
type localItem struct {
	path  string
	hash  string
	event Event
	mtime time.Time
}

// hashBytes returns the SHA-256 hex digest used as the local file identity.
func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// MARK: - Planning

// PlanAll builds the plan for every calendar of the account passing the
// include filter (same semantics as Export), without mutating anything.
// Calendar directories live at accountDir/<sanitized calendar name>.
func PlanAll(ctx context.Context, c *Client, accountDir string, include []string, state *SyncState) ([]CalendarPlan, error) {
	calendars, err := c.ListCalendars(ctx)
	if err != nil {
		return nil, err
	}
	state.normalize()

	var plans []CalendarPlan
	for _, cal := range calendars {
		if !calendarIncluded(cal.Name, include) {
			slog.Debug("Skipping calendar not in include list", "module", "GRAPHCAL",
				"calendar", cal.Name)
			continue
		}
		calDir := filepath.Join(accountDir, sanitizeName(cal.Name))
		plan, err := Plan(ctx, c, cal, calDir, state.Calendars[cal.ID])
		if err != nil {
			return nil, fmt.Errorf("failed to plan calendar %s: %w", cal.Name, err)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// Plan classifies every UID of one calendar into an Action per the decision
// matrix in the file header, without mutating status, local files or the
// remote calendar. status is read-only here.
func Plan(ctx context.Context, c *Client, cal Calendar, calDir string, status CalendarStatus) (CalendarPlan, error) {
	plan := CalendarPlan{Calendar: cal, Dir: calDir}

	remoteEvents, err := c.FetchMasterEvents(ctx, cal.ID)
	if err != nil {
		return plan, err
	}
	remote := make(map[string]Event, len(remoteEvents))
	for _, ev := range remoteEvents {
		if ev.ICalUID == "" {
			slog.Warn("Skipping remote event without iCalUID", "module", "GRAPHCAL",
				"calendar", cal.Name, "id", ev.ID)
			continue
		}
		if prev, dup := remote[ev.ICalUID]; dup {
			slog.Warn("Skipping remote event with duplicate iCalUID", "module", "GRAPHCAL",
				"calendar", cal.Name, "uid", ev.ICalUID, "id", ev.ID, "kept", prev.ID)
			continue
		}
		remote[ev.ICalUID] = ev
	}

	local, err := scanLocalItems(calDir, c.owner)
	if err != nil {
		return plan, err
	}

	for _, uid := range unionUIDs(remote, local, status.Items) {
		rev, remoteHas := remote[uid]
		li, localHas := local[uid]
		st, tracked := status.Items[uid]

		remoteChanged := remoteHas && (!tracked || st.RemoteHash != eventContentHash(rev, c.owner))
		localChanged := localHas && (!tracked || st.LocalHash != li.hash)
		remoteDeleted := tracked && !remoteHas
		localDeleted := tracked && !localHas

		a := Action{UID: uid, GraphID: st.GraphID, Prior: st, Tracked: tracked}
		if remoteHas {
			a.Remote = rev
			a.RemoteExists = true
			a.Summary = summarizeEvent(rev)
			a.OwnerIsOrganizer = ownerIsOrganizer(rev, c.owner)
			a.Recipients = countRecipients(rev.Attendees, c.owner)
		}
		if localHas {
			a.LocalPath = li.path
			a.LocalEvent = li.event
			a.LocalHash = li.hash
			a.LocalMtime = li.mtime
			a.LocalExists = true
			if !remoteHas {
				a.Summary = summarizeEvent(li.event)
				a.OwnerIsOrganizer = localOwnerIsOrganizer(li.event, c.owner)
				a.Recipients = countRecipients(li.event.Attendees, c.owner)
			}
		}

		switch {
		case !tracked && remoteHas && !localHas:
			a.Kind = ActionDownloadNew

		case !tracked && localHas && !remoteHas:
			a.Kind = ActionUploadCreate

		case !tracked: // remoteHas && localHas
			// First sight of a pair present on both sides: adopt if the local
			// file already matches the remote rendering, else remote wins (see
			// file header for the rationale). First sight can never notify:
			// only Download/Adopt come out of this branch.
			data, err := EventToICal(rev)
			if err != nil {
				return plan, fmt.Errorf("failed to serialize remote event %s: %w", uid, err)
			}
			if hashBytes(data) == li.hash {
				a.Kind = ActionAdopt
			} else {
				a.Kind = ActionDownloadUpdate
			}

		case remoteHas && localHas:
			// A local edit that only touches the owner's own PARTSTAT (or pure
			// formatting) is not a content change — it feeds the owner-RSVP
			// sub-matrix below instead of a notifying upload.
			ownerEditOnly := localChanged && !remoteChanged &&
				localEventMatchesRemote(li.event, rev, c.owner)
			switch {
			case remoteChanged && localChanged:
				a.Kind = ActionConflict
			case remoteChanged:
				a.Kind = ActionDownloadUpdate
			case localChanged && !ownerEditOnly:
				a.Kind = ActionUploadUpdate
			default:
				// Content unchanged on both sides: owner-RSVP sub-matrix.
				if !planRsvp(&a, li.event, rev, st, localChanged) {
					continue
				}
			}

		case remoteDeleted && localDeleted:
			a.Kind = ActionDropStatus
			a.Summary = uid

		case remoteDeleted:
			if localChanged {
				a.Kind = ActionConflict
			} else {
				a.Kind = ActionPruneLocal
			}

		default: // localDeleted
			if remoteChanged {
				a.Kind = ActionConflict
			} else {
				a.Kind = ActionDeleteRemote
			}
		}

		slog.Debug("Planned action", "module", "GRAPHCAL", "calendar", cal.Name,
			"uid", uid, "kind", a.Kind)
		plan.Actions = append(plan.Actions, a)
	}

	slog.Debug("Planned calendar", "module", "GRAPHCAL", "calendar", cal.Name,
		"remote", len(remote), "local", len(local), "tracked", len(status.Items),
		"actions", len(plan.Actions))
	return plan, nil
}

// summarizeEvent renders a short human-readable event description for plan
// listings.
func summarizeEvent(e Event) string {
	subject := e.Subject
	if subject == "" {
		subject = "(no subject)"
	}
	return fmt.Sprintf("%q %s", subject, e.Start.UTC().Format(graphDateFormat))
}

// planRsvp classifies the owner-RSVP state of a tracked pair whose content is
// unchanged on both sides, per the owner-RSVP sub-matrix in the file header.
// It fills a.Kind (and Rsvp/RsvpCall) and reports whether an action is needed
// at all. localBytesChanged marks a byte-level local edit that turned out to
// be owner-RSVP-only or formatting-only — those still need a LocalHash
// rebaseline so they stop being re-examined every run.
func planRsvp(a *Action, local, remote Event, st ItemStatus, localBytesChanged bool) bool {
	l, b, r := local.OwnerResponse, st.OwnerResponse, remote.OwnerResponse

	rebaseline := func(resp OwnerResp) bool {
		a.Kind = ActionRsvp
		a.Rsvp = resp
		a.RsvpCall = false
		return true
	}

	switch {
	case a.OwnerIsOrganizer:
		// The organizer has no RSVP to send; only rebaseline a formatting
		// edit so it is not re-examined forever.
		if !localBytesChanged {
			return false
		}
		return rebaseline(b)
	case l != b && l == r:
		// Local already matches remote (the response was also recorded in
		// Outlook, or the baseline predates Stage 2): rebaseline only — the
		// idempotency guard. No Graph call, no email.
		return rebaseline(l)
	case l != b && l != OwnerRespNone:
		a.Kind = ActionRsvp
		a.Rsvp = l
		a.RsvpCall = true
		return true
	case l != b:
		// l == None: a local file cannot express "un-respond" (and files that
		// simply lack the owner's ATTENDEE line land here too) — never turn
		// this into a Graph call.
		if !localBytesChanged {
			return false
		}
		return rebaseline(b)
	case r != b:
		// The owner responded elsewhere (Outlook, phone): refresh the local
		// rendering and baseline. Download-direction only, no email.
		a.Kind = ActionDownloadUpdate
		return true
	default:
		if !localBytesChanged {
			return false
		}
		return rebaseline(b)
	}
}

// localEventMatchesRemote reports whether a locally parsed event matches the
// remote event in every uploadable respect EXCEPT the owner's own RSVP: core
// content, organizer, and the attendee set excluding the owner. Fields the
// iCal parse cannot recover (online-meeting link, cancellation) are ignored,
// and the lossy iCal round-trip is applied to the remote side so a faithful
// re-parse compares equal. Used to classify owner-RSVP-only local edits.
func localEventMatchesRemote(local, remote Event, owner string) bool {
	if !coreContentEqual(local, remote) {
		return false
	}
	var localOrg, remoteOrg string
	if local.Organizer != nil {
		localOrg = strings.ToLower(local.Organizer.Email)
	}
	if remote.Organizer != nil {
		remoteOrg = strings.ToLower(remote.Organizer.Email)
	}
	if localOrg != remoteOrg {
		return false
	}
	return lossyAttendeeSet(local.Attendees, owner) == lossyAttendeeSet(remote.Attendees, owner)
}

// coreContentEqual reports whether the user-editable core (subject, times,
// all-day flag, location, description, recurrence) of a locally parsed event
// matches the remote event. The remote recurrence is passed through the lossy
// RRULE round-trip first, so Graph-only details a local re-parse cannot
// reproduce (e.g. firstDayOfWeek) do not read as differences.
func coreContentEqual(local, remote Event) bool {
	if local.Subject != remote.Subject ||
		!local.Start.Equal(remote.Start) ||
		!local.End.Equal(remote.End) ||
		local.AllDay != remote.AllDay ||
		local.Location != remote.Location ||
		normalizeText(local.Description) != normalizeText(remote.Description) {
		return false
	}
	return recurrenceJSON(local.Recurrence, "") ==
		recurrenceJSON(lossyRecurrence(remote.Recurrence, remote.Start), "")
}

// lossyRecurrence passes a Graph recurrence through the RRULE round-trip
// (recurrenceToROption -> roptionToRecurrence), yielding exactly what a
// faithful local re-parse of the emitted .ics produces. Unmappable
// recurrences come back unchanged.
func lossyRecurrence(rec *Recurrence, start time.Time) *Recurrence {
	if rec == nil {
		return nil
	}
	opt, err := recurrenceToROption(*rec)
	if err != nil {
		return rec
	}
	out, err := roptionToRecurrence(opt, start)
	if err != nil {
		return rec
	}
	return out
}

// lossyAttendeeSet canonicalizes an attendee list (excluding the owner) with
// the lossy iCal round-trip applied to type and response, so a remote value
// and its local re-parse compare equal.
func lossyAttendeeSet(attendees []Attendee, owner string) string {
	keys := make([]string, 0, len(attendees))
	for _, a := range attendees {
		if owner != "" && strings.EqualFold(a.Email, owner) {
			continue
		}
		keys = append(keys, strings.ToLower(a.Email)+
			"|"+attendeeTypeFromRole(attendeeRole(a.Type))+
			"|"+responseFromPartStat(attendeePartStat(a.Response)))
	}
	sort.Strings(keys)
	return strings.Join(keys, "\n")
}

// isPreconditionFailed reports whether err is a Graph 412 — the If-Match
// guard tripped because the remote event changed after planning.
func isPreconditionFailed(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusPreconditionFailed
}

// MARK: - Applying

// ApplyAll executes the plans (see Apply), storing each calendar's updated
// status back into state — also on partial failure, so remote operations that
// already succeeded are not replayed on the next run. The caller persists
// state afterwards (unless opts.DryRun).
func ApplyAll(ctx context.Context, c *Client, state *SyncState, plans []CalendarPlan, opts SyncOptions) (SyncStats, error) {
	var total SyncStats
	state.normalize()

	for _, plan := range plans {
		status, ok := state.Calendars[plan.Calendar.ID]
		if !ok {
			status = CalendarStatus{Items: make(map[string]ItemStatus)}
		}
		stats, err := Apply(ctx, c, plan, &status, opts)
		total.add(stats)
		if !opts.DryRun {
			state.Calendars[plan.Calendar.ID] = status
		}
		if err != nil {
			return total, fmt.Errorf("failed to apply plan for calendar %s: %w", plan.Calendar.Name, err)
		}
	}

	slog.Info("Calendar sync complete", "module", "GRAPHCAL",
		"downloaded", total.Downloaded, "pruned", total.Pruned,
		"uploaded", total.Uploaded, "deletedRemote", total.DeletedRemote,
		"conflicts", total.Conflicts, "rsvps", total.Rsvps,
		"skipped", total.Skipped, "dryRun", opts.DryRun)
	return total, nil
}

// SyncAll plans and immediately applies every calendar of the account —
// WITHOUT any confirmation gate. The CLI command uses PlanAll/ApplyAll with a
// prompt in between instead; this convenience wrapper is for callers that
// have already decided (e.g. tests, or automation running with --yes
// semantics).
func SyncAll(ctx context.Context, c *Client, accountDir string, include []string, state *SyncState, opts SyncOptions) (SyncStats, error) {
	plans, err := PlanAll(ctx, c, accountDir, include, state)
	if err != nil {
		return SyncStats{}, err
	}
	return ApplyAll(ctx, c, state, plans, opts)
}

// Sync plans and immediately applies one calendar (no confirmation gate; see
// SyncAll). The caller persists status afterwards (unless opts.DryRun).
func Sync(ctx context.Context, c *Client, cal Calendar, calDir string, status *CalendarStatus, opts SyncOptions) (SyncStats, error) {
	plan, err := Plan(ctx, c, cal, calDir, *status)
	if err != nil {
		return SyncStats{}, err
	}
	return Apply(ctx, c, plan, status, opts)
}

// Apply executes one calendar's plan, updating status in place as it goes.
// It performs exactly what the plan says — remote-mutating actions included —
// so any confirmation must happen before calling it. With opts.DryRun it
// mutates nothing and only counts what it would do.
func Apply(ctx context.Context, c *Client, plan CalendarPlan, status *CalendarStatus, opts SyncOptions) (SyncStats, error) {
	var stats SyncStats
	if status.Items == nil {
		status.Items = make(map[string]ItemStatus)
	}

	if opts.DryRun {
		for _, a := range plan.Actions {
			switch a.Kind {
			case ActionDownloadNew, ActionDownloadUpdate:
				stats.Downloaded++
			case ActionPruneLocal:
				stats.Pruned++
			case ActionUploadCreate, ActionUploadUpdate:
				stats.Uploaded++
			case ActionDeleteRemote:
				stats.DeletedRemote++
			case ActionConflict:
				stats.Conflicts++
			case ActionRsvp:
				if a.RsvpCall {
					stats.Rsvps++
				}
			}
		}
		slog.Info("Dry run: plan not applied", "module", "GRAPHCAL",
			"calendar", plan.Calendar.Name, "actions", len(plan.Actions))
		return stats, nil
	}

	if err := writeCalendarMeta(plan.Dir, plan.Calendar); err != nil {
		return stats, err
	}

	for _, a := range plan.Actions {
		var err error
		switch a.Kind {
		case ActionDownloadNew, ActionDownloadUpdate:
			slog.Info("Downloading remote event", "module", "GRAPHCAL",
				"calendar", plan.Calendar.Name, "uid", a.UID, "kind", a.Kind)
			if err = writeRemoteEvent(plan.Dir, a.LocalPath, a.UID, a.Remote, status, c.owner); err == nil {
				stats.Downloaded++
			}

		case ActionAdopt:
			slog.Debug("Adopting identical untracked pair", "module", "GRAPHCAL",
				"calendar", plan.Calendar.Name, "uid", a.UID)
			status.Items[a.UID] = ItemStatus{
				GraphID:       a.Remote.ID,
				RemoteHash:    eventContentHash(a.Remote, c.owner),
				LocalHash:     a.LocalHash,
				OwnerResponse: a.Remote.OwnerResponse,
				AttendeeHash:  attendeeSetHash(a.Remote.Attendees),
			}

		case ActionDropStatus:
			slog.Debug("Event deleted on both sides, dropping status", "module", "GRAPHCAL",
				"calendar", plan.Calendar.Name, "uid", a.UID)
			delete(status.Items, a.UID)

		case ActionPruneLocal:
			slog.Info("Pruning local event deleted remotely", "module", "GRAPHCAL",
				"calendar", plan.Calendar.Name, "uid", a.UID, "path", a.LocalPath)
			if rmErr := os.Remove(a.LocalPath); rmErr != nil {
				err = fmt.Errorf("failed to prune %s: %w", a.LocalPath, rmErr)
			} else {
				delete(status.Items, a.UID)
				stats.Pruned++
			}

		case ActionUploadCreate:
			if err = createFromLocal(ctx, c, plan, a, status); err == nil {
				stats.Uploaded++
			}

		case ActionUploadUpdate:
			if err = patchFromLocal(ctx, c, plan, a, status); err == nil {
				stats.Uploaded++
			}

		case ActionDeleteRemote:
			if err = removeRemoteEvent(ctx, c, plan, a, opts); err == nil {
				delete(status.Items, a.UID)
				stats.DeletedRemote++
			}

		case ActionRsvp:
			st := status.Items[a.UID]
			if a.RsvpCall {
				slog.Info("Sending owner RSVP", "module", "GRAPHCAL",
					"calendar", plan.Calendar.Name, "uid", a.UID, "graphID", a.GraphID,
					"response", a.Rsvp, "send", !opts.SilentRSVP)
				err = c.RespondToEvent(ctx, a.GraphID, a.Rsvp, !opts.SilentRSVP, "")
			} else {
				slog.Debug("Rebaselining owner RSVP, no Graph call", "module", "GRAPHCAL",
					"calendar", plan.Calendar.Name, "uid", a.UID, "response", a.Rsvp)
			}
			if err == nil {
				st.OwnerResponse = a.Rsvp
				if a.LocalExists {
					st.LocalHash = a.LocalHash
				}
				status.Items[a.UID] = st
				if a.RsvpCall {
					stats.Rsvps++
				}
			}

		case ActionConflict:
			var skipped bool
			if skipped, err = applyConflict(ctx, c, plan, a, status, opts); err == nil {
				if skipped {
					stats.Skipped++
				} else {
					stats.Conflicts++
				}
			}
		}
		if err != nil {
			if isPreconditionFailed(err) {
				// The remote event changed between planning and this write:
				// never clobber — skip and let the next run re-plan from
				// fresh state (R2).
				slog.Warn("Remote event changed since planning, skipping action", "module", "GRAPHCAL",
					"calendar", plan.Calendar.Name, "uid", a.UID, "kind", a.Kind)
				stats.Skipped++
				continue
			}
			return stats, err
		}
	}

	slog.Debug("Applied calendar plan", "module", "GRAPHCAL",
		"calendar", plan.Calendar.Name, "stats", fmt.Sprintf("%+v", stats))
	return stats, nil
}

// writeRemoteEvent serializes ev and writes it to path — the UID-derived
// default when path is empty — recording the pair in status (content, owner
// RSVP and attendee-set baselines).
func writeRemoteEvent(calDir, path, uid string, ev Event, status *CalendarStatus, owner string) error {
	if path == "" {
		path = filepath.Join(calDir, sanitizeName(uid)+".ics")
	}
	data, err := EventToICal(ev)
	if err != nil {
		return fmt.Errorf("failed to serialize remote event %s: %w", uid, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	status.Items[uid] = ItemStatus{
		GraphID:       ev.ID,
		RemoteHash:    eventContentHash(ev, owner),
		LocalHash:     hashBytes(data),
		OwnerResponse: ev.OwnerResponse,
		AttendeeHash:  attendeeSetHash(ev.Attendees),
	}
	return nil
}

// createFromLocal POSTs the local event to Outlook, then rewrites the local
// .ics from the created event and records status keyed by the remote iCalUId
// — Graph assigns its own UID on create, see the file header. The original
// local file is removed once the rewritten one is in place.
//
// The created event is read back via GetEvent before recording status: Graph
// normalizes events server-side right after a write, so the settled read-back
// — not the POST response — is the canonical content the next
// FetchMasterEvents will report. The local rewrite and the status baseline
// (RemoteHash = eventContentHash of the settled event, LocalHash = hash of
// the rewritten bytes) both come from the read-back, making the next sync a
// clean no-op. If the read-back fails, the create response is hashed instead
// — a slightly worse baseline, but never a failed sync.
func createFromLocal(ctx context.Context, c *Client, plan CalendarPlan, a Action, status *CalendarStatus) error {
	// Role gate: attendees (= invitations) go out only when the owner is the
	// organizer of this event. A file carrying a foreign ORGANIZER never
	// invites on that person's behalf.
	includeAttendees := a.OwnerIsOrganizer && len(a.LocalEvent.Attendees) > 0
	slog.Info("Creating remote event from local file", "module", "GRAPHCAL",
		"calendar", plan.Calendar.Name, "uid", a.UID, "path", a.LocalPath,
		"invites", includeAttendees, "recipients", a.Recipients)

	body := EventToGraphBody(a.LocalEvent, includeAttendees)
	if a.LocalEvent.RequestOnlineMeeting {
		// One-shot Teams marker: honored on create only. The post-create
		// rewrite below drops the marker from the local file.
		body["isOnlineMeeting"] = true
		body["onlineMeetingProvider"] = "teamsForBusiness"
	}
	// transactionId dedups a retried POST server-side (R1): the same create
	// replayed after a throttle/transient retry can never produce a duplicate
	// event or a second invitation wave.
	body["transactionId"] = uuid.NewString()

	created, err := c.CreateEvent(ctx, plan.Calendar.ID, body)
	if err != nil {
		return fmt.Errorf("failed to create remote event for %s: %w", a.UID, err)
	}
	settled, err := c.GetEvent(ctx, created.ID)
	if err != nil {
		slog.Warn("Failed to read back created event, recording create-response baseline", "module", "GRAPHCAL",
			"calendar", plan.Calendar.Name, "id", created.ID, "err", err)
		settled = created
	}
	uid := settled.ICalUID
	if uid == "" {
		uid = settled.ID
	}

	path := a.LocalPath
	if uid != a.UID {
		path = filepath.Join(plan.Dir, sanitizeName(uid)+".ics")
	}
	if err := writeRemoteEvent(plan.Dir, path, uid, settled, status, c.owner); err != nil {
		return err
	}
	if uid != a.UID {
		if a.LocalPath != "" && a.LocalPath != path {
			if err := os.Remove(a.LocalPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to remove pre-create file %s: %w", a.LocalPath, err)
			}
		}
		// Any stale status under the old local UID (delete-conflict recreate)
		// must go — the pair is tracked under the remote UID now.
		delete(status.Items, a.UID)
	}
	slog.Info("Created remote event, local file rewritten with remote UID", "module", "GRAPHCAL",
		"calendar", plan.Calendar.Name, "localUID", a.UID, "remoteUID", uid,
		"graphID", settled.ID, "path", path)
	return nil
}

// patchFromLocal PATCHes the tracked remote event with the local content and
// records the new baseline: RemoteHash from a GetEvent read-back (the settled
// server-normalized content is what the next FetchMasterEvents will report,
// see createFromLocal) and LocalHash of the current local bytes. The local
// file is deliberately left exactly as the user wrote it — no normalization
// rewrite — so the next sync sees neither side changed and is a clean no-op.
// If the read-back fails, the uploaded local event is hashed instead (the
// PATCH response carries no content) rather than failing the sync.
//
// Scheduling behavior: the planned changeKey travels as If-Match, so a
// concurrent remote edit yields a 412 (skipped by Apply) instead of a
// clobber. Attendees are included only when the owner organizes the meeting
// AND the attendee set actually changed against the baseline; when ONLY the
// attendee set changed, the PATCH carries just the attendees, so Graph
// notifies only the added/removed attendees instead of everyone.
func patchFromLocal(ctx context.Context, c *Client, plan CalendarPlan, a Action, status *CalendarStatus) error {
	attendeeBase := a.Prior.AttendeeHash
	if attendeeBase == "" {
		// Unknown baseline (pre-Stage-2 status, or a conflict where the
		// remote side changed too): diff against the current remote set.
		attendeeBase = attendeeSetHash(a.Remote.Attendees)
	}
	attendeesChanged := a.OwnerIsOrganizer &&
		attendeeSetHash(a.LocalEvent.Attendees) != attendeeBase
	coreChanged := !coreContentEqual(a.LocalEvent, a.Remote)

	var body map[string]any
	switch {
	case attendeesChanged && !coreChanged:
		body = map[string]any{"attendees": attendeesToGraph(a.LocalEvent.Attendees)}
	default:
		body = EventToGraphBody(a.LocalEvent, attendeesChanged)
	}

	var etag string
	if a.RemoteExists {
		etag = a.Remote.ChangeKey
	}
	slog.Info("Updating remote event from local edit", "module", "GRAPHCAL",
		"calendar", plan.Calendar.Name, "uid", a.UID, "graphID", a.GraphID, "path", a.LocalPath,
		"attendeesChanged", attendeesChanged, "recipients", a.Recipients)

	if _, err := c.UpdateEvent(ctx, a.GraphID, etag, body); err != nil {
		return fmt.Errorf("failed to update remote event for %s: %w", a.UID, err)
	}
	remoteHash := eventContentHash(a.LocalEvent, c.owner)
	ownerResp := a.Prior.OwnerResponse
	attendeeHash := attendeeSetHash(a.LocalEvent.Attendees)
	if settled, err := c.GetEvent(ctx, a.GraphID); err != nil {
		slog.Warn("Failed to read back updated event, recording uploaded-content baseline", "module", "GRAPHCAL",
			"calendar", plan.Calendar.Name, "id", a.GraphID, "err", err)
	} else {
		remoteHash = eventContentHash(settled, c.owner)
		ownerResp = settled.OwnerResponse
		attendeeHash = attendeeSetHash(settled.Attendees)
	}
	status.Items[a.UID] = ItemStatus{
		GraphID:       a.GraphID,
		RemoteHash:    remoteHash,
		LocalHash:     a.LocalHash,
		OwnerResponse: ownerResp,
		AttendeeHash:  attendeeHash,
	}
	return nil
}

// removeRemoteEvent propagates a local deletion to Graph with meeting-aware
// routing: a meeting the owner merely attends is DECLINED (POST /decline,
// sendResponse per SilentRSVP) so the organizer's tracking stays correct;
// everything else — organizer-owned meetings (Graph cancels all attendees)
// and plain appointments (silent) — is DELETEd with the planned changeKey as
// If-Match.
func removeRemoteEvent(ctx context.Context, c *Client, plan CalendarPlan, a Action, opts SyncOptions) error {
	if a.RemoteExists && !a.OwnerIsOrganizer && len(a.Remote.Attendees) > 0 {
		slog.Info("Declining remote meeting (local file deleted)", "module", "GRAPHCAL",
			"calendar", plan.Calendar.Name, "uid", a.UID, "graphID", a.GraphID,
			"send", !opts.SilentRSVP)
		err := c.RespondToEvent(ctx, a.GraphID, OwnerRespDeclined, !opts.SilentRSVP, "")
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusNotFound {
			slog.Info("Remote event already gone on decline", "module", "GRAPHCAL", "id", a.GraphID)
			return nil
		}
		return err
	}

	var etag string
	if a.RemoteExists {
		etag = a.Remote.ChangeKey
	}
	slog.Info("Deleting remote event (local file deleted)", "module", "GRAPHCAL",
		"calendar", plan.Calendar.Name, "uid", a.UID, "graphID", a.GraphID,
		"cancelsMeeting", a.OwnerIsOrganizer && a.Recipients > 0, "recipients", a.Recipients)
	return c.DeleteEvent(ctx, a.GraphID, etag)
}

// conflictWinner resolves which side a conflict action favors under the
// policy: "local", or "remote" (also the fallback for unknown policy strings
// — the conservative default). For "newer" the side modified later wins;
// a deleted side carries no timestamp, so the surviving side wins.
func conflictWinner(a Action, policy string) string {
	switch policy {
	case "local":
		return "local"
	case "newer":
		switch {
		case !a.RemoteExists:
			return "local"
		case !a.LocalExists:
			return "remote"
		}
		localMod := a.LocalEvent.LastModified
		if localMod.IsZero() {
			localMod = a.LocalMtime
		}
		if localMod.After(a.Remote.LastModified) {
			return "local"
		}
		return "remote"
	default:
		return "remote"
	}
}

// applyConflict resolves one conflict per the policy (see the file header).
// A local file is always backed up before being overwritten or removed —
// conflicting local data is never silently lost. skipped reports a refused
// resolution (re-creating a deleted meeting with attendees would re-invite
// everyone — R3): nothing is changed and the next run re-plans it.
func applyConflict(ctx context.Context, c *Client, plan CalendarPlan, a Action, status *CalendarStatus, opts SyncOptions) (skipped bool, err error) {
	policy := opts.conflictPolicy()
	winner := conflictWinner(a, policy)
	if policy == "newer" {
		slog.Info("Conflict: newer side wins", "module", "GRAPHCAL",
			"calendar", plan.Calendar.Name, "uid", a.UID, "winner", winner,
			"remoteModified", a.Remote.LastModified, "localModified", a.LocalEvent.LastModified)
	}

	if winner == "local" {
		switch {
		case !a.LocalExists:
			// The local deletion wins over the remote edit; routed like a
			// plain delete (organizer cancels, attendee declines).
			slog.Warn("Conflict: local deletion wins, removing remote event", "module", "GRAPHCAL",
				"calendar", plan.Calendar.Name, "uid", a.UID, "graphID", a.GraphID)
			if err := removeRemoteEvent(ctx, c, plan, a, opts); err != nil {
				return false, fmt.Errorf("failed to remove remote event for conflict %s: %w", a.UID, err)
			}
			delete(status.Items, a.UID)
		case !a.RemoteExists:
			if len(a.LocalEvent.Attendees) > 0 {
				// R3: re-creating a meeting would send a fresh invitation
				// wave for an event someone deliberately cancelled. Refuse
				// and leave both sides untouched for manual resolution.
				slog.Warn("Conflict: remote meeting deleted but local edit wins — refusing to re-create a meeting, resolve manually", "module", "GRAPHCAL",
					"calendar", plan.Calendar.Name, "uid", a.UID, "path", a.LocalPath,
					"recipients", a.Recipients)
				return true, nil
			}
			// The remote event was deleted but the local edit wins: re-create
			// (attendee-free, so no invitations are involved).
			slog.Warn("Conflict: local edit wins, re-creating deleted remote event", "module", "GRAPHCAL",
				"calendar", plan.Calendar.Name, "uid", a.UID, "path", a.LocalPath)
			if err := createFromLocal(ctx, c, plan, a, status); err != nil {
				return false, err
			}
		default:
			slog.Warn("Conflict: local wins, overwriting remote event", "module", "GRAPHCAL",
				"calendar", plan.Calendar.Name, "uid", a.UID, "graphID", a.GraphID)
			if err := patchFromLocal(ctx, c, plan, a, status); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	// Remote wins.
	if a.LocalExists {
		backupPath := fmt.Sprintf("%s.conflict-%d", a.LocalPath, time.Now().Unix())
		if err := os.Rename(a.LocalPath, backupPath); err != nil {
			return false, fmt.Errorf("failed to back up conflicting file %s: %w", a.LocalPath, err)
		}
		slog.Warn("Conflict: remote wins, local file backed up", "module", "GRAPHCAL",
			"calendar", plan.Calendar.Name, "uid", a.UID, "backup", backupPath)
	}
	if a.RemoteExists {
		if err := writeRemoteEvent(plan.Dir, a.LocalPath, a.UID, a.Remote, status, c.owner); err != nil {
			return false, err
		}
	} else {
		// The remote deletion wins; the local edit survives only as the
		// backup, and the pair is untracked.
		slog.Warn("Conflict: remote deletion wins, local file kept only as backup", "module", "GRAPHCAL",
			"calendar", plan.Calendar.Name, "uid", a.UID)
		delete(status.Items, a.UID)
	}
	return false, nil
}

// writeCalendarMeta ensures calDir exists and carries the vdir displayname and
// color files (same convention as Export).
func writeCalendarMeta(calDir string, cal Calendar) error {
	if err := os.MkdirAll(calDir, 0o700); err != nil {
		return fmt.Errorf("failed to create calendar dir %s: %w", calDir, err)
	}
	if err := os.WriteFile(filepath.Join(calDir, "displayname"), []byte(cal.Name+"\n"), 0o600); err != nil {
		return fmt.Errorf("failed to write displayname for %s: %w", cal.Name, err)
	}
	if cal.HexColor != "" {
		if err := os.WriteFile(filepath.Join(calDir, "color"), []byte(cal.HexColor+"\n"), 0o600); err != nil {
			return fmt.Errorf("failed to write color for %s: %w", cal.Name, err)
		}
	}
	return nil
}

// scanLocalItems enumerates the *.ics files of calDir keyed by their parsed
// iCalendar UID, with the SHA-256 of the raw file bytes as identity (plus the
// parsed event and file mtime for uploads and conflict resolution). A missing
// dir yields an empty map (fresh calendar, or plan before first sync). Files
// that fail to parse, lack a UID or duplicate an already-seen UID are logged
// and skipped — they are invisible to the diff, never treated as items.
// Conflict backups (<file>.conflict-<ts>) do not end in .ics and are ignored.
// owner is the account email, threaded into ICalToEvent so the owner's RSVP
// is recognized in the parsed events.
func scanLocalItems(calDir, owner string) (map[string]localItem, error) {
	entries, err := os.ReadDir(calDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]localItem{}, nil
		}
		return nil, fmt.Errorf("failed to read calendar dir %s: %w", calDir, err)
	}

	items := make(map[string]localItem)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ics") {
			continue
		}
		path := filepath.Join(calDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", path, err)
		}
		ev, err := ICalToEvent(data, owner)
		if err != nil {
			slog.Warn("Skipping unparseable local .ics", "module", "GRAPHCAL",
				"path", path, "err", err)
			continue
		}
		if ev.ICalUID == "" {
			slog.Warn("Skipping local .ics without UID", "module", "GRAPHCAL", "path", path)
			continue
		}
		if prev, dup := items[ev.ICalUID]; dup {
			slog.Warn("Skipping local .ics with duplicate UID", "module", "GRAPHCAL",
				"path", path, "uid", ev.ICalUID, "kept", prev.path)
			continue
		}
		var mtime time.Time
		if info, err := entry.Info(); err == nil {
			mtime = info.ModTime()
		}
		items[ev.ICalUID] = localItem{path: path, hash: hashBytes(data), event: ev, mtime: mtime}
	}
	return items, nil
}

// unionUIDs returns the sorted union of the UIDs of all three maps, so the
// diff walks every item exactly once in deterministic order.
func unionUIDs(remote map[string]Event, local map[string]localItem, tracked map[string]ItemStatus) []string {
	seen := make(map[string]struct{}, len(remote)+len(local)+len(tracked))
	for uid := range remote {
		seen[uid] = struct{}{}
	}
	for uid := range local {
		seen[uid] = struct{}{}
	}
	for uid := range tracked {
		seen[uid] = struct{}{}
	}
	uids := make([]string, 0, len(seen))
	for uid := range seen {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
