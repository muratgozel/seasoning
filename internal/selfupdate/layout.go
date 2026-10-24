package selfupdate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/muratgozel/seasoning/internal/paths"
)

// NotManagedError means no seasoning command symlink points into
// VersionsDir, so this copy of seasoning was installed some other way and
// `update` must not touch it.
type NotManagedError struct {
	Link   string
	Reason string
}

func (e *NotManagedError) Error() string { return e.Link + " " + e.Reason }

// ActiveLink returns the seasoning command symlink that `update` switches
// and the version it points to (its file name in VersionsDir).
//
// It first tries paths.FindLink, the link that resolves to the running
// binary ($SEASONING_BIN_DIR, ~/.local/bin, then PATH): an install made
// with SEASONING_BIN_DIR set is usually updated without it. Then
// paths.BinLink, where a dangling link into VersionsDir still counts
// (activating a version repairs it). *NotManagedError when neither is a
// symlink into VersionsDir; it names the FindLink link when there is one,
// since that is the command being run.
func ActiveLink() (link, ver string, err error) {
	vdir, err := paths.VersionsDir()
	if err != nil {
		return "", "", err
	}
	var found error
	if l, ok := paths.FindLink(); ok {
		v, err := managedVersion(l, vdir)
		if err == nil {
			return l, v, nil
		}
		var nm *NotManagedError
		if !errors.As(err, &nm) {
			return "", "", err
		}
		found = err
	}
	bl, err := paths.BinLink()
	if err != nil {
		return "", "", err
	}
	v, err := managedVersion(bl, vdir)
	if err == nil {
		return bl, v, nil
	}
	var nm *NotManagedError
	if found != nil && errors.As(err, &nm) {
		return "", "", found
	}
	return "", "", err
}

// managedVersion returns the version file name link points to, or
// *NotManagedError when link is not a symlink into vdir.
func managedVersion(link, vdir string) (string, error) {
	fi, err := os.Lstat(link)
	if errors.Is(err, fs.ErrNotExist) {
		return "", &NotManagedError{Link: link, Reason: "does not exist"}
	}
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", link, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return "", &NotManagedError{Link: link, Reason: "is a regular file, not a symlink into " + vdir + " (installed by hand or by an older installer)"}
	}
	target, err := os.Readlink(link)
	if err != nil {
		return "", fmt.Errorf("read symlink %s: %w", link, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	target = filepath.Clean(target)
	if !sameDir(filepath.Dir(target), vdir) {
		return "", &NotManagedError{Link: link, Reason: "points to " + target + ", outside " + vdir}
	}
	name := filepath.Base(target)
	if !validVersionName(name) {
		return "", &NotManagedError{Link: link, Reason: "points to " + target + ", which is not a seasoning version file"}
	}
	return name, nil
}

func sameDir(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// Activate atomically points link (the command symlink from ActiveLink) at
// VersionsDir/<ver>: a temp symlink next to the link is renamed over it, so
// the command never disappears. An existing regular file at link (pre-v2
// install) is replaced.
func Activate(link, ver string) error {
	if !validVersionName(ver) {
		return fmt.Errorf("invalid version %q", ver)
	}
	// Guards the rename below from replacing anything but a seasoning command.
	if filepath.Base(link) != "seasoning" {
		return fmt.Errorf("refusing to replace %q: not a seasoning command path", link)
	}
	vdir, err := paths.VersionsDir()
	if err != nil {
		return err
	}
	target, err := filepath.Abs(filepath.Join(vdir, ver))
	if err != nil {
		return fmt.Errorf("resolve %s: %w", ver, err)
	}
	fi, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("version %s is not installed (%s is missing)", ver, target)
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", target, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", target)
	}

	bdir := filepath.Dir(link)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", bdir, err)
	}
	if li, err := os.Lstat(link); err == nil && li.IsDir() {
		return fmt.Errorf("%s is a directory; remove it and retry", link)
	}
	tmp := filepath.Join(bdir, ".seasoning."+randSuffix()+".tmp")
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("create symlink in %s: %w", bdir, err)
	}
	// rename(2) replaces the link itself, never what it points to.
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("switch %s to %s: %w", link, ver, err)
	}
	return nil
}

// Prune deletes version files beyond the keep most recent (by mtime), never
// the target of link (the command symlink from ActiveLink), of BinLink, nor
// the running binary. Only names that look like versions are considered.
// Returns the removed names.
func Prune(keep int, link string) ([]string, error) {
	if keep < 1 {
		return nil, fmt.Errorf("prune: keep must be at least 1, got %d", keep)
	}
	vdir, err := paths.VersionsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(vdir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", vdir, err)
	}

	var protected []os.FileInfo
	keepTargets := []string{link}
	if bl, err := paths.BinLink(); err == nil {
		keepTargets = append(keepTargets, bl)
	}
	if exe, err := paths.Executable(); err == nil {
		keepTargets = append(keepTargets, exe)
	}
	for _, p := range keepTargets {
		if p == "" {
			continue
		}
		if fi, err := os.Stat(p); err == nil {
			protected = append(protected, fi)
		}
	}

	type cand struct {
		name string
		fi   os.FileInfo
	}
	var cands []cand
	for _, e := range entries {
		if !validVersionName(e.Name()) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(vdir, e.Name()))
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		cands = append(cands, cand{e.Name(), fi})
	}
	sort.Slice(cands, func(i, j int) bool {
		ti, tj := cands[i].fi.ModTime(), cands[j].fi.ModTime()
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return cands[i].name > cands[j].name
	})

	var removed []string
	var errs []error
	for i, c := range cands {
		if i < keep || isAny(c.fi, protected) {
			continue
		}
		p := filepath.Join(vdir, c.name)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove old version %s: %w", p, err))
			continue
		}
		removed = append(removed, c.name)
	}
	return removed, errors.Join(errs...)
}

func isAny(fi os.FileInfo, set []os.FileInfo) bool {
	for _, p := range set {
		if os.SameFile(fi, p) {
			return true
		}
	}
	return false
}
