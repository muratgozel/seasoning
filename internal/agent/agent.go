// Package agent is the session engine behind `seasoning mcp` (SPEC §4.2,
// §9, §10). One runs per Claude Code process. It follows the process's
// current session through the runtime binding (so /clear rebinds), keeps the
// session registered at the relay with heartbeats and presence, consumes its
// event stream into the local store, and serves the MCP tools.
package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/relayclient"
	"github.com/muratgozel/seasoning/internal/runtime"
	"github.com/muratgozel/seasoning/internal/session"
)

// Options wire an Agent to its surroundings. Zero intervals get the SPEC
// defaults; tests shrink them.
type Options struct {
	Config    config.Store
	DB        *localdb.DB
	Runtime   runtime.Dir
	Getenv    func(string) string
	Now       func() time.Time
	NewClient func(base string) *relayclient.Client
	Log       func(where string, err error)
	Version   string

	Heartbeat        time.Duration // relay heartbeat (20s)
	PresenceDebounce time.Duration // minimum gap between presence publishes (5s)
	PresencePoll     time.Duration // how often local state is checked for changes (2s)
	PeersRefresh     time.Duration // full peer list refresh (60s)
	BindPoll         time.Duration // binding.json check (1s)
	BindWait         time.Duration // wait for SessionStart before starting alone (10s)
	History          time.Duration // local history kept (30 days)
}

func (o *Options) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.Heartbeat, 20*time.Second)
	def(&o.PresenceDebounce, 5*time.Second)
	def(&o.PresencePoll, 2*time.Second)
	def(&o.PeersRefresh, 60*time.Second)
	def(&o.BindPoll, time.Second)
	def(&o.BindWait, 10*time.Second)
	def(&o.History, 30*24*time.Hour)
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.NewClient == nil {
		o.NewClient = relayclient.New
	}
	if o.Log == nil {
		o.Log = func(string, error) {}
	}
}

// Agent is one Claude Code process's connection to seasoning.
type Agent struct {
	o Options

	mu        sync.Mutex
	cur       *bound
	downSince time.Time
	kick      chan struct{} // re-check the binding now
	bindings  chan struct{} // closed and replaced after each rebind
}

// New builds an agent.
func New(o Options) *Agent {
	o.defaults()
	return &Agent{o: o, kick: make(chan struct{}, 1), bindings: make(chan struct{})}
}

// bound is the session the agent currently serves.
type bound struct {
	localID string
	j       *session.Joined
	sid     string
	client  *relayclient.Client
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	poke    chan struct{}

	mu            sync.Mutex
	lastPresence  string
	lastPublish   time.Time
	rosterFetched time.Time
}

// Run serves until ctx ends, then ends the session at the relay.
func (a *Agent) Run(ctx context.Context) error {
	start := a.o.Now()
	runtime.Sweep(start)
	a.o.DB.Prune(ctx, start.Add(-a.o.History))
	var current string
	fallbackTried := false
	t := time.NewTicker(a.o.BindPoll)
	defer t.Stop()
	for {
		b, err := a.o.Runtime.Binding()
		if err != nil {
			a.o.Log("read binding", err)
		}
		if b == nil && !fallbackTried && a.o.Now().Sub(start) >= a.o.BindWait {
			fallbackTried = true
			b = a.fallback(ctx)
		}
		if b != nil && session.Off(a.o.Getenv, a.o.Runtime) {
			b.Project = "" // invisible: bound to nothing
		}
		if b != nil && b.SessionID+"|"+b.Project != current {
			current = b.SessionID + "|" + b.Project
			a.rebind(ctx, b)
			a.mu.Lock()
			close(a.bindings)
			a.bindings = make(chan struct{})
			a.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			a.unbind(true)
			return nil
		case <-t.C:
		case <-a.kick:
		}
	}
}

// fallback starts the session itself when no SessionStart hook ran (the
// plugin's hooks are missing or broken): tools still work, though nothing is
// injected without hooks.
func (a *Agent) fallback(ctx context.Context) *runtime.Binding {
	id := a.o.Getenv("CLAUDE_CODE_SESSION_ID")
	if id == "" {
		a.o.Log("start session", errors.New("no SessionStart binding and no CLAUDE_CODE_SESSION_ID; is the seasoning plugin's hooks file loaded?"))
		return nil
	}
	cwd := a.o.Getenv("CLAUDE_PROJECT_DIR")
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	headless := a.o.Getenv("CLAUDE_CODE_SESSION_ATTENDED") == "0"
	if _, _, err := session.Start(ctx, a.o.Config, a.o.DB, a.o.Runtime, id, cwd, "mcp", headless, session.Off(a.o.Getenv, a.o.Runtime), a.o.Now()); err != nil {
		a.o.Log("start session", err)
	}
	b, _ := a.o.Runtime.Binding()
	return b
}

