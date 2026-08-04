package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file caches the result of evaluating a Pkl config file so the expensive
// pkl.NewEvaluator run (see pkl.go, ~1s per file) is skipped when nothing
// relevant changed. It is a pure optimization: any cache error falls back to a
// fresh evaluation, so a broken or stale cache can never change what a command
// sees.

// DisableCacheEnv is the environment variable that disables the config cache
// when set to a non-empty value — used by `durian validate` (which must always
// re-evaluate) and available for debugging.
const DisableCacheEnv = "DURIAN_NO_CONFIG_CACHE"

// cachedLoad populates out with the evaluated result for sourcePath, skipping
// the Pkl evaluation (eval) when a fresh cache entry exists. eval must fill out
// (it wraps the loadInto/normalization the caller would otherwise run).
//
// The cache key combines the source file's CONTENT hash and the durian binary's
// identity (see executableID), so it invalidates whenever the file content
// changes or the binary is rebuilt/reinstalled — the only ways the embedded
// schema or the Go structs behind out can change. It is bypassed for the .json
// test path, when the env opt-out is set, and for files that import an on-disk
// Pkl module (whose transitive inputs the key does not track).
func cachedLoad(sourcePath string, out any, eval func() error) error {
	data, readErr := os.ReadFile(sourcePath)
	if readErr != nil ||
		os.Getenv(DisableCacheEnv) != "" ||
		strings.HasSuffix(sourcePath, ".json") ||
		hasOnDiskImport(data) {
		return eval()
	}
	execID, ok := executableID()
	if !ok {
		return eval()
	}

	key := cacheKey{SourceHash: sha256Hex(data), ExecID: execID}
	cachePath := configCachePath(sourcePath)
	if payload, hit := readCacheEntry(cachePath, key); hit {
		if json.Unmarshal(payload, out) == nil {
			return nil
		}
		// A decodable entry with an undecodable payload: fall through and
		// rebuild it from a fresh evaluation.
	}

	if err := eval(); err != nil {
		return err
	}
	writeCacheEntry(cachePath, key, out)
	return nil
}

// cacheKey identifies the inputs a cache entry was produced from.
type cacheKey struct {
	SourceHash string `json:"source_hash"`
	ExecID     string `json:"exec_id"`
}

// cacheEntry is one on-disk cache file: the key plus the JSON-encoded value.
type cacheEntry struct {
	cacheKey
	Payload json.RawMessage `json:"payload"`
}

// readCacheEntry returns the cached payload when the file exists and its key
// matches want; otherwise ok is false (treated as a miss).
func readCacheEntry(path string, want cacheKey) (payload json.RawMessage, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry cacheEntry
	if json.Unmarshal(data, &entry) != nil {
		return nil, false
	}
	if entry.SourceHash != want.SourceHash || entry.ExecID != want.ExecID {
		return nil, false
	}
	return entry.Payload, true
}

// writeCacheEntry serializes out under key to path, atomically (temp + rename).
// All failures are silent — the cache is best-effort.
func writeCacheEntry(path string, key cacheKey, out any) {
	payload, err := json.Marshal(out)
	if err != nil {
		return
	}
	data, err := json.Marshal(cacheEntry{cacheKey: key, Payload: payload})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

// configCachePath returns the cache file for a source config file, named by a
// hash of its absolute path so different files (and -c overrides) never collide.
func configCachePath(sourcePath string) string {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		abs = sourcePath
	}
	return filepath.Join(cacheDir(), "config-cache", sha256Hex([]byte(abs))+".json")
}

// cacheDir resolves the durian cache directory (XDG_CACHE_HOME/durian, else
// ~/.cache/durian), mirroring the sync cursor/state stores.
func cacheDir() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "durian")
}

// executableID identifies the running durian binary by its mtime and size, so a
// rebuilt or reinstalled binary — carrying a possibly different embedded schema
// or different Go structs — invalidates every cache entry. It reports ok=false
// when the executable cannot be resolved, in which case the caller skips the
// cache rather than risk a stale hit.
func executableID() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	info, err := os.Stat(exe)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(info.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(info.Size(), 10), true
}

// hasOnDiskImport reports whether a Pkl source imports/amends/extends another
// Pkl module by an on-disk reference (anything other than the embedded
// modulepath:, a package: dependency or an https: URL). Such a file has
// transitive inputs the content-hash key does not track, so it bypasses the
// cache. The shipped configs only reference the embedded schema via
// modulepath:, so this never fires for them.
func hasOnDiskImport(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		for _, kw := range []string{"import ", "amends ", "extends "} {
			rest, ok := strings.CutPrefix(line, kw)
			if !ok {
				continue
			}
			rest = strings.TrimSpace(rest)
			if strings.HasPrefix(rest, `"`) &&
				!strings.Contains(rest, "modulepath:") &&
				!strings.Contains(rest, "package:") &&
				!strings.Contains(rest, "https:") {
				return true
			}
		}
	}
	return false
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
