package main

// The update notice: after a command typed at a terminal (`ygg status`,
// `ygg version`…), one line on stderr when a newer release than this ygg is
// out, with the `ygg upgrade` that moves this machine to it.
//
// The newest release is asked of GitHub at most every 6 hours and kept in
// ~/.cache/yggdrasil/latest-release.json, so most commands make no request.
// The ask runs beside the command and is waited for at most a second after
// it, so a slow or missing network never holds a command up. No notice for a
// ygg built from a checkout (its version is a commit, not a release), when
// stderr is not a terminal, under CI, or with YGG_NO_UPDATE_CHECK set.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"yggdrasil/internal/part"
	"yggdrasil/internal/release"
)

const (
	updateMaxAge = 6 * time.Hour
	updateAsk    = 3 * time.Second
	updateWait   = time.Second
)

// latestCache is what the cache file holds.
type latestCache struct {
	Tag       string    `json:"tag"`
	CheckedAt time.Time `json:"checkedAt"`
}

// updateCheck is one command's look for a newer release.
type updateCheck struct {
	version string
	done    chan struct{}
	latest  string
}

// wantUpdateCheck is whether this run may show the notice.
func wantUpdateCheck() bool {
	if !release.ValidVersion(part.Version) || os.Getenv("YGG_NO_UPDATE_CHECK") != "" || os.Getenv("CI") != "" {
		return false
	}
	info, err := os.Stderr.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// updateCachePath is the cache file, or "" when there is no cache folder.
func updateCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "yggdrasil", "latest-release.json")
}

// startUpdateCheck reads the cache and, when it is older than 6 hours, asks
// GitHub in the background. cachePath "" means no cache: always ask.
func startUpdateCheck(version, cachePath string, client *http.Client, now time.Time) *updateCheck {
	check := &updateCheck{version: version, done: make(chan struct{})}
	cached := readLatestCache(cachePath)
	if cached.Tag != "" && now.Sub(cached.CheckedAt) < updateMaxAge && !cached.CheckedAt.After(now) {
		check.latest = cached.Tag
		close(check.done)
		return check
	}
	go func() {
		defer close(check.done)
		ctx, cancel := context.WithTimeout(context.Background(), updateAsk)
		defer cancel()
		tag, err := latestVersion(ctx, client)
		if err != nil {
			// Offline or rate limited: the last answer stands, asked again in 6 hours.
			tag = cached.Tag
		}
		check.latest = tag
		writeLatestCache(cachePath, latestCache{Tag: tag, CheckedAt: now})
	}()
	return check
}

// finish waits at most wait for the ask, then writes the notice when a
// newer release is out.
func (c *updateCheck) finish(w io.Writer, wait time.Duration) {
	select {
	case <-c.done:
	case <-time.After(wait):
		return
	}
	if c.latest == "" || release.Compare(c.version, c.latest) >= 0 {
		return
	}
	fmt.Fprintf(w, "\nYggdrasil %s is out (this machine runs %s): ygg upgrade %s\n", c.latest, c.version, c.latest)
}

func readLatestCache(path string) latestCache {
	var cached latestCache
	if path == "" {
		return cached
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &cached) != nil || !release.ValidVersion(cached.Tag) {
		return latestCache{}
	}
	return cached
}

func writeLatestCache(path string, cached latestCache) {
	if path == "" {
		return
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// withUpdateNotice runs a typed command and shows the notice after it.
func withUpdateNotice(run func() int) int {
	if !wantUpdateCheck() {
		return run()
	}
	check := startUpdateCheck(part.Version, updateCachePath(), &http.Client{Timeout: updateAsk}, time.Now())
	code := run()
	check.finish(os.Stderr, updateWait)
	return code
}