func (a *Agent) rebind(ctx context.Context, b *runtime.Binding) {
	a.unbind(true)
	if b.Project == "" {
		return
	}
	j, err := session.ForProject(ctx, a.o.Config, b.Project, b.Cwd)
	if err != nil {
		a.o.Log("bind session", err)
		a.o.Runtime.SetProblem("seasoning could not load this session's project (" + err.Error() + "); run `seasoning doctor`")
		return
	}
	if j == nil {
		return
	}
	if s, err := a.o.DB.Session(ctx, b.SessionID); err != nil || s == nil {
		if _, _, err := session.Start(ctx, a.o.Config, a.o.DB, a.o.Runtime, b.SessionID, b.Cwd, b.Source, false, false, a.o.Now()); err != nil {
			a.o.Log("record session", err)
			return
		}
	}
	bctx, cancel := context.WithCancel(context.Background())
	bnd := &bound{localID: b.SessionID, j: j, sid: j.Keys.SessionID(b.SessionID), client: a.o.NewClient(j.Project.Relay), cancel: cancel, poke: make(chan struct{}, 1)}
	a.mu.Lock()
	a.cur = bnd
	a.mu.Unlock()
	bnd.wg.Add(3)
	go a.presenceLoop(bctx, bnd)
	go a.streamLoop(bctx, bnd)
	go a.peersLoop(bctx, bnd)
}

// unbind stops the current session's loops and, with endRelay, ends it at
// the relay (held messages stay there for a resume).
func (a *Agent) unbind(endRelay bool) {
	a.mu.Lock()
	b := a.cur
	a.cur = nil
	a.mu.Unlock()
	if b == nil {
		return
	}
	b.cancel()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	if endRelay {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := b.client.EndSession(ctx, b.j.Keys, b.sid); err != nil && !relayclient.IsCode(err, relayapi.CodeNotFound) {
			a.o.Log("end session at relay", err)
		}
	}
}

func (a *Agent) bound() *bound {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cur
}

// relayDown records that the relay can't be reached; hooks tell the agent
// at most once an hour (SPEC §17).
func (a *Agent) relayDown(b *bound, err error) {
	var ue *relayclient.UnreachableError
	if !errors.As(err, &ue) {
		return
	}
	a.mu.Lock()
	if a.downSince.IsZero() {
		a.downSince = a.o.Now()
	}
	since := a.downSince
	a.mu.Unlock()
	a.o.Runtime.SetProblem(fmt.Sprintf("relay %s unreachable since %s UTC; messages will arrive when it is back", b.j.Project.Relay, since.UTC().Format("15:04")))
}

func (a *Agent) relayUp() {
	a.mu.Lock()
	wasDown := !a.downSince.IsZero()
	a.downSince = time.Time{}
	a.mu.Unlock()
	if wasDown {
		a.o.Runtime.ClearProblem()
	}
}

// DownSince reports when the relay became unreachable (zero when it is up).
func (a *Agent) DownSince() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.downSince
}

// --- presence --------------------------------------------------------------

func (a *Agent) presenceLoop(ctx context.Context, b *bound) {
	defer b.wg.Done()
	a.publish(ctx, b, true)
	beat := time.NewTicker(a.o.Heartbeat)
	check := time.NewTicker(a.o.PresencePoll)
	defer beat.Stop()
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.poke:
			a.publish(ctx, b, true)
		case <-check.C:
			a.publish(ctx, b, false)
		case <-beat.C:
			a.heartbeat(ctx, b)
		}
	}
}

// presence builds the session's presence document from local state.
func (a *Agent) presence(ctx context.Context, b *bound) (*envelope.Presence, error) {
	s, err := a.o.DB.Session(ctx, b.localID)
	if err != nil || s == nil {
		return nil, fmt.Errorf("session %s is not recorded: %v", b.localID, err)
	}
	files, err := a.o.DB.Files(ctx, b.localID, envelope.MaxFiles)
	if err != nil {
		return nil, err
	}
	p := &envelope.Presence{
		Handle: s.Handle(), Aliases: s.AliasHandles(), Person: s.Person, Status: s.Status,
		Activity: s.Activity, Repo: s.Repo, Branch: s.Branch, Worktree: s.Worktree, Headless: s.Headless,
		StartedAt: envelope.FormatTime(time.UnixMilli(s.StartedAt)), UpdatedAt: envelope.FormatTime(a.o.Now()),
		AutonomousTurns: s.AutonomousTurns, Files: []envelope.PresenceFile{},
		Client: envelope.ClientInfo{Seasoning: a.o.Version}, Key: envelopeKey(b),
	}
	for _, f := range files {
		p.Files = append(p.Files, envelope.PresenceFile{Path: f.Path, At: envelope.FormatTime(time.UnixMilli(f.At)), Via: f.Via})
	}
	return p, p.Validate()
}

