// Two-way calendar sync engine (vdirsyncer model), split into two phases so
// the CLI command can show the full plan and ask for confirmation before any
// remote mutation happens:
//
//   - Plan / PlanAll enumerate both sides of a calendar — remote master
//     events via the provider's FetchMasterEvents (keyed by UID = iCalUID,
//     identity = eventContentHash of the meaningful fields; the remote etag
//     is NOT usable as the change signal, Graph churns it without content
//     changes) and local .ics files in
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
//	not tracked, both present, differ   -> DownloadUpdate (remote wins on first
//	  sight; the local file is backed up first)
//	tracked, only remote changed        -> DownloadUpdate
//	tracked, only local changed         -> UploadUpdate
//	tracked, both changed               -> Conflict
//	tracked, remote deleted, local same -> PruneLocal
//	tracked, local deleted, remote same -> DeleteRemote (routed: organizer
//	  cancels via DeleteEvent, a mere attendee declines via RespondToEvent, a
//	  plain appointment is deleted silently)
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
//	                                       NO remote call)
//	L != B, L != R, L != None           -> Rsvp (RespondToEvent accept/decline/
//	                                       tentative, sendResponse per
//	                                       SilentRSVP)
//	L != B, L != R, L == None           -> no remote action (a file cannot
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
// there. Because that local file may hold edits made before the first sync
// ever ran, it is moved to <file>.conflict-<unixts> first — first sight loses
// no local data, it only stops preferring it.
//
// Unreadable local files: a tracked .ics that fails to parse is missing from
// the local scan, which is byte-for-byte the same signal as the user deleting
// it — and "locally deleted" routes to a remote delete, which cancels an
// owned meeting and mails every attendee. So while ANY file in a calendar dir
// is unreadable, no item in that calendar is classified as locally deleted;
// those UIDs are skipped and re-planned once the file parses again. Every
// other action (downloads, uploads, RSVPs) keeps working.
//
// Scheduling safety rails (a bug here emails real people):
//
//   - Creates carry a client-generated idempotency key, so a retried create
//     can never produce a duplicate event or a second invitation wave.
//   - Updates/deletes send the planned etag as the write precondition; an
//     ErrPrecondition (remote changed since planning) skips that action —
//     counted in Stats.Skipped — and the next run re-plans from fresh state
//     instead of clobbering.
//   - Attendees are only uploaded for meetings the OWNER organizes (role
//     gate); for meetings the owner merely attends, attendee changes are
//     never pushed and a local deletion is routed as a decline.
//   - A conflict must never cancel+re-invite: the "local wins, remote
//     deleted" resolution refuses to re-create an event that has attendees
//     (logged + counted as skipped) instead of blasting a new invite wave.
//   - When only the attendee set changed, the update is scoped to just the
//     attendees, so the provider notifies only the added/removed attendees.
//
// Conflicts are resolved by SyncOptions.Conflict ("remote" when empty):
//
//	"remote" — the remote version wins. A conflicting local file is FIRST
//	  backed up to <file>.conflict-<unixts> (never silently lost), then
//	  overwritten with the remote rendering. For a "remote deleted, local
//	  changed" conflict, remote-wins moves the local file to the backup and
//	  untracks the pair.
//	"local"  — the local version wins: the remote event is updated, or
//	  re-created when the remote side was deleted, or deleted remotely when
//	  the local side was deleted.
//	"newer"  — the side modified later wins: remote lastModifiedDateTime vs
//	  the local LAST-MODIFIED (file mtime when absent); ties go to remote. A
//	  deleted side carries no timestamp, so the surviving side's content
//	  wins. Local overwrites are still backed up first.
//
// Upload-create caveat: a provider may assign its own immutable UID on create
// and ignore the client UID (Graph does), so the created event's UID will not
// match the local file's UID. Apply therefore rewrites the local .ics from the
// created event (remote UID inside, UID-derived filename) and keys the status
// by the remote UID; the original local file is removed. The event content is
// preserved — only its UID/filename change.
//
// Filenames: the two-way sync names files <sanitized UID>.ics — the UID is
// the one identifier stable across both sides. This differs from the one-way
// Export (which names expanded instances by their per-occurrence Graph event
// id); the two commands own separate directory trees, so the schemes never
// mix. A pre-existing local file whose name differs from the UID scheme keeps
// its name on updates (the UID inside, not the filename, is the identity).

package calendarsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// RemoteID is the tracked provider event id (updates, deletes, conflicts).
	RemoteID string

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

	// Rsvp is the owner response ActionRsvp records; sent to the provider only
	// when RsvpCall is set (otherwise the action just rebaselines the status).
	Rsvp     OwnerResp
	RsvpCall bool

	// BackupLocal asks Apply to move the existing local file aside to
	// <file>.conflict-<ts> before writing the remote version over it. Set for
	// the first-sight overwrite, where the local file may hold edits that were
	// never uploaded and that no baseline can reconstruct — the same guarantee
	// applyConflict gives on remote-wins.
	BackupLocal bool

	// Summary is a short human-readable description ("subject" date) for the
	// command's plan listing.
	Summary string
}

// RemoteMutation reports whether applying this action may write to the remote
// calendar. Conflicts always count — even under the "remote" policy they are
// gated behind the confirmation, since they overwrite one side. An ActionRsvp
// counts only when it actually calls the provider (a rebaseline is
// status-only).
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

// RemoteMutations returns the actions of the plan that may write to the
// remote calendar.
func (p *CalendarPlan) RemoteMutations() []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.RemoteMutation() {
			out = append(out, a)
		}
	}
	return out
}

// FilterDownloadOnly returns copies of the plans with every remote-mutating
// action (see Action.RemoteMutation: uploads, remote deletes, conflicts, and
// RSVPs that call the provider) removed, plus the count of removed actions.
// The input plans are not mutated. Applying a filtered plan can therefore
// never write to the remote calendar or make the provider send any email —
// this is the safety mechanism behind unattended (autosync) runs, which must
// leave every notifying action to the interactive `durian calendar sync`
// confirmation gate. Non-mutating actions (downloads, prunes, adopts,
// drop-status, RSVP rebaselines) are preserved in order.
func FilterDownloadOnly(plans []CalendarPlan) (filtered []CalendarPlan, suppressed int) {
	filtered = make([]CalendarPlan, 0, len(plans))
	for _, p := range plans {
		fp := CalendarPlan{Calendar: p.Calendar, Dir: p.Dir}
		for _, a := range p.Actions {
			if a.RemoteMutation() {
				suppressed++
				continue
			}
			fp.Actions = append(fp.Actions, a)
		}
		filtered = append(filtered, fp)
	}
	return filtered, suppressed
}

// MARK: - Stats and options

// SyncStats reports what one Apply run did (or, in dry-run mode, would do).
type SyncStats struct {
	// Downloaded counts remote items written to local .ics files.
	Downloaded int
	// Pruned counts local files removed because the remote item disappeared.
	Pruned int
	// Uploaded counts local creations/edits pushed to the remote calendar.
	Uploaded int
	// DeletedRemote counts local deletions propagated to the remote calendar.
	DeletedRemote int
	// Conflicts counts items changed on both sides resolved per the policy.
	Conflicts int
	// Rsvps counts owner RSVP responses sent to the provider.
	Rsvps int
	// Skipped counts actions aborted gracefully instead of risking a wrong
	// notification: precondition failures (remote changed since planning)
	// and refused meeting re-creates. They re-plan on the next run.
	Skipped int
	// Failed counts actions that errored (e.g. the provider rejecting one
	// event, or a local file write failure) without aborting the run: the
	// error is logged, the item's status baseline is left untouched, and the
	// remaining actions still execute. Failed items re-plan on the next run.
	// Auth errors and context cancellation still abort the whole Apply —
	// they would fail every remaining action identically.
	Failed int
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
	s.Failed += o.Failed
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
	// decline-routed deletes (sendResponse=false). The zero value sends the
	// response — the standard scheduling-client behavior.
	SilentRSVP bool
}

// conflictPolicy returns the effective conflict policy.
func (o SyncOptions) conflictPolicy() string {
	if o.Conflict == "" {
		return "remote"
	}
	return o.Conflict
}

// MARK: - Planning

