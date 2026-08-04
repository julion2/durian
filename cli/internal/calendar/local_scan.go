// Local .ics scan for the vdir: enumerate and parse the event files of one
// calendar collection directory, keyed by UID, with the file-byte hash the
// two-way sync engine uses as the local-side identity.

package calendar

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// LocalItem is one local .ics file found in the calendar dir.
type LocalItem struct {
	Path  string
	Hash  string
	Event Event
	Mtime time.Time
}

// HashBytes returns the SHA-256 hex digest used as the local file identity.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ScanLocalItems enumerates the *.ics files of calDir keyed by their parsed
// iCalendar UID, with the SHA-256 of the raw file bytes as identity (plus the
// parsed event and file mtime for uploads and conflict resolution). A missing
// dir yields an empty map (fresh calendar, or plan before first sync). Files
// that fail to parse, lack a UID or duplicate an already-seen UID are logged
// and skipped — they are invisible to the diff, never treated as items.
// Conflict backups (<file>.conflict-<ts>) do not end in .ics and are ignored.
// owner is the account email, threaded into ICalToEvent so the owner's RSVP
// is recognized in the parsed events.
//
// The second return value lists the files that were skipped because they
// could not be parsed into an identifiable event. They are indistinguishable
// from deletions in the map alone, and "deleted" is a destructive
// classification, so the sync planner needs to know they exist.
func ScanLocalItems(calDir, owner string) (map[string]LocalItem, []string, error) {
	entries, err := os.ReadDir(calDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]LocalItem{}, nil, nil
		}
		return nil, nil, fmt.Errorf("failed to read calendar dir %s: %w", calDir, err)
	}

	// Collect .ics entries in directory order; dedup happens after parsing so
	// "keep first" stays deterministic regardless of worker scheduling.
	type fileJob struct {
		path  string
		mtime time.Time
	}
	var jobs []fileJob
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ics") {
			continue
		}
		var mtime time.Time
		if info, err := entry.Info(); err == nil {
			mtime = info.ModTime()
		}
		jobs = append(jobs, fileJob{path: filepath.Join(calDir, entry.Name()), mtime: mtime})
	}

	// Parse files concurrently: go-ical decoding dominates the cost, so a
	// worker pool across cores turns a serial O(n) parse into ~O(n/cores).
	type parseResult struct {
		item    LocalItem
		ok      bool
		readErr error
		// unreadable is the path of a file that exists but yielded no usable
		// event (parse failure, or no UID to key it by).
		unreadable string
	}
	results := make([]parseResult, len(jobs))
	workers := runtime.NumCPU()
	if workers > len(jobs) {
		workers = len(jobs)
	}
	if workers > 0 {
		next := make(chan int, len(jobs))
		for i := range jobs {
			next <- i
		}
		close(next)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range next {
					data, err := os.ReadFile(jobs[i].path)
					if err != nil {
						results[i] = parseResult{readErr: fmt.Errorf("failed to read %s: %w", jobs[i].path, err)}
						continue
					}
					ev, err := ICalToEvent(data, owner)
					if err != nil {
						slog.Warn("Skipping unparseable local .ics", "module", "CALENDAR",
							"path", jobs[i].path, "err", err)
						results[i] = parseResult{unreadable: jobs[i].path}
						continue
					}
					if ev.ICalUID == "" {
						slog.Warn("Skipping local .ics without UID", "module", "CALENDAR", "path", jobs[i].path)
						results[i] = parseResult{unreadable: jobs[i].path}
						continue
					}
					results[i] = parseResult{item: LocalItem{Path: jobs[i].path, Hash: HashBytes(data), Event: ev, Mtime: jobs[i].mtime}, ok: true}
				}
			}()
		}
		wg.Wait()
	}

	items := make(map[string]LocalItem)
	var unreadable []string
	for i := range results {
		switch {
		case results[i].readErr != nil:
			return nil, nil, results[i].readErr
		case results[i].unreadable != "":
			unreadable = append(unreadable, results[i].unreadable)
			continue
		case !results[i].ok:
			continue
		}
		it := results[i].item
		if prev, dup := items[it.Event.ICalUID]; dup {
			slog.Warn("Skipping local .ics with duplicate UID", "module", "GRAPHCAL",
				"path", it.Path, "uid", it.Event.ICalUID, "kept", prev.Path)
			continue
		}
		items[it.Event.ICalUID] = it
	}
	return items, unreadable, nil
}
