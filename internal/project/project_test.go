package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muratgozel/seasoning/internal/config"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// CI machines have no git identity, and commit refuses to run without one.
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func repo(t *testing.T, remotes ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	for i, r := range remotes {
		git(t, dir, "remote", "add", []string{"origin", "upstream"}[i], r)
	}
	return dir
}

func TestResolveByRemoteForkAndWorktree(t *testing.T) {
	ctx := context.Background()
	main := repo(t, "git@github.com:example/acme-api.git")
	ps := &config.Projects{Projects: []config.Project{{ID: "p_aaaaaaaaaaaaaaaa", Name: "acme-api"}}}
	tgt, err := Inspect(ctx, main)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.DefaultName() != "acme-api" {
		t.Fatalf("default name %q", tgt.DefaultName())
	}
	if added := Link(&ps.Projects[0], tgt); len(added) != 1 || added[0] != "github.com/example/acme-api" {
		t.Fatalf("link added %v", added)
	}

	// A fork whose upstream is the canonical repo resolves to the same project.
	fork := repo(t, "git@github.com:alice/acme-api.git", "https://github.com/Example/acme-api")
	ft, _ := Inspect(ctx, fork)
	if m := Resolve(ft, ps); m == nil || m.Via != "remote github.com/example/acme-api" {
		t.Fatalf("fork: %+v", m)
	}

	// A worktree of a remote-less repo resolves through the common dir.
	local := repo(t)
	git(t, local, "commit", "-q", "--allow-empty", "-m", "init", "--author", "T <t@example.com>")
	lt, _ := Inspect(ctx, local)
	ps.Projects = append(ps.Projects, config.Project{ID: "p_bbbbbbbbbbbbbbbb", Name: "local"})
	Link(&ps.Projects[1], lt)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, local, "worktree", "add", "-q", wt)
	wtt, err := Inspect(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if m := Resolve(wtt, ps); m == nil || m.Project.ID != "p_bbbbbbbbbbbbbbbb" {
		t.Fatalf("worktree: %+v (common dir %s)", m, wtt.Path)
	}

	removed := Unlink(ps, ft)
	if len(removed["p_aaaaaaaaaaaaaaaa"]) != 1 || Resolve(tgt, ps) != nil {
		t.Fatalf("unlink: %v", removed)
	}
}

func TestResolveFolderByAncestor(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	root := t.TempDir()
	sub := filepath.Join(root, "notes", "deep")
	os.MkdirAll(sub, 0o755)
	rt, err := Inspect(ctx, root)
	if err != nil || rt.InRepo {
		t.Fatalf("root: %+v %v", rt, err)
	}
	ps := &config.Projects{Projects: []config.Project{{ID: "p_cccccccccccccccc", Name: "notes"}}}
	Link(&ps.Projects[0], rt)
	st, _ := Inspect(ctx, sub)
	if m := Resolve(st, ps); m == nil {
		t.Fatal("subfolder of a linked folder did not resolve")
	}
	other, _ := Inspect(ctx, t.TempDir())
	if m := Resolve(other, ps); m != nil {
		t.Fatalf("unrelated folder resolved: %+v", m)
	}
}

func TestMarkerTakesPrecedence(t *testing.T) {
	ctx := context.Background()
	dir := repo(t, "git@github.com:example/acme-api.git")
	ps := &config.Projects{Projects: []config.Project{{ID: "p_aaaaaaaaaaaaaaaa", Name: "by-remote", Repos: []string{"github.com/example/acme-api"}}, {ID: "p_bbbbbbbbbbbbbbbb", Name: "by-marker"}}}
	tgt, _ := Inspect(ctx, dir)
	if m, err := ResolveMarked(tgt, ps); err != nil || m.Project.Name != "by-remote" {
		t.Fatalf("no marker: %+v %v", m, err)
	}
	path, err := WriteMarker(tgt.MarkerDir(), Marker{Relay: "https://relay.example.com", Project: "p_bbbbbbbbbbbbbbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o644 {
		t.Fatalf("marker mode %v", info.Mode().Perm())
	}
	sub := filepath.Join(dir, "deep")
	os.MkdirAll(sub, 0o755)
	st, _ := Inspect(ctx, sub)
	if m, err := ResolveMarked(st, ps); err != nil || m.Project.Name != "by-marker" {
		t.Fatalf("marker should win: %+v %v", m, err)
	}
	WriteMarker(tgt.MarkerDir(), Marker{Relay: "https://relay.example.com", Project: "p_cccccccccccccccc"})
	var nk *MarkerNoKeyError
	if _, err := ResolveMarked(tgt, ps); !errors.As(err, &nk) || !strings.Contains(err.Error(), "ask a member for an invite") {
		t.Fatalf("unknown marked project: %v", err)
	}
	for _, bad := range []string{`{"version":1,"relay":"https://r.example.com","project":"p_bbbbbbbbbbbbbbbb","secret":"x"}`, `{"version":2,"relay":"https://r.example.com","project":"p_bbbbbbbbbbbbbbbb"}`, `{"version":1,"relay":"http://r.example.com","project":"p_bbbbbbbbbbbbbbbb"}`} {
		os.WriteFile(path, []byte(bad), 0o644)
		if _, err := ResolveMarked(tgt, ps); err == nil || errors.As(err, &nk) {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
}
