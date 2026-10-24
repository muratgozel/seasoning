package cli

import (
	"fmt"
	"time"

	"github.com/muratgozel/seasoning/internal/agent"
	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/invite"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/project"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/relayclient"
	"github.com/muratgozel/seasoning/internal/roster"
)

// retireDelay is how long a rotated-away project keeps working, so its
// members can read the notice (SPEC §7.4).
const retireDelay = 24 * time.Hour

func init() {
	Register(&Command{
		Name:    "project",
		Summary: "manage a project: rotate its secret (to remove a member, or after an invite leaked)",
		Usage:   "project rotate [--project ID|NAME]   (needs SEASONING_RELAY_TOKEN)",
		Run:     runProject,
	})
}

func runProject(env Env) error {
	fs := NewFlagSet("project", env)
	ref := fs.String("project", "", "project ID or name (default: this repository's project)")
	args, err := parse(fs, env.Args, 1)
	if err != nil {
		return err
	}
	if args[0] != "rotate" {
		return Usagef("unknown project subcommand %q; expected rotate", args[0])
	}
	m, err := requireMe()
	if err != nil {
		return err
	}
	token := env.Getenv(EnvRelayToken)
	ps, err := config.LoadProjects()
	if err != nil {
		return err
	}
	old, err := pickProject(env, ps, *ref, "")
	if err != nil {
		return err
	}
	if token == "" {
		return fmt.Errorf("rotating creates a new project, which needs the relay's token; run: %s=<token> seasoning project rotate", EnvRelayToken)
	}
	oldKeys, _, err := old.Keys()
	if err != nil {
		return err
	}
	ctx, cancel := netCtx()
	defer cancel()
	client := newClient(old.Relay)

	secret, err := keys.NewSecret()
	if err != nil {
		return err
	}
	fresh := keys.Derive(secret)
	if _, err := client.CreateProject(ctx, token, fresh); err != nil {
		if relayclient.IsCode(err, relayapi.CodeUnauthorized) {
			return fmt.Errorf("the relay rejected %s; check it with the relay operator", EnvRelayToken)
		}
		return relayError(err)
	}
	rec, err := rosterRecord(fresh, m)
	if err != nil {
		return err
	}
	if _, err := client.PostRoster(ctx, fresh, rec); err != nil {
		return fmt.Errorf("the new project was created but your roster record was not stored: %w", relayError(err))
	}

	// Tell the old project, without the new invite: everyone there,
	// including whoever is being removed, can read it.
	notice := "not sent"
	if a, _, stop, err := humanSession(env, old.ID); err != nil {
		notice += " (" + err.Error() + ")"
	} else {
		no := false
		_, err := a.Send(ctx, agent.SendInput{
			To: []string{"*"}, Subject: fmt.Sprintf("Project %q was rotated by %s", old.Name, m.handle), ReplyExpected: &no,
			Body: fmt.Sprintf("%s rotated this project's secret. This project stops working in %d hours. Ask %s for the new invite over a private channel, not here: everyone in this project can read this message. Then, in your clone: seasoning join --move '<new invite>'", m.handle, int(retireDelay.Hours()), m.handle),
		})
		stop()
		if err == nil {
			notice = "sent to every live session"
		} else {
			notice += " (" + err.Error() + ")"
		}
	}
	retireAt, err := client.RetireProject(ctx, oldKeys, retireDelay)
	if err != nil {
		fmt.Fprintf(env.Stderr, "warning: the old project could not be scheduled for removal (%v); it will be removed after 90 idle days\n", relayError(err))
	}

	var markers []string
	err = config.Locked(func() error {
		ps, err := config.LoadProjects()
		if err != nil {
			return err
		}
		p := ps.Get(old.ID)
		if p == nil {
			return fmt.Errorf("project %s disappeared from projects.json", old.ID)
		}
		p.ID, p.Secret, p.JoinedAt = fresh.ID, secret.Encode(), envelope.FormatTime(now())
		if err := ps.Save(); err != nil {
			return err
		}
		st, err := config.Default()
		if err != nil {
			return err
		}
		if _, err := roster.PinLocked(st, fresh.ID, []roster.Member{{Handle: m.handle, Key: keys.FormatPublic(m.id.Public())}}, now()); err != nil {
			return err
		}
		// Rewrite repo markers that named the old project, so they can be
		// committed along with the change.
		for _, dir := range append(append([]string{}, p.Paths...), env.Dir) {
			t, err := project.Inspect(ctx, dir)
			if err != nil {
				continue
			}
			if mk, path, err := project.ReadMarker(t); err == nil && mk != nil && mk.Project == old.ID {
				if _, err := project.WriteMarker(t.MarkerDir(), project.Marker{Relay: p.Relay, Project: fresh.ID}); err == nil {
					markers = append(markers, path)
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("the new project %s exists, but saving it locally failed: %w", fresh.ID, err)
	}

	fmt.Fprintf(env.Stdout, "Rotated %q: %s → %s on %s.\n", old.Name, old.ID, fresh.ID, old.Relay)
	fmt.Fprintf(env.Stdout, "Notice in the old project: %s. It will be removed at %s.\n", notice, retireAt)
	for _, mk := range markers {
		fmt.Fprintf(env.Stdout, "Updated %s; commit it.\n", mk)
	}
	inv := invite.Format(old.Relay, secret)
	fmt.Fprintf(env.Stdout, "\nNew invite:\n  %s\n\n%s Give it only to the people who should stay; each runs, in their clone:\n  seasoning join --move '%s'\n", inv, inviteWarning, inv)
	return nil
}
