// Notification preview for the two-way calendar sync: before the CLI's
// confirmation gate, every email message that applying the plans would make
// Graph send is enumerated — invitations (creates with attendees), updates
// (organizer edits of meetings), cancellations (organizer deletes) and RSVP
// responses (owner replies and decline-routed deletes). The classification
// here mirrors exactly what Apply does (both read the same precomputed
// Action fields), so the preview and the execution can never disagree.

package graphcalendar

// Notification categories.
const (
	NotifyInvite = "INVITE"
	NotifyUpdate = "UPDATE"
	NotifyCancel = "CANCEL"
	NotifyRSVP   = "RSVP"
)

// Notification describes one email message that applying a plan will make
// Graph send: its category, the affected event, the calendar, and how many
// recipients it reaches (all attendees for invite/update/cancel; the
// organizer — one recipient — for an RSVP).
type Notification struct {
	Category   string
	Summary    string
	Calendar   string
	Recipients int
}

// NotifiesRecipients reports whether applying this action makes Graph send
// email, and to how many recipients. Conflicts answer for the worst case (a
// local-wins resolution) and RSVPs assume the response is sent; use
// PlanNotifications for the policy- and flag-aware preview.
func (a Action) NotifiesRecipients() (bool, int) {
	n, ok := notificationFor(a, "local", false)
	return ok, n.Recipients
}

// PlanNotifications enumerates every email message applying the plans would
// trigger under the given conflict policy. With silentRSVP, RSVP responses
// (owner replies and decline-routed deletes) are sent with
// sendResponse=false and therefore do not appear.
func PlanNotifications(plans []CalendarPlan, policy string, silentRSVP bool) []Notification {
	var out []Notification
	for _, p := range plans {
		for _, a := range p.Actions {
			if n, ok := notificationFor(a, policy, silentRSVP); ok {
				n.Summary = a.Summary
				n.Calendar = p.Calendar.Name
				out = append(out, n)
			}
		}
	}
	return out
}

// notificationFor classifies one action's email side effect. Conflicts are
// mapped to the equivalent write their policy resolution performs; a refused
// meeting re-create (R3) sends nothing.
func notificationFor(a Action, policy string, silentRSVP bool) (Notification, bool) {
	kind := a.Kind
	if kind == ActionConflict {
		if conflictWinner(a, policy) != "local" {
			// Remote wins: only local files change, no Graph write.
			return Notification{}, false
		}
		switch {
		case !a.LocalExists:
			kind = ActionDeleteRemote
		case !a.RemoteExists:
			// Re-create: refused for meetings (R3), silent for plain events.
			return Notification{}, false
		default:
			kind = ActionUploadUpdate
		}
	}

	switch kind {
	case ActionUploadCreate:
		if a.OwnerIsOrganizer && a.Recipients > 0 {
			return Notification{Category: NotifyInvite, Recipients: a.Recipients}, true
		}
	case ActionUploadUpdate:
		if a.OwnerIsOrganizer && a.Recipients > 0 {
			return Notification{Category: NotifyUpdate, Recipients: a.Recipients}, true
		}
	case ActionDeleteRemote:
		if a.RemoteExists && len(a.Remote.Attendees) > 0 {
			if a.OwnerIsOrganizer {
				if a.Recipients > 0 {
					return Notification{Category: NotifyCancel, Recipients: a.Recipients}, true
				}
				return Notification{}, false
			}
			if !silentRSVP {
				return Notification{Category: NotifyRSVP, Recipients: 1}, true
			}
		}
	case ActionRsvp:
		if a.RsvpCall && !silentRSVP {
			return Notification{Category: NotifyRSVP, Recipients: 1}, true
		}
	}
	return Notification{}, false
}
