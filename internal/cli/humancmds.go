package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/seasoning/internal/agent"
	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/errlog"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/runtime"
	"github.com/muratgozel/seasoning/internal/session"
	"github.com/muratgozel/seasoning/internal/version"
)

func init() {
	Register(&Command{
		Name:    "peers",
		Summary: "list the sessions working on this repository's project",
		Usage:   "peers [--project ID|NAME] [--path DIR]",
		Run:     runPeers,
	})
	Register(&Command{
		Name:    "send",
		Summary: "message agents in this repository's project, as <you>/human (no reply expected)",
		Usage:   "send --to ADDRESS [--to ADDRESS…] --subject TEXT (--body TEXT | --body -) [--urgent] [--reply-to ID] [--project ID|NAME]",
		Run:     runSend,
	})
}

// localTime renders a relay timestamp in the local zone with its offset
// (DST-correct through time.Local).
func localTime(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).In(time.Local).Format("2006-01-02 15:04:05 (UTC-07:00)")
}

func runPeers(env Env) error {
	fs := NewFlagSet("peers", env)
	ref := fs.String("project", "", "project ID or name (default: this repository's project)")
	path := fs.String("path", "", "directory whose project to use")
	if _, err := parse(fs, env.Args, 0); err != nil {
		return err
	}
	ps, err := config.LoadProjects()
	if err != nil {
		return err
	}
	p, err := pickProject(env, ps, *ref, *path)
	if err != nil {
		return err
	}
	st, err := config.Default()
	if err != nil {
		return err
	}
	ctx, cancel := netCtx()
	defer cancel()
	j, err := session.ForProject(ctx, st, p.ID, dirArg(env, *path))
	if err != nil {
		return err
	}
	if j == nil {
		return errors.New("you have no identity yet; run: seasoning setup")
	}
	peers, err := agent.Snapshot(ctx, st, j, newClient(p.Relay), now())
	if err != nil {
		return relayError(err)
	}
	sort.Slice(peers, func(a, b int) bool {
		if (peers[a].State == relayapi.StateEnded) != (peers[b].State == relayapi.StateEnded) {
			return peers[b].State == relayapi.StateEnded
		}
		return handleOf(peers[a]) < handleOf(peers[b])
	})
	fmt.Fprintf(env.Stdout, "Project %s on %s\n", p.Label(), p.Relay)
	if len(peers) == 0 {
		fmt.Fprintln(env.Stdout, "No sessions.")
	}
	for _, pr := range peers {
		h := handleOf(pr)
		line := fmt.Sprintf("%-32s %-6s", h, pr.State)
		if x := pr.Presence; x != nil {
			line += " " + x.Activity
			if x.Branch != "" {
				line += ", branch " + x.Branch
			}
			if x.Repo != "" {
				line += ", " + x.Repo
			}
		}
		fmt.Fprintln(env.Stdout, line)
		if pr.Presence != nil && pr.Presence.Status != "" {
			fmt.Fprintf(env.Stdout, "%-32s status: %s\n", "", pr.Presence.Status)
		}
		when := "last seen " + localTime(pr.LastSeenAt)
		if pr.State == relayapi.StateEnded {
			when = "ended " + localTime(pr.EndedAt)
		}
		fmt.Fprintf(env.Stdout, "%-32s %s\n", "", when)
	}
	return nil
}

func handleOf(p localdb.Peer) string {
	if p.Presence != nil {
		return p.Presence.Handle
	}
	return "(unverified " + p.Session[:10] + "…)"
}

// humanSession runs an agent for the person's human session in this
// process; stop ends it at the relay and removes its runtime directory.
func humanSession(env Env, ref string) (a *agent.Agent, s *localdb.Session, stop func(), err error) {
	ps, err := config.LoadProjects()
	if err != nil {
		return nil, nil, nil, err
	}
	p, err := pickProject(env, ps, ref, "")
	if err != nil {
		return nil, nil, nil, err
	}
	st, err := config.Default()
	if err != nil {
		return nil, nil, nil, err
	}
	state, err := paths.StateDir()
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := localdb.Open()
	if err != nil {
		return nil, nil, nil, err
	}
	rt := runtime.ForBase(state, os.Getpid())
	ctx := context.Background()
	s, err = session.StartHuman(ctx, st, db, rt, p.ID, env.Dir, now())
	if err != nil {
		db.Close()
		return nil, nil, nil, err
	}
	a = agent.New(agent.Options{Config: st, DB: db, Runtime: rt, Getenv: env.Getenv, Log: errlog.Default().Log, Version: version.Version})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { a.Run(runCtx); close(done) }()
	stop = func() {
		cancel()
		<-done
		db.EndSession(context.Background(), s.LocalID, now())
		db.Close()
		rt.Remove()
	}
	if err := a.WaitBound(ctx, 10*time.Second); err != nil {
		stop()
		return nil, nil, nil, err
	}
	return a, s, stop, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func runSend(env Env) error {
	fs := NewFlagSet("send", env)
	var to multiFlag
	fs.Var(&to, "to", "recipient: person/label, person/* or * (repeatable)")
	subject := fs.String("subject", "", "one-line subject")
	body := fs.String("body", "", "message text, or - to read it from stdin")
	urgent := fs.Bool("urgent", false, "wake idle recipients even when addressing person/* or *")
	replyTo := fs.String("reply-to", "", "message ID you are answering")
	ref := fs.String("project", "", "project ID or name (default: this repository's project)")
	if _, err := parse(fs, env.Args, 0); err != nil {
		return err
	}
	if len(to) == 0 || *subject == "" || *body == "" {
		return Usagef("--to, --subject and --body are required")
	}
	text := *body
	if text == "-" {
		b, err := io.ReadAll(io.LimitReader(env.Stdin, 64<<10))
		if err != nil {
			return fmt.Errorf("read the body from stdin: %w", err)
		}
		text = string(b)
	}
	a, s, stop, err := humanSession(env, *ref)
	if err != nil {
		return err
	}
	defer stop()
	// Nothing reads replies addressed to <you>/human, so agents are told not
	// to send any; to get answers, ask in your own Claude Code session. A
	// person writing to a session wants it to act, so it still wakes.
	expected, wake := false, true
	ctx, cancel := netCtx()
	defer cancel()
	out, err := a.Send(ctx, agent.SendInput{To: to, Subject: *subject, Body: text, ReplyExpected: &expected, Urgent: *urgent, Wake: &wake, InReplyTo: *replyTo})
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "As %s: %s", s.Handle(), out)
	return nil
}
