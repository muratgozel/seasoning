// Package hooks implements `seasoning hook <event>` for the Claude Code
// plugin (SPEC §9.3, §9.5, §10.2).
//
// Hooks never break the session: an internal failure is logged and the hook
// exits 0 with no output. The only deliberate interventions are the Stop
// block (wake-eligible messages arrived) and the waiter's exit 2 (wake).
//
// PostToolUse and PreToolUse fire around every tool call, so their fast path
// is one stat of the runtime "joined" marker; the database is opened only
// when a marker or an edit tool calls for it.
package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/gitremote"
	"github.com/muratgozel/seasoning/internal/inject"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/project"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/runtime"
	"github.com/muratgozel/seasoning/internal/session"
)

// Events, as the CLI names them.
const (
	SessionStart     = "session-start"
	UserPromptSubmit = "user-prompt-submit"
	PreToolUse       = "pre-tool-use"
	PostToolUse      = "post-tool-use"
	Stop             = "stop"
	SessionEnd       = "session-end"
	Wait             = "wait"
)

// Events lists every event in a stable order.
var Events = []string{SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop, SessionEnd, Wait}

var eventNames = map[string]string{
	SessionStart: "SessionStart", UserPromptSubmit: "UserPromptSubmit", PreToolUse: "PreToolUse",
	PostToolUse: "PostToolUse", Stop: "Stop", SessionEnd: "SessionEnd",
}

// editTools change files and name the file in their input.
var editTools = map[string]string{"Edit": "file_path", "Write": "file_path", "MultiEdit": "file_path", "NotebookEdit": "notebook_path"}

// Deps wire hooks to their surroundings.
type Deps struct {
	Config   config.Store
	StateDir string // base of runtime directories
	DBPath   string
	Getenv   func(string) string
	Now      func() time.Time
	Log      func(where string, err error)
	WaitPoll time.Duration
	// BindGrace is how long a new waiter waits for SessionStart to write the
	// binding it belongs to (they run concurrently).
	BindGrace time.Duration
}

// Input is the hook's stdin JSON; unknown fields are ignored.
type Input struct {
	SessionID      string                     `json:"session_id"`
	Cwd            string                     `json:"cwd"`
	HookEventName  string                     `json:"hook_event_name"`
	Source         string                     `json:"source"`
	Reason         string                     `json:"reason"`
	Prompt         string                     `json:"prompt"`
	ToolName       string                     `json:"tool_name"`
	ToolInput      map[string]json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage            `json:"tool_response"`
	AgentID        string                     `json:"agent_id"`
	StopHookActive bool                       `json:"stop_hook_active"`
}

type run struct {
	d      Deps
	in     Input
	event  string
	rt     runtime.Dir
	db     *localdb.DB
	stdout io.Writer
	stderr io.Writer
}

// Run handles one hook invocation and returns its exit code.
func Run(ctx context.Context, event string, stdin io.Reader, stdout, stderr io.Writer, d Deps) int {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.Log == nil {
		d.Log = func(string, error) {}
	}
	if d.WaitPoll == 0 {
		d.WaitPoll = 300 * time.Millisecond
	}
	if d.BindGrace == 0 {
		d.BindGrace = 10 * time.Second
	}
	if event == Wait && d.Getenv("CLAUDE_CODE_SESSION_ATTENDED") != "1" {
		// asyncRewake runs synchronously when nobody is attending (claude -p,
		// SDK hosts without streaming input): waiting would block the run.
		io.Copy(io.Discard, stdin)
		return 0
	}
	r := &run{d: d, event: event, stdout: stdout, stderr: stderr}
	raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		d.Log("hook "+event, fmt.Errorf("read input: %w", err))
		return 0
	}
	if err := json.Unmarshal(raw, &r.in); err != nil || r.in.SessionID == "" {
		d.Log("hook "+event, fmt.Errorf("hook input is not the JSON Claude Code sends (%v)", err))
		return 0
	}
	pid, err := strconv.Atoi(d.Getenv("CLAUDE_PID"))
	if err != nil || pid <= 1 {
		d.Log("hook "+event, errors.New("CLAUDE_PID is not set; seasoning hooks must run under Claude Code"))
		return 0
	}
	r.rt = runtime.ForBase(d.StateDir, pid)
	defer func() {
		if r.db != nil {
			r.db.Close()
		}
	}()

	var code int
	switch event {
	case SessionStart:
		err = r.sessionStart(ctx)
	case UserPromptSubmit:
		err = r.userPromptSubmit(ctx)
	case PreToolUse:
		err = r.preToolUse(ctx)
	case PostToolUse:
		err = r.postToolUse(ctx)
	case Stop:
		err = r.stop(ctx)
	case SessionEnd:
		err = r.sessionEnd(ctx)
	case Wait:
		code, err = r.wait(ctx)
	default:
		err = fmt.Errorf("unknown hook event %q", event)
	}
	if err != nil {
		d.Log("hook "+event, err)
	}
	return code
}

