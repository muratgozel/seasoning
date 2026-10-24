package relay_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/relay"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/relayclient"
)

const relayToken = "test-relay-token-0123456789abcdefghij"

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	t      *testing.T
	srv    *relay.Server
	http   *httptest.Server
	client *relayclient.Client
	clock  *clock
	logs   *syncBuffer
	dbPath string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newEnv(t *testing.T, mutate func(*relay.Options)) *env {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	opts := relay.Options{
		DBPath:     filepath.Join(t.TempDir(), "relay.db"),
		RelayToken: relayToken,
		Limits:     relay.DefaultLimits(),
		TTLs:       relay.DefaultTTLs(),
		Logger:     slog.New(slog.NewJSONHandler(logs, nil)),
		Now:        c.Now,
		KeepAlive:  50 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv, err := relay.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { hs.Close(); srv.Close() })
	return &env{t: t, srv: srv, http: hs, client: relayclient.New(hs.URL), clock: c, logs: logs, dbPath: opts.DBPath}
}

type member struct {
	handle string
	id     keys.Identity
	sid    string
}

type project struct {
	keys   keys.Project
	people map[string]*member
}

func (e *env) project(handles ...string) *project {
	e.t.Helper()
	s, _ := keys.NewSecret()
	p := &project{keys: keys.Derive(s), people: map[string]*member{}}
	if _, err := e.client.CreateProject(context.Background(), relayToken, p.keys); err != nil {
		e.t.Fatal(err)
	}
	for _, h := range handles {
		id, _ := keys.GenerateIdentity()
		m := &member{handle: h, id: id, sid: p.keys.SessionID(h + "-session")}
		p.people[h] = m
		if _, err := e.client.PutSession(context.Background(), p.keys, m.sid, nil); err != nil {
			e.t.Fatal(err)
		}
	}
	return p
}

