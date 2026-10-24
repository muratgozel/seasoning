package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/relay"
)

const testToken = "cli-test-relay-token-0123456789abcdef"

// world is a relay plus a set of people, each with their own config dir and
// clone. Tests never touch the real home.
type world struct {
	t      *testing.T
	relay  string
	root   string
	people map[string]string // handle → config dir
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	for _, v := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", paths.EnvDataDir, paths.EnvStateDir} {
		t.Setenv(v, "")
	}
	srv, err := relay.New(relay.Options{DBPath: filepath.Join(root, "relay.db"), RelayToken: testToken})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { hs.Close(); srv.Close() })
	return &world{t: t, relay: hs.URL, root: root, people: map[string]string{}}
}

// clone makes a repository with the given origin (none if empty).
func (w *world) clone(name, origin string) string {
	w.t.Helper()
	dir := filepath.Join(w.root, "repos", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.t.Fatal(err)
	}
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			w.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	if origin != "" {
		git("remote", "add", "origin", origin)
	}
	return dir
}

type result struct {
	out, err string
	code     int
}

// as runs a command as person in dir, with extra environment variables.
func (w *world) as(person, dir string, extra map[string]string, args ...string) result {
	w.t.Helper()
	cfg, ok := w.people[person]
	if !ok {
		cfg = filepath.Join(w.root, "people", person, "config")
		w.people[person] = cfg
	}
	w.t.Setenv(paths.EnvConfigDir, cfg)
	var stdout, stderr bytes.Buffer
	code := Run(Env{
		Args: args, Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr, Dir: dir,
		Getenv: func(k string) string { return extra[k] },
	})
	return result{out: stdout.String(), err: stderr.String(), code: code}
}

func (r result) must(t *testing.T) result {
	t.Helper()
	if r.code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", r.code, r.out, r.err)
	}
	return r
}

func (r result) fails(t *testing.T, code int, want string) {
	t.Helper()
	if r.code != code || !strings.Contains(r.err, want) {
		t.Fatalf("want exit %d with %q\ngot exit %d\nstdout: %s\nstderr: %s", code, want, r.code, r.out, r.err)
	}
}

func inviteFrom(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); strings.Contains(s, "/join#k=") && !strings.HasPrefix(s, "seasoning") {
			return s
		}
	}
	t.Fatalf("no invite in output:\n%s", out)
	return ""
}

func TestCreateJoinAndPins(t *testing.T) {
	w := newWorld(t)
	token := map[string]string{EnvRelayToken: testToken}
	aliceRepo := w.clone("alice", "git@github.com:example/acme-api.git")
	bobRepo := w.clone("bob", "https://github.com/Example/acme-api")

	w.as("alice", aliceRepo, nil, "init", "--relay", w.relay).fails(t, ExitError, "seasoning setup")
	out := w.as("alice", aliceRepo, nil, "setup", "--handle", "alice").must(t).out
	if !strings.Contains(out, "Handle:   alice") || !strings.Contains(out, "SHA256:") {
		t.Fatalf("setup output:\n%s", out)
	}
	w.as("alice", aliceRepo, nil, "init", "--relay", w.relay).fails(t, ExitError, EnvRelayToken)
	w.as("alice", aliceRepo, map[string]string{EnvRelayToken: "wrong-token-wrong-token-wrong-token"}, "init", "--relay", w.relay).fails(t, ExitError, "rejected")

	out = w.as("alice", aliceRepo, token, "init", "--relay", w.relay).must(t).out
	inv := inviteFrom(t, out)
	if !strings.Contains(out, "acme-api (p_") || !strings.Contains(out, "linked github.com/example/acme-api") || !strings.Contains(out, "secret") {
		t.Fatalf("init output:\n%s", out)
	}
	w.as("alice", aliceRepo, token, "init", "--relay", w.relay).fails(t, ExitError, "already linked")
	if got := strings.TrimSpace(w.as("alice", aliceRepo, nil, "invite").must(t).out); got != inv {
		t.Fatalf("invite = %q, want %q", got, inv)
	}

	w.as("bob", bobRepo, nil, "join", inv).fails(t, ExitError, "seasoning setup")
	w.as("bob", bobRepo, nil, "setup", "--handle", "bob").must(t)
	out = w.as("bob", bobRepo, nil, "join", inv).must(t).out
	if !strings.Contains(out, "as bob") || !strings.Contains(out, "Members: alice, bob") || !strings.Contains(out, "Linked github.com/example/acme-api") {
		t.Fatalf("join output:\n%s", out)
	}
	if out := w.as("bob", bobRepo, nil, "join", inv).must(t).out; !strings.Contains(out, "already linked") {
		t.Fatalf("second join:\n%s", out)
	}

	// Bob pinned alice's key on first sight.
	t.Setenv(paths.EnvConfigDir, w.people["alice"])
	aliceID, err := config.LoadIdentity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.EnvConfigDir, w.people["bob"])
	ps, _ := config.LoadProjects()
	pins, err := config.LoadPins(ps.Projects[0].ID)
	if err != nil || !keys.SamePublic(pins.People["alice"].Key, keys.FormatPublic(aliceID.Public())) {
		t.Fatalf("bob's pins: %+v %v", pins, err)
	}

	// Someone else can't take an existing handle.
	mallory := w.clone("mallory", "git@github.com:example/acme-api.git")
	w.as("mallory", mallory, nil, "setup", "--handle", "alice").must(t)
	w.as("mallory", mallory, nil, "join", inv).fails(t, ExitError, `handle "alice" is already used`)

	// A wrong secret opens nothing, and says what to do.
	s, _ := keys.NewSecret()
	bogus := strings.SplitN(inv, "#k=", 2)[0] + "#k=" + s.Encode()
	w.as("bob", bobRepo, nil, "join", bogus).fails(t, ExitError, "Ask a member for a fresh invite")
	w.as("bob", bobRepo, nil, "join", "not-an-invite").fails(t, ExitError, "#k=")
}

