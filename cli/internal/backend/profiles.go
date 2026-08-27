package backend

// The capability bits in Capabilities parameterize the sync engine's flag and
// tag behavior. Each production adapter declares its own combination, and the
// engine branches on the bits rather than on the provider — which means the
// engine's real input space is the set of combinations actually shipped, not
// the 2^n the type permits.
//
// These are those combinations, named. They exist so tests can exercise a
// profile as a unit instead of setting bits one at a time: a scenario run
// against "FlagChangesInDelta only" is a Graph scenario, and calling it that
// makes it obvious when a profile has no coverage at all.
//
// The adapters keep their own literal declaration next to the code that
// justifies it. These values duplicate that declaration on purpose, so a
// change on either side has to be made twice, and the drift test in
// backendfactory catches the case where it was made once.

// ProfileIMAP is the classic IMAP adapter: no delta stream, so the engine polls
// flags for the whole mailbox; folder roles drive tags; \Answered round-trips.
// IDLE provides push.
var ProfileIMAP = Capabilities{
	PushWatch: true,
}

// ProfileGraph is Microsoft Graph: a delta stream that carries flag changes, so
// the engine reconciles from the delta instead of polling. Folder roles drive
// tags and \Answered round-trips, as with IMAP. No local push transport.
var ProfileGraph = Capabilities{
	FlagChangesInDelta: true,
}

// ProfileJMAP is JMAP: delta-carried flag changes plus keyword-as-tag
// mirroring. Unlike Gmail, JMAP persists $answered, so the flag three-way runs
// unmodified.
var ProfileJMAP = Capabilities{
	PushWatch:          true,
	FlagChangesInDelta: true,
	LabelsAreTags:      true,
}

// ProfileGmail is the Gmail API: delta-carried flag changes, labels as tags,
// and no \Answered equivalent. It is the only profile that sets all three flag
// bits, and the only one where the engine suppresses Answered in the merge.
var ProfileGmail = Capabilities{
	FlagChangesInDelta:  true,
	LabelsAreTags:       true,
	AnsweredUnsupported: true,
}

// ProductionProfiles maps each shipped adapter to the capability set it
// declares. Ranging over this is how a test covers the deployed matrix rather
// than an arbitrary subset of it; the key is the adapter's package name.
var ProductionProfiles = map[string]Capabilities{
	"imapbackend":  ProfileIMAP,
	"graphbackend": ProfileGraph,
	"jmapbackend":  ProfileJMAP,
	"gmailbackend": ProfileGmail,
}