func (p *project) message(t *testing.T, from string, to envelope.Recipients, body string) *envelope.Envelope {
	t.Helper()
	m := p.people[from]
	id, _ := envelope.NewMessageID(time.Now())
	msg := envelope.Message{
		MessageID: id, Project: p.keys.ID, From: from + "/work", FromSession: m.sid,
		FromKey: keys.FormatPublic(m.id.Public()), To: []string{"*"}, Date: envelope.FormatTime(time.Now()),
		Subject: "subject", References: []string{}, Body: body,
	}
	e, err := envelope.Seal(p.keys.EncKey, m.id, envelope.KindMessage, p.keys.ID, m.sid, to, msg)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func to(sids ...string) envelope.Recipients { return envelope.Recipients{Sessions: sids} }

// collect reads a session's stream until it has n events or the deadline.
func (e *env) collect(p *project, who string, after int64, n int) []relayclient.Event {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got []relayclient.Event
	stop := errors.New("enough")
	err := e.client.Stream(ctx, p.keys, p.people[who].sid, after, func(ev relayclient.Event) error {
		got = append(got, ev)
		if len(got) >= n {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		e.t.Fatalf("stream for %s: got %d of %d events, err %v", who, len(got), n, err)
	}
	return got
}

// drain reads a session's stream for a short window and returns everything
// that arrived, for assertions where the count is the point.
func (e *env) drain(p *project, who string, after int64) []relayclient.Event {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var got []relayclient.Event
	err := e.client.Stream(ctx, p.keys, p.people[who].sid, after, func(ev relayclient.Event) error {
		got = append(got, ev)
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		e.t.Fatalf("stream for %s ended early: %v", who, err)
	}
	return got
}

func ofType(evs []relayclient.Event, typ string) []relayclient.Event {
	var out []relayclient.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func apiCode(err error) string {
	var ae *relayclient.APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func TestHealthAndJoinPage(t *testing.T) {
	e := newEnv(t, nil)
	h, err := e.client.Health(context.Background())
	if err != nil || !h.OK || h.Schema != relay.SchemaVersion {
		t.Fatalf("health: %+v %v", h, err)
	}
	resp, err := http.Get(e.http.URL + "/join")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || resp.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("join page: %d %v", resp.StatusCode, resp.Header)
	}
	// The page's one inline script must match the CSP hash exactly, or the
	// browser silently refuses to run it; and the CSP must forbid network
	// access, so the script can never send the invite anywhere.
	body, _ := io.ReadAll(resp.Body)
	page := string(body)
	start, end := strings.Index(page, "<script>"), strings.Index(page, "</script>")
	if start < 0 || end < start {
		t.Fatal("join page has no inline script")
	}
	sum := sha256.Sum256([]byte(page[start+len("<script>") : end]))
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Fatalf("CSP %q doesn't allow the page's script", csp)
	}
	if !strings.HasPrefix(csp, "default-src 'none';") || strings.Contains(csp, "connect-src") || strings.Contains(csp, "unsafe-eval") {
		t.Fatalf("CSP %q must forbid network access", csp)
	}
	if strings.Count(page, "<script") != 1 {
		t.Fatal("join page must have exactly one script")
	}
}

func TestNewRejectsShortToken(t *testing.T) {
	_, err := relay.New(relay.Options{DBPath: filepath.Join(t.TempDir(), "r.db"), RelayToken: "short"})
	if err == nil || !strings.Contains(err.Error(), "openssl rand") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateProject(t *testing.T) {
	e := newEnv(t, func(o *relay.Options) { o.Limits.MaxProjects = 2 })
	ctx := context.Background()
	s, _ := keys.NewSecret()
	p := keys.Derive(s)
	if _, err := e.client.CreateProject(ctx, "wrong-token", p); apiCode(err) != relayapi.CodeUnauthorized {
		t.Fatalf("wrong token: %v", err)
	}
	if created, err := e.client.CreateProject(ctx, relayToken, p); err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if created, err := e.client.CreateProject(ctx, relayToken, p); err != nil || created {
		t.Fatalf("idempotent create: %v %v", created, err)
	}
	clash := p
	clash.AuthToken[0] ^= 1
	if _, err := e.client.CreateProject(ctx, relayToken, clash); apiCode(err) != relayapi.CodeConflict {
		t.Fatalf("clash: %v", err)
	}
	s2, _ := keys.NewSecret()
	s3, _ := keys.NewSecret()
	e.client.CreateProject(ctx, relayToken, keys.Derive(s2))
	if _, err := e.client.CreateProject(ctx, relayToken, keys.Derive(s3)); apiCode(err) != relayapi.CodeLimitReached {
		t.Fatalf("project limit: %v", err)
	}
}

func TestProjectAuthIsIndistinguishableAndRateLimited(t *testing.T) {
	e := newEnv(t, func(o *relay.Options) { o.Limits.AuthFailuresPerMinute = 3 })
	ctx := context.Background()
	p := e.project()
	wrong := p.keys
	wrong.AuthToken[0] ^= 1
	s, _ := keys.NewSecret()
	unknown := keys.Derive(s)

	_, err1 := e.client.Sessions(ctx, wrong)
	_, err2 := e.client.Sessions(ctx, unknown)
	if apiCode(err1) != relayapi.CodeNotFound || err1.Error() != err2.Error() {
		t.Fatalf("wrong token %v vs unknown project %v", err1, err2)
	}
	e.client.Sessions(ctx, wrong)
	if _, err := e.client.Sessions(ctx, p.keys); apiCode(err) != relayapi.CodeRateLimited {
		t.Fatalf("after 3 failures even a valid call is limited: %v", err)
	}
	e.clock.Add(61 * time.Second)
	if _, err := e.client.Sessions(ctx, p.keys); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice")
	state := func() string {
		t.Helper()
		resp, err := e.client.Sessions(ctx, p.keys)
		if err != nil || len(resp.Sessions) != 1 {
			t.Fatalf("sessions: %+v %v", resp, err)
		}
		return resp.Sessions[0].State
	}
	if s := state(); s != relayapi.StateLive {
		t.Fatalf("registered: %s", s)
	}
	e.clock.Add(61 * time.Second)
	if s := state(); s != relayapi.StateAway {
		t.Fatalf("no heartbeat: %s", s)
	}
	e.client.PutSession(ctx, p.keys, p.people["alice"].sid, nil)
	if s := state(); s != relayapi.StateLive {
		t.Fatalf("heartbeat: %s", s)
	}
	if err := e.client.EndSession(ctx, p.keys, p.people["alice"].sid); err != nil {
		t.Fatal(err)
	}
	if s := state(); s != relayapi.StateEnded {
		t.Fatalf("ended: %s", s)
	}
	e.client.PutSession(ctx, p.keys, p.people["alice"].sid, nil)
	if s := state(); s != relayapi.StateLive {
		t.Fatalf("resumed: %s", s)
	}
	if err := e.client.EndSession(ctx, p.keys, p.keys.SessionID("never")); apiCode(err) != relayapi.CodeNotFound {
		t.Fatalf("end unknown: %v", err)
	}
}

func TestPresenceIsValidatedAndBroadcast(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice", "bob")
	a := p.people["alice"]
	pres := map[string]any{"key": keys.FormatPublic(a.id.Public()), "handle": "alice/work", "status": "testing presence"}
	env, err := envelope.Seal(p.keys.EncKey, a.id, envelope.KindPresence, p.keys.ID, a.sid, envelope.Recipients{All: true}, pres)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.PutSession(ctx, p.keys, a.sid, env); err != nil {
		t.Fatal(err)
	}
	// Presence must come from the session it describes.
	if _, err := e.client.PutSession(ctx, p.keys, p.people["bob"].sid, env); apiCode(err) != relayapi.CodeBadRequest {
		t.Fatalf("presence for another session: %v", err)
	}
	evs := e.collect(p, "bob", 0, 3)
	sess := ofType(evs, relayapi.EventSession)
	var last relayapi.Session
	json.Unmarshal(sess[len(sess)-1].Data, &last)
	if last.Session != a.sid || len(last.Presence) == 0 {
		t.Fatalf("presence event: %+v", last)
	}
	opened, err := envelope.ParseEnvelope(last.Presence, envelope.MaxPresenceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envelope.Open(p.keys.EncKey, opened); err != nil {
		t.Fatalf("presence from the relay does not open: %v", err)
	}
}

func TestDirectMessageDeliveryAckAndReceipt(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice", "bob")
	a, b := p.people["alice"], p.people["bob"]

	resp, err := e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(b.sid), "secret-plan-7f3a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Recipients) != 1 || resp.Recipients[0].Status != relayapi.StatusQueued || resp.Recipients[0].State != relayapi.StateLive {
		t.Fatalf("recipients: %+v", resp.Recipients)
	}

	msgs := ofType(e.collect(p, "bob", 0, 3), relayapi.EventMessage)
	if len(msgs) != 1 || msgs[0].ID != resp.Seq {
		t.Fatalf("bob's messages: %+v", msgs)
	}
	var me relayapi.MessageEvent
	json.Unmarshal(msgs[0].Data, &me)
	got, err := envelope.ParseEnvelope(me.Envelope, envelope.MaxEnvelopeBytes)
	if err != nil {
		t.Fatal(err)
	}
	o, err := envelope.Open(p.keys.EncKey, got)
	if err != nil {
		t.Fatal(err)
	}
	m, err := envelope.DecodeMessage(o, got)
	if err != nil || m.Body != "secret-plan-7f3a" {
		t.Fatalf("decoded: %+v %v", m, err)
	}

	// The relay holds ciphertext only.
	raw, _ := os.ReadFile(e.dbPath)
	wal, _ := os.ReadFile(e.dbPath + "-wal")
	if bytes.Contains(raw, []byte("secret-plan-7f3a")) || bytes.Contains(wal, []byte("secret-plan-7f3a")) {
		t.Fatal("plaintext reached the relay database")
	}

	if err := e.client.Ack(ctx, p.keys, b.sid, resp.Seq); err != nil {
		t.Fatal(err)
	}
	if again := ofType(e.drain(p, "bob", 0), relayapi.EventMessage); len(again) != 0 {
		t.Fatalf("acked message redelivered: %+v", again)
	}
	receipts := ofType(e.drain(p, "alice", 0), relayapi.EventReceipt)
	var r relayapi.ReceiptEvent
	if len(receipts) != 1 || json.Unmarshal(receipts[0].Data, &r) != nil || r.Status != relayapi.ReceiptDelivered || r.RefSeq != resp.Seq || r.Recipient != b.sid {
		t.Fatalf("alice's receipt: %+v %+v", receipts, r)
	}
	_ = a
}

func TestBroadcastHoldAndResume(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice", "bob", "carol", "dave")
	e.client.EndSession(ctx, p.keys, p.people["dave"].sid)

	resp, err := e.client.PostMessage(ctx, p.keys, p.message(t, "alice", envelope.Recipients{All: true}, "to everyone"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range resp.Recipients {
		got[r.Session] = true
	}
	if len(got) != 2 || !got[p.people["bob"].sid] || !got[p.people["carol"].sid] {
		t.Fatalf("broadcast recipients: %+v", resp.Recipients)
	}

	// A direct message to an ended session is held until it resumes.
	held, err := e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(p.people["dave"].sid), "for dave"))
	if err != nil || held.Recipients[0].State != relayapi.StateEnded || held.Recipients[0].Status != relayapi.StatusQueued {
		t.Fatalf("held: %+v %v", held, err)
	}
	e.client.PutSession(ctx, p.keys, p.people["dave"].sid, nil)
	msgs := ofType(e.drain(p, "dave", 0), relayapi.EventMessage)
	if len(msgs) != 1 || msgs[0].ID != held.Seq {
		t.Fatalf("dave after resume: %+v", msgs)
	}

	// An ended sender can't send; unknown recipients are named.
	e.client.EndSession(ctx, p.keys, p.people["carol"].sid)
	if _, err := e.client.PostMessage(ctx, p.keys, p.message(t, "carol", to(p.people["bob"].sid), "x")); apiCode(err) != relayapi.CodeSessionEnded {
		t.Fatalf("ended sender: %v", err)
	}
	ghost := p.keys.SessionID("ghost")
	_, err = e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(ghost), "x"))
	if apiCode(err) != relayapi.CodeUnknownSessions || !strings.Contains(err.Error(), ghost) {
		t.Fatalf("unknown recipient: %v", err)
	}
}

func TestBroadcastWithNobodyElse(t *testing.T) {
	e := newEnv(t, nil)
	p := e.project("alice")
	if _, err := e.client.PostMessage(context.Background(), p.keys, p.message(t, "alice", envelope.Recipients{All: true}, "x")); apiCode(err) != relayapi.CodeNoRecipients {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamResumesFromLastEventID(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice", "bob")
	var seqs []int64
	for i := 0; i < 3; i++ {
		r, err := e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(p.people["bob"].sid), "m"))
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, r.Seq)
	}
	rest := ofType(e.collect(p, "bob", seqs[0], 2), relayapi.EventMessage)
	if len(rest) != 2 || rest[0].ID != seqs[1] || rest[1].ID != seqs[2] {
		t.Fatalf("resumed stream: %+v", rest)
	}
}

func TestStreamWakesOnNewMessages(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice", "bob")
	ctxS, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	got := make(chan relayclient.Event, 8)
	go e.client.Stream(ctxS, p.keys, p.people["bob"].sid, 0, func(ev relayclient.Event) error {
		got <- ev
		return nil
	})
	time.Sleep(100 * time.Millisecond) // let the stream drain its backlog
	r, err := e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(p.people["bob"].sid), "live"))
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ev := <-got:
			if ev.Type == relayapi.EventMessage && ev.ID == r.Seq {
				return
			}
		case <-ctxS.Done():
			t.Fatal("stream did not deliver a message posted while it was open")
		}
	}
}

