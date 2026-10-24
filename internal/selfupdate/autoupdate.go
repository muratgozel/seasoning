package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/version"
)

// LockFile in paths.StateDir() keeps concurrent background processes from
// checking and installing at the same time.
const LockFile = "update.lock"

// Vars so tests can shrink them.
var (
	// autoUpdateBudget bounds one background check plus install. A slow link
	// needs minutes for the archive, which is why callers run this outside
	// the claim-sync lock and pass a context without their own short budget.
	autoUpdateBudget = 5 * time.Minute
	// lockStale is when an update lock counts as left behind by a dead
	// process. It must exceed autoUpdateBudget, or a slow but live install
	// would lose its lock and a second process would install concurrently.
	lockStale = 10 * time.Minute
)

// AutoUpdateIfDue is the daily background update for detached processes
// (send-usage upkeep, claim-sync). At most once per checkInterval (the
// checked_at of update-check.json) and under LockFile, it looks up the
// latest release and records it for Hint; then, when auto-update is on
// (cfg.AutoUpdateEnabled), the command is a managed symlink (ActiveLink) to a
// release and the latest release is newer than it, it installs (download,
// checksum, `version` run), activates (atomic symlink swap) and prunes, and
// records the installed version so the next command announces it once.
//
// Processes already running keep their binary: versions are separate files
// and Prune never deletes the running one. Dev builds never check. A failed
// lookup leaves the cache alone, so the next background run retries it; any
// later failure is still stamped as a check, so a broken release or slow link
// is retried the next day rather than downloaded on every run. Errors are
// for the caller's errors.log and name no email.
func AutoUpdateIfDue(ctx context.Context, cfg *config.Config) error {
	if IsDevBuild(version.Version) {
		return nil
	}
	// A missing or corrupt cache just means "due"; the write below replaces it.
	if c, err := readCheck(); err == nil && c.fresh() {
		return nil
	}
	release, ok, err := acquireLock()
	if err != nil {
		return fmt.Errorf("auto-update: %w", err)
	}
	if !ok {
		return nil // another process is checking right now
	}
	defer release()
	prev, err := readCheck()
	if err == nil && prev.fresh() {
		return nil // that process finished between our first read and the lock
	}
	if err != nil {
		prev = checkCache{}
	}

	ctx, cancel := context.WithTimeout(ctx, autoUpdateBudget)
	defer cancel()
	tag, err := LatestTag(ctx)
	if errors.Is(err, ErrNoRelease) {
		// Recorded, so background runs back off for a day.
		tag, err = "", nil
	}
	if err != nil {
		return err
	}

	next := prev
	next.CheckedAt = now().UTC()
	next.Latest = DisplayVersion(tag)
	installed, installErr := autoInstall(ctx, cfg, tag)
	if installed != "" {
		next.Installed, next.InstalledAt, next.Notified = installed, now().UTC(), false
	}
	if err := saveCheck(next); err != nil {
		return errors.Join(installErr, err)
	}
	return installErr
}

// autoInstall installs and activates tag when auto-update is on, the
// command is managed and tag is newer than the active version. It returns
// the version it activated, "" when it changed nothing.
func autoInstall(ctx context.Context, cfg *config.Config, tag string) (string, error) {
	if tag == "" {
		return "", nil
	}
	on, err := cfg.AutoUpdateEnabled()
	if !on {
		return "", err // non-nil only for an invalid SEASONING_AUTO_UPDATE
	}
	link, active, err := ActiveLink()
	if err != nil {
		var nm *NotManagedError
		if errors.As(err, &nm) {
			// Installed some other way (copied binary, package manager):
			// Hint keeps pointing at `seasoning update`, as before.
			return "", nil
		}
		return "", fmt.Errorf("auto-update: %w", err)
	}
	// `make install` points the link at "dev"; never replace a local build
	// behind the developer's back, whatever binary spawned this process.
	if IsDevBuild(active) {
		return "", nil
	}
	ver := DisplayVersion(tag)
	cmp, err := CompareVersions(ver, active)
	if err != nil {
		return "", fmt.Errorf("auto-update: %w", err)
	}
	if cmp <= 0 {
		return "", nil
	}
	if _, err := Install(ctx, tag); err != nil {
		return "", fmt.Errorf("auto-update to %s: %w", ver, err)
	}
	if err := Activate(link, ver); err != nil {
		return "", fmt.Errorf("auto-update to %s: %w", ver, err)
	}
	if _, err := Prune(KeepVersions, link); err != nil {
		return ver, fmt.Errorf("auto-update to %s succeeded, but old versions could not be removed: %w", ver, err)
	}
	return ver, nil
}

// acquireLock takes LockFile with O_EXCL. ok=false means another live
// process holds it. The returned release is always safe to call; it removes
// the file only while it still holds this process's pid, so a holder whose
// lock was broken as stale (e.g. the machine slept mid-install) cannot
// delete its successor's lock.
func acquireLock() (release func(), ok bool, err error) {
	noop := func() {}
	p, err := statePath(LockFile)
	if err != nil {
		return noop, false, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return noop, false, fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	n := now()
	mine := []byte(strconv.Itoa(os.Getpid()) + "\n")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		switch {
		case err == nil:
			_, werr := f.Write(mine)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(p)
				return noop, false, fmt.Errorf("write %s: %w", p, werr)
			}
			// The mtime is the lock's clock; stamping it from now() lets tests
			// with a frozen clock observe staleness.
			_ = os.Chtimes(p, n, n)
			return func() {
				if b, err := os.ReadFile(p); err == nil && bytes.Equal(b, mine) {
					os.Remove(p)
				}
			}, true, nil
		case !errors.Is(err, os.ErrExist):
			return noop, false, fmt.Errorf("create %s: %w", p, err)
		}
		fi, statErr := os.Stat(p)
		if statErr != nil {
			continue // released between the two calls: retry once
		}
		// Slightly in the future is normal: the holder created the file after
		// we read the clock. Far in the future means the clock moved back.
		if age := n.Sub(fi.ModTime()); age > -time.Minute && age < lockStale {
			return noop, false, nil
		}
		// Stale (or from the future after a clock change): the holder died.
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return noop, false, fmt.Errorf("remove stale %s: %w", p, err)
		}
	}
	return noop, false, nil // lost both races: someone else is updating
}
