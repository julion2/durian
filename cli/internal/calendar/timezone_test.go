package calendar

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestZonedAndFloatingSeriesRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, start, end, rule, extra string
		want                          []string
	}{
		{"spring", ";TZID=Europe/Berlin:20260322T090000", ";TZID=Europe/Berlin:20260322T100000", "FREQ=WEEKLY;COUNT=3", "", []string{"2026-03-22T08:00:00Z", "2026-03-29T07:00:00Z", "2026-04-05T07:00:00Z"}},
		{"autumn", ";TZID=Europe/Berlin:20261018T090000", ";TZID=Europe/Berlin:20261018T100000", "FREQ=WEEKLY;COUNT=3", "", []string{"2026-10-18T07:00:00Z", "2026-10-25T08:00:00Z", "2026-11-01T08:00:00Z"}},
		{"gap skips count", ";TZID=Europe/Berlin:20260322T023000", ";TZID=Europe/Berlin:20260322T033000", "FREQ=WEEKLY;COUNT=3", "", []string{"2026-03-22T01:30:00Z", "2026-04-05T00:30:00Z", "2026-04-12T00:30:00Z"}},
		{"first fold", ";TZID=Europe/Berlin:20261018T023000", ";TZID=Europe/Berlin:20261018T033000", "FREQ=WEEKLY;COUNT=2", "", []string{"2026-10-18T00:30:00Z", "2026-10-25T00:30:00Z"}},
		{"explicit second fold", ":20261025T013000Z", ":20261025T023000Z", "FREQ=WEEKLY;COUNT=2", "X-DURIAN-START-TZID:Europe/Berlin\r\nX-DURIAN-END-TZID:Europe/Berlin\r\n", []string{"2026-10-25T01:30:00Z", "2026-11-01T01:30:00Z"}},
		{"cancel after DST", ";TZID=Europe/Berlin:20260322T090000", ";TZID=Europe/Berlin:20260322T100000", "FREQ=WEEKLY;COUNT=3", "EXDATE;TZID=Europe/Berlin:20260329T090000\r\n", []string{"2026-03-22T08:00:00Z", "2026-04-05T07:00:00Z"}},
		{"exact until", ";TZID=Europe/Berlin:20260322T090000", ";TZID=Europe/Berlin:20260322T100000", "FREQ=WEEKLY;UNTIL=20260329T065959Z", "", []string{"2026-03-22T08:00:00Z"}},
		{"floating", ":20260322T090000", ":20260322T101700", "FREQ=WEEKLY;COUNT=3", "", []string{"2026-03-22T09:00:00Z", "2026-03-29T09:00:00Z", "2026-04-05T09:00:00Z"}},
		{"UTC control", ":20260322T090000Z", ":20260322T100000Z", "FREQ=WEEKLY;COUNT=3", "", []string{"2026-03-22T09:00:00Z", "2026-03-29T09:00:00Z", "2026-04-05T09:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:test\r\nBEGIN:VEVENT\r\nUID:tz\r\nDTSTAMP:20260101T000000Z\r\nDTSTART" + tc.start + "\r\nDTEND" + tc.end + "\r\nRRULE:" + tc.rule + "\r\n" + tc.extra + "END:VEVENT\r\nEND:VCALENDAR\r\n")
			for pass := 0; pass < 3; pass++ {
				e, err := ICalToEvent(data, "")
				if err != nil {
					t.Fatal(err)
				}
				// A real JSON mirror restart loses time.Location pointers. Metadata
				// must be sufficient to restore the same recurrence without them.
				encoded, _ := json.Marshal(e)
				if err := json.Unmarshal(encoded, &e); err != nil {
					t.Fatal(err)
				}
				got := starts(ExpandOccurrences(e, mustTime(t, "2026-01-01T00:00:00Z"), mustTime(t, "2027-01-01T00:00:00Z")))
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("pass %d: got %v, want %v", pass, got, tc.want)
				}
				if e.Floating != (tc.name == "floating") {
					t.Fatalf("floating=%v", e.Floating)
				}
				data, err = EventToICal(e)
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFloatingWindowAndFoldIdentity(t *testing.T) {
	floating := Event{ICalUID: "clock", Floating: true, Start: mustTime(t, "2026-10-25T00:15:00Z"), End: mustTime(t, "2026-10-25T01:00:00Z")}
	exported, err := ICalToEvent([]byte(EventToICS(floating, "test")), "")
	if err != nil || !exported.Floating || !exported.Start.Equal(floating.Start) || !exported.End.Equal(floating.End) {
		t.Fatalf("export changed floating semantics: %+v / %v", exported, err)
	}
	for _, offset := range []string{"+02:00", "-04:00"} {
		got := ExpandOccurrences(floating, mustTime(t, "2026-10-25T00:00:00"+offset), mustTime(t, "2026-10-26T00:00:00"+offset))
		if len(got) != 1 {
			t.Fatalf("floating local day %s: %v", offset, got)
		}
	}
	for _, start := range []string{"2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z"} {
		e := Event{ICalUID: "fold", TimeZone: "Europe/Berlin", Start: mustTime(t, start), End: mustTime(t, start).Add(30 * time.Minute)}
		data, err := EventToICal(e)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ICalToEvent(data, "")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Start.Equal(e.Start) || !got.End.Equal(e.End) || got.TimeZone != e.TimeZone {
			t.Fatalf("fold lost: %+v\n%s", got, data)
		}
	}
	bad := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:bad\r\nDTSTART;TZID=Unknown/Zone:20261025T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	if _, err := ICalToEvent(bad, ""); err == nil || !strings.Contains(err.Error(), "timezone") {
		t.Fatalf("unknown zone error: %v", err)
	}
}