func TestQueueLimit(t *testing.T) {
	e := newEnv(t, func(o *relay.Options) { o.Limits.MaxQueueMessages = 2 })
	ctx := context.Background()
	p := e.project("alice", "bob")
	for i := 0; i < 2; i++ {
		if _, err := e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(p.people["bob"].sid), "m")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(p.people["bob"].sid), "m")); apiCode(err) != relayapi.CodeQueueFull {
		t.Fatalf("full queue: %v", err)
	}
}

func TestGCExpiresHeldMessagesAndPurges(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice", "bob")
	r, err := e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(p.people["bob"].sid), "never read"))
	if err != nil {
		t.Fatal(err)
	}
	// Alice stays around (heartbeats); bob never reads.
	for i := 0; i < 7; i++ {
		e.clock.Add(24 * time.Hour)
		e.client.PutSession(ctx, p.keys, p.people["alice"].sid, nil)
	}
	e.clock.Add(time.Minute)
	if err := e.srv.GC(ctx); err != nil {
		t.Fatal(err)
	}
	receipts := ofType(e.drain(p, "alice", 0), relayapi.EventReceipt)
	var rc relayapi.ReceiptEvent
	if len(receipts) != 1 || json.Unmarshal(receipts[0].Data, &rc) != nil || rc.Status != relayapi.ReceiptExpired || rc.RefSeq != r.Seq {
		t.Fatalf("expiry receipt: %+v %+v", receipts, rc)
	}

	// Bob was last seen 7 days ago: purged, and no longer addressable.
	resp, err := e.client.Sessions(ctx, p.keys)
	if err != nil || len(resp.Sessions) != 1 || resp.Sessions[0].Session != p.people["alice"].sid {
		t.Fatalf("after purge: %+v %v", resp, err)
	}

	// A stream resuming from before the pruned events is told to resync.
	evs := e.collect(p, "alice", 1, 1)
	if evs[0].Type != relayapi.EventResync || evs[0].ID != 0 {
		t.Fatalf("expected a resync first, got %+v", evs[0])
	}
}

