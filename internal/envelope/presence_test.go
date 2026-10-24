package envelope

import (
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/keys"
)

func validPresence(f fixture) *Presence {
	now := FormatTime(time.Now())
	return &Presence{
		Handle: "alice/auth-refactor", Aliases: []string{"alice/amber-fox"}, Person: "alice",
		Status: "Moving session storage", Activity: ActivityWorking, Repo: "github.com/example/acme-api",
		Branch: "auth-refactor", StartedAt: now, UpdatedAt: now,
		Files:  []PresenceFile{{Path: "src/auth/session.ts", At: now, Via: "edit"}},
		Client: ClientInfo{Seasoning: "dev"}, Key: keys.FormatPublic(f.alice.Public()),
	}
}

func TestPresenceRoundTrip(t *testing.T) {
	f := newFixture(t)
	p := validPresence(f)
	e, err := Seal(f.project.EncKey, f.alice, KindPresence, f.project.ID, f.from, Recipients{All: true}, p)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Open(f.project.EncKey, e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePresence(o)
	if err != nil || got.Label() != "auth-refactor" || got.Files[0].Path != "src/auth/session.ts" {
		t.Fatalf("decoded: %+v %v", got, err)
	}
}

func TestPresenceValidate(t *testing.T) {
	f := newFixture(t)
	cases := map[string]func(*Presence){
		"wildcard handle":  func(p *Presence) { p.Handle = "alice/*" },
		"other person":     func(p *Presence) { p.Person = "bob" },
		"foreign alias":    func(p *Presence) { p.Aliases = []string{"bob/x"} },
		"long status":      func(p *Presence) { p.Status = strings.Repeat("s", MaxStatusRunes+1) },
		"bad activity":     func(p *Presence) { p.Activity = "sleeping" },
		"absolute file":    func(p *Presence) { p.Files[0].Path = "/etc/passwd" },
		"escaping file":    func(p *Presence) { p.Files[0].Path = "../secrets" },
		"unclean file":     func(p *Presence) { p.Files[0].Path = "src//a.go" },
		"bad via":          func(p *Presence) { p.Files[0].Via = "magic" },
		"negative turns":   func(p *Presence) { p.AutonomousTurns = -1 },
		"branch newline":   func(p *Presence) { p.Branch = "a\nb" },
		"local start time": func(p *Presence) { p.StartedAt = "2026-10-21T10:00:00+02:00" },
	}
	for name, mutate := range cases {
		p := validPresence(f)
		mutate(p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validPresence(f).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateLabel(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		l := GenerateLabel()
		if !ValidLabel(l) || !strings.Contains(l, "-") {
			t.Fatalf("label %q", l)
		}
		seen[l] = true
	}
	if len(seen) < 10 {
		t.Fatalf("labels barely vary: %v", seen)
	}
}
