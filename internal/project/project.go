// Package project maps a working directory to a configured project (SPEC
// §5.1): by any of the repository's normalized remotes first, then by path
// (the git common dir, so every worktree resolves the same, or a linked
// folder for directories outside git).
package project

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/gitremote"
)

// Target describes a directory for mapping purposes.
type Target struct {
	Dir     string // as given, absolute
	InRepo  bool
	Repo    gitremote.Repo
	Remotes []string // normalized, distinct
	// Path is the path key: the git common dir inside a repository,
	// otherwise the directory itself (symlinks resolved).
	Path string
}

// Inspect describes dir.
func Inspect(ctx context.Context, dir string) (Target, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Target{}, fmt.Errorf("resolve %s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Target{}, fmt.Errorf("%s: %w", abs, err)
	}
	if !info.IsDir() {
		return Target{}, fmt.Errorf("%s is not a directory", abs)
	}
	t := Target{Dir: abs}
	repo, ok, err := gitremote.Inspect(ctx, abs)
	if err != nil {
		return Target{}, fmt.Errorf("read git repository at %s: %w", abs, err)
	}
	if ok {
		t.InRepo = true
		t.Repo = repo
		t.Remotes = repo.Normalized()
		t.Path = repo.CommonDir
		return t, nil
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		t.Path = real
	} else {
		t.Path = abs
	}
	return t, nil
}

// Describe says what the target maps by, for messages.
func (t Target) Describe() string {
	switch {
	case len(t.Remotes) > 0:
		return strings.Join(t.Remotes, ", ")
	case t.InRepo:
		return "the repository at " + t.Repo.TopLevel + " (no remotes; linked by path)"
	default:
		return "the folder " + t.Path
	}
}

// DefaultName suggests a project name: the last path segment of the first
// remote, else the repository or folder name.
func (t Target) DefaultName() string {
	if len(t.Remotes) > 0 {
		r := t.Remotes[0]
		return r[strings.LastIndexByte(r, '/')+1:]
	}
	if t.InRepo {
		return filepath.Base(t.Repo.TopLevel)
	}
	return filepath.Base(t.Path)
}

// Match is a resolved project and what matched.
type Match struct {
	Project *config.Project
	Via     string // "remote <id>" or "path <path>"
}

// Resolve finds the project t maps to, or nil.
func Resolve(t Target, ps *config.Projects) *Match {
	for _, r := range t.Remotes {
		for i := range ps.Projects {
			if slices.Contains(ps.Projects[i].Repos, r) {
				return &Match{Project: &ps.Projects[i], Via: "remote " + r}
			}
		}
	}
	for i := range ps.Projects {
		for _, p := range ps.Projects[i].Paths {
			if p == t.Path || (!t.InRepo && within(t.Path, p)) {
				return &Match{Project: &ps.Projects[i], Via: "path " + p}
			}
		}
	}
	return nil
}

// within reports whether path is inside dir.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Link adds t's mapping keys to p: its remotes, or its path when it has
// none. It returns what was added.
func Link(p *config.Project, t Target) []string {
	var added []string
	if len(t.Remotes) > 0 {
		for _, r := range t.Remotes {
			if !slices.Contains(p.Repos, r) {
				p.Repos = append(p.Repos, r)
				added = append(added, r)
			}
		}
		return added
	}
	if !slices.Contains(p.Paths, t.Path) {
		p.Paths = append(p.Paths, t.Path)
		added = append(added, t.Path)
	}
	return added
}

// Unlink removes t's mapping keys from every project and returns what was
// removed, per project ID.
func Unlink(ps *config.Projects, t Target) map[string][]string {
	removed := map[string][]string{}
	for i := range ps.Projects {
		p := &ps.Projects[i]
		p.Repos = slices.DeleteFunc(p.Repos, func(r string) bool {
			if slices.Contains(t.Remotes, r) {
				removed[p.ID] = append(removed[p.ID], r)
				return true
			}
			return false
		})
		p.Paths = slices.DeleteFunc(p.Paths, func(x string) bool {
			if x == t.Path {
				removed[p.ID] = append(removed[p.ID], x)
				return true
			}
			return false
		})
	}
	return removed
}