func (r *run) open() (*localdb.DB, error) {
	if r.db == nil {
		db, err := localdb.OpenPath(r.d.DBPath)
		if err != nil {
			return nil, err
		}
		r.db = db
	}
	return r.db, nil
}

// session returns the hook's session, or nil when it isn't joined.
func (r *run) session(ctx context.Context) (*localdb.Session, error) {
	db, err := r.open()
	if err != nil {
		return nil, err
	}
	s, err := db.Session(ctx, r.in.SessionID)
	if err != nil || s == nil || s.EndedAt != 0 {
		return nil, err
	}
	return s, nil
}

func (r *run) context(text string) error { return r.output(text, "") }

// output sends text to the model (additionalContext) and display to the
// human watching the session (systemMessage, rendered as "<hook> says: …").
func (r *run) output(text, display string) error {
	if text == "" && display == "" {
		return nil
	}
	out := map[string]any{}
	if text != "" {
		out["hookSpecificOutput"] = map[string]any{"hookEventName": eventNames[r.event], "additionalContext": text}
	}
	if display != "" {
		out["systemMessage"] = display
	}
	return json.NewEncoder(r.stdout).Encode(out)
}

func join(parts ...string) string {
	var keep []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, "\n\n")
}

// --- SessionStart ------------------------------------------------------------

const summaryBudget = 1500

func (r *run) sessionStart(ctx context.Context) error {
	db, err := r.open()
	if err != nil {
		return err
	}
	headless := r.d.Getenv("CLAUDE_CODE_SESSION_ATTENDED") == "0"
	s, _, err := session.Start(ctx, r.d.Config, db, r.rt, r.in.SessionID, r.in.Cwd, r.in.Source, headless, session.Off(r.d.Getenv, r.rt), r.d.Now())
	var noKey *project.MarkerNoKeyError
	if errors.As(err, &noKey) {
		return r.context("seasoning: " + noKey.Error() + ". Until then this session is not connected to peers.")
	}
	if err != nil || s == nil {
		return err
	}
	now := r.d.Now()
	var w strings.Builder
	fmt.Fprintf(&w, "seasoning: you are %s in project %q.", s.Handle(), s.ProjectName)
	peers, err := db.Peers(ctx, s.Project)
	if err != nil {
		return err
	}
	var live []string
	fresh := false
	for _, p := range peers {
		if now.Sub(time.UnixMilli(p.UpdatedAt)) < 2*time.Minute {
			fresh = true
		}
		if p.Session == s.RelaySession || p.Presence == nil || p.State == relayapi.StateEnded {
			continue
		}
		d := p.Presence.Handle + " (" + p.Presence.Activity
		if p.Presence.Branch != "" {
			d += ", " + p.Presence.Branch
		}
		if p.Presence.Status != "" {
			d += fmt.Sprintf(": %q", p.Presence.Status)
		}
		live = append(live, d+")")
	}
	switch {
	case !fresh:
		w.WriteString(" The peer list is still loading; call the seasoning peers tool to see who else is working.")
	case len(live) == 0:
		w.WriteString(" No other sessions are live in this project right now.")
	default:
		fmt.Fprintf(&w, " Live peers: %s.", strings.Join(live, "; "))
	}
	if unread, _, err := db.Pending(ctx, s.RelaySession); err == nil && unread > 0 {
		fmt.Fprintf(&w, " You have %d unread message(s); call the seasoning inbox tool.", unread)
	}
	if r.in.Source == "compact" {
		if open, err := db.OpenReplies(ctx, s.RelaySession, 5); err == nil && len(open) > 0 {
			var items []string
			for _, m := range open {
				items = append(items, fmt.Sprintf("%q from %s (id %s)", m.Subject, m.FromHandle, m.MessageID))
			}
			fmt.Fprintf(&w, " Still waiting for your reply: %s.", strings.Join(items, "; "))
		}
	}
	w.WriteString(" Once you know your task, set a short label and status with the seasoning set_status tool. Use the seasoning skill before messaging peers.")
	text := w.String()
	if len(text) > summaryBudget {
		text = text[:summaryBudget-1] + "…"
	}
	return r.context(text)
}

