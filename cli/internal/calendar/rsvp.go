// Local-first RSVP: setting the account owner's response on a meeting only
// edits the owner's own ATTENDEE entry (and the canonical OwnerResponse) of a
// local Event — nothing talks to a provider here. The organizer learns of the
// response on the next `durian calendar sync`, where an RSVP is a notifying
// action behind the sync's preview/confirmation gate.

package calendar

import (
	"fmt"
	"strings"
)

// ParseRSVPVerb maps an RSVP verb to the canonical owner response. It accepts
// both the CLI verbs (accept/decline/tentative) and the Graph-style response
// values API clients send (accepted/declined/tentativelyAccepted),
// case-insensitively.
func ParseRSVPVerb(verb string) (OwnerResp, error) {
	switch strings.ToLower(verb) {
	case "accept", "accepted":
		return OwnerRespAccepted, nil
	case "decline", "declined":
		return OwnerRespDeclined, nil
	case "tentative", "tentativelyaccepted":
		return OwnerRespTentative, nil
	default:
		return "", fmt.Errorf("unknown RSVP %q, use accept, decline or tentative", verb)
	}
}

// SetOwnerResponse updates the owner's own ATTENDEE response (the field that
// EventToICal turns into the PARTSTAT parameter the sync later reads) and the
// canonical OwnerResponse. The owner's attendee entry is matched by email,
// case-insensitively. It reports false when the owner is not among the
// attendees, so a local file cannot carry an RSVP for a meeting the owner was
// not invited to.
func SetOwnerResponse(e *Event, owner string, resp OwnerResp) bool {
	graphResp := map[OwnerResp]string{
		OwnerRespAccepted:  "accepted",
		OwnerRespDeclined:  "declined",
		OwnerRespTentative: "tentativelyAccepted",
	}[resp]
	for i := range e.Attendees {
		if strings.EqualFold(e.Attendees[i].Email, owner) {
			e.Attendees[i].Response = graphResp
			e.OwnerResponse = resp
			return true
		}
	}
	return false
}
