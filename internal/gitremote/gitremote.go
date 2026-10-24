// Package gitremote reads a repository's remotes and normalizes their URLs
// to "host/path" (SPEC §5.2), so that every clone, fork and protocol of the
// same repository maps to the same local project lookup key. The normalized
// form never leaves the machine except inside encrypted presence.
package gitremote

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// caseInsensitiveHosts serve paths case-insensitively, so their paths are
// lowercased; other hosts keep the path as written.
var caseInsensitiveHosts = map[string]bool{
	"github.com":    true,
	"gitlab.com":    true,
	"dev.azure.com": true,
	"bitbucket.org": true,
}

// ErrLocal means the remote is a local path or file URL, which has no host
// to normalize; such repos are linked by path instead.
var ErrLocal = errors.New("remote is a local path")

// Normalize maps a remote URL to "host/path".
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("remote URL is empty")
	}
	host, path, err := split(raw)
	if err != nil {
		return "", err
	}
	host = strings.ToLower(host)
	host = strings.TrimPrefix(host, "www.")
	if host == "ssh.github.com" {
		host = "github.com"
	}
	path = strings.Trim(path, "/")
	host, path = azure(host, path)
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return "", fmt.Errorf("remote %q has no repository path", raw)
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("remote %q has an invalid path", raw)
		}
	}
	if caseInsensitiveHosts[host] {
		path = strings.ToLower(path)
	}
	return host + "/" + path, nil
}

// split returns the host and path of a URL-style or scp-style remote.
func split(raw string) (host, path string, err error) {
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", fmt.Errorf("remote %q is not a valid URL", raw)
		}
		switch u.Scheme {
		case "file":
			return "", "", ErrLocal
		case "ssh", "git", "http", "https", "git+ssh", "ssh+git", "git+https":
		default:
			return "", "", fmt.Errorf("remote %q uses unsupported scheme %q", raw, u.Scheme)
		}
		if u.Hostname() == "" {
			return "", "", fmt.Errorf("remote %q has no host", raw)
		}
		return u.Hostname(), u.Path, nil
	}
	// scp-like syntax, [user@]host:path, is recognized only when no slash
	// comes before the first colon (git's own rule).
	colon := strings.IndexByte(raw, ':')
	if colon <= 0 || strings.Contains(raw[:colon], "/") {
		return "", "", ErrLocal
	}
	host = raw[:colon]
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if host == "" {
		return "", "", fmt.Errorf("remote %q has no host", raw)
	}
	return host, raw[colon+1:], nil
}

// azure maps Azure DevOps' four URL shapes onto dev.azure.com/org/project/repo.
func azure(host, path string) (string, string) {
	segs := strings.Split(path, "/")
	switch {
	case host == "ssh.dev.azure.com" || host == "vs-ssh.visualstudio.com":
		if len(segs) == 4 && segs[0] == "v3" {
			return "dev.azure.com", strings.Join(segs[1:], "/")
		}
	case host == "dev.azure.com":
		if len(segs) == 4 && segs[2] == "_git" {
			return host, strings.Join([]string{segs[0], segs[1], segs[3]}, "/")
		}
	case strings.HasSuffix(host, ".visualstudio.com"):
		org := strings.TrimSuffix(host, ".visualstudio.com")
		if len(segs) > 0 && strings.EqualFold(segs[0], "DefaultCollection") {
			segs = segs[1:]
		}
		if len(segs) == 3 && segs[1] == "_git" {
			return "dev.azure.com", strings.Join([]string{org, segs[0], segs[2]}, "/")
		}
	}
	return host, path
}

// Remote is one configured remote of a repository.
type Remote struct {
	Name       string
	URL        string
	Normalized string // empty when Err is set
	Err        error
}

// Repo describes the git repository containing a directory.
type Repo struct {
	TopLevel  string // absolute, symlinks resolved
	CommonDir string // shared by every worktree; absolute, symlinks resolved
	Remotes   []Remote
}

// gitTimeout bounds each git call; hooks resolve projects on a hot path.
var gitTimeout = 3 * time.Second

// Inspect reads the repository at dir. ok is false when dir is not inside a
// git work tree.
func Inspect(ctx context.Context, dir string) (repo Repo, ok bool, err error) {
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return Repo{}, false, nil // not a repository
		}
		return Repo{}, false, err
	}
	common, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Repo{}, false, err
	}
	repo.TopLevel = realpath(top)
	repo.CommonDir = realpath(common)
	names, err := git(ctx, dir, "remote")
	if err != nil {
		return Repo{}, false, err
	}
	for _, name := range strings.Fields(names) {
		r := Remote{Name: name}
		// get-url applies insteadOf rewrites, unlike reading the config.
		r.URL, r.Err = git(ctx, dir, "remote", "get-url", name)
		if r.Err == nil {
			r.Normalized, r.Err = Normalize(r.URL)
		}
		repo.Remotes = append(repo.Remotes, r)
	}
	return repo, true, nil
}

// Normalized returns the distinct normalized remotes, in remote order.
func (r Repo) Normalized() []string {
	var out []string
	seen := map[string]bool{}
	for _, rem := range r.Remotes {
		if rem.Normalized != "" && !seen[rem.Normalized] {
			seen[rem.Normalized] = true
			out = append(out, rem.Normalized)
		}
	}
	return out
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// gitRaw is git without trimming, for machine formats where leading spaces
// are significant (git status --porcelain).
func gitRaw(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("git is not installed or not on PATH")
		}
		return "", err
	}
	return string(out), nil
}

func realpath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// Branch returns the checked-out branch, or "(detached)".
func Branch(ctx context.Context, dir string) (string, error) {
	b, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		// An unborn branch has no HEAD commit yet; ask for its name instead.
		if name, err2 := git(ctx, dir, "symbolic-ref", "--short", "HEAD"); err2 == nil {
			return name, nil
		}
		return "", err
	}
	if b == "HEAD" {
		return "(detached)", nil
	}
	return b, nil
}

// Changed lists repo-relative paths that differ from HEAD or are untracked,
// as `git status` sees them, within the given time budget.
func Changed(ctx context.Context, dir string, budget time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	out, err := gitRaw(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var paths []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		status, path := e[:2], e[3:]
		if status[0] == 'R' || status[0] == 'C' {
			i++ // a rename or copy is followed by its source path
		}
		paths = append(paths, path)
	}
	return paths, nil
}
