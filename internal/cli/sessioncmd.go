package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/runtime"
	"github.com/muratgozel/seasoning/internal/session"
)

func init() {
	Register(&Command{
		Name:    "session",
		Summary: "switch seasoning off or on for the current Claude Code session, or show its status",
		Usage:   "session off|on|status   (run inside a Claude Code session, e.g. via /seasoning:off)",
		Run:     runSession,
	})
}

func runSession(env Env) error {
	fs := NewFlagSet("session", env)
	args, err := parse(fs, env.Args, 1)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(env.Getenv("CLAUDE_PID"))
	if err != nil || pid <= 1 {
		return errors.New("this must run inside a Claude Code session (CLAUDE_PID is not set); use the /seasoning:off, /seasoning:on and /seasoning:status commands")
	}
	state, err := paths.StateDir()
	if err != nil {
		return err
	}
	rt := runtime.ForBase(state, pid)
	b, err := rt.Binding()
	if err != nil {
		return err
	}
	if b == nil {
		return errors.New("seasoning hasn't seen this session start; the plugin's hooks may not be loaded. Run `seasoning doctor`")
	}
	db, err := localdb.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()

	switch args[0] {
	case "off":
		if err := rt.Set(runtime.Off); err != nil {
			return err
		}
		for _, m := range []string{runtime.Joined, runtime.Unread, runtime.Wake} {
			rt.Clear(m)
		}
		fmt.Fprintln(env.Stdout, "Seasoning is off for this Claude Code session: peers no longer see it, and messages to it wait at the relay for up to 7 days. Turn it back on with /seasoning:on.")
		return nil
	case "on":
		if env.Getenv("SEASONING") == "off" {
			return errors.New("SEASONING=off is set in this session's environment; restart Claude Code without it")
		}
		if err := rt.Clear(runtime.Off); err != nil {
			return err
		}
		st, err := config.Default()
		if err != nil {
			return err
		}
		s, _, err := session.Start(ctx, st, db, rt, b.SessionID, b.Cwd, "on", env.Getenv("CLAUDE_CODE_SESSION_ATTENDED") == "0", false, now())
		if err != nil {
			return err
		}
		if s == nil {
			return errors.New("seasoning is on, but this directory isn't linked to a project; run `seasoning join '<invite>'` or `seasoning link` here")
		}
		fmt.Fprintf(env.Stdout, "Seasoning is on: this session is %s in project %q again. Messages held while it was off will arrive now.\n", s.Handle(), s.ProjectName)
		return nil
	case "status":
		if session.Off(env.Getenv, rt) {
			fmt.Fprintln(env.Stdout, "Seasoning is off for this session (peers can't see it). Turn it on with /seasoning:on.")
			return nil
		}
		s, err := db.Session(ctx, b.SessionID)
		if err != nil {
			return err
		}
		if b.Project == "" || s == nil {
			fmt.Fprintln(env.Stdout, "This session isn't connected to a seasoning project: its directory isn't linked. Run `seasoning join '<invite>'` or `seasoning link` there, then /seasoning:on.")
			return nil
		}
		unread, wake, _ := db.Pending(ctx, s.RelaySession)
		fmt.Fprintf(env.Stdout, "Session:  %s in project %q\n", s.Handle(), s.ProjectName)
		if s.Status != "" {
			fmt.Fprintf(env.Stdout, "Status:   %s\n", s.Status)
		}
		fmt.Fprintf(env.Stdout, "Pending:  %d unsurfaced message(s), %d wake-eligible\n", unread, wake)
		if p := rt.Problem(); p != "" {
			fmt.Fprintf(env.Stdout, "Problem:  %s\n", p)
		} else {
			fmt.Fprintln(env.Stdout, "Relay:    no problems reported")
		}
		return nil
	default:
		return Usagef("expected off, on or status, not %q", args[0])
	}
}
