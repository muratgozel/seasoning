package cli

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/invite"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/project"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/relayclient"
	"github.com/muratgozel/seasoning/internal/roster"
)

func init() {
	Register(&Command{
		Name:    "init",
		Summary: "create a project on a relay and link this repository to it",
		Usage:   "init --relay URL [--name NAME] [--path DIR]   (needs SEASONING_RELAY_TOKEN)",
		Run:     runInit,
	})
	Register(&Command{
		Name:    "join",
		Summary: "join a project with an invite and link this repository to it",
		Usage:   "join INVITE [--name NAME] [--path DIR] [--move]",
		Run:     runJoin,
	})
	Register(&Command{
		Name:    "invite",
		Summary: "print the invite of this repository's project",
		Usage:   "invite [--project ID|NAME] [--path DIR]",
		Run:     runInvite,
	})
}

const inviteWarning = "The invite is a secret: anyone holding it can join the project and read every message. Share it privately."

func runInit(env Env) error {
	fs := NewFlagSet("init", env)
	relayURL := fs.String("relay", "", "relay URL, e.g. https://relay.example.com")
	name := fs.String("name", "", "project name (default: the repository name)")
	path := fs.String("path", "", "directory to link (default: the current directory)")
	if _, err := parse(fs, env.Args, 0); err != nil {
		return err
	}
	if *relayURL == "" {
		return Usagef("--relay is required")
	}
	relay, err := invite.NormalizeRelay(*relayURL)
	if err != nil {
		return err
	}
	m, err := requireMe()
	if err != nil {
		return err
	}
	token := env.Getenv(EnvRelayToken)
	if token == "" {
		return fmt.Errorf("creating a project needs the relay's token; ask the relay operator for it and run: %s=<token> seasoning init --relay %s", EnvRelayToken, relay)
	}
	ctx, cancel := netCtx()
	defer cancel()
	target, err := project.Inspect(ctx, dirArg(env, *path))
	if err != nil {
		return err
	}
	projName := *name
	if projName == "" {
		projName = target.DefaultName()
	}
	if err := checkName(projName); err != nil {
		return err
	}
	if m, path, err := project.ReadMarker(target); err != nil {
		return err
	} else if m != nil {
		return fmt.Errorf("%s already names project %s on %s; join that project instead (ask a member for an invite)", path, m.Project, m.Relay)
	}

	return config.Locked(func() error {
		ps, err := config.LoadProjects()
		if err != nil {
			return err
		}
		if hit := project.Resolve(target, ps); hit != nil {
			return fmt.Errorf("%s is already linked to project %s (by %s); run `seasoning unlink` first to move it", target.Describe(), hit.Project.Label(), hit.Via)
		}
		secret, err := keys.NewSecret()
		if err != nil {
			return err
		}
		p := keys.Derive(secret)
		client := newClient(relay)
		if _, err := client.CreateProject(ctx, token, p); err != nil {
			if relayclient.IsCode(err, relayapi.CodeUnauthorized) {
				return fmt.Errorf("the relay rejected %s; check it with the relay operator", EnvRelayToken)
			}
			return relayError(err)
		}
		rec, err := rosterRecord(p, m)
		if err != nil {
			return err
		}
		if _, err := client.PostRoster(ctx, p, rec); err != nil {
			return fmt.Errorf("the project was created but your roster record was not stored: %w", relayError(err))
		}
		if err := pin(p.ID, []roster.Member{{Handle: m.handle, Key: keys.FormatPublic(m.id.Public())}}); err != nil {
			return err
		}
		entry := config.Project{ID: p.ID, Name: projName, Relay: relay, Secret: secret.Encode(), JoinedAt: envelope.FormatTime(now())}
		linked := project.Link(&entry, target)
		ps.Projects = append(ps.Projects, entry)
		if err := ps.Save(); err != nil {
			return fmt.Errorf("the project was created on the relay but could not be saved locally: %w", err)
		}
		fmt.Fprintf(env.Stdout, "Created project %s on %s and linked %s.\n\n", entry.Label(), relay, strings.Join(linked, ", "))
		fmt.Fprintf(env.Stdout, "Invite:\n  %s\n\n%s\n\nTeammates run this inside their clone:\n  seasoning join '%s'\n",
			invite.Format(relay, secret), inviteWarning, invite.Format(relay, secret))
		return nil
	})
}

