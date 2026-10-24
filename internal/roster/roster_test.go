package roster

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/relayapi"
)

func entry(t *testing.T, p keys.Project, id keys.Identity, handle string, seq int64) relayapi.RosterEntry {
	t.Helper()
	rec := envelope.RosterRecord{Handle: handle, Key: keys.FormatPublic(id.Public()), JoinedAt: envelope.FormatTime(time.Now())}
	e, err := envelope.Seal(p.EncKey, id, envelope.KindRoster, p.ID, p.SessionID("cli:"+handle), envelope.Recipients{All: true}, rec)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e)
	return relayapi.RosterEntry{Seq: seq, Record: raw}
}

func TestReadAndPin(t *testing.T) {
	s, _ := keys.NewSecret()
	p := keys.Derive(s)
	alice, _ := keys.GenerateIdentity()
	bob, _ := keys.GenerateIdentity()
	mallory, _ := keys.GenerateIdentity()
	other, _ := keys.NewSecret()
	foreign := entry(t, keys.Derive(other), mallory, "carol", 4)
	entries := []relayapi.RosterEntry{
		entry(t, p, alice, "alice", 1),
		entry(t, p, bob, "bob", 2),
		entry(t, p, mallory, "alice", 3), // later record claiming alice
		foreign,                          // another project's key
		{Seq: 5, Record: json.RawMessage(`{"v":1}`)},
	}
	members, problems := Read(entries, p)
	if len(members) != 2 || members[0].Handle != "alice" || !keys.SamePublic(members[0].Key, keys.FormatPublic(alice.Public())) {
		t.Fatalf("members: %+v", members)
	}
	if len(problems) != 3 || !strings.Contains(strings.Join(problems, "\n"), "different key than its first record") {
		t.Fatalf("problems: %v", problems)
	}

	st := config.Store{Dir: filepath.Join(t.TempDir(), "config")}
	if c, err := Pin(st, p.ID, members, time.Now()); err != nil || len(c) != 0 {
		t.Fatalf("first pin: %v %v", c, err)
	}
	if v, _ := Check(st, p.ID, "alice", keys.FormatPublic(alice.Public())); v != Trusted {
		t.Fatalf("alice = %v", v)
	}
	if v, _ := Check(st, p.ID, "alice", keys.FormatPublic(mallory.Public())); v != KeyChanged {
		t.Fatalf("mallory as alice = %v", v)
	}
	if v, _ := Check(st, p.ID, "dave", keys.FormatPublic(mallory.Public())); v != Unknown {
		t.Fatalf("dave = %v", v)
	}
	c, err := Pin(st, p.ID, []Member{{Handle: "alice", Key: keys.FormatPublic(mallory.Public())}}, time.Now())
	if err != nil || len(c) != 1 || !strings.Contains(c[0].Error(), "verify it with alice") {
		t.Fatalf("conflict: %v %v", c, err)
	}
	if v, _ := Check(st, p.ID, "alice", keys.FormatPublic(alice.Public())); v != Trusted {
		t.Fatal("a conflicting pin replaced the original")
	}
}
