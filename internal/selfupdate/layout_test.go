package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/paths"
)

func TestActivateSwapsSymlink(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.versions, "0.2.1"), "old", 0o755)
	writeFile(t, filepath.Join(e.versions, "0.3.0"), "new", 0o755)
	link := filepath.Join(e.bin, "seasoning")

	if _, _, err := ActiveLink(); !isNotManaged(err) {
		t.Fatalf("before install: err = %v", err)
	}
	if err := Activate(link, "0.2.1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != filepath.Join(e.versions, "0.2.1") {
		t.Fatalf("link = %q", got)
	}
	if err := Activate(link, "0.3.0"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != filepath.Join(e.versions, "0.3.0") {
		t.Fatalf("link = %q", got)
	}
	if body := readFile(t, link); body != "new" {
		t.Fatalf("through link = %q", body)
	}
	if l, v, err := ActiveLink(); err != nil || l != link || v != "0.3.0" {
		t.Fatalf("ActiveLink = %q, %q, %v", l, v, err)
	}
	if left := dotFiles(t, e.bin); len(left) != 0 {
		t.Fatalf("temp links left behind: %v", left)
	}
	// Both version files are untouched by the swap.
	if readFile(t, filepath.Join(e.versions, "0.2.1")) != "old" {
		t.Fatal("old version changed")
	}
}

func TestActivateReplacesPreV2File(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.versions, "0.3.0"), "new", 0o755)
	link := filepath.Join(e.bin, "seasoning")
	writeFile(t, link, "copied binary", 0o755)

	if _, _, err := ActiveLink(); !isNotManaged(err) || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("regular file: err = %v", err)
	}
	if err := Activate(link, "0.3.0"); err != nil {
		t.Fatal(err)
	}
	if l, v, err := ActiveLink(); err != nil || l != link || v != "0.3.0" {
		t.Fatalf("ActiveLink = %q, %q, %v", l, v, err)
	}
}

func TestActivateErrors(t *testing.T) {
	e := newEnv(t)
	link := filepath.Join(e.bin, "seasoning")
	if err := Activate(link, "0.9.9"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing version: err = %v", err)
	}
	if err := Activate(link, "../x"); err == nil || !strings.Contains(err.Error(), "invalid version") {
		t.Fatalf("bad name: err = %v", err)
	}
	writeFile(t, filepath.Join(e.versions, "0.3.0"), "new", 0o755)
	other := filepath.Join(e.bin, "claude")
	writeFile(t, other, "not ours", 0o755)
	if err := Activate(other, "0.3.0"); err == nil || !strings.Contains(err.Error(), "not a seasoning command") {
		t.Fatalf("foreign path: err = %v", err)
	}
	if readFile(t, other) != "not ours" {
		t.Fatal("foreign file replaced")
	}
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Activate(link, "0.3.0"); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("dir at link: err = %v", err)
	}
}

func TestActiveLinkOutsideVersionsDir(t *testing.T) {
	e := newEnv(t)
	elsewhere := filepath.Join(e.home, "go", "bin", "seasoning")
	writeFile(t, elsewhere, "x", 0o755)
	if err := os.MkdirAll(e.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(e.bin, "seasoning")); err != nil {
		t.Fatal(err)
	}
	_, _, err := ActiveLink()
	if !isNotManaged(err) || !strings.Contains(err.Error(), "outside "+e.versions) {
		t.Fatalf("err = %v", err)
	}
}

func TestActiveLinkRelativeAndDangling(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.versions, 0o755); err != nil {
		t.Fatal(err)
	}
	// Relative target, file already pruned: still ours, update repairs it.
	if err := os.Symlink("../versions/0.1.0", filepath.Join(e.bin, "seasoning")); err != nil {
		t.Fatal(err)
	}
	if _, v, err := ActiveLink(); err != nil || v != "0.1.0" {
		t.Fatalf("ActiveLink = %q, %v", v, err)
	}
}

