package selfupdate

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/muratgozel/seasoning/internal/fsutil"
	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/version"
)

// CheckFile caches the latest known release, and the last auto-update, in
// paths.StateDir().
const CheckFile = "update-check.json"

// checkInterval is how often background processes look up the latest tag
// (and auto-update).
const checkInterval = 24 * time.Hour

// now is the clock; a var for tests.
var now = time.Now

// checkCache is update-check.json. The installed fields describe the last
// background auto-update and survive later checks until a manual `update`
// rewrites the file (RecordLatest).
type checkCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"` // display version ("0.3.0"); "" when nothing is published
	// Installed is the version the last auto-update installed and activated
	// ("" when none did); Notified is set once Hint has announced it.
	Installed   string    `json:"installed,omitempty"`
	InstalledAt time.Time `json:"installed_at,omitzero"`
	Notified    bool      `json:"notified,omitempty"`
}

func readCheck() (checkCache, error) {
	var c checkCache
	p, err := statePath(CheckFile)
	if err != nil {
		return c, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", p, err)
	}
	return c, nil
}

func saveCheck(c checkCache) error {
	p, err := statePath(CheckFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode %s: %w", CheckFile, err)
	}
	return fsutil.WriteFileAtomic(p, append(raw, '\n'), 0o600)
}

// RecordLatest stores tag as the latest release (e.g. after `update` resolved
// it). An empty tag records "nothing published". It replaces the whole file:
// a manual update supersedes any pending auto-update notice.
func RecordLatest(tag string) error {
	if tag != "" {
		if _, err := parseVersion(tag); err != nil {
			return fmt.Errorf("record latest release: %w", err)
		}
	}
	return saveCheck(checkCache{CheckedAt: now().UTC(), Latest: DisplayVersion(tag)})
}

// fresh reports whether the cache is younger than checkInterval. A
// checked_at in the future (clock moved back) counts as stale so a skewed
// clock cannot suppress checks indefinitely.
func (c checkCache) fresh() bool {
	age := now().Sub(c.CheckedAt)
	return age >= -time.Minute && age < checkInterval
}

// noticePending reports whether an auto-update to the running version cur
// has not been announced yet.
func (c checkCache) noticePending(cur string) bool {
	if c.Installed == "" || c.Notified {
		return false
	}
	cmp, err := CompareVersions(c.Installed, cur)
	return err == nil && cmp == 0
}

// autoInstalled reports whether the last auto-update already installed
// Latest (or something newer), so the next command runs it and an
// "is available" hint would be stale.
func (c checkCache) autoInstalled() bool {
	if c.Installed == "" {
		return false
	}
	cmp, err := CompareVersions(c.Installed, c.Latest)
	return err == nil && cmp >= 0
}

// Hint prints at most one line to w, from the cache only (never the
// network), and nothing for dev builds:
//
//   - "seasoning updated to 0.3.0" once, the first time the version a
//     background auto-update installed runs (then marked notified);
//   - "seasoning 0.3.0 is available (you have 0.2.1): seasoning update"
//     when a newer release is known that auto-update has not installed:
//     auto-update is off, the install is not managed, or the last attempt
//     failed (it retries the next day; `update` shows the error now).
func Hint(w io.Writer) {
	cur := version.Version
	if IsDevBuild(cur) {
		return
	}
	// Read errors (missing, unreadable or corrupt cache) are ignored on
	// purpose: the hint is purely cosmetic and must never fail or clutter an
	// interactive command. The next background check rewrites the file.
	c, err := readCheck()
	if err != nil {
		return
	}
	if c.noticePending(cur) {
		fmt.Fprintf(w, "seasoning updated to %s\n", DisplayVersion(cur))
		markNotified(cur)
		return
	}
	if c.Latest == "" || c.autoInstalled() {
		return
	}
	if cmp, err := CompareVersions(c.Latest, cur); err != nil || cmp <= 0 {
		return
	}
	fmt.Fprintf(w, "seasoning %s is available (you have %s): seasoning update\n", DisplayVersion(c.Latest), DisplayVersion(cur))
}

// markNotified records that the auto-update notice for cur was shown. It
// re-reads the file to keep the window small in which a background check
// writing at the same time is overwritten (no lock: the worst case is a
// notice shown twice or not at all). A failed write is ignored like every
// other Hint error; the notice then shows once more on the next command.
func markNotified(cur string) {
	c, err := readCheck()
	if err != nil || !c.noticePending(cur) {
		return
	}
	c.Notified = true
	_ = saveCheck(c)
}

// statePath is where the update cache and lock live (SPEC §15.1).
func statePath(name string) (string, error) {
	d, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name), nil
}
