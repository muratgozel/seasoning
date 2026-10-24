package hooks

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
)

func deps(t *testing.T, env map[string]string, logs *[]string) Deps {
	dir := t.TempDir()
	return Deps{
		Config: config.Store{Dir: filepath.Join(dir, "config")}, StateDir: filepath.Join(dir, "state"),
		DBPath: filepath.Join(dir, "local.db"), Getenv: func(k string) string { return env[k] },
		Log: func(where string, err error) { *logs = append(*logs, where+": "+err.Error()) },
	}
}

func TestHooksFailSafe(t *testing.T) {
	cases := map[string]struct {
		event, input string
		env          map[string]string
		wantLog      string
	}{
		"garbage input":     {PostToolUse, "not json", map[string]string{"CLAUDE_PID": "4242"}, "not the JSON Claude Code sends"},
		"no session id":     {PostToolUse, `{"cwd":"/x"}`, map[string]string{"CLAUDE_PID": "4242"}, "not the JSON Claude Code sends"},
		"no CLAUDE_PID":     {SessionStart, `{"session_id":"s","cwd":"/x"}`, nil, "CLAUDE_PID is not set"},
		"unknown event":     {"bogus", `{"session_id":"s"}`, map[string]string{"CLAUDE_PID": "4242"}, "unknown hook event"},
		"session start cwd": {SessionStart, `{"session_id":"s","cwd":"/definitely/missing"}`, map[string]string{"CLAUDE_PID": "4242"}, "missing"},
	}
	for name, c := range cases {
		var logs []string
		var out, errOut bytes.Buffer
		code := Run(context.Background(), c.event, strings.NewReader(c.input), &out, &errOut, deps(t, c.env, &logs))
		if code != 0 || out.Len() != 0 || errOut.Len() != 0 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; hooks must never break the session", name, code, out.String(), errOut.String())
		}
		if !strings.Contains(strings.Join(logs, "\n"), c.wantLog) {
			t.Errorf("%s: log %v lacks %q", name, logs, c.wantLog)
		}
	}
}

func TestWaitReturnsImmediatelyWhenUnattended(t *testing.T) {
	var logs []string
	start := time.Now()
	code := Run(context.Background(), Wait, strings.NewReader(`{"session_id":"s"}`), &bytes.Buffer{}, &bytes.Buffer{},
		deps(t, map[string]string{"CLAUDE_PID": "4242", "CLAUDE_CODE_SESSION_ATTENDED": "0"}, &logs))
	if code != 0 || time.Since(start) > time.Second {
		t.Fatalf("exit %d after %v", code, time.Since(start))
	}
}

func TestResolve(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	if got := resolve(filepath.Join(dir, "not", "yet", "file.go")); got != filepath.Join(dir, "not", "yet", "file.go") {
		t.Fatalf("resolve = %q", got)
	}
}