func TestIdleProjectsArePurged(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice")
	e.clock.Add(91 * 24 * time.Hour)
	if err := e.srv.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.Sessions(ctx, p.keys); apiCode(err) != relayapi.CodeNotFound {
		t.Fatalf("idle project still there: %v", err)
	}
}

func TestRejectsMismatchedEnvelopes(t *testing.T) {
	e := newEnv(t, func(o *relay.Options) { o.Limits.MaxEnvelopeBytes = 2048 })
	ctx := context.Background()
	p := e.project("alice", "bob")
	other := e.project("carol")
	foreign := other.message(t, "carol", to(other.people["carol"].sid), "x")
	if _, err := e.client.PostMessage(ctx, p.keys, foreign); apiCode(err) != relayapi.CodeBadRequest {
		t.Fatalf("other project's envelope: %v", err)
	}
	big := p.message(t, "alice", to(p.people["bob"].sid), strings.Repeat("y", 4000))
	if _, err := e.client.PostMessage(ctx, p.keys, big); apiCode(err) != relayapi.CodePayloadTooLarge {
		t.Fatalf("oversized: %v", err)
	}
	if _, err := e.client.PostRoster(ctx, p.keys, p.message(t, "alice", to(p.people["bob"].sid), "x")); apiCode(err) != relayapi.CodeBadRequest {
		t.Fatalf("message posted as roster: %v", err)
	}
}