func TestLinkAndUnlink(t *testing.T) {
	w := newWorld(t)
	token := map[string]string{EnvRelayToken: testToken}
	main := w.clone("main", "git@github.com:example/acme-api.git")
	w.as("alice", main, nil, "setup", "--handle", "alice").must(t)
	w.as("alice", main, token, "init", "--relay", w.relay, "--name", "acme").must(t)

	// A remote-less folder links by path; it then resolves for invite.
	notes := filepath.Join(w.root, "notes")
	os.MkdirAll(filepath.Join(notes, "sub"), 0o755)
	t.Setenv("GIT_CEILING_DIRECTORIES", w.root)
	if out := w.as("alice", notes, nil, "link").must(t).out; !strings.Contains(out, "Linked") || !strings.Contains(out, "acme") {
		t.Fatalf("link output:\n%s", out)
	}
	w.as("alice", filepath.Join(notes, "sub"), nil, "invite").must(t)
	w.as("alice", notes, nil, "unlink").must(t)
	w.as("alice", notes, nil, "unlink").fails(t, ExitError, "nothing to unlink")
	w.as("alice", notes, nil, "invite").fails(t, ExitError, "not linked")

	// --remote maps a URL the repo doesn't have (an SSH alias, say).
	alias := w.clone("alias", "work-gh:example/acme-api.git")
	w.as("alice", alias, nil, "link", "--remote", "git@github.com:example/other.git", "--project", "acme").must(t)
	w.as("alice", alias, nil, "link", "--project", "nope").fails(t, ExitError, "no project named")

	// With two projects, link must be told which.
	other := w.clone("other", "git@github.com:example/other-api.git")
	w.as("alice", other, token, "init", "--relay", w.relay).must(t)
	third := w.clone("third", "")
	w.as("alice", third, nil, "link").fails(t, ExitUsage, "pass --project")
}