// PlanAll builds the plan for every calendar of the account passing the
// include filter (same semantics as Export), without mutating anything.
// Calendar directories live at accountDir/<sanitized calendar name>.
func PlanAll(ctx context.Context, p CalendarProvider, accountDir string, include []string, state *SyncState) ([]CalendarPlan, error) {
	calendars, err := p.ListCalendars(ctx)
	if err != nil {
		return nil, err
	}
	state.normalize()

	var plans []CalendarPlan
	for _, cal := range calendars {
		if !calendarIncluded(cal.Name, include) {
			slog.Debug("Skipping calendar not in include list", "module", "CALSYNC",
				"calendar", cal.Name)
			continue
		}
		calDir := filepath.Join(accountDir, sanitizeName(cal.Name))
		plan, err := Plan(ctx, p, cal, calDir, state.Calendars[cal.ID])
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
func Plan(ctx context.Context, p CalendarProvider, cal Calendar, calDir string, status CalendarStatus) (CalendarPlan, error) {
	plan := CalendarPlan{Calendar: cal, Dir: calDir}
	owner := p.Owner()

	remoteEvents, err := p.FetchMasterEvents(ctx, cal.ID)
	if err != nil {
		return plan, err
	}
	remote := make(map[string]Event, len(remoteEvents))
	for _, ev := range remoteEvents {
		if ev.ICalUID == "" {
			slog.Warn("Skipping remote event without iCalUID", "module", "CALSYNC",
				"calendar", cal.Name, "id", ev.ID)
			continue
		}
		if prev, dup := remote[ev.ICalUID]; dup {
			slog.Warn("Skipping remote event with duplicate iCalUID", "module", "CALSYNC",
				"calendar", cal.Name, "uid", ev.ICalUID, "id", ev.ID, "kept", prev.ID)
			continue
		}
		remote[ev.ICalUID] = ev
	}

	local, unreadable, err := scanLocalItems(calDir, owner)
	if err != nil {
		return plan, err
	}
	if len(unreadable) > 0 {
		slog.Warn("Local calendar dir has unreadable .ics files, suppressing local-deletion actions",
			"module", "CALSYNC", "calendar", cal.Name, "files", len(unreadable), "paths", unreadable)
	}

	for _, uid := range unionUIDs(remote, local, status.Items) {
		rev, remoteHas := remote[uid]
		li, localHas := local[uid]
		st, tracked := status.Items[uid]

		remoteChanged := remoteHas && (!tracked || st.RemoteHash != eventContentHash(rev, owner))
		localChanged := localHas && (!tracked || st.LocalHash != li.Hash)
		remoteDeleted := tracked && !remoteHas
		localDeleted := tracked && !localHas

		// A tracked file that failed to parse is missing from `local`, which
		// is indistinguishable from the user deleting it — and a local
		// deletion routes to ActionDeleteRemote, which for an owned meeting
		// cancels it and mails every attendee. A corrupt (or merely
		// unsupported) .ics must never do that, so while any file in this
		// directory is unreadable, no item is classified as locally deleted:
		// it is skipped entirely and re-planned once the file parses again.
		if localDeleted && len(unreadable) > 0 {
			slog.Warn("Not treating a missing local event as deleted: unreadable files present",
				"module", "CALSYNC", "calendar", cal.Name, "uid", uid)
			continue
		}

		a := Action{UID: uid, RemoteID: st.RemoteID, Prior: st, Tracked: tracked}
		if remoteHas {
			a.Remote = rev
			a.RemoteExists = true
			a.Summary = summarizeEvent(rev)
			a.OwnerIsOrganizer = ownerIsOrganizer(rev, owner)
			a.Recipients = countRecipients(rev.Attendees, owner)
		}
		if localHas {
			a.LocalPath = li.Path
			a.LocalEvent = li.Event
			a.LocalHash = li.Hash
			a.LocalMtime = li.Mtime
			a.LocalExists = true
			if !remoteHas {
				a.Summary = summarizeEvent(li.Event)
				a.OwnerIsOrganizer = localOwnerIsOrganizer(li.Event, owner)
				a.Recipients = countRecipients(li.Event.Attendees, owner)
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
			if hashBytes(data) == li.Hash {
				a.Kind = ActionAdopt
			} else {
				// The sides differ and there is no baseline to tell which one
				// moved, so the local file may carry never-uploaded edits.
				// Remote wins, but the local version is kept as a backup.
				a.Kind = ActionDownloadUpdate
				a.BackupLocal = true
			}

		case remoteHas && localHas:
			// A local edit that only touches the owner's own PARTSTAT (or pure
			// formatting) is not a content change — it feeds the owner-RSVP
			// sub-matrix below instead of a notifying upload.
			ownerEditOnly := localChanged && !remoteChanged &&
				localEventMatchesRemote(li.Event, rev, owner)
			switch {
			case remoteChanged && localChanged:
				a.Kind = ActionConflict
			case remoteChanged:
				a.Kind = ActionDownloadUpdate
			case localChanged && !ownerEditOnly:
				a.Kind = ActionUploadUpdate
			default:
				// Content unchanged on both sides: owner-RSVP sub-matrix.
				if !planRsvp(&a, li.Event, rev, st, localChanged) {
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

		slog.Debug("Planned action", "module", "CALSYNC", "calendar", cal.Name,
			"uid", uid, "kind", a.Kind)
		plan.Actions = append(plan.Actions, a)
	}

	slog.Debug("Planned calendar", "module", "CALSYNC", "calendar", cal.Name,
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
	return fmt.Sprintf("%q %s", subject, e.Start.UTC().Format(dateOnlyFormat))
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
		// idempotency guard. No remote call, no email.
		return rebaseline(l)
	case l != b && l != OwnerRespNone:
		a.Kind = ActionRsvp
		a.Rsvp = l
		a.RsvpCall = true
		return true
	case l != b:
		// l == None: a local file cannot express "un-respond" (and files that
		// simply lack the owner's ATTENDEE line land here too) — never turn
		// this into a remote call.
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

// localOwnerIsOrganizer reports whether the account owner organizes a locally
// parsed event: no ORGANIZER in the file (the owner creates, so the provider
// will make them the organizer) or an ORGANIZER matching the owner. A foreign
// ORGANIZER means the file was copied from someone else's meeting — durian
// must not invite on their behalf.
func localOwnerIsOrganizer(e Event, owner string) bool {
	if e.Organizer == nil {
		return true
	}
	return owner != "" && strings.EqualFold(e.Organizer.Email, owner)
}

// MARK: - Applying

// ApplyAll executes the plans (see Apply), storing each calendar's updated
// status back into state — also on partial failure, so remote operations that
// already succeeded are not replayed on the next run. The caller persists
// state afterwards (unless opts.DryRun).
func ApplyAll(ctx context.Context, p CalendarProvider, state *SyncState, plans []CalendarPlan, opts SyncOptions) (SyncStats, error) {
	var total SyncStats
	state.normalize()

	for _, plan := range plans {
		status, ok := state.Calendars[plan.Calendar.ID]
		if !ok {
			status = CalendarStatus{Items: make(map[string]ItemStatus)}
		}
		stats, err := Apply(ctx, p, plan, &status, opts)
		total.add(stats)
		if !opts.DryRun {
			state.Calendars[plan.Calendar.ID] = status
		}
		if err != nil {
			return total, fmt.Errorf("failed to apply plan for calendar %s: %w", plan.Calendar.Name, err)
		}
	}

	slog.Info("Calendar sync complete", "module", "CALSYNC",
		"downloaded", total.Downloaded, "pruned", total.Pruned,
		"uploaded", total.Uploaded, "deletedRemote", total.DeletedRemote,
		"conflicts", total.Conflicts, "rsvps", total.Rsvps,
		"skipped", total.Skipped, "failed", total.Failed, "dryRun", opts.DryRun)
	return total, nil
}

// SyncAll plans and immediately applies every calendar of the account —
// WITHOUT any confirmation gate. The CLI command uses PlanAll/ApplyAll with a
// prompt in between instead; this convenience wrapper is for callers that
// have already decided (e.g. tests, or automation running with --yes
// semantics).
func SyncAll(ctx context.Context, p CalendarProvider, accountDir string, include []string, state *SyncState, opts SyncOptions) (SyncStats, error) {
	plans, err := PlanAll(ctx, p, accountDir, include, state)
	if err != nil {
		return SyncStats{}, err
	}
	return ApplyAll(ctx, p, state, plans, opts)
}

// Sync plans and immediately applies one calendar (no confirmation gate; see
// SyncAll). The caller persists status afterwards (unless opts.DryRun).
func Sync(ctx context.Context, p CalendarProvider, cal Calendar, calDir string, status *CalendarStatus, opts SyncOptions) (SyncStats, error) {
	plan, err := Plan(ctx, p, cal, calDir, *status)
	if err != nil {
		return SyncStats{}, err
	}
	return Apply(ctx, p, plan, status, opts)
}

// Apply executes one calendar's plan, updating status in place as it goes.
// It performs exactly what the plan says — remote-mutating actions included —
// so any confirmation must happen before calling it. With opts.DryRun it
// mutates nothing and only counts what it would do.
func Apply(ctx context.Context, p CalendarProvider, plan CalendarPlan, status *CalendarStatus, opts SyncOptions) (SyncStats, error) {
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
		slog.Info("Dry run: plan not applied", "module", "CALSYNC",
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
			slog.Info("Downloading remote event", "module", "CALSYNC",
				"calendar", plan.Calendar.Name, "uid", a.UID, "kind", a.Kind)
			if a.BackupLocal && a.LocalExists {
				if _, err = backupLocalFile(a.LocalPath); err != nil {
					break
				}
				slog.Warn("First sight: remote wins, local file backed up", "module", "CALSYNC",
					"calendar", plan.Calendar.Name, "uid", a.UID)
			}
			if err = writeRemoteEvent(plan.Dir, a.LocalPath, a.UID, a.Remote, status, p.Owner()); err == nil {
				stats.Downloaded++
			}

		case ActionAdopt:
			slog.Debug("Adopting identical untracked pair", "module", "CALSYNC",
				"calendar", plan.Calendar.Name, "uid", a.UID)
			status.Items[a.UID] = ItemStatus{
				RemoteID:      a.Remote.ID,
				RemoteHash:    eventContentHash(a.Remote, p.Owner()),
				LocalHash:     a.LocalHash,
				OwnerResponse: a.Remote.OwnerResponse,
				AttendeeHash:  attendeeSetHash(a.Remote.Attendees),
			}

		case ActionDropStatus:
			slog.Debug("Event deleted on both sides, dropping status", "module", "CALSYNC",
				"calendar", plan.Calendar.Name, "uid", a.UID)
			delete(status.Items, a.UID)

		case ActionPruneLocal:
			slog.Info("Pruning local event deleted remotely", "module", "CALSYNC",
				"calendar", plan.Calendar.Name, "uid", a.UID, "path", a.LocalPath)
			if rmErr := os.Remove(a.LocalPath); rmErr != nil {
				err = fmt.Errorf("failed to prune %s: %w", a.LocalPath, rmErr)
			} else {
				delete(status.Items, a.UID)
				stats.Pruned++
			}

		case ActionUploadCreate:
			if err = createFromLocal(ctx, p, plan, a, status); err == nil {
				stats.Uploaded++
			}

		case ActionUploadUpdate:
			if err = patchFromLocal(ctx, p, plan, a, status); err == nil {
				stats.Uploaded++
			}

		case ActionDeleteRemote:
			if err = removeRemoteEvent(ctx, p, plan, a, opts); err == nil {
				delete(status.Items, a.UID)
				stats.DeletedRemote++
			}

		case ActionRsvp:
			st := status.Items[a.UID]
			if a.RsvpCall {
				slog.Info("Sending owner RSVP", "module", "CALSYNC",
					"calendar", plan.Calendar.Name, "uid", a.UID, "remoteID", a.RemoteID,
					"response", a.Rsvp, "send", !opts.SilentRSVP)
				err = p.RespondToEvent(ctx, plan.Calendar.ID, a.RemoteID, a.Rsvp, !opts.SilentRSVP, "")
			} else {
				slog.Debug("Rebaselining owner RSVP, no remote call", "module", "CALSYNC",
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
			if skipped, err = applyConflict(ctx, p, plan, a, status, opts); err == nil {
				if skipped {
					stats.Skipped++
				} else {
					stats.Conflicts++
				}
			}
		}
		if err != nil {
			switch {
			case errors.Is(err, ErrPrecondition):
				// The remote event changed between planning and this write:
				// never clobber — skip and let the next run re-plan from
				// fresh state (R2).
				slog.Warn("Remote event changed since planning, skipping action", "module", "CALSYNC",
					"calendar", plan.Calendar.Name, "uid", a.UID, "kind", a.Kind)
				stats.Skipped++
			case ctx.Err() != nil:
				// Cancellation fails every remaining action the same way:
				// abort instead of logging one failure per event.
				return stats, err
			case p.IsAuthError(err):
				// An expired token / missing consent also fails everything
				// identically — abort so the command can print the auth hint.
				return stats, err
			default:
				// One bad event (a malformed local file, a provider 400 like
				// "all-day must span whole days", a transient 5xx that ran
				// out of retries) must not block syncing the rest: log it,
				// leave its baseline untouched so the next run re-plans it,
				// and continue with the remaining actions.
				slog.Error("Sync action failed, continuing with remaining events", "module", "CALSYNC",
					"calendar", plan.Calendar.Name, "uid", a.UID, "kind", a.Kind, "err", err)
				stats.Failed++
			}
			continue
		}
	}

	slog.Debug("Applied calendar plan", "module", "CALSYNC",
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
	if err := WriteFileAtomic(path, data, 0o600); err != nil {
		return err
	}
	status.Items[uid] = ItemStatus{
		RemoteID:      ev.ID,
		RemoteHash:    eventContentHash(ev, owner),
		LocalHash:     hashBytes(data),
		OwnerResponse: ev.OwnerResponse,
		AttendeeHash:  attendeeSetHash(ev.Attendees),
	}
	return nil
}

// createFromLocal creates the local event remotely, then rewrites the local
// .ics from the created event and records status keyed by the remote UID —
// the provider may assign its own UID on create, see the file header. The
// original local file is removed once the rewritten one is in place.
//
// The created event is read back via GetEvent before recording status: the
// server may normalize events right after a write, so the settled read-back —
// not the create response — is the canonical content the next
// FetchMasterEvents will report. The local rewrite and the status baseline
// (RemoteHash = eventContentHash of the settled event, LocalHash = hash of
// the rewritten bytes) both come from the read-back, making the next sync a
// clean no-op. If the read-back fails, the create response is hashed instead
// — a slightly worse baseline, but never a failed sync.
// notifyAttendees is the provider-neutral notification policy: it reports
// whether the remote write for this action should make the provider send mail
// to the event's attendees (invitation on create, change notice on update,
// cancellation on delete).
//
// This is the single place that decision is made. Graph ignores the answer and
// notifies by its own rules; Google obeys it literally, so a wrong answer here
// is the difference between attendees learning a meeting moved and them
// showing up at the old time. It must also agree with what PlanNotifications
// promised the user in the confirmation preview — the preview and the wire
// must never disagree.
//
// Available inputs on the action:
//
//	a.OwnerIsOrganizer — whether the account owner organizes this event
//	                     (already role-gated: false for meetings the owner
//	                     merely attends, where attendee changes are never
//	                     pushed and a deletion is routed as a decline instead)
//	a.Recipients       — the number of attendees other than the owner
//	a.RemoteExists     — whether a remote side exists for this action
//	a.LocalEvent       — the local event content (a.LocalEvent.Attendees)
//	a.Remote           — the remote event content (a.Remote.Attendees)
func notifyAttendees(a Action) bool {
	// One predicate for create, update and delete, matching notificationFor's
	// create/update condition exactly. Delete looks stricter there
	// (a.RemoteExists && len(a.Remote.Attendees) > 0), but those terms are
	// implied: Recipients is counted from the REMOTE attendees whenever a
	// remote side exists, and when it does not, the delete 404s and sends
	// nothing regardless of the flag.
	//
	// Both terms are load-bearing. OwnerIsOrganizer keeps the engine from
	// mailing on a foreign organizer's behalf — a meeting the owner merely
	// attends never takes this path, it is routed to an RSVP whose own
	// sendResponse flag decides. Recipients > 0 is what the autosync
	// safe-upload gate rests on: an attendee-less appointment scores zero, so
	// "provably non-notifying" stays provable.
	return a.OwnerIsOrganizer && a.Recipients > 0
}

func createFromLocal(ctx context.Context, p CalendarProvider, plan CalendarPlan, a Action, status *CalendarStatus) error {
	// Role gate: attendees (= invitations) go out only when the owner is the
	// organizer of this event. A file carrying a foreign ORGANIZER never
	// invites on that person's behalf.
	includeAttendees := a.OwnerIsOrganizer && len(a.LocalEvent.Attendees) > 0
	notify := notifyAttendees(a)
	slog.Info("Creating remote event from local file", "module", "CALSYNC",
		"calendar", plan.Calendar.Name, "uid", a.UID, "path", a.LocalPath,
		"invites", includeAttendees, "notify", notify, "recipients", a.Recipients)

	opts := CreateOptions{
		IncludeAttendees: includeAttendees,
		NotifyAttendees:  notify,
		// One-shot online-meeting marker: honored on create only. The
		// post-create rewrite below drops the marker from the local file.
		RequestOnlineMeeting: a.LocalEvent.RequestOnlineMeeting,
		// The idempotency key dedups a retried create server-side (R1): the
		// same create replayed after a throttle/transient retry can never
		// produce a duplicate event or a second invitation wave.
		IdempotencyKey: uuid.NewString(),
	}

	created, err := p.CreateEvent(ctx, plan.Calendar.ID, a.LocalEvent, opts)
	if err != nil {
		return fmt.Errorf("failed to create remote event for %s: %w", a.UID, err)
	}
	settled, err := p.GetEvent(ctx, plan.Calendar.ID, created.ID)
	if err != nil {
		slog.Warn("Failed to read back created event, recording create-response baseline", "module", "CALSYNC",
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
	if err := writeRemoteEvent(plan.Dir, path, uid, settled, status, p.Owner()); err != nil {
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
	slog.Info("Created remote event, local file rewritten with remote UID", "module", "CALSYNC",
		"calendar", plan.Calendar.Name, "localUID", a.UID, "remoteUID", uid,
		"remoteID", settled.ID, "path", path)
	return nil
}

// patchFromLocal updates the tracked remote event with the local content and
// records the new baseline: RemoteHash from a GetEvent read-back (the settled
// server-normalized content is what the next FetchMasterEvents will report,
// see createFromLocal) and LocalHash of the current local bytes. The local
// file is deliberately left exactly as the user wrote it — no normalization
// rewrite — so the next sync sees neither side changed and is a clean no-op.
// If the read-back fails, the uploaded local event is hashed instead (the
// update response carries no content) rather than failing the sync.
//
// Scheduling behavior: the planned etag travels as the write precondition, so
// a concurrent remote edit yields an ErrPrecondition (skipped by Apply)
// instead of a clobber. Attendees are included only when the owner organizes
// the meeting AND the attendee set actually changed against the baseline;
// when ONLY the attendee set changed, the update is scoped to just the
// attendees (UpdateSpec.AttendeesOnly), so the provider notifies only the
// added/removed attendees instead of everyone.
func patchFromLocal(ctx context.Context, p CalendarProvider, plan CalendarPlan, a Action, status *CalendarStatus) error {
	attendeeBase := a.Prior.AttendeeHash
	if attendeeBase == "" {
		// Unknown baseline (pre-Stage-2 status, or a conflict where the
		// remote side changed too): diff against the current remote set.
		attendeeBase = attendeeSetHash(a.Remote.Attendees)
	}
	attendeesChanged := a.OwnerIsOrganizer &&
		attendeeSetHash(a.LocalEvent.Attendees) != attendeeBase
	coreChanged := !coreContentEqual(a.LocalEvent, a.Remote)

	var etag string
	if a.RemoteExists {
		etag = a.Remote.ETag
	}
	notify := notifyAttendees(a)
	spec := UpdateSpec{
		Event:            a.LocalEvent,
		IncludeAttendees: attendeesChanged,
		NotifyAttendees:  notify,
		AttendeesOnly:    attendeesChanged && !coreChanged,
		ETag:             etag,
	}
	slog.Info("Updating remote event from local edit", "module", "CALSYNC",
		"calendar", plan.Calendar.Name, "uid", a.UID, "remoteID", a.RemoteID, "path", a.LocalPath,
		"attendeesChanged", attendeesChanged, "notify", notify, "recipients", a.Recipients)

	if err := p.UpdateEvent(ctx, plan.Calendar.ID, a.RemoteID, spec); err != nil {
		return fmt.Errorf("failed to update remote event for %s: %w", a.UID, err)
	}
	remoteHash := eventContentHash(a.LocalEvent, p.Owner())
	ownerResp := a.Prior.OwnerResponse
	attendeeHash := attendeeSetHash(a.LocalEvent.Attendees)
	if settled, err := p.GetEvent(ctx, plan.Calendar.ID, a.RemoteID); err != nil {
		slog.Warn("Failed to read back updated event, recording uploaded-content baseline", "module", "CALSYNC",
			"calendar", plan.Calendar.Name, "id", a.RemoteID, "err", err)
	} else {
		remoteHash = eventContentHash(settled, p.Owner())
		ownerResp = settled.OwnerResponse
		attendeeHash = attendeeSetHash(settled.Attendees)
	}
	status.Items[a.UID] = ItemStatus{
		RemoteID:      a.RemoteID,
		RemoteHash:    remoteHash,
		LocalHash:     a.LocalHash,
		OwnerResponse: ownerResp,
		AttendeeHash:  attendeeHash,
	}
	return nil
}

// removeRemoteEvent propagates a local deletion to the provider with
// meeting-aware routing: a meeting the owner merely attends is DECLINED
// (RespondToEvent, sendResponse per SilentRSVP) so the organizer's tracking
// stays correct; everything else — organizer-owned meetings (the provider
// cancels all attendees) and plain appointments (silent) — is deleted with
// the planned etag as the write precondition. An ErrNotFound is folded into
// success on both routes: the goal (event absent remotely) is reached either
// way.
func removeRemoteEvent(ctx context.Context, p CalendarProvider, plan CalendarPlan, a Action, opts SyncOptions) error {
	if a.RemoteExists && !a.OwnerIsOrganizer && len(a.Remote.Attendees) > 0 {
		slog.Info("Declining remote meeting (local file deleted)", "module", "CALSYNC",
			"calendar", plan.Calendar.Name, "uid", a.UID, "remoteID", a.RemoteID,
			"send", !opts.SilentRSVP)
		err := p.RespondToEvent(ctx, plan.Calendar.ID, a.RemoteID, OwnerRespDeclined, !opts.SilentRSVP, "")
		if errors.Is(err, ErrNotFound) {
			slog.Info("Remote event already gone on decline", "module", "CALSYNC", "id", a.RemoteID)
			return nil
		}
		return err
	}

	var etag string
	if a.RemoteExists {
		etag = a.Remote.ETag
	}
	notify := notifyAttendees(a)
	slog.Info("Deleting remote event (local file deleted)", "module", "CALSYNC",
		"calendar", plan.Calendar.Name, "uid", a.UID, "remoteID", a.RemoteID,
		"cancelsMeeting", notify, "recipients", a.Recipients)
	if err := p.DeleteEvent(ctx, plan.Calendar.ID, a.RemoteID, etag, notify); err != nil {
		if errors.Is(err, ErrNotFound) {
			slog.Info("Remote event already gone on delete", "module", "CALSYNC", "id", a.RemoteID)
			return nil
		}
		return err
	}
	return nil
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
func applyConflict(ctx context.Context, p CalendarProvider, plan CalendarPlan, a Action, status *CalendarStatus, opts SyncOptions) (skipped bool, err error) {
	policy := opts.conflictPolicy()
	winner := conflictWinner(a, policy)
	if policy == "newer" {
		slog.Info("Conflict: newer side wins", "module", "CALSYNC",
			"calendar", plan.Calendar.Name, "uid", a.UID, "winner", winner,
			"remoteModified", a.Remote.LastModified, "localModified", a.LocalEvent.LastModified)
	}

	if winner == "local" {
		switch {
		case !a.LocalExists:
			// The local deletion wins over the remote edit; routed like a
			// plain delete (organizer cancels, attendee declines).
			slog.Warn("Conflict: local deletion wins, removing remote event", "module", "CALSYNC",
				"calendar", plan.Calendar.Name, "uid", a.UID, "remoteID", a.RemoteID)
			if err := removeRemoteEvent(ctx, p, plan, a, opts); err != nil {
				return false, fmt.Errorf("failed to remove remote event for conflict %s: %w", a.UID, err)
			}
			delete(status.Items, a.UID)
		case !a.RemoteExists:
			if len(a.LocalEvent.Attendees) > 0 {
				// R3: re-creating a meeting would send a fresh invitation
				// wave for an event someone deliberately cancelled. Refuse
				// and leave both sides untouched for manual resolution.
				slog.Warn("Conflict: remote meeting deleted but local edit wins — refusing to re-create a meeting, resolve manually", "module", "CALSYNC",
					"calendar", plan.Calendar.Name, "uid", a.UID, "path", a.LocalPath,
					"recipients", a.Recipients)
				return true, nil
			}
			// The remote event was deleted but the local edit wins: re-create
			// (attendee-free, so no invitations are involved).
			slog.Warn("Conflict: local edit wins, re-creating deleted remote event", "module", "CALSYNC",
				"calendar", plan.Calendar.Name, "uid", a.UID, "path", a.LocalPath)
			if err := createFromLocal(ctx, p, plan, a, status); err != nil {
				return false, err
			}
		default:
			slog.Warn("Conflict: local wins, overwriting remote event", "module", "CALSYNC",
				"calendar", plan.Calendar.Name, "uid", a.UID, "remoteID", a.RemoteID)
			if err := patchFromLocal(ctx, p, plan, a, status); err != nil {
				return false, err
			}
		}
		return false, nil
	}

	// Remote wins.
	if a.LocalExists {
		backupPath, err := backupLocalFile(a.LocalPath)
		if err != nil {
			return false, err
		}
		slog.Warn("Conflict: remote wins, local file backed up", "module", "CALSYNC",
			"calendar", plan.Calendar.Name, "uid", a.UID, "backup", backupPath)
	}
	if a.RemoteExists {
		if err := writeRemoteEvent(plan.Dir, a.LocalPath, a.UID, a.Remote, status, p.Owner()); err != nil {
			return false, err
		}
	} else {
		// The remote deletion wins; the local edit survives only as the
		// backup, and the pair is untracked.
		slog.Warn("Conflict: remote deletion wins, local file kept only as backup", "module", "CALSYNC",
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
	if err := WriteFileAtomic(filepath.Join(calDir, "displayname"), []byte(cal.Name+"\n"), 0o600); err != nil {
		return fmt.Errorf("failed to write displayname for %s: %w", cal.Name, err)
	}
	if cal.HexColor != "" {
		if err := WriteFileAtomic(filepath.Join(calDir, "color"), []byte(cal.HexColor+"\n"), 0o600); err != nil {
			return fmt.Errorf("failed to write color for %s: %w", cal.Name, err)
		}
	}
	return nil
}

// backupLocalFile moves path aside to <path>.conflict-<unixts> and returns the
// backup path. Local data the sync is about to overwrite is never discarded
// outright: the backup does not end in .ics, so the next scan ignores it and
// the user can diff or restore it by hand.
func backupLocalFile(path string) (string, error) {
	backupPath := fmt.Sprintf("%s.conflict-%d", path, time.Now().Unix())
	if err := os.Rename(path, backupPath); err != nil {
		return "", fmt.Errorf("failed to back up local file %s: %w", path, err)
	}
	return backupPath, nil
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