// --- UserPromptSubmit --------------------------------------------------------

func (r *run) userPromptSubmit(ctx context.Context) error {
	if !r.rt.Has(runtime.Joined) {
		return nil
	}
	s, err := r.session(ctx)
	if err != nil || s == nil {
		return err
	}
	woken := strings.HasPrefix(strings.TrimSpace(r.in.Prompt), "<task-notification>") && strings.Contains(r.in.Prompt, inject.RewakePrefix)
	woke := ""
	if woken {
		woke = r.rt.TakeWoke()
	}
	if _, err := r.db.UpdateSession(ctx, s.LocalID, func(x *localdb.Session) error {
		x.Activity = envelope.ActivityWorking
		if woken {
			x.AutonomousTurns++
		} else {
			x.AutonomousTurns = 0
		}
		x.UpdatedAt = r.d.Now().UnixMilli()
		return nil
	}); err != nil {
		return err
	}
	return r.deliver(ctx, s, false, woke)
}

// deliver injects waiting messages (and a due problem notice) as context,
// and shows them to the human; extra is shown first (a wake note).
func (r *run) deliver(ctx context.Context, s *localdb.Session, wakeOnly bool, extra string) error {
	text, display, err := r.claim(ctx, s, wakeOnly)
	if err != nil {
		return err
	}
	notice := r.notice(ctx, s)
	return r.output(notice+text, join(extra, notice, display))
}

// claim takes waiting messages and formats them for the model (text) and
// for the human (display).
func (r *run) claim(ctx context.Context, s *localdb.Session, wakeOnly bool) (text, display string, err error) {
	marker := runtime.Unread
	if wakeOnly {
		marker = runtime.Wake
	}
	if !r.rt.Has(marker) {
		return "", "", nil
	}
	db, err := r.open()
	if err != nil {
		return "", "", err
	}
	now := r.d.Now()
	msgs, err := db.Claim(ctx, s.RelaySession, wakeOnly, now)
	if err != nil {
		return "", "", err
	}
	text, full := inject.Format(msgs, inject.Options{ProjectName: s.ProjectName, Now: now})
	if err := db.MarkRead(ctx, s.RelaySession, full, now); err != nil {
		return "", "", err
	}
	return text, inject.Display(msgs), session.SyncMarkers(ctx, db, r.rt, s.RelaySession)
}

// notice returns the runtime problem at most once an hour per session.
func (r *run) notice(ctx context.Context, s *localdb.Session) string {
	p := r.rt.Problem()
	if p == "" {
		return ""
	}
	db, err := r.open()
	if err != nil {
		return ""
	}
	if due, err := db.NoticeDue(ctx, s.LocalID, "problem", r.d.Now(), time.Hour); err != nil || !due {
		return ""
	}
	return "seasoning: " + p + "\n"
}

// --- PreToolUse / PostToolUse ------------------------------------------------

// repoPath maps the edit tool's file to a repo-relative path, or "".
func (r *run) repoPath(s *localdb.Session) string {
	field, ok := editTools[r.in.ToolName]
	if !ok || s.RepoRoot == "" {
		return ""
	}
	var p string
	if err := json.Unmarshal(r.in.ToolInput[field], &p); err != nil || p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.in.Cwd, p)
	}
	p = resolve(p)
	root, _ := worktree(s, p)
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if envelope.CheckRepoPath(rel) != nil {
		return ""
	}
	return rel
}