func TestSetupRules(t *testing.T) {
	w := newWorld(t)
	dir := w.clone("r", "git@github.com:example/acme-api.git")
	w.as("alice", dir, nil, "setup").fails(t, ExitUsage, "--handle is required")
	w.as("alice", dir, nil, "setup", "--handle", "Alice").fails(t, ExitError, "must be")
	w.as("alice", dir, nil, "setup", "--handle", "alice").must(t)
	if out := w.as("alice", dir, nil, "setup").must(t).out; !strings.Contains(out, "existing key") {
		t.Fatalf("rerun setup:\n%s", out)
	}
	info, err := os.Stat(filepath.Join(w.people["alice"], config.IdentityFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file: %v %v", info, err)
	}
	w.as("alice", dir, map[string]string{EnvRelayToken: testToken}, "init", "--relay", w.relay).must(t)
	w.as("alice", dir, nil, "setup", "--handle", "alice2").fails(t, ExitError, "not supported")
	if out := w.as("alice", dir, nil, "setup", "--new-key").must(t).out; !strings.Contains(out, "seasoning trust alice") || !strings.Contains(out, "new key") {
		t.Fatalf("new key while in a project:\n%s", out)
	}
}

func TestHelpVersionAndUsage(t *testing.T) {
	w := newWorld(t)
	dir := w.root
	if r := w.as("x", dir, nil, "help"); r.code != ExitOK || !strings.Contains(r.out, "join") || !strings.Contains(r.out, "serve") {
		t.Fatalf("help: %+v", r)
	}
	if r := w.as("x", dir, nil, "version"); r.code != ExitOK || !strings.HasPrefix(r.out, "seasoning ") {
		t.Fatalf("version: %+v", r)
	}
	w.as("x", dir, nil, "nope").fails(t, ExitUsage, "unknown command")
	w.as("x", dir, nil, "join").fails(t, ExitUsage, "expected 1 argument")
	w.as("x", dir, nil, "init", "--relay", "http://relay.example.com").fails(t, ExitError, "https")
	w.as("x", dir, nil, "serve").fails(t, ExitError, EnvRelayToken)
	w.as("x", dir, nil, "serve", "--project-idle-ttl", "soon").fails(t, ExitUsage, "duration")
}

func TestParseDays(t *testing.T) {
	cases := map[string]time.Duration{"90d": 90 * 24 * time.Hour, "36h": 36 * time.Hour}
	for in, want := range cases {
		if got, err := parseDays(in); err != nil || got != want {
			t.Errorf("%s = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"0d", "-1d", "xd", "", "-5h"} {
		if _, err := parseDays(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestHookCommand(t *testing.T) {
	w := newWorld(t)
	t.Setenv(paths.EnvStateDir, filepath.Join(w.root, "state"))
	w.as("x", w.root, nil, "hook").fails(t, ExitUsage, "expected one event")
	w.as("x", w.root, nil, "hook", "bogus").fails(t, ExitUsage, "expected one event")
	if r := w.as("x", w.root, nil, "hook", "post-tool-use", "--plugin-proto", "99"); r.code != ExitOK || r.out != "" {
		t.Fatalf("newer plugin: %+v", r)
	}
	log, _ := os.ReadFile(filepath.Join(w.root, "state", "errors.log"))
	if !strings.Contains(string(log), "seasoning update") {
		t.Fatalf("errors.log lacks the update hint: %q", log)
	}
	w.as("x", w.root, nil, "hook", "post-tool-use", "--plugin-proto", "x").fails(t, ExitUsage, "not a number")
	w.as("x", w.root, nil, "mcp", "--plugin-proto", "99").fails(t, ExitError, "seasoning update")
}

func TestDoctor(t *testing.T) {
	w := newWorld(t)
	prevClaude, prevSampler := runClaude, hookSampler
	t.Cleanup(func() { runClaude, hookSampler = prevClaude, prevSampler })
	plugin := "seasoning@seasoning  enabled"
	runClaude = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "--version" {
			return "2.1.306 (Claude Code)\n", nil
		}
		return plugin, nil
	}
	hookSampler = func(context.Context, string) (time.Duration, error) { return 8 * time.Millisecond, nil }
	dir := w.clone("r", "git@github.com:example/acme-api.git")

	w.as("alice", dir, nil, "doctor").fails(t, ExitError, "")
	r := w.as("alice", dir, nil, "doctor")
	if !strings.Contains(r.out, "no identity yet; run: seasoning setup") {
		t.Fatalf("doctor without setup:\n%s", r.out)
	}
	w.as("alice", dir, nil, "setup", "--handle", "alice").must(t)
	w.as("alice", dir, map[string]string{EnvRelayToken: testToken}, "init", "--relay", w.relay).must(t)
	out := w.as("alice", dir, nil, "doctor").must(t).out
	for _, want := range []string{"handle alice", "this directory belongs to acme-api", "credential valid", "Claude Code 2.1.306", "plugin is installed", "hook p95 latency 8ms", "All good."} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	plugin = ""
	runClaude = func(_ context.Context, args ...string) (string, error) {
		if args[0] == "--version" {
			return "2.1.100 (Claude Code)\n", nil
		}
		return plugin, nil
	}
	r = w.as("alice", dir, nil, "doctor")
	if r.code != ExitError || !strings.Contains(r.out, "older than 2.1.306") || !strings.Contains(r.out, "claude plugin install seasoning@seasoning") {
		t.Fatalf("old Claude Code without the plugin:\n%s", r.out)
	}
}
