package cli

import (
	"bufio"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
)

func init() {
	Register(&Command{
		Name:    "setup",
		Summary: "create your identity key and choose your handle",
		Usage:   "setup [--handle NAME] [--new-key]",
		Run:     runSetup,
	})
}

func runSetup(env Env) error {
	fs := NewFlagSet("setup", env)
	handle := fs.String("handle", "", "your person handle, e.g. alice ("+envelope.PersonRule+")")
	newKey := fs.Bool("new-key", false, "replace your identity key")
	if _, err := parse(fs, env.Args, 0); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	projects, err := config.LoadProjects()
	if err != nil {
		return err
	}

	want := *handle
	if want == "" {
		want = cfg.Handle
	}
	if want == "" {
		if !isTerminal(env.Stdin) {
			return Usagef("--handle is required when not running interactively")
		}
		fmt.Fprintf(env.Stdout, "Choose your handle, the name teammates' agents see (e.g. alice; %s): ", envelope.PersonRule)
		line, err := bufio.NewReader(env.Stdin).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			return errors.New("no handle entered")
		}
		want = strings.TrimSpace(line)
	}
	if !envelope.ValidPerson(want) {
		return fmt.Errorf("handle %q must be %s", want, envelope.PersonRule)
	}
	inProjects := len(projects.Projects) > 0
	if cfg.Handle != "" && want != cfg.Handle && inProjects {
		return fmt.Errorf("your handle is %q in %d project(s); changing it is not supported while you are in projects", cfg.Handle, len(projects.Projects))
	}

	id, err := config.LoadIdentity()
	created := false
	switch {
	case errors.Is(err, config.ErrNoIdentity):
		created = true
	case err != nil:
		return err
	case *newKey:
		created = true
	}
	if created {
		if id, err = keys.GenerateIdentity(); err != nil {
			return err
		}
		if err := config.SaveIdentity(id); err != nil {
			return err
		}
	}
	cfg.Handle = want
	if err := cfg.Save(); err != nil {
		return err
	}
	if created && *newKey && inProjects {
		announceNewKey(env, projects, me{handle: want, id: id})
	}

	dir, _ := config.Dir()
	fmt.Fprintf(env.Stdout, "Handle:   %s\n", want)
	verb := "existing"
	if created {
		verb = "new"
	}
	fmt.Fprintf(env.Stdout, "Identity: %s (%s key in %s)\n", keys.Fingerprint(id.Public()), verb, filepath.Join(dir, config.IdentityFile))
	fmt.Fprintf(env.Stdout, "\nInstall the Claude Code plugin, if you haven't:\n  claude plugin marketplace add muratgozel/seasoning\n  claude plugin install seasoning@seasoning\n")
	if !inProjects {
		fmt.Fprintf(env.Stdout, "\nNext, inside a repository: create a project with\n  seasoning init --relay https://relay.example.com\nor join one with\n  seasoning join '<invite>'\n")
	}
	return nil
}

// announceNewKey posts the new key to each project's roster and pins it
// locally. Peers pinned the old key, so they hold back this person's
// messages until they run `seasoning trust` (SPEC §6.4).
func announceNewKey(env Env, projects *config.Projects, m me) {
	pub := keys.FormatPublic(m.id.Public())
	for i := range projects.Projects {
		p := &projects.Projects[i]
		k, _, err := p.Keys()
		if err != nil {
			fmt.Fprintf(env.Stderr, "warning: %s: %v\n", p.Label(), err)
			continue
		}
		ctx, cancel := netCtx()
		rec, err := rosterRecord(k, m)
		if err == nil {
			_, err = newClient(p.Relay).PostRoster(ctx, k, rec)
		}
		cancel()
		if err != nil {
			fmt.Fprintf(env.Stderr, "warning: could not announce the new key to %s: %v\n", p.Label(), relayError(err))
		}
		st, err := config.Default()
		if err == nil {
			err = st.Locked(func() error {
				pins, err := st.LoadPins(p.ID)
				if err != nil {
					return err
				}
				pins.People[m.handle] = config.Pin{Key: pub, FirstSeen: envelope.FormatTime(now())}
				return st.SavePins(p.ID, pins)
			})
		}
		if err != nil {
			fmt.Fprintf(env.Stderr, "warning: %s: %v\n", p.Label(), err)
		}
	}
	fmt.Fprintf(env.Stdout, "\nYour peers pinned your old key, so they will hold back your messages until each of them\nverifies your new fingerprint with you and runs: seasoning trust %s\nTell them your new fingerprint: %s\n", m.handle, keys.Fingerprint(m.id.Public()))
}
