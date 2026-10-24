package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/runtime"
	"github.com/muratgozel/seasoning/internal/session"
)

func init() {
	Register(&Command{
		Name:    "trust",
		Summary: "trust a peer's new identity key after verifying it with them",
		Usage:   "trust HANDLE [--key ed25519:…] [--project ID|NAME] [--yes]",
		Run:     runTrust,
	})
}

func runTrust(env Env) error {
	fs := NewFlagSet("trust", env)
	keyFlag := fs.String("key", "", "the key to trust (default: the key on their held-back messages)")
	ref := fs.String("project", "", "project ID or name (default: this repository's project)")
	assumeYes := fs.Bool("yes", false, "don't ask for confirmation")
	args, err := parse(fs, env.Args, 1)
	if err != nil {
		return err
	}
	handle := args[0]
	if !envelope.ValidPerson(handle) {
		return Usagef("%q is not a person handle (%s)", handle, envelope.PersonRule)
	}
	ps, err := config.LoadProjects()
	if err != nil {
		return err
	}
	p, err := pickProject(env, ps, *ref, "")
	if err != nil {
		return err
	}
	db, err := localdb.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()

	key := *keyFlag
	held, err := db.HeldKeys(ctx, p.ID, handle)
	if err != nil {
		return err
	}
	if key == "" {
		switch len(held) {
		case 0:
			return fmt.Errorf("no held-back messages from %s in %s, so there is no key to trust; to pin a key you verified another way, pass --key", handle, p.Label())
		case 1:
			key = held[0].Key
		default:
			var lines []string
			for _, h := range held {
				pub, _ := keys.ParsePublic(h.Key)
				lines = append(lines, fmt.Sprintf("  %s on %d message(s): --key %s", keys.Fingerprint(pub), h.Messages, h.Key))
			}
			return fmt.Errorf("%s's held-back messages carry %d different keys; verify the right one with %s and pass it:\n%s", handle, len(held), handle, strings.Join(lines, "\n"))
		}
	}
	pub, err := keys.ParsePublic(key)
	if err != nil {
		return fmt.Errorf("--key: %w", err)
	}
	st, err := config.Default()
	if err != nil {
		return err
	}
	pins, err := st.LoadPins(p.ID)
	if err != nil {
		return err
	}
	if pin, ok := pins.People[handle]; ok {
		if keys.SamePublic(pin.Key, key) {
			fmt.Fprintf(env.Stdout, "%s's key %s is already trusted in %s.\n", handle, keys.Fingerprint(pub), p.Label())
			return nil
		}
		old, _ := keys.ParsePublic(pin.Key)
		fmt.Fprintf(env.Stdout, "%s's pinned key in %s: %s (first seen %s)\n", handle, p.Label(), keys.Fingerprint(old), pin.FirstSeen)
	} else {
		fmt.Fprintf(env.Stdout, "%s has no pinned key in %s yet.\n", handle, p.Label())
	}
	fmt.Fprintf(env.Stdout, "New key: %s\nCompare this fingerprint with %s over a channel you trust (a call, a chat you know is theirs).\n", keys.Fingerprint(pub), handle)

	if !*assumeYes {
		if !isTerminal(env.Stdin) {
			return Usagef("--yes is required when not running interactively")
		}
		fmt.Fprintf(env.Stdout, "Type yes to trust it: ")
		line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return errors.New("not trusted; nothing changed")
		}
	}
	if err := st.Locked(func() error {
		pins, err := st.LoadPins(p.ID)
		if err != nil {
			return err
		}
		pins.People[handle] = config.Pin{Key: key, FirstSeen: envelope.FormatTime(now())}
		return st.SavePins(p.ID, pins)
	}); err != nil {
		return err
	}

	released, err := db.Release(ctx, p.ID, handle, key)
	if err != nil {
		return err
	}
	n := 0
	for _, h := range held {
		if keys.SamePublic(h.Key, key) {
			n = h.Messages
		}
	}
	// Running sessions pick the released messages up at their next hook.
	if state, err := paths.StateDir(); err == nil {
		sessions, _ := db.SessionsByRelay(ctx, released)
		for _, s := range sessions {
			if s.EndedAt == 0 {
				session.SyncMarkers(ctx, db, runtime.ForBase(state, s.ClaudePID), s.RelaySession)
			}
		}
	}
	fmt.Fprintf(env.Stdout, "Trusted %s's key %s in %s; released %d held-back message(s).\n", handle, keys.Fingerprint(pub), p.Label(), n)
	return nil
}
