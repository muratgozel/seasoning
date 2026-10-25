package gitremote

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Example/Acme-API.git":                        "github.com/example/acme-api",
		"https://github.com/example/acme-api":                        "github.com/example/acme-api",
		"https://github.com/example/acme-api.git/":                   "github.com/example/acme-api",
		"https://x-token:ghp_secret@github.com/example/acme-api.git": "github.com/example/acme-api",
		"ssh://git@ssh.github.com:443/example/acme-api.git":          "github.com/example/acme-api",
		"git+ssh://git@github.com/example/acme-api":                  "github.com/example/acme-api",
		"https://WWW.GitHub.com/example/acme-api":                    "github.com/example/acme-api",
		"git://github.com/example/acme-api.git":                      "github.com/example/acme-api",
		"git@gitlab.com:Group/Sub/Deeper/Project.git":                "gitlab.com/group/sub/deeper/project",
		"https://git.example.org:8443/Team/Repo.git":                 "git.example.org/Team/Repo",
		"gitea@git.example.org:Team/Repo":                            "git.example.org/Team/Repo",
		"git@ssh.dev.azure.com:v3/Org/Project/Repo":                  "dev.azure.com/org/project/repo",
		"https://Org@dev.azure.com/Org/Project/_git/Repo":            "dev.azure.com/org/project/repo",
		"https://org.visualstudio.com/Project/_git/Repo":             "dev.azure.com/org/project/repo",
		"https://org.visualstudio.com/DefaultCollection/P/_git/R":    "dev.azure.com/org/p/r",
		"vs-ssh.visualstudio.com:v3/org/project/repo":                "dev.azure.com/org/project/repo",
		"git@bitbucket.org:Team/Repo.git":                            "bitbucket.org/team/repo",
	}
	for in, want := range cases {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, in := range []string{"", "/srv/git/repo.git", "./relative", "file:///srv/git/repo.git", "C/path:x", "https://github.com/", "https://github.com/a/../b", "svn://host/repo"} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", in, got)
		}
	}
	if _, err := Normalize("/srv/git/repo.git"); !errors.Is(err, ErrLocal) {
		t.Errorf("local path: err = %v", err)
	}
}

func TestInspect(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "git@github.com:alice/acme-api-fork.git")
	run("remote", "add", "upstream", "https://github.com/example/acme-api")
	run("remote", "add", "local", "/srv/mirror.git")
	run("config", "url.https://github.com/.insteadOf", "gh:")
	run("remote", "add", "short", "gh:example/acme-api")

	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, ok, err := Inspect(context.Background(), sub)
	if err != nil || !ok {
		t.Fatalf("Inspect: %v %v", ok, err)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if repo.TopLevel != want {
		t.Fatalf("TopLevel = %q, want %q", repo.TopLevel, want)
	}
	if repo.CommonDir != filepath.Join(want, ".git") {
		t.Fatalf("CommonDir = %q", repo.CommonDir)
	}
	got := repo.Normalized()
	if len(got) != 2 || got[0] != "github.com/alice/acme-api-fork" || got[1] != "github.com/example/acme-api" {
		t.Fatalf("Normalized = %v", got)
	}

	if _, ok, err := Inspect(context.Background(), t.TempDir()); ok || err != nil {
		t.Fatalf("non-repo: ok=%v err=%v", ok, err)
	}
}

func TestBranchAndChanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	ctx := context.Background()
	run("init", "-q", "-b", "main")
	if b, err := Branch(ctx, dir); err != nil || b != "main" {
		t.Fatalf("unborn branch = %q, %v", b, err)
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644)
	run("mv", "b.txt", "c d.txt")
	os.MkdirAll(filepath.Join(dir, "new"), 0o755)
	os.WriteFile(filepath.Join(dir, "new", "n.txt"), []byte("n"), 0o644)
	got, err := Changed(ctx, dir, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a.txt": true, "c d.txt": true, "new/n.txt": true}
	if len(got) != len(want) {
		t.Fatalf("Changed = %q", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Fatalf("Changed = %q", got)
		}
	}
	run("checkout", "-q", "--detach")
	if b, _ := Branch(ctx, dir); b != "(detached)" {
		t.Fatalf("detached branch = %q", b)
	}
}

func TestWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	root, _ := filepath.EvalSymlinks(t.TempDir())
	main := filepath.Join(root, "app")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(main, 0o755)
	git(main, "init", "-q", "-b", "main")
	git(main, "commit", "-q", "--allow-empty", "-m", "init")
	git(main, "worktree", "add", "-q", "-b", "nested", filepath.Join(main, ".claude", "worktrees", "a"))
	git(main, "worktree", "add", "-q", "-b", "sibling", filepath.Join(root, "app-b"))
	common := filepath.Join(main, ".git")

	cases := map[string]string{
		filepath.Join(main, "src", "new", "file.go"):                    main, // need not exist
		filepath.Join(main, ".claude", "worktrees", "a", "src", "x.go"): filepath.Join(main, ".claude", "worktrees", "a"),
		filepath.Join(root, "app-b", "x.go"):                            filepath.Join(root, "app-b"),
		filepath.Join(root, "app-b"):                                    filepath.Join(root, "app-b"),
	}
	for p, wantTop := range cases {
		top, c, ok := Worktree(p)
		if !ok || top != wantTop || c != common {
			t.Errorf("Worktree(%s) = %q, %q, %v; want %q, %q", p, top, c, ok, wantTop, common)
		}
		// git agrees on the common dir.
		if repo, ok, err := Inspect(context.Background(), wantTop); err != nil || !ok || repo.CommonDir != c {
			t.Errorf("%s: git says %q (%v %v), Worktree %q", wantTop, repo.CommonDir, ok, err, c)
		}
	}
	if _, _, ok := Worktree(filepath.Join(root, "elsewhere", "x.go")); ok {
		t.Error("a path outside any repository has a work tree")
	}
	// A submodule's .git file names its own repository: no commondir.
	sub := filepath.Join(root, "super", "sub")
	os.MkdirAll(filepath.Join(root, "super", ".git", "modules", "sub"), 0o755)
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: ../.git/modules/sub\n"), 0o644)
	if top, c, ok := Worktree(filepath.Join(sub, "f.c")); !ok || top != sub || c != filepath.Join(root, "super", ".git", "modules", "sub") {
		t.Errorf("submodule: %q, %q, %v", top, c, ok)
	}
	os.WriteFile(filepath.Join(sub, ".git"), []byte("garbage"), 0o644)
	if _, _, ok := Worktree(filepath.Join(sub, "f.c")); ok {
		t.Error("a malformed .git file was followed")
	}
}