func TestPrune(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// Oldest first.
	names := []string{"dev", "0.1.0", "0.2.0", "0.3.0", "0.4.0", "0.5.0", "0.6.0"}
	for i, n := range names {
		p := filepath.Join(e.versions, n)
		writeFile(t, p, n, 0o755)
		mt := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	// Not versions or not regular files: never touched.
	writeFile(t, filepath.Join(e.versions, "notes.txt"), "x", 0o644)
	writeFile(t, filepath.Join(e.versions, ".download-123.tar.gz"), "x", 0o644)
	if err := os.MkdirAll(filepath.Join(e.versions, "0.0.1"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Active: the oldest release. Running: 0.2.0.
	link := filepath.Join(e.bin, "seasoning")
	if err := Activate(link, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	paths.Executable = func() (string, error) { return filepath.Join(e.versions, "0.2.0"), nil }

	removed, err := Prune(KeepVersions, link)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(removed)
	if got := strings.Join(removed, ","); got != "0.3.0,dev" {
		t.Fatalf("removed = %q", got)
	}
	entries, _ := os.ReadDir(e.versions)
	var left []string
	for _, en := range entries {
		left = append(left, en.Name())
	}
	if got := strings.Join(left, ","); got != ".download-123.tar.gz,0.0.1,0.1.0,0.2.0,0.4.0,0.5.0,0.6.0,notes.txt" {
		t.Fatalf("left = %q", got)
	}

	// Idempotent.
	if removed, err := Prune(KeepVersions, link); err != nil || len(removed) != 0 {
		t.Fatalf("second prune = %v, %v", removed, err)
	}
}

func TestPruneEdgeCases(t *testing.T) {
	newEnv(t)
	if _, err := Prune(0, ""); err == nil {
		t.Fatal("keep=0 accepted")
	}
	if removed, err := Prune(3, ""); err != nil || removed != nil {
		t.Fatalf("missing versions dir: %v, %v", removed, err)
	}
}

// A link made with SEASONING_BIN_DIR=X is found through PATH once that env
// var is gone, and activating swaps that link, not ~/.local/bin/seasoning.
func TestActiveLinkFindsLinkOnPath(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.versions, "0.2.1"), "old", 0o755)
	writeFile(t, filepath.Join(e.versions, "0.3.0"), "new", 0o755)
	custom := filepath.Join(e.home, "tools")
	link := filepath.Join(custom, "seasoning")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.versions, "0.2.1"), link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.EnvBinDir, "")
	t.Setenv("PATH", filepath.Join(e.home, "empty")+string(filepath.ListSeparator)+custom)
	paths.Executable = func() (string, error) { return filepath.Join(e.versions, "0.2.1"), nil }

	l, v, err := ActiveLink()
	if err != nil || l != link || v != "0.2.1" {
		t.Fatalf("ActiveLink = %q, %q, %v", l, v, err)
	}
	if err := Activate(l, "0.3.0"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != filepath.Join(e.versions, "0.3.0") {
		t.Fatalf("link = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(e.home, ".local", "bin", "seasoning")); !os.IsNotExist(err) {
		t.Fatalf("a second link was created in ~/.local/bin (%v)", err)
	}
	// Pruning after the switch keeps the new target of that link.
	if err := os.Chtimes(filepath.Join(e.versions, "0.3.0"), time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if removed, err := Prune(1, link); err != nil || len(removed) != 0 {
		t.Fatalf("Prune = %v, %v", removed, err)
	}
}

func TestActiveLinkFallsBackToBinLink(t *testing.T) {
	e := newEnv(t)
	// The running binary is some other copy with its own (unmanaged) link on
	// PATH; the managed link at BinLink is still what update switches.
	other := filepath.Join(e.home, "opt", "seasoning-copy")
	writeFile(t, other, "copy", 0o755)
	onPath := filepath.Join(e.home, "path")
	if err := os.MkdirAll(onPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(onPath, "seasoning")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath)
	paths.Executable = func() (string, error) { return other, nil }

	// No managed link anywhere: the refusal names the command being run.
	_, _, err := ActiveLink()
	if !isNotManaged(err) || !strings.Contains(err.Error(), filepath.Join(onPath, "seasoning")+" points to "+other) {
		t.Fatalf("err = %v", err)
	}

	writeFile(t, filepath.Join(e.versions, "0.2.1"), "old", 0o755)
	bin := filepath.Join(e.bin, "seasoning")
	if err := Activate(bin, "0.2.1"); err != nil {
		t.Fatal(err)
	}
	if l, v, err := ActiveLink(); err != nil || l != bin || v != "0.2.1" {
		t.Fatalf("ActiveLink = %q, %q, %v", l, v, err)
	}
}

func isNotManaged(err error) bool {
	var nm *NotManagedError
	return errors.As(err, &nm)
}
