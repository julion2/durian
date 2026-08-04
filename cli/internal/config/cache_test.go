package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type cachePayload struct {
	Value string `json:"value"`
}

// cacheProbe wires a source file to a counting eval so tests can assert cache
// hits (eval not called) vs misses (eval called). Each load fills got with the
// source file's current content.
type cacheProbe struct {
	src   string
	calls int
	got   cachePayload
}

func newCacheProbe(t *testing.T, name, content string) *cacheProbe {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	src := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return &cacheProbe{src: src}
}

func (p *cacheProbe) load(t *testing.T) {
	t.Helper()
	err := cachedLoad(p.src, &p.got, func() error {
		p.calls++
		data, err := os.ReadFile(p.src)
		if err != nil {
			return err
		}
		p.got = cachePayload{Value: string(data)}
		return nil
	})
	if err != nil {
		t.Fatalf("cachedLoad: %v", err)
	}
}

func (p *cacheProbe) rewrite(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(p.src, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCachedLoadMissThenHit(t *testing.T) {
	p := newCacheProbe(t, "config.pkl", "content-A")
	p.load(t)
	p.load(t)
	if p.calls != 1 {
		t.Errorf("eval called %d times, want 1 (second load must hit the cache)", p.calls)
	}
	if p.got.Value != "content-A" {
		t.Errorf("got %q, want content-A", p.got.Value)
	}
}

func TestCachedLoadContentChangeInvalidates(t *testing.T) {
	p := newCacheProbe(t, "config.pkl", "content-A")
	p.load(t)
	p.rewrite(t, "content-B")
	p.load(t)
	if p.calls != 2 {
		t.Errorf("eval called %d times, want 2 (content change must miss)", p.calls)
	}
	if p.got.Value != "content-B" {
		t.Errorf("got %q, want content-B", p.got.Value)
	}
}

func TestCachedLoadStaleExecIDInvalidates(t *testing.T) {
	p := newCacheProbe(t, "config.pkl", "content-A")
	p.load(t) // writes a cache entry with the current execID

	// Rewrite the cache file keeping the source hash but a stale execID, as if
	// a different durian binary had produced it.
	cachePath := configCachePath(p.src)
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	entry.ExecID = "stale-binary"
	data, _ = json.Marshal(entry)
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	p.load(t)
	if p.calls != 2 {
		t.Errorf("eval called %d times, want 2 (stale execID must miss)", p.calls)
	}
}

func TestCachedLoadCorruptCacheFallsBack(t *testing.T) {
	p := newCacheProbe(t, "config.pkl", "content-A")
	p.load(t)
	if err := os.WriteFile(configCachePath(p.src), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.load(t)
	if p.calls != 2 {
		t.Errorf("eval called %d times, want 2 (corrupt cache must miss)", p.calls)
	}
	if p.got.Value != "content-A" {
		t.Errorf("got %q, want content-A", p.got.Value)
	}
}

func TestCachedLoadEnvDisable(t *testing.T) {
	p := newCacheProbe(t, "config.pkl", "content-A")
	t.Setenv(DisableCacheEnv, "1")
	p.load(t)
	p.load(t)
	if p.calls != 2 {
		t.Errorf("eval called %d times, want 2 (%s disables the cache)", p.calls, DisableCacheEnv)
	}
}

func TestCachedLoadJSONPathSkips(t *testing.T) {
	p := newCacheProbe(t, "config.json", "content-A")
	p.load(t)
	p.load(t)
	if p.calls != 2 {
		t.Errorf("eval called %d times, want 2 (.json path is not cached)", p.calls)
	}
}

func TestCachedLoadOnDiskImportBypasses(t *testing.T) {
	p := newCacheProbe(t, "config.pkl", "import \"sibling.pkl\" as S\nfoo = 1\n")
	p.load(t)
	p.load(t)
	if p.calls != 2 {
		t.Errorf("eval called %d times, want 2 (on-disk import bypasses the cache)", p.calls)
	}

	// A modulepath: import (the shipped-config case) must still be cached.
	q := newCacheProbe(t, "config.pkl", "import \"modulepath:/Config.pkl\" as C\nfoo = 1\n")
	q.load(t)
	q.load(t)
	if q.calls != 1 {
		t.Errorf("eval called %d times, want 1 (modulepath: import is cacheable)", q.calls)
	}
}

func TestHasOnDiskImport(t *testing.T) {
	cases := map[string]bool{
		`import "modulepath:/Config.pkl" as C`: false,
		`import "package://example.com/foo@1"`: false,
		`import "https://example.com/foo.pkl"`: false,
		`amends "modulepath:/Profiles.pkl"`:    false,
		`foo = 1`:                              false,
		`import "sibling.pkl"`:                 true,
		`  amends "../base.pkl"`:               true,
		`extends "shared.pkl"`:                 true,
	}
	for line, want := range cases {
		if got := hasOnDiskImport([]byte(line)); got != want {
			t.Errorf("hasOnDiskImport(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestCachedLoadGroupsFidelity(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	src := filepath.Join(t.TempDir(), "groups.pkl")
	if err := os.WriteFile(src, []byte("groups content"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := map[string]GroupEntry{
		"team": {Description: "The team", Members: [][]string{{"a@x.com"}, {"b@x.com", "b@y.com"}}},
	}

	var got map[string]GroupEntry
	eval := func() error {
		got = map[string]GroupEntry{
			"team": {Description: "The team", Members: [][]string{{"a@x.com"}, {"b@x.com", "b@y.com"}}},
		}
		return nil
	}
	if err := cachedLoad(src, &got, eval); err != nil { // miss
		t.Fatal(err)
	}
	got = nil
	if err := cachedLoad(src, &got, func() error { t.Fatal("eval must not run on a hit"); return nil }); err != nil { // hit
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cached groups = %+v, want %+v", got, want)
	}
}
