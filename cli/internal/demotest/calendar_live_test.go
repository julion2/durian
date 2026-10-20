//go:build calendarintegration

package demotest_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/julion2/durian/cli/internal/calendarsync"
	"github.com/julion2/durian/cli/internal/calendartest"
	"github.com/julion2/durian/cli/internal/googlecalendar"
	"github.com/julion2/durian/cli/internal/graphcalendar"
)

func TestLiveMonthlyCalendarThroughSync(t *testing.T) {
	if os.Getenv("DURIAN_CALENDAR_LIVE_TEST") != "1" {
		t.Skip("requires explicit calendar live-test opt-in")
	}
	provider := os.Getenv("DURIAN_CALENDAR_PROVIDER")
	if provider != "google" && provider != "graph" {
		t.Fatal("select exactly one calendar provider: google or graph")
	}
	path := os.Getenv("DURIAN_CALENDAR_RECORDING")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("a new writable recording path is required")
	}
	f.Close()
	kind := calendartest.Google
	if provider == "graph" {
		kind = calendartest.Microsoft
	}
	source, err := calendartest.NewTokenSource(kind)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal("generate run identity")
	}
	prefix := "Durian Test " + hex.EncodeToString(nonce[:]) + " "
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	recorder := &calendartest.Recorder{Source: source, Recording: calendartest.Recording{Prefix: prefix}}
	var p calendarsync.CalendarProvider
	if provider == "google" {
		cal, err := calendartest.EnsureGoogleCalendar(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		guard, err := calendartest.NewGoogleGuard(cal, prefix, source)
		if err != nil {
			t.Fatal(err)
		}
		recorder.CalendarID, recorder.Next = cal, guard
		client := guard.HTTPClient()
		client.Transport = recorder
		p = googlecalendar.NewWithToken(os.Getenv("DURIAN_GOOGLE_TEST_EMAIL"), calendartest.GoogleBase, "guard-supplies-token", client)
	} else {
		pin, err := calendartest.EnsureGraphCalendar(ctx, source, "")
		if err != nil {
			t.Fatal(err)
		}
		guard, err := calendartest.NewGraphGuard(pin, prefix)
		if err != nil {
			t.Fatal(err)
		}
		recorder.CalendarID, recorder.Next = pin.ID(), guard
		client := guard.HTTPClient()
		client.Transport = recorder
		p = graphcalendar.NewWithToken(os.Getenv("DURIAN_MS_TEST_EMAIL"), calendartest.GraphBase, "guard-supplies-token", client)
		t.Cleanup(func() {
			if len(guard.UnknownTransactions()) != 0 {
				t.Error("unknown Graph create outcome retained; no automatic resend")
			}
		})
	}
	t.Cleanup(func() {
		if err := recorder.Save(path); err != nil {
			t.Error("save sanitized monthly recording failed")
		}
		t.Logf("recorded %d scoped %s exchanges", len(recorder.Recording.Exchanges), provider)
	})
	exerciseMonthlyCalendarSync(t, p, recorder.CalendarID, prefix, provider == "graph")
}
