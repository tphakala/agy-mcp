package manager

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// loadCache reads agy's conversation cache. A missing file is a normal empty
// cache (fresh agy install, no conversations yet). A read or parse failure is
// reported: agy rewrites the file in place (O_TRUNC, no lock), so a concurrent
// read can be torn, and callers must not mistake a torn read for "no entry"
// when the difference matters.
func loadCache(cacheFile string) (map[string]string, error) {
	b, err := os.ReadFile(cacheFile)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse conversation cache %s: %w", cacheFile, err)
	}
	return raw, nil
}

// loadCacheRetry reads the cache, retrying once on failure: torn reads are
// transient (agy's rewrite completes in microseconds), so an immediate re-read
// usually observes a complete file.
func loadCacheRetry(cacheFile string) (map[string]string, error) {
	cache, err := loadCache(cacheFile)
	if err == nil {
		return cache, nil
	}
	return loadCache(cacheFile)
}

// resolveLatest returns the most recent conversation UUID for cwd, if any. An
// unreadable cache resolves to not-found: continue_latest then starts a fresh
// conversation, the documented fallback for "no prior conversation".
func resolveLatest(cacheFile, cwd string) (string, bool) {
	cache, err := loadCacheRetry(cacheFile)
	if err != nil {
		return "", false
	}
	id, ok := cache[cwd]
	return id, ok && id != ""
}

// snapshotCwd records the cwd's UUID before a run. ok=false means the snapshot
// could not be taken (torn or corrupt cache even after a retry); the caller
// must disable capture for the run rather than guess, because an empty-by-error
// snapshot would make a pre-existing conversation look newly created.
func snapshotCwd(cacheFile, cwd string) (string, bool) {
	cache, err := loadCacheRetry(cacheFile)
	if err != nil {
		return "", false
	}
	return cache[cwd], true
}

// captureNewUUID returns the cwd's UUID after a run, but only if it changed
// from the pre-run snapshot. A failed run leaves the cache untouched, and a
// torn read yields no capture, so a single call never misattributes an old
// conversation to this run. The completion-goroutine call site retries in a
// loop; the lazy-capture path is best-effort single-shot (re-tried across Status
// polls), so no internal retry is needed here.
func captureNewUUID(cacheFile, cwd, before string) (string, bool) {
	cache, err := loadCache(cacheFile)
	if err != nil {
		return "", false
	}
	after := cache[cwd]
	if after != "" && after != before {
		return after, true
	}
	return "", false
}
