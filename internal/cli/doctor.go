package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/errlog"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/project"
	"github.com/muratgozel/seasoning/internal/selfupdate"
	"github.com/muratgozel/seasoning/internal/version"
)

// MinClaudeCode is the oldest Claude Code verified with seasoning's hooks
// (asyncRewake and exec-form hooks; SPEC §23).
const MinClaudeCode = "2.1.306"

func init() {
	Register(&Command{
		Name:    "doctor",
		Summary: "check this machine's seasoning setup and say what to fix",
		Usage:   "doctor",
		Run:     runDoctor,
	})
}

// runClaude runs the claude CLI; tests replace it.
var runClaude = func(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "claude", args...).Output()
	return string(out), err
}

type report struct {
	w     io.Writer
	fails int
}

func (r *report) ok(format string, a ...any)   { fmt.Fprintf(r.w, "  ok    "+format+"\n", a...) }
func (r *report) warn(format string, a ...any) { fmt.Fprintf(r.w, "  warn  "+format+"\n", a...) }
func (r *report) fail(format string, a ...any) {
	r.fails++
	fmt.Fprintf(r.w, "  FAIL  "+format+"\n", a...)
}
func (r *report) section(name string) { fmt.Fprintf(r.w, "\n%s\n", name) }

func runDoctor(env Env) error {
	if _, err := parse(NewFlagSet("doctor", env), env.Args, 0); err != nil {
		return err
	}
	r := &report{w: env.Stdout}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	r.section("Binary")
	r.ok("seasoning %s", version.Version)
	if link, ver, err := selfupdate.ActiveLink(); err == nil {
		r.ok("managed install: %s -> %s (updates with `seasoning update`)", link, ver)
	} else {
		var nm *selfupdate.NotManagedError
		if errors.As(err, &nm) {
			r.warn("not installed by the installer (%v); updates won't work. Install with: %s", nm, selfupdate.InstallerCommand())
		} else {
			r.warn("install layout: %v", err)
		}
	}
	if _, err := exec.LookPath("seasoning"); err != nil {
		if bin, err := paths.BinDir(); err == nil {
			r.warn("seasoning is not on PATH; add %s to it (the plugin still finds ~/.local/bin/seasoning)", bin)
		}
	}

	r.section("Identity")
	cfg, err := config.Load()
	if err != nil {
		r.fail("%v", err)
	}
	id, idErr := config.LoadIdentity()
	switch {
	case errors.Is(idErr, config.ErrNoIdentity) || (cfg != nil && cfg.Handle == ""):
		r.fail("no identity yet; run: seasoning setup")
	case idErr != nil:
		r.fail("%v", idErr)
	default:
		r.ok("handle %s, key %s", cfg.Handle, keys.Fingerprint(id.Public()))
	}

	r.section("Projects")
	ps, err := config.LoadProjects()
	if err != nil {
		r.fail("%v", err)
		ps = &config.Projects{}
	}
	if len(ps.Projects) == 0 {
		r.warn("you are in no projects; create one with `seasoning init` or join one with `seasoning join`")
	}
	if target, err := project.Inspect(ctx, env.Dir); err == nil {
		hit, err := project.ResolveMarked(target, ps)
		switch {
		case err != nil:
			r.warn("this directory: %v", err)
		case hit == nil:
			r.warn("this directory (%s) is not linked to a project", target.Describe())
		default:
			r.ok("this directory belongs to %s (by %s)", hit.Project.Label(), hit.Via)
		}
	}
	db, dbErr := localdb.Open()
	if dbErr != nil {
		r.fail("local database: %v", dbErr)
	} else {
		defer db.Close()
	}
	for i := range ps.Projects {
		p := &ps.Projects[i]
		k, _, err := p.Keys()
		if err != nil {
			r.fail("%s: %v", p.Label(), err)
			continue
		}
		start := time.Now()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.Relay+"/healthz", nil)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			r.fail("%s: relay %s unreachable (%v); check the URL and your network", p.Label(), p.Relay, err)
			continue
		}
		resp.Body.Close()
		rtt := time.Since(start)
		if date, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
			if skew := time.Until(date).Abs() - rtt/2; skew > 2*time.Minute {
				r.warn("%s: this machine's clock is %s off the relay's; fix the clock (timestamps and replay checks depend on it)", p.Label(), skew.Round(time.Second))
			}
		}
		if _, err := newClient(p.Relay).Roster(ctx, k); err != nil {
			r.fail("%s: the relay rejects this project (%v)", p.Label(), relayError(err))
			continue
		}
		r.ok("%s: relay %s reachable in %s, credential valid", p.Label(), p.Relay, rtt.Round(time.Millisecond))
		if db != nil {
			if counts, err := db.Counters(ctx, p.ID); err == nil && len(counts) > 0 {
				var parts []string
				for kind, n := range counts {
					parts = append(parts, fmt.Sprintf("%s %d", kind, n))
				}
				sort.Strings(parts)
				r.warn("%s: discarded or held-back items: %s (see `seasoning trust` for key changes)", p.Label(), strings.Join(parts, ", "))
			}
		}
	}

	r.section("Claude Code")
	if out, err := runClaude(ctx, "--version"); err != nil {
		r.warn("the claude CLI was not found; seasoning's plugin runs inside Claude Code")
	} else {
		v := strings.Fields(out)
		if len(v) > 0 {
			if cmp, err := selfupdate.CompareVersions(v[0], MinClaudeCode); err == nil && cmp < 0 {
				r.fail("Claude Code %s is older than %s, the oldest verified with seasoning's hooks; update Claude Code", v[0], MinClaudeCode)
			} else {
				r.ok("Claude Code %s", v[0])
			}
		}
		if list, err := runClaude(ctx, "plugin", "list"); err == nil {
			if strings.Contains(list, "seasoning") {
				r.ok("the seasoning plugin is installed")
			} else {
				r.fail("the seasoning plugin is not installed; run: claude plugin marketplace add muratgozel/seasoning && claude plugin install seasoning@seasoning")
			}
		}
	}

	r.section("Hooks")
	if exe, err := paths.Executable(); err == nil {
		p95, err := hookSampler(ctx, exe)
		switch {
		case err != nil:
			r.fail("running a hook failed: %v", err)
		case p95 > 15*time.Millisecond:
			r.warn("hook p95 latency %s exceeds the 15 ms budget; every tool call pays it", p95.Round(100*time.Microsecond))
		default:
			r.ok("hook p95 latency %s", p95.Round(100*time.Microsecond))
		}
	}
	if logPath := errlog.Default().Path; logPath != "" {
		if b, err := os.ReadFile(logPath); err == nil && len(b) > 0 {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) > 5 {
				lines = lines[len(lines)-5:]
			}
			r.warn("recent errors in %s:", logPath)
			for _, l := range lines {
				fmt.Fprintf(env.Stdout, "          %s\n", l)
			}
		}
	}

	if r.fails > 0 {
		fmt.Fprintf(env.Stdout, "\n%d problem(s) to fix.\n", r.fails)
		return Exit(ExitError)
	}
	fmt.Fprintln(env.Stdout, "\nAll good.")
	return nil
}

// hookSampler measures hook latency; tests replace it (a test binary can't
// stand in for seasoning).
var hookSampler = sampleHook

// sampleHook times the hook hot path: a PostToolUse in a process with no
// seasoning session, which every tool call in an unlinked repository pays.
func sampleHook(ctx context.Context, exe string) (time.Duration, error) {
	dir, err := os.MkdirTemp("", "seasoning-doctor-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	input := `{"session_id":"doctor","cwd":"/","hook_event_name":"PostToolUse","tool_name":"Bash"}`
	var ds []time.Duration
	for i := 0; i < 20; i++ {
		cmd := exec.CommandContext(ctx, exe, "hook", "post-tool-use", "--plugin-proto", fmt.Sprint(PluginProto))
		cmd.Stdin = strings.NewReader(input)
		cmd.Env = append(os.Environ(), "CLAUDE_PID=1999999999", paths.EnvStateDir+"="+filepath.Join(dir, "state"), paths.EnvDataDir+"="+filepath.Join(dir, "data"))
		start := time.Now()
		if out, err := cmd.CombinedOutput(); err != nil {
			return 0, fmt.Errorf("%v: %s", err, out)
		}
		ds = append(ds, time.Since(start))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)*95/100-1], nil
}
