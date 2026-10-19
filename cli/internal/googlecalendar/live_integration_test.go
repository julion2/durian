//go:build calendarintegration

package googlecalendar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendartest"
)

func TestGoogleLiveTimezones(t *testing.T) {
	if os.Getenv("DURIAN_CALENDAR_LIVE_TEST") != "1" {
		t.Skip("requires explicit calendar live-test opt-in")
	}
	path := os.Getenv("DURIAN_CALENDAR_RECORDING")
	if path == "" {
		t.Fatal("DURIAN_CALENDAR_RECORDING is required")
	}
	// Reserve a new recording before any remote mutation; never overwrite
	// earlier evidence or discover an unwritable destination after cleanup.
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("recording requires a new writable destination")
	}
	if err := output.Close(); err != nil {
		t.Fatal("close recording destination")
	}
	source, err := calendartest.NewTokenSource(calendartest.Google)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cal, err := calendartest.EnsureGoogleCalendar(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal("generate run identity")
	}
	prefix := "Durian Test " + hex.EncodeToString(nonce[:]) + " "
	guard, err := calendartest.NewGoogleGuard(cal, prefix, source)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &calendartest.Recorder{CalendarID: cal, Next: guard, Source: source, Recording: calendartest.Recording{Prefix: prefix}}
	httpClient := guard.HTTPClient()
	httpClient.Transport = recorder
	t.Cleanup(func() {
		if err := recorder.Save(path); err != nil {
			t.Error("saving sanitized recording failed")
		}
		t.Logf("recorded %d scoped exchanges; %d owned master IDs", len(recorder.Recording.Exchanges), len(guard.CreatedIDs()))
	})
	// NewWithToken never touches persisted OAuth. The guard injects the
	// in-memory credential only after validating the actual request boundary.
	c := NewWithToken(os.Getenv("DURIAN_GOOGLE_TEST_EMAIL"), calendartest.GoogleBase, "guard-supplies-token", httpClient)
	exerciseProviderTimezones(t, c, cal, prefix)
}
