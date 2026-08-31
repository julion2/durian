package backendfactory

import (
	"testing"

	"github.com/julion2/durian/cli/internal/backend"
	"github.com/julion2/durian/cli/internal/gmailbackend"
	"github.com/julion2/durian/cli/internal/graphbackend"
	"github.com/julion2/durian/cli/internal/imapbackend"
	"github.com/julion2/durian/cli/internal/jmapbackend"
)

// The sync engine's behavior is parameterized by backend.Capabilities, and the
// contract matrix in syncengine exercises it against the named profiles in
// backend/profiles.go. That coverage is only worth anything if the profiles
// still describe what the adapters actually declare — otherwise the matrix
// stays green while production drifts out from under it.
//
// This test is that link. It is deliberately in backendfactory: that is the one
// package that already imports all four adapters, so asserting here adds no new
// import edge.
//
// Capabilities() returns a literal on every adapter and reads no receiver
// state, so a zero-value Backend is a legitimate way to ask what a provider
// declares without opening a connection. If an adapter ever needs live state to
// answer, this test will panic rather than silently pass, which is the outcome
// we want: the profile would no longer be a static fact.
func TestAdapterCapabilitiesMatchNamedProfiles(t *testing.T) {
	declared := map[string]backend.Capabilities{
		"imapbackend":  (&imapbackend.Backend{}).Capabilities(),
		"graphbackend": (&graphbackend.Backend{}).Capabilities(),
		"jmapbackend":  (&jmapbackend.Backend{}).Capabilities(),
		"gmailbackend": (&gmailbackend.Backend{}).Capabilities(),
	}

	for name, want := range backend.ProductionProfiles {
		got, ok := declared[name]
		if !ok {
			t.Errorf("profile %q has no adapter in this test; add it or drop the profile", name)
			continue
		}
		if got != want {
			t.Errorf("%s declares %+v, named profile says %+v", name, got, want)
		}
	}

	for name := range declared {
		if _, ok := backend.ProductionProfiles[name]; !ok {
			t.Errorf("adapter %q has no named profile; the contract matrix will not cover it", name)
		}
	}
}

// A new capability bit is only meaningful once the engine reads it, and the
// contract matrix can only cover a bit that some profile sets. A bit that no
// shipped adapter enables is either dead or unfinished; either way the matrix
// silently proves nothing about it.
//
// PushWatch is the current example. All four adapters carry it, the engine
// never reads it (backendfactory.PushWatch is config-driven and separate), so
// it is excluded here rather than pretended about.
func TestEveryCapabilityBitIsExercisedBySomeProfile(t *testing.T) {
	notReadByEngine := map[string]string{
		"PushWatch": "declared by adapters but read nowhere in syncengine; " +
			"transport selection uses backendfactory.PushWatch instead",
	}

	var union backend.Capabilities
	for _, caps := range backend.ProductionProfiles {
		union.PushWatch = union.PushWatch || caps.PushWatch
		union.FlagChangesInDelta = union.FlagChangesInDelta || caps.FlagChangesInDelta
		union.LabelsAreTags = union.LabelsAreTags || caps.LabelsAreTags
		union.AnsweredUnsupported = union.AnsweredUnsupported || caps.AnsweredUnsupported
	}

	bits := map[string]bool{
		"PushWatch":           union.PushWatch,
		"FlagChangesInDelta":  union.FlagChangesInDelta,
		"LabelsAreTags":       union.LabelsAreTags,
		"AnsweredUnsupported": union.AnsweredUnsupported,
	}
	for name, set := range bits {
		if _, known := notReadByEngine[name]; known {
			continue
		}
		if !set {
			t.Errorf("capability %q is set by no production profile, so no contract "+
				"scenario can cover it", name)
		}
	}
}