func TestLogsCarryNoSecrets(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice", "bob")
	e.client.PostMessage(ctx, p.keys, p.message(t, "alice", to(p.people["bob"].sid), "m"))
	logs := e.logs.String()
	for _, secret := range []string{p.keys.Bearer(), relayToken, p.keys.ID, p.people["bob"].sid} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log contains %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, p.keys.ID[:6]+"…") {
		t.Fatalf("log lacks the truncated project ID:\n%s", logs)
	}
}

func TestErrorShape(t *testing.T) {
	e := newEnv(t, nil)
	resp, err := http.Get(e.http.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb relayapi.ErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&eb); err != nil || resp.StatusCode != 404 || eb.Error.Code != relayapi.CodeNotFound || eb.Error.Message == "" || eb.Error.Hint == "" {
		t.Fatalf("error body: %d %+v %v", resp.StatusCode, eb, err)
	}
}

func TestRetireDeletesAfterTheDelay(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	p := e.project("alice")
	if _, err := e.client.RetireProject(ctx, p.keys, 8*24*time.Hour); apiCode(err) != relayapi.CodeBadRequest {
		t.Fatalf("too long: %v", err)
	}
	at, err := e.client.RetireProject(ctx, p.keys, 24*time.Hour)
	if err != nil || at == "" {
		t.Fatalf("retire: %q %v", at, err)
	}
	e.clock.Add(23 * time.Hour)
	e.client.PutSession(ctx, p.keys, p.people["alice"].sid, nil) // activity doesn't cancel retirement
	e.srv.GC(ctx)
	if _, err := e.client.Sessions(ctx, p.keys); err != nil {
		t.Fatalf("deleted early: %v", err)
	}
	e.clock.Add(2 * time.Hour)
	e.srv.GC(ctx)
	if _, err := e.client.Sessions(ctx, p.keys); apiCode(err) != relayapi.CodeNotFound {
		t.Fatalf("not deleted after the delay: %v", err)
	}
}

func TestMigratesOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	srv, err := relay.New(relay.Options{DBPath: path, RelayToken: relayToken})
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	db, _ := sql.Open("sqlite", "file:"+path)
	db.Exec(`ALTER TABLE projects DROP COLUMN retire_at`)
	db.Exec(`UPDATE meta SET v = '1' WHERE k = 'schema'`)
	db.Close()
	srv, err = relay.New(relay.Options{DBPath: path, RelayToken: relayToken})
	if err != nil {
		t.Fatalf("v1 database not migrated: %v", err)
	}
	srv.Close()
	db, _ = sql.Open("sqlite", "file:"+path)
	db.Exec(`UPDATE meta SET v = '99' WHERE k = 'schema'`)
	db.Close()
	if _, err := relay.New(relay.Options{DBPath: path, RelayToken: relayToken}); err == nil || !strings.Contains(err.Error(), "newer than this relay") {
		t.Fatalf("newer schema: %v", err)
	}
}
