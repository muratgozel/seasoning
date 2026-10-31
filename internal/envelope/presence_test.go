package envelope

import (
	"encoding/json"
	"fmt"
	"reflect"
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

// wireSize is the envelope's size as the relay measures it.
func wireSize(t *testing.T, e *Envelope) int {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

func TestSealPresenceFitsTheRelayLimit(t *testing.T) {
	f := newFixture(t)
	p := validPresence(f)
	// 200 monorepo paths of about 60 bytes seal to over 30 KB (seen live).
	start := time.Now().Add(-time.Hour)
	p.Files = nil
	for i := range MaxFiles {
		p.Files = append(p.Files, PresenceFile{
			Path: fmt.Sprintf("packages/acme-api-session-store/src/handlers/session-%03d.ts", i),
			At:   FormatTime(start.Add(-time.Duration(i) * time.Second)), Via: "edit",
		})
	}
	all := append([]PresenceFile(nil), p.Files...)
	e, err := SealPresence(f.project.EncKey, f.alice, f.project.ID, f.from, p, MaxPresenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if n := wireSize(t, e); n > MaxPresenceBytes {
		t.Fatalf("sealed presence is %d bytes; the limit is %d", n, MaxPresenceBytes)
	}
	if _, err := ParseEnvelope(mustJSON(t, e), MaxPresenceBytes); err != nil {
		t.Fatalf("the relay would reject it: %v", err)
	}
	o, err := Open(f.project.EncKey, e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePresence(o)
	if err != nil {
		t.Fatal(err)
	}
	kept := len(got.Files)
	if kept == 0 || kept == MaxFiles {
		t.Fatalf("kept %d of %d files", kept, MaxFiles)
	}
	for i, file := range got.Files {
		if file != all[i] {
			t.Fatalf("file %d is %+v; want the most recent ones in order, %+v", i, file, all[i])
		}
	}
	if !reflect.DeepEqual(p.Files, all) {
		t.Fatal("SealPresence changed the caller's files")
	}
	// It keeps as many as fit: one more would not.
	c := *p
	c.Files = all[:kept+1]
	bigger, err := Seal(f.project.EncKey, f.alice, KindPresence, f.project.ID, f.from, Recipients{All: true}, &c)
	if err != nil {
		t.Fatal(err)
	}
	if n := wireSize(t, bigger); n <= MaxPresenceBytes {
		t.Fatalf("dropped files that fit: %d files seal to %d bytes", kept+1, n)
	}
}

func TestSealPresenceKeepsSmallPresenceWhole(t *testing.T) {
	f := newFixture(t)
	p := validPresence(f)
	e, err := SealPresence(f.project.EncKey, f.alice, f.project.ID, f.from, p, MaxPresenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Open(f.project.EncKey, e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePresence(o)
	if err != nil || !reflect.DeepEqual(got.Files, p.Files) || e.Kind != KindPresence || !e.To.All {
		t.Fatalf("decoded %+v (%v) from %+v", got, err, e)
	}
}

func TestSealPresenceRejectsWhatCannotFit(t *testing.T) {
	f := newFixture(t)
	_, err := SealPresence(f.project.EncKey, f.alice, f.project.ID, f.from, validPresence(f), 600)
	if err == nil || !strings.Contains(err.Error(), "600") {
		t.Fatalf("err = %v", err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
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
