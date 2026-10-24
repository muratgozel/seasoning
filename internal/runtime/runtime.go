// Package runtime manages a Claude Code process's runtime directory,
// StateDir/runtime/<claude pid>/ (SPEC §4.2, §9.5). The directory is keyed by
// the Claude process, not the session, because /clear starts a new session
// in the same process and the same MCP server.
//
//	binding.json  the process's current session, written by every SessionStart
//	joined        present while that session is joined to a project
//	unread        present while the session has messages not yet surfaced
//	wake          present while some of those are wake-eligible
//	waiter.pid    the one live asyncRewake waiter
//	problem       a one-line problem for the agent (relay down, project gone)
//	off           present while the user switched seasoning off for the process
//	woke          what the waiter woke the session with, for the human (the
//	              woken turn's UserPromptSubmit shows it, then removes it)
//
// Hooks on the hot path only stat these files. Writers create a marker after
// committing the rows it describes; clearers remove it and then re-check the
// database, recreating it if rows arrived meanwhile (see ClearChecked), so a
// wake-up is never lost.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/muratgozel/seasoning/internal/fsutil"
	"github.com/muratgozel/seasoning/internal/paths"
)

// Marker names.
const (
	Joined = "joined"
	Unread = "unread"
	Wake   = "wake"
	// Off keeps the process's sessions invisible (/seasoning:off) until
	// `seasoning session on`, across /clear.
	Off = "off"
)

// Dir is one Claude process's runtime directory.
type Dir struct {
	Path string
	PID  int
}

// For returns the runtime directory of Claude process pid.
func For(pid int) (Dir, error) {
	if pid <= 1 {
		return Dir{}, fmt.Errorf("invalid Claude Code process ID %d", pid)
	}
	base, err := paths.StateDir()
	if err != nil {
		return Dir{}, err
	}
	return Dir{Path: filepath.Join(base, "runtime", strconv.Itoa(pid)), PID: pid}, nil
}

// FromEnv returns the runtime directory named by CLAUDE_PID, which Claude
// Code sets for hooks (not for MCP servers, which use their parent PID).
func FromEnv(getenv func(string) string) (Dir, error) {
	pid, err := strconv.Atoi(getenv("CLAUDE_PID"))
	if err != nil {
		return Dir{}, errors.New("CLAUDE_PID is not set; seasoning hooks must run under Claude Code")
	}
	return For(pid)
}

// Binding is the process's current session.
type Binding struct {
	SessionID   string `json:"session_id"`
	Cwd         string `json:"cwd"`
	Source      string `json:"source"`
	Project     string `json:"project,omitempty"` // empty when not joined
	ProjectName string `json:"project_name,omitempty"`
	At          string `json:"at"`
}

func (d Dir) file(name string) string { return filepath.Join(d.Path, name) }

// WriteBinding replaces binding.json.
func (d Dir) WriteBinding(b Binding) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(d.file("binding.json"), data, 0o600)
}

// Binding reads binding.json; nil when there is none yet.
func (d Dir) Binding() (*Binding, error) {
	data, err := os.ReadFile(d.file("binding.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b Binding
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", d.file("binding.json"), err)
	}
	return &b, nil
}

// Has reports whether a marker exists: one stat, for the hot path.
func (d Dir) Has(marker string) bool {
	_, err := os.Stat(d.file(marker))
	return err == nil
}

// Set creates a marker (and the directory).
func (d Dir) Set(marker string) error {
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(d.file(marker), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	now := time.Now()
	f.Close()
	return os.Chtimes(d.file(marker), now, now) // bump mtime for pollers
}

// Clear removes a marker; a missing marker is fine.
func (d Dir) Clear(marker string) error {
	err := os.Remove(d.file(marker))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ClearChecked removes a marker, then asks still whether it should exist and
// recreates it if so. Use it whenever a writer may race the removal.
func (d Dir) ClearChecked(marker string, still func() (bool, error)) error {
	if err := d.Clear(marker); err != nil {
		return err
	}
	ok, err := still()
	if err != nil {
		return err
	}
	if ok {
		return d.Set(marker)
	}
	return nil
}

// SetProblem records a problem the agent should hear about.
func (d Dir) SetProblem(text string) error {
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(d.file("problem"), []byte(text), 0o600)
}

// Problem returns the current problem, or "".
func (d Dir) Problem() string {
	b, err := os.ReadFile(d.file("problem"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ClearProblem removes the problem note.
func (d Dir) ClearProblem() error { return d.Clear("problem") }

// SetWoke records what the waiter woke the session with. An asyncRewake
// hook can't show anything to the user itself, so the woken turn's
// UserPromptSubmit hook shows this instead.
func (d Dir) SetWoke(text string) error {
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(d.file("woke"), []byte(text), 0o600)
}

// TakeWoke returns and removes the wake note ("" when there is none).
func (d Dir) TakeWoke() string {
	b, err := os.ReadFile(d.file("woke"))
	if err != nil {
		return ""
	}
	os.Remove(d.file("woke"))
	return strings.TrimSpace(string(b))
}

// ForBase returns pid's runtime directory under an explicit state directory
// (tests, multi-session harnesses).
func ForBase(stateDir string, pid int) Dir {
	return Dir{Path: filepath.Join(stateDir, "runtime", strconv.Itoa(pid)), PID: pid}
}

// WriteWaiter records pid as the live waiter, superseding any other.
func (d Dir) WriteWaiter(pid int) error {
	return fsutil.WriteFileAtomic(d.file("waiter.pid"), []byte(strconv.Itoa(pid)), 0o600)
}

// Waiter returns the live waiter's PID, or 0.
func (d Dir) Waiter() int {
	b, err := os.ReadFile(d.file("waiter.pid"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// Remove deletes the directory.
func (d Dir) Remove() error { return os.RemoveAll(d.Path) }

// Sweep removes runtime directories of Claude processes that no longer run
// and haven't been touched for an hour.
func Sweep(now time.Time) error {
	base, err := paths.StateDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(base, "runtime"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() || Alive(pid) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < time.Hour {
			continue
		}
		os.RemoveAll(filepath.Join(base, "runtime", e.Name()))
	}
	return nil
}

// Alive reports whether process pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