// publish sends presence when it changed (and, unless forced, when the
// debounce allows).
func (a *Agent) publish(ctx context.Context, b *bound, force bool) {
	p, err := a.presence(ctx, b)
	if err != nil {
		a.o.Log("build presence", err)
		return
	}
	key := presenceKey(p)
	b.mu.Lock()
	changed := key != b.lastPresence
	due := force || a.o.Now().Sub(b.lastPublish) >= a.o.PresenceDebounce
	b.mu.Unlock()
	if !changed || !due {
		return
	}
	env, err := envelope.Seal(b.j.Keys.EncKey, b.j.Identity, envelope.KindPresence, b.j.Keys.ID, b.sid, envelope.Recipients{All: true}, p)
	if err != nil {
		a.o.Log("seal presence", err)
		return
	}
	if _, err := b.client.PutSession(ctx, b.j.Keys, b.sid, env); err != nil {
		a.putFailed(ctx, b, err)
		return
	}
	a.relayUp()
	b.mu.Lock()
	b.lastPresence, b.lastPublish = key, a.o.Now()
	b.mu.Unlock()
}

// heartbeat keeps the session live; it republishes presence if the relay
// lost it (its last publish failed).
func (a *Agent) heartbeat(ctx context.Context, b *bound) {
	b.mu.Lock()
	published := b.lastPresence != ""
	b.mu.Unlock()
	if !published {
		a.publish(ctx, b, true)
		return
	}
	if _, err := b.client.PutSession(ctx, b.j.Keys, b.sid, nil); err != nil {
		a.putFailed(ctx, b, err)
		return
	}
	a.relayUp()
}

// putFailed records a failed registration or heartbeat. Failures caused by
// the binding being torn down are not problems.
func (a *Agent) putFailed(ctx context.Context, b *bound, err error) {
	if ctx.Err() != nil {
		return
	}
	switch {
	case relayclient.IsCode(err, relayapi.CodeNotFound):
		a.o.Runtime.SetProblem(fmt.Sprintf("project %q no longer exists on its relay (it was rotated or removed); ask a member for a fresh invite", b.j.Project.Name))
	default:
		a.relayDown(b, err)
	}
	a.o.Log("publish presence", err)
	b.mu.Lock()
	b.lastPresence = "" // republish once the relay is back
	b.mu.Unlock()
}

// registered reports whether the session's presence reached the relay.
func (b *bound) registered() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastPresence != ""
}

// Poke publishes presence now (after set_status).
func (b *bound) Poke() {
	select {
	case b.poke <- struct{}{}:
	default:
	}
}

// --- stream ----------------------------------------------------------------

func (a *Agent) streamLoop(ctx context.Context, b *bound) {
	defer b.wg.Done()
	backoff := time.Second
	for ctx.Err() == nil {
		s, err := a.o.DB.Session(ctx, b.localID)
		if err != nil || s == nil {
			a.o.Log("read cursor", fmt.Errorf("session %s: %v", b.localID, err))
			sleep(ctx, time.Second)
			continue
		}
		connected := a.o.Now()
		err = b.client.Stream(ctx, b.j.Keys, b.sid, s.Cursor, func(ev relayclient.Event) error {
			a.relayUp()
			a.handle(ctx, b, ev)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if relayclient.IsCode(err, relayapi.CodeNotFound) {
			// Not registered yet (the first presence PUT is racing us) or
			// purged: re-register, then reconnect.
			a.publish(ctx, b, true)
		} else {
			a.relayDown(b, err)
		}
		if a.o.Now().Sub(connected) > 30*time.Second {
			backoff = time.Second
		}
		sleep(ctx, backoff+time.Duration(rand.Int64N(int64(backoff/2+1))))
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *Agent) peersLoop(ctx context.Context, b *bound) {
	defer b.wg.Done()
	a.refreshRoster(ctx, b, true)
	a.refreshPeers(ctx, b)
	t := time.NewTicker(a.o.PeersRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refreshPeers(ctx, b)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