// worktree returns the directory a session reports p relative to: the top
// of the work tree containing p, when that is a worktree of the session's
// repository, else the session's root. inRepo is false outside it.
//
// The session's root is the checkout it started in, but sessions move into
// other worktrees (EnterWorktree, cd, ../repo-feature checkouts). Relative
// to the starting root, two sessions in two worktrees would name the same
// file .claude/worktrees/a/f.go and .claude/worktrees/b/f.go (or ../app-b/
// f.go, which is dropped as outside the repo), and never warn each other.
func worktree(s *localdb.Session, p string) (dir string, inRepo bool) {
	top, common, ok := gitremote.Worktree(p)
	if !ok {
		return s.RepoRoot, false
	}
	if _, mine, ok := gitremote.Worktree(s.RepoRoot); ok && mine == common {
		return top, true
	}
	return s.RepoRoot, false
}

// resolve resolves symlinks in p's deepest existing ancestor, since the file
// (and even its directory) may not exist yet. RepoRoot has symlinks
// resolved (/var vs /private/var on macOS), so p must too.
func resolve(p string) string {
	dir, rest := filepath.Clean(p), ""
	for {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

func (r *run) preToolUse(ctx context.Context) error {
	if _, ok := editTools[r.in.ToolName]; !ok || !r.rt.Has(runtime.Joined) {
		return nil
	}
	s, err := r.session(ctx)
	if err != nil || s == nil {
		return err
	}
	rel := r.repoPath(s)
	if rel == "" {
		return nil
	}
	peers, err := r.db.Peers(ctx, s.Project)
	if err != nil {
		return err
	}
	now := r.d.Now()
	var warnings []string
	for _, p := range peers {
		pr := p.Presence
		if p.Session == s.RelaySession || pr == nil || p.State == relayapi.StateEnded || pr.Repo != s.Repo {
			continue
		}
		for _, f := range pr.Files {
			if f.Path != rel {
				continue
			}
			first, err := r.db.FirstWarning(ctx, s.LocalID, rel, p.Session)
			if err != nil || !first {
				break
			}
			at, _ := envelope.ParseTime(f.At)
			where := pr.Activity
			if pr.Branch != "" {
				where = "branch " + pr.Branch + ", " + where
			}
			verb := "edited"
			if f.Via == "git" {
				verb = "has uncommitted changes to"
			}
			warnings = append(warnings, fmt.Sprintf("seasoning: %s (%s) %s %s %s. Consider messaging them before changing it.", pr.Handle, where, verb, rel, inject.Age(now.Sub(at))))
			break
		}
	}
	return r.context(strings.Join(warnings, "\n"))
}

func (r *run) postToolUse(ctx context.Context) error {
	if !r.rt.Has(runtime.Joined) {
		return nil
	}
	sent := r.sentDisplay()
	_, edit := editTools[r.in.ToolName]
	deliver := r.in.AgentID == "" && (r.rt.Has(runtime.Unread) || r.rt.Problem() != "")
	if !edit && !deliver {
		return r.output("", sent)
	}
	s, err := r.session(ctx)
	if err != nil || s == nil {
		return err
	}
	if edit {
		// Subagents' edits count too: they change the same checkout.
		if rel := r.repoPath(s); rel != "" {
			if err := r.db.TouchFile(ctx, s.LocalID, rel, "edit", r.d.Now()); err != nil {
				return err
			}
		}
	}
	if !deliver {
		return r.output("", sent)
	}
	return r.deliver(ctx, s, false, sent)
}

// anyString reports whether any string in a decoded JSON value satisfies f.
func anyString(v any, f func(string) bool) bool {
	switch x := v.(type) {
	case string:
		return f(x)
	case []any:
		for _, e := range x {
			if anyString(e, f) {
				return true
			}
		}
	case map[string]any:
		for _, e := range x {
			if anyString(e, f) {
				return true
			}
		}
	}
	return false
}

// sendTool is seasoning's own send tool as Claude Code names plugin MCP
// tools (mcp__plugin_<plugin>_<server>__<tool>).
const sendTool = "mcp__plugin_seasoning_seasoning__send"

// sentDisplay renders a message this session just sent with the send tool,
// for the human; "" for other tools and failed sends.
func (r *run) sentDisplay() string {
	if r.in.ToolName != sendTool {
		return ""
	}
	// The tool's success text starts "Sent <message id>"; errors don't.
	// Decode rather than search the raw JSON, which may escape "<".
	var resp any
	json.Unmarshal(r.in.ToolResponse, &resp)
	if !anyString(resp, func(s string) bool { return strings.HasPrefix(s, "Sent <") }) {
		return ""
	}
	var in struct {
		To            envelope.AddressList `json:"to"`
		Subject       string               `json:"subject"`
		Body          string               `json:"body"`
		ReplyExpected bool                 `json:"reply_expected"`
		Urgent        bool                 `json:"urgent"`
	}
	raw, _ := json.Marshal(r.in.ToolInput)
	if json.Unmarshal(raw, &in) != nil || len(in.To) == 0 {
		return ""
	}
	return inject.DisplaySent(in.To, in.ReplyExpected, in.Urgent, in.Subject, in.Body)
}

// --- Stop ----------------------------------------------------------------------

func (r *run) stop(ctx context.Context) error {
	if !r.rt.Has(runtime.Joined) {
		return nil
	}
	s, err := r.session(ctx)
	if err != nil || s == nil {
		return err
	}
	// The work tree the session is in now (cwd follows EnterWorktree and cd):
	// scan it for changes made through Bash, and report its branch.
	dir, inRepo := s.RepoRoot, false
	if r.in.Cwd != "" {
		dir, inRepo = worktree(s, resolve(r.in.Cwd))
	}
	if dir != "" {
		changed, err := gitremote.Changed(ctx, dir, 500*time.Millisecond)
		if err == nil {
			now := r.d.Now()
			for i, p := range changed {
				if i == envelope.MaxFiles {
					break
				}
				if envelope.CheckRepoPath(p) == nil {
					r.db.NoteFile(ctx, s.LocalID, p, now)
				}
			}
		}
	}
	branch := ""
	if inRepo {
		bctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		branch, _ = gitremote.Branch(bctx, dir)
		cancel()
	}
	if _, err := r.db.UpdateSession(ctx, s.LocalID, func(x *localdb.Session) error {
		x.Activity = envelope.ActivityIdle
		if branch != "" {
			x.Branch, x.Worktree = branch, filepath.Base(dir)
		}
		x.UpdatedAt = r.d.Now().UnixMilli()
		return nil
	}); err != nil {
		return err
	}
	// Only wake-eligible messages hold the turn open; the rest wait for the
	// next turn (SPEC D5).
	text, display, err := r.claim(ctx, s, true)
	if err != nil || text == "" {
		return err
	}
	return json.NewEncoder(r.stdout).Encode(map[string]any{"decision": "block", "reason": text, "systemMessage": display})
}

// --- SessionEnd --------------------------------------------------------------

func (r *run) sessionEnd(ctx context.Context) error {
	db, err := r.open()
	if err != nil {
		return err
	}
	if err := db.EndSession(ctx, r.in.SessionID, r.d.Now()); err != nil {
		return err
	}
	for _, m := range []string{runtime.Joined, runtime.Unread, runtime.Wake} {
		r.rt.Clear(m)
	}
	return r.rt.ClearProblem()
}

// --- asyncRewake waiter ------------------------------------------------------

// wait blocks until a wake-eligible message arrives for its session, then
// prints the messages on stderr and exits 2, which wakes Claude (SPEC §9.5).
// It exits 0 when a newer waiter replaces it or the process rebinds.
func (r *run) wait(ctx context.Context) (int, error) {
	me := os.Getpid()
	if err := r.rt.WriteWaiter(me); err != nil {
		return 0, err
	}
	start := r.d.Now()
	matched := false
	t := time.NewTicker(r.d.WaitPoll)
	defer t.Stop()
	for {
		if r.rt.Waiter() != me {
			return 0, nil
		}
		b, err := r.rt.Binding()
		if err != nil {
			return 0, err
		}
		switch {
		case b != nil && b.SessionID == r.in.SessionID:
			matched = true
		case matched || r.d.Now().Sub(start) > r.d.BindGrace:
			return 0, nil // the process moved on to another session
		}
		if matched && b.Project != "" && r.rt.Has(runtime.Wake) {
			s, err := r.session(ctx)
			if err != nil {
				return 0, err
			}
			if s != nil {
				text, display, err := r.claim(ctx, s, true)
				if err != nil {
					return 0, err
				}
				if text != "" {
					if err := r.rt.SetWoke(display); err != nil {
						r.d.Log("hook wait", err) // the wake still happens; only the human view lacks it
					}
					fmt.Fprint(r.stderr, text)
					return 2, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return 0, nil
		case <-t.C:
		}
	}
}
