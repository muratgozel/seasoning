package envelope

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/muratgozel/seasoning/internal/jcs"
	"github.com/muratgozel/seasoning/internal/keys"
)

type fixture struct {
	project keys.Project
	alice   keys.Identity
	from    string
	to      string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	s, err := keys.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	id, err := keys.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p := keys.Derive(s)
	return fixture{project: p, alice: id, from: p.SessionID("alice-1"), to: p.SessionID("bob-1")}
}

func (f fixture) message(t *testing.T) *Message {
	t.Helper()
	id, err := NewMessageID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &Message{
		MessageID:     id,
		Project:       f.project.ID,
		From:          "alice/auth-refactor",
		FromSession:   f.from,
		FromKey:       keys.FormatPublic(f.alice.Public()),
		To:            []string{"bob/fix-session-store"},
		Date:          FormatTime(time.Now()),
		Subject:       "Renaming SessionStore.get to load",
		References:    []string{},
		ReplyExpected: true,
		Body:          "I'm renaming it in auth/session.ts.\n\tIndented line.",
	}
}

func (f fixture) seal(t *testing.T, m any) *Envelope {
	t.Helper()
	e, err := Seal(f.project.EncKey, f.alice, KindMessage, f.project.ID, f.from, Recipients{Sessions: []string{f.to}}, m)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// reseal encrypts arbitrary plaintext with a valid AEAD, to reach the checks
// that run after decryption.
func reseal(t *testing.T, key [32]byte, e *Envelope, plaintext []byte) *Envelope {
	t.Helper()
	out := *e
	aad, err := out.aad()
	if err != nil {
		t.Fatal(err)
	}
	aead, _ := chacha20poly1305.NewX(key[:])
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	rand.Read(nonce)
	out.Nonce = b64.EncodeToString(nonce)
	out.CT = b64.EncodeToString(aead.Seal(nil, nonce, plaintext, aad))
	return &out
}

func TestSealOpenRoundTrip(t *testing.T) {
	f := newFixture(t)
	m := f.message(t)
	e := f.seal(t, m)

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"alice", "bob", "Renaming", "session.ts"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("outer envelope leaks %q: %s", leak, raw)
		}
	}
	parsed, err := ParseEnvelope(raw, MaxEnvelopeBytes)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Open(f.project.EncKey, parsed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeMessage(o, parsed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != m.Subject || got.Body != m.Body || got.MessageID != m.MessageID || !got.ReplyExpected {
		t.Fatalf("round trip changed the message: %+v", got)
	}
	if got.Thread() != m.MessageID {
		t.Fatalf("root thread = %s", got.Thread())
	}
}

func TestOpenRejectsRewrittenRouting(t *testing.T) {
	f := newFixture(t)
	e := f.seal(t, f.message(t))
	other := f.project.SessionID("mallory")
	rewrites := map[string]func(*Envelope){
		"to":           func(e *Envelope) { e.To = Recipients{Sessions: []string{other}} },
		"to all":       func(e *Envelope) { e.To = Recipients{All: true} },
		"from_session": func(e *Envelope) { e.FromSession = other },
		"project":      func(e *Envelope) { e.Project = "p_aaaaaaaaaaaaaaaa" },
		"kind":         func(e *Envelope) { e.Kind = KindRoster },
	}
	for name, rewrite := range rewrites {
		c := *e
		rewrite(&c)
		if _, err := Open(f.project.EncKey, &c); !errors.Is(err, ErrUndecryptable) {
			t.Errorf("%s rewrite: err = %v", name, err)
		}
	}
	var wrong [32]byte
	if _, err := Open(wrong, e); !errors.Is(err, ErrUndecryptable) {
		t.Errorf("wrong key: err = %v", err)
	}
}

func TestOpenRejectsBadSignatures(t *testing.T) {
	f := newFixture(t)
	m := f.message(t)
	e := f.seal(t, m)
	o, err := Open(f.project.EncKey, e)
	if err != nil {
		t.Fatal(err)
	}

	// A member edits the body and re-encrypts, keeping the old signature.
	var obj map[string]any
	json.Unmarshal(o.Plaintext, &obj)
	obj["body"] = "Please delete the repo."
	edited, _ := jcs.Marshal(obj)
	if _, err := Open(f.project.EncKey, reseal(t, f.project.EncKey, e, edited)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("edited body: err = %v", err)
	}

	// The signature is bound to the kind: byte-identical plaintext signed as
	// a message does not verify as a roster record.
	mallory, _ := keys.GenerateIdentity()
	pub := keys.FormatPublic(mallory.Public())
	both := map[string]any{"from_key": pub, "key": pub, "handle": "alice", "joined_at": FormatTime(time.Now())}
	sealed, err := Seal(f.project.EncKey, mallory, KindMessage, f.project.ID, f.from, Recipients{All: true}, both)
	if err != nil {
		t.Fatal(err)
	}
	op, err := Open(f.project.EncKey, sealed)
	if err != nil {
		t.Fatal(err)
	}
	asRoster := *sealed
	asRoster.Kind = KindRoster
	if _, err := Open(f.project.EncKey, reseal(t, f.project.EncKey, &asRoster, op.Plaintext)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("cross-kind signature: err = %v", err)
	}

	// Seal refuses a payload that names someone else's key.
	m.FromKey = pub
	if _, err := Seal(f.project.EncKey, f.alice, KindMessage, f.project.ID, f.from, Recipients{All: true}, m); err == nil {
		t.Fatal("sealed a payload naming another key")
	}
}

func TestDecodeMessageChecksEnvelopeAgreement(t *testing.T) {
	f := newFixture(t)
	m := f.message(t)
	m.FromSession = f.project.SessionID("someone-else")
	e := f.seal(t, m)
	o, err := Open(f.project.EncKey, e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeMessage(o, e); !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownFieldsStayVerifiable(t *testing.T) {
	f := newFixture(t)
	m := f.message(t)
	b, _ := json.Marshal(m)
	var obj map[string]any
	json.Unmarshal(b, &obj)
	obj["x_future_field"] = map[string]any{"nested": true}
	e := f.seal(t, obj)
	o, err := Open(f.project.EncKey, e)
	if err != nil {
		t.Fatalf("newer sender's message did not verify: %v", err)
	}
	if _, err := DecodeMessage(o, e); err != nil {
		t.Fatal(err)
	}
}

func TestMessageValidate(t *testing.T) {
	f := newFixture(t)
	long := strings.Repeat("x", MaxSubjectRunes+1)
	bad := "<nope@seasoning>"
	cases := map[string]func(*Message){
		"subject newline":   func(m *Message) { m.Subject = "two\nlines" },
		"subject too long":  func(m *Message) { m.Subject = long },
		"subject blank":     func(m *Message) { m.Subject = "   " },
		"subject separator": func(m *Message) { m.Subject = "a" + string(rune(0x2028)) + "b" },
		"body CR":           func(m *Message) { m.Body = "line\r\nline" },
		"body NUL":          func(m *Message) { m.Body = "a\x00b" },
		"body too big":      func(m *Message) { m.Body = strings.Repeat("y", MaxBodyBytes+1) },
		"body empty":        func(m *Message) { m.Body = "" },
		"bad utf8":          func(m *Message) { m.Body = "\xff" },
		"no recipients":     func(m *Message) { m.To = nil },
		"duplicate to":      func(m *Message) { m.To = []string{"bob/x", "bob/x"} },
		"bad address":       func(m *Message) { m.To = []string{"Bob/x"} },
		"from wildcard":     func(m *Message) { m.From = "alice/*" },
		"local date":        func(m *Message) { m.Date = "2026-10-21T14:03:11+03:00" },
		"bad in_reply_to":   func(m *Message) { m.InReplyTo = &bad },
		"bad message id":    func(m *Message) { m.MessageID = "<x@host>" },
		"too many refs": func(m *Message) {
			for i := 0; i <= MaxReferences; i++ {
				m.References = append(m.References, m.MessageID)
			}
		},
	}
	for name, mutate := range cases {
		m := f.message(t)
		mutate(m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := f.message(t).Validate(); err != nil {
		t.Fatalf("valid message rejected: %v", err)
	}
}

func TestParseAddress(t *testing.T) {
	for in, session := range map[string]bool{"bob/fix-auth": true, "bob/*": false, "*": false, "a1/b2": true} {
		a, err := ParseAddress(in)
		if err != nil || a.String() != in || a.IsSession() != session {
			t.Errorf("%s: %+v %v", in, a, err)
		}
	}
	for _, in := range []string{"bob", "/x", "bob/", "-bob/x", "bob/X", "bob/a/b", "**", "bob/" + strings.Repeat("a", 41)} {
		if _, err := ParseAddress(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestMessageIDs(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 14, 3, 11, 0, time.UTC)
	var ids []string
	for i := 0; i < 50; i++ {
		id, err := NewMessageID(t0.Add(time.Duration(i) * time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if !ValidMessageID(id) {
			t.Fatalf("invalid id %s", id)
		}
		ids = append(ids, id)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ULIDs do not sort by time")
	}
	a, _ := NewMessageID(t0)
	b, _ := NewMessageID(t0)
	if a == b {
		t.Fatal("same-millisecond IDs collide")
	}
	if a[1] > '7' {
		t.Fatalf("first ULID digit %c exceeds 7", a[1])
	}
}

func TestRecipientsJSON(t *testing.T) {
	var r Recipients
	if err := json.Unmarshal([]byte(`"*"`), &r); err != nil || !r.All {
		t.Fatalf("star: %+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(`["s_aaaaaaaaaaaaaaaaaaaaaaaa"]`), &r); err != nil || r.All || len(r.Sessions) != 1 {
		t.Fatalf("list: %+v %v", r, err)
	}
	for _, bad := range []string{`"all"`, `5`, `{"a":1}`} {
		if err := json.Unmarshal([]byte(bad), &r); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestEnvelopeValidate(t *testing.T) {
	f := newFixture(t)
	good := f.seal(t, f.message(t))
	cases := map[string]func(*Envelope){
		"future version": func(e *Envelope) { e.V = 2 },
		"unknown kind":   func(e *Envelope) { e.Kind = "file" },
		"bad project":    func(e *Envelope) { e.Project = "acme" },
		"bad from":       func(e *Envelope) { e.FromSession = "s_x" },
		"empty to":       func(e *Envelope) { e.To = Recipients{} },
		"dup to":         func(e *Envelope) { e.To = Recipients{Sessions: []string{f.to, f.to}} },
		"short nonce":    func(e *Envelope) { e.Nonce = "AAAA" },
		"bad ct":         func(e *Envelope) { e.CT = "!" },
	}
	for name, mutate := range cases {
		c := *good
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	big := make([]byte, MaxEnvelopeBytes+1)
	if _, err := ParseEnvelope(big, MaxEnvelopeBytes); err == nil {
		t.Fatal("oversized envelope accepted")
	}
}

func TestRosterRecord(t *testing.T) {
	f := newFixture(t)
	rec := RosterRecord{Handle: "alice", Key: keys.FormatPublic(f.alice.Public()), JoinedAt: FormatTime(time.Now())}
	e, err := Seal(f.project.EncKey, f.alice, KindRoster, f.project.ID, f.from, Recipients{All: true}, rec)
	if err != nil {
		t.Fatal(err)
	}
	o, err := Open(f.project.EncKey, e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRoster(o)
	if err != nil || got.Handle != "alice" {
		t.Fatalf("roster: %+v %v", got, err)
	}
	rec.Handle = "Alice"
	if err := rec.Validate(); err == nil {
		t.Fatal("uppercase handle accepted")
	}
}
