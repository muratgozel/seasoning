// Package paths resolves where seasoning keeps its files (SPEC §15.1). It uses
// XDG locations on macOS too: config under ${XDG_CONFIG_HOME:-~/.config},
// data under ${XDG_DATA_HOME:-~/.local/share} and state under
// ${XDG_STATE_HOME:-~/.local/state}, each in a "seasoning" subdirectory.
// SEASONING_* variables override each directory (tests, unusual layouts).
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Env overrides. They are read at runtime and never written to config.
const (
	EnvConfigDir   = "SEASONING_CONFIG_DIR"
	EnvDataDir     = "SEASONING_DATA_DIR"
	EnvStateDir    = "SEASONING_STATE_DIR"
	EnvBinDir      = "SEASONING_BIN_DIR"
	EnvVersionsDir = "SEASONING_VERSIONS_DIR"
)

// ConfigDir holds config.json, the identity key, projects.json and pins.
func ConfigDir() (string, error) {
	return xdg(EnvConfigDir, "XDG_CONFIG_HOME", ".config", "seasoning")
}

// DataDir holds local.db, the relay's default database and versions/.
func DataDir() (string, error) {
	return xdg(EnvDataDir, "XDG_DATA_HOME", filepath.Join(".local", "share"), "seasoning")
}

// StateDir holds the update cache, errors.log and per-session runtime files.
func StateDir() (string, error) {
	return xdg(EnvStateDir, "XDG_STATE_HOME", filepath.Join(".local", "state"), "seasoning")
}

// VersionsDir holds one binary file per installed version.
func VersionsDir() (string, error) {
	if d := os.Getenv(EnvVersionsDir); d != "" {
		return d, nil
	}
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "versions"), nil
}

// BinDir is where the stable `seasoning` symlink lives.
func BinDir() (string, error) {
	if d := os.Getenv(EnvBinDir); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// BinLink is the stable path of the seasoning command.
func BinLink() (string, error) {
	d, err := BinDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "seasoning"), nil
}

// Executable is the running binary with symlinks resolved; a var for tests.
var Executable = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate running executable: %w", err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, nil
	}
	return p, nil
}

// FindLink returns a `seasoning` symlink that resolves to the running
// binary: $SEASONING_BIN_DIR/seasoning, then ~/.local/bin/seasoning, then the
// first `seasoning` on PATH. The PATH scan matters because the installer
// honors SEASONING_BIN_DIR but later runs usually do not have it set.
func FindLink() (string, bool) {
	exe, err := Executable()
	if err != nil {
		return "", false
	}
	var candidates []string
	if l, err := BinLink(); err == nil {
		candidates = append(candidates, l)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "bin", "seasoning"))
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d != "" {
			candidates = append(candidates, filepath.Join(d, "seasoning"))
		}
	}
	for _, c := range candidates {
		fi, err := os.Lstat(c)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if target, err := filepath.EvalSymlinks(c); err == nil && sameFile(target, exe) {
			return c, true
		}
	}
	return "", false
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// xdg resolves override, then $xdgVar/sub, then ~/fallback/sub. Relative XDG
// values are ignored, as the XDG Base Directory spec requires.
func xdg(override, xdgVar, fallback, sub string) (string, error) {
	if d := os.Getenv(override); d != "" {
		return d, nil
	}
	if x := os.Getenv(xdgVar); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, sub), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, fallback, sub), nil
}