func runJoin(env Env) error {
	fs := NewFlagSet("join", env)
	name := fs.String("name", "", "local project name (default: the repository name)")
	path := fs.String("path", "", "directory to link (default: the current directory)")
	move := fs.Bool("move", false, "move this repository's link from its current project (after a rotation)")
	args, err := parse(fs, env.Args, 1)
	if err != nil {
		return err
	}
	relay, secret, err := invite.Parse(args[0])
	if err != nil {
		return err
	}
	m, err := requireMe()
	if err != nil {
		return err
	}
	p := keys.Derive(secret)
	ctx, cancel := netCtx()
	defer cancel()
	target, err := project.Inspect(ctx, dirArg(env, *path))
	if err != nil {
		return err
	}

	client := newClient(relay)
	entries, err := client.Roster(ctx, p)
	if relayclient.IsCode(err, relayapi.CodeNotFound) {
		return fmt.Errorf("this invite doesn't open a project on %s: the project was rotated or removed, or the invite is damaged. Ask a member for a fresh invite", relay)
	}
	if err != nil {
		return relayError(err)
	}
	if m, path, err := project.ReadMarker(target); err == nil && m != nil && m.Project != p.ID {
		fmt.Fprintf(env.Stderr, "warning: %s names project %s, not the one you are joining (%s); in this repository the marker wins\n", path, m.Project, p.ID)
	}
	members, problems := roster.Read(entries, p)
	for _, pr := range problems {
		fmt.Fprintf(env.Stderr, "warning: %s\n", pr)
	}
	mine := keys.FormatPublic(m.id.Public())
	listed := false
	for _, mem := range members {
		if mem.Handle != m.handle {
			continue
		}
		if !keys.SamePublic(mem.Key, mine) {
			return fmt.Errorf("the handle %q is already used in this project by another person's key; pick another handle with: seasoning setup --handle <new>", m.handle)
		}
		listed = true
	}

	return config.Locked(func() error {
		ps, err := config.LoadProjects()
		if err != nil {
			return err
		}
		var moved []string
		if hit := project.Resolve(target, ps); hit != nil && hit.Project.ID != p.ID {
			if !*move {
				return fmt.Errorf("%s is already linked to project %s (by %s); to move it to this project (e.g. after a rotation), run: seasoning join --move '<invite>'", target.Describe(), hit.Project.Label(), hit.Via)
			}
			for id := range project.Unlink(ps, target) {
				moved = append(moved, id)
			}
		}
		if err := pin(p.ID, members); err != nil {
			return err
		}
		if !listed {
			rec, err := rosterRecord(p, m)
			if err != nil {
				return err
			}
			if _, err := client.PostRoster(ctx, p, rec); err != nil {
				return relayError(err)
			}
			members = append(members, roster.Member{Handle: m.handle, Key: mine})
			if err := pin(p.ID, members); err != nil {
				return err
			}
		}
		entry := ps.Get(p.ID)
		if entry == nil {
			projName := *name
			if projName == "" {
				projName = target.DefaultName()
			}
			if err := checkName(projName); err != nil {
				return err
			}
			ps.Projects = append(ps.Projects, config.Project{ID: p.ID, Name: projName, Relay: relay, Secret: secret.Encode(), JoinedAt: envelope.FormatTime(now())})
			entry = &ps.Projects[len(ps.Projects)-1]
		}
		linked := project.Link(entry, target)
		// A project left with no mappings (rotated away) is dropped.
		for _, id := range moved {
			if old := ps.Get(id); old != nil && len(old.Repos) == 0 && len(old.Paths) == 0 {
				fmt.Fprintf(env.Stdout, "Moved the link from %s, which is no longer linked anywhere; removed it from your projects.\n", old.Label())
				ps.Projects = slices.DeleteFunc(ps.Projects, func(x config.Project) bool { return x.ID == id })
				entry = ps.Get(p.ID)
			}
		}
		if err := ps.Save(); err != nil {
			return err
		}
		var handles []string
		for _, mem := range members {
			handles = append(handles, mem.Handle)
		}
		fmt.Fprintf(env.Stdout, "Joined %s on %s as %s.\nMembers: %s\n", entry.Label(), relay, m.handle, strings.Join(handles, ", "))
		if len(linked) > 0 {
			fmt.Fprintf(env.Stdout, "Linked %s.\n", strings.Join(linked, ", "))
		} else {
			fmt.Fprintf(env.Stdout, "%s was already linked.\n", target.Describe())
		}
		return nil
	})
}

func runInvite(env Env) error {
	fs := NewFlagSet("invite", env)
	ref := fs.String("project", "", "project ID or name (default: this repository's project)")
	path := fs.String("path", "", "directory whose project to use (default: the current directory)")
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
	_, secret, err := p.Keys()
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s\n", invite.Format(p.Relay, secret))
	fmt.Fprintf(env.Stderr, "%s\n", inviteWarning)
	return nil
}

// pin records first-seen keys and refuses keys that differ from existing
// pins. Callers hold the config lock.
func pin(projectID string, members []roster.Member) error {
	st, err := config.Default()
	if err != nil {
		return err
	}
	conflicts, err := roster.PinLocked(st, projectID, members, now())
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return conflicts[0]
	}
	return nil
}

// pickProject resolves --project, or else the directory's project.
func pickProject(env Env, ps *config.Projects, ref, path string) (*config.Project, error) {
	if ref != "" {
		return ps.Find(ref)
	}
	ctx, cancel := netCtx()
	defer cancel()
	target, err := project.Inspect(ctx, dirArg(env, path))
	if err != nil {
		return nil, err
	}
	hit, err := project.ResolveMarked(target, ps)
	if err != nil {
		return nil, err
	}
	if hit == nil {
		return nil, errors.New(target.Describe() + " is not linked to a project; pass --project, or link it with `seasoning link`")
	}
	return hit.Project, nil
}
