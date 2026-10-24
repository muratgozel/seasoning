// Package roster reads a project's roster and applies trust on first use
// (SPEC §6.3, §6.4): the first key seen for a handle is pinned, and a later
// different key is never trusted silently.
package roster

import (
	"fmt"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/relayapi"
)

// Member is a person in a project's roster.
type Member struct {
	Handle   string
	Key      string
	JoinedAt string
}

// Read decrypts and verifies roster entries. Within the roster the first
// key for a handle wins; records that fail to verify and later conflicting
// keys are reported as problems, not trusted.
func Read(entries []relayapi.RosterEntry, p keys.Project) (members []Member, problems []string) {
	byHandle := map[string]string{}
	for _, e := range entries {
		env, err := envelope.ParseEnvelope(e.Record, envelope.MaxPresenceBytes)
		if err != nil {
			problems = append(problems, fmt.Sprintf("roster record %d is malformed (%v)", e.Seq, err))
			continue
		}
		o, err := envelope.Open(p.EncKey, env)
		if err != nil {
			problems = append(problems, fmt.Sprintf("roster record %d does not verify (%v)", e.Seq, err))
			continue
		}
		r, err := envelope.DecodeRoster(o)
		if err != nil {
			problems = append(problems, fmt.Sprintf("roster record %d is invalid (%v)", e.Seq, err))
			continue
		}
		if prev, ok := byHandle[r.Handle]; ok {
			if !keys.SamePublic(prev, r.Key) {
				problems = append(problems, fmt.Sprintf("roster record %d claims handle %q with a different key than its first record; ignored", e.Seq, r.Handle))
			}
			continue
		}
		byHandle[r.Handle] = r.Key
		members = append(members, Member{Handle: r.Handle, Key: r.Key, JoinedAt: r.JoinedAt})
	}
	return members, problems
}

// Conflict is a member whose roster key differs from the pinned one.
type Conflict struct {
	Handle      string
	Pinned, Got string
}

func (c Conflict) Error() string {
	pub, _ := keys.ParsePublic(c.Got)
	return fmt.Sprintf("%s's identity key in the roster (%s) differs from the one you pinned earlier; verify it with %s before trusting it", c.Handle, keys.Fingerprint(pub), c.Handle)
}

// Pin records first-seen keys for members under the store's lock and
// returns the members whose keys conflict with existing pins; those are left
// as they were.
func Pin(st config.Store, projectID string, members []Member, now time.Time) ([]Conflict, error) {
	var conflicts []Conflict
	err := st.Locked(func() error {
		var err error
		conflicts, err = PinLocked(st, projectID, members, now)
		return err
	})
	return conflicts, err
}

// PinLocked is Pin for a caller that already holds the store's lock. (flock
// locks per open file, so taking the lock again would deadlock.)
func PinLocked(st config.Store, projectID string, members []Member, now time.Time) ([]Conflict, error) {
	pins, err := st.LoadPins(projectID)
	if err != nil {
		return nil, err
	}
	var conflicts []Conflict
	changed := false
	for _, m := range members {
		if pin, ok := pins.People[m.Handle]; ok {
			if !keys.SamePublic(pin.Key, m.Key) {
				conflicts = append(conflicts, Conflict{Handle: m.Handle, Pinned: pin.Key, Got: m.Key})
			}
			continue
		}
		pins.People[m.Handle] = config.Pin{Key: m.Key, FirstSeen: envelope.FormatTime(now)}
		changed = true
	}
	if !changed {
		return conflicts, nil
	}
	return conflicts, st.SavePins(projectID, pins)
}

// Verdict is the trust decision for a (person, key) pair.
type Verdict int

const (
	// Trusted: the key is the person's pinned key.
	Trusted Verdict = iota
	// Unknown: no key is pinned for the person yet.
	Unknown
	// KeyChanged: a different key is pinned for the person.
	KeyChanged
)

// Check looks a person's key up in the pins.
func Check(st config.Store, projectID, person, key string) (Verdict, error) {
	pins, err := st.LoadPins(projectID)
	if err != nil {
		return Unknown, err
	}
	pin, ok := pins.People[person]
	switch {
	case !ok:
		return Unknown, nil
	case keys.SamePublic(pin.Key, key):
		return Trusted, nil
	default:
		return KeyChanged, nil
	}
}
