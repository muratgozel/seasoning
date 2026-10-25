package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/relayclient"
	"github.com/muratgozel/seasoning/internal/roster"
	"github.com/muratgozel/seasoning/internal/runtime"
)

// Quarantine reasons and discard counters (visible in doctor and tail).
const (
	QuarantineKeyChanged    = "key_changed"
	QuarantineUnknownPerson = "unknown_person"
	QuarantineClock         = "clock_window"

	countUndecryptable      = "undecryptable"
	countBadSignature       = "bad_signature"
	countMalformed          = "malformed"
	countMisrouted          = "misrouted"
	countUnverifiedPresence = "unverified_presence"
	countRosterConflict     = "roster_conflict"
)

// Replay window (SPEC §7.3): a message dated well outside its arrival time
// is held back as a suspected replay.
const (
	maxAge   = 7*24*time.Hour + time.Hour
	maxAhead = 10 * time.Minute
)

func envelopeKey(b *bound) string { return keys.FormatPublic(b.j.Identity.Public()) }

// presenceKey identifies a presence document's content, ignoring its
// timestamp, to detect changes worth publishing.
func presenceKey(p *envelope.Presence) string {
	c := *p
	c.UpdatedAt = ""
	out, _ := json.Marshal(c)
	return string(out)
}

// handle processes one stream item. A bad item is counted and acked, never
// allowed to stall the stream.
func (a *Agent) handle(ctx context.Context, b *bound, ev relayclient.Event) {
	ack := false
	switch ev.Type {
	case relayapi.EventMessage:
		a.onMessage(ctx, b, ev.Data)
		ack = true
	case relayapi.EventReceipt:
		a.onReceipt(ctx, b, ev.Data)
		ack = true
	case relayapi.EventSession:
		var info relayapi.Session
		if err := json.Unmarshal(ev.Data, &info); err == nil {
			if p, ok := a.peerFrom(ctx, b, info); ok {
				a.o.DB.PutPeer(ctx, p)
			}
		}
	case relayapi.EventRoster:
		var e relayapi.RosterEntry
		if err := json.Unmarshal(ev.Data, &e); err == nil {
			a.pinRoster(ctx, b, []relayapi.RosterEntry{e})
		}
	case relayapi.EventResync:
		a.refreshRoster(ctx, b, true)
		a.refreshPeers(ctx, b)
	}
	if ev.ID > 0 {
		if err := a.o.DB.SetCursor(ctx, b.localID, ev.ID); err != nil {
			a.o.Log("save cursor", err)
		}
		if ack {
			if err := b.client.Ack(ctx, b.j.Keys, b.sid, ev.ID); err != nil && ctx.Err() == nil {
				a.o.Log("ack", err) // replayed on reconnect; StoreMessage dedupes
			}
		}
	}
}

func (a *Agent) count(ctx context.Context, b *bound, kind string) {
	a.o.DB.Count(ctx, b.j.Keys.ID, kind, a.o.Now())
}

func (a *Agent) onMessage(ctx context.Context, b *bound, data []byte) {
	var me relayapi.MessageEvent
	if err := json.Unmarshal(data, &me); err != nil {
		a.count(ctx, b, countMalformed)
		return
	}
	env, err := envelope.ParseEnvelope(me.Envelope, envelope.MaxEnvelopeBytes)
	if err != nil {
		a.count(ctx, b, countMalformed)
		return
	}
	o, err := envelope.Open(b.j.Keys.EncKey, env)
	switch {
	case errors.Is(err, envelope.ErrUndecryptable):
		a.count(ctx, b, countUndecryptable)
		return
	case errors.Is(err, envelope.ErrBadSignature):
		a.count(ctx, b, countBadSignature)
		return
	case err != nil:
		a.count(ctx, b, countMalformed)
		return
	}
	m, err := envelope.DecodeMessage(o, env)
	if err != nil {
		a.count(ctx, b, countMalformed)
		return
	}
	s, err := a.o.DB.Session(ctx, b.localID)
	if err != nil || s == nil {
		a.o.Log("receive", fmt.Errorf("session %s not recorded: %v", b.localID, err))
		return
	}
	direct, addressed := addressedTo(m.To, s)
	if !addressed {
		a.count(ctx, b, countMisrouted)
		return
	}
	from, _ := envelope.ParseAddress(m.From)
	quarantine := ""
	switch a.trust(ctx, b, from.Person, m.FromKey) {
	case roster.KeyChanged:
		quarantine = QuarantineKeyChanged
	case roster.Unknown:
		quarantine = QuarantineUnknownPerson
	}
	received, err := envelope.ParseTime(me.ReceivedAt)
	if err != nil {
		received = a.o.Now()
	}
	date, _ := envelope.ParseTime(m.Date)
	if quarantine == "" && (date.Before(received.Add(-maxAge)) || date.After(received.Add(maxAhead))) {
		quarantine = QuarantineClock
	}
	inReply := ""
	if m.InReplyTo != nil {
		inReply = *m.InReplyTo
	}
	wake := m.Urgent || (direct && m.WakesDirect())
	stored, err := a.o.DB.StoreMessage(ctx, localdb.Message{
		RelaySession: b.sid, Seq: me.Seq, Project: b.j.Keys.ID, MessageID: m.MessageID, Thread: m.Thread(),
		References: m.References, FromHandle: m.From, FromPerson: from.Person, FromSession: m.FromSession,
		To: m.To, Subject: m.Subject, Body: m.Body, Date: m.Date, InReplyTo: inReply,
		ReplyExpected: m.ReplyExpected, Urgent: m.Urgent, Wake: wake, ReceivedAt: received.UnixMilli(),
		StoredAt: a.o.Now().UnixMilli(), Quarantine: quarantine, FromKey: m.FromKey,
	})
	if err != nil {
		a.o.Log("store message", err)
		return
	}
	if !stored || quarantine != "" {
		return
	}
	// Markers after the commit: a hook that sees one finds the row.
	if err := a.o.Runtime.Set(runtime.Unread); err != nil {
		a.o.Log("set unread marker", err)
	}
	if wake {
		if err := a.o.Runtime.Set(runtime.Wake); err != nil {
			a.o.Log("set wake marker", err)
		}
	}
}

// addressedTo reports whether a message's addresses include session s, and
// whether one names s specifically (only those can wake without "urgent").
func addressedTo(to []string, s *localdb.Session) (direct, ok bool) {
	mine := map[string]bool{s.Handle(): true}
	for _, h := range s.AliasHandles() {
		mine[h] = true
	}
	for _, raw := range to {
		addr, err := envelope.ParseAddress(raw)
		if err != nil {
			continue
		}
		switch {
		case addr.All:
			ok = true
		case !addr.IsSession() && addr.Person == s.Person:
			ok = true
		case addr.IsSession() && mine[raw]:
			ok, direct = true, true
		}
	}
	return direct, ok
}

func (a *Agent) onReceipt(ctx context.Context, b *bound, data []byte) {
	var r relayapi.ReceiptEvent
	if err := json.Unmarshal(data, &r); err != nil {
		a.count(ctx, b, countMalformed)
		return
	}
	at, err := envelope.ParseTime(r.At)
	if err != nil {
		at = a.o.Now()
	}
	if err := a.o.DB.StoreReceipt(ctx, b.sid, r.RefSeq, localdb.Receipt{Recipient: r.Recipient, Status: r.Status, At: at.UnixMilli()}); err != nil {
		a.o.Log("store receipt", err)
	}
}

// trust decides whether key belongs to person. An unknown person triggers a
// roster refresh first: a new member's roster record may have just landed.
func (a *Agent) trust(ctx context.Context, b *bound, person, key string) roster.Verdict {
	v, err := roster.Check(a.o.Config, b.j.Keys.ID, person, key)
	if err != nil {
		a.o.Log("check pin", err)
		return roster.Unknown
	}
	if v == roster.Unknown && a.refreshRoster(ctx, b, false) {
		v, _ = roster.Check(a.o.Config, b.j.Keys.ID, person, key)
	}
	return v
}

// refreshRoster fetches and pins the roster, at most every 30 seconds unless
// forced. It reports whether it fetched.
func (a *Agent) refreshRoster(ctx context.Context, b *bound, force bool) bool {
	b.mu.Lock()
	if !force && a.o.Now().Sub(b.rosterFetched) < 30*time.Second {
		b.mu.Unlock()
		return false
	}
	b.rosterFetched = a.o.Now()
	b.mu.Unlock()
	entries, err := b.client.Roster(ctx, b.j.Keys)
	if err != nil {
		if ctx.Err() == nil {
			a.relayDown(b, err)
			a.o.Log("fetch roster", err)
		}
		return false
	}
	a.pinRoster(ctx, b, entries)
	return true
}

func (a *Agent) pinRoster(ctx context.Context, b *bound, entries []relayapi.RosterEntry) {
	members, problems := roster.Read(entries, b.j.Keys)
	for range problems {
		a.count(ctx, b, countRosterConflict)
	}
	conflicts, err := roster.Pin(a.o.Config, b.j.Keys.ID, members, a.o.Now())
	if err != nil {
		a.o.Log("pin roster", err)
	}
	for range conflicts {
		a.count(ctx, b, countRosterConflict)
	}
}

// peerFrom turns a relay session listing into a cache row, keeping its
// presence only if it opens, verifies and belongs to a trusted key.
func (a *Agent) peerFrom(ctx context.Context, b *bound, info relayapi.Session) (localdb.Peer, bool) {
	if !keys.ValidSessionID(info.Session) {
		return localdb.Peer{}, false
	}
	p := localdb.Peer{Project: b.j.Keys.ID, Session: info.Session, State: info.State, UpdatedAt: a.o.Now().UnixMilli()}
	if t, err := envelope.ParseTime(info.LastSeenAt); err == nil {
		p.LastSeenAt = t.UnixMilli()
	}
	if t, err := envelope.ParseTime(info.EndedAt); err == nil {
		p.EndedAt = t.UnixMilli()
	}
	if len(info.Presence) == 0 {
		return p, true
	}
	env, err := envelope.ParseEnvelope(info.Presence, envelope.MaxPresenceBytes)
	if err != nil || env.FromSession != info.Session {
		a.count(ctx, b, countUnverifiedPresence)
		return p, true
	}
	o, err := envelope.Open(b.j.Keys.EncKey, env)
	if err != nil {
		a.count(ctx, b, countUnverifiedPresence)
		return p, true
	}
	pres, err := envelope.DecodePresence(o)
	if err != nil {
		a.count(ctx, b, countUnverifiedPresence)
		return p, true
	}
	if info.Session != b.sid && a.trust(ctx, b, pres.Person, pres.Key) != roster.Trusted {
		a.count(ctx, b, countUnverifiedPresence)
		return p, true
	}
	p.Presence = pres
	return p, true
}

// refreshPeers replaces the peer cache with the relay's listing.
func (a *Agent) refreshPeers(ctx context.Context, b *bound) error {
	resp, err := b.client.Sessions(ctx, b.j.Keys)
	if err != nil {
		if ctx.Err() == nil {
			a.relayDown(b, err)
		}
		return err
	}
	a.relayUp()
	var peers []localdb.Peer
	for _, info := range resp.Sessions {
		if p, ok := a.peerFrom(ctx, b, info); ok {
			peers = append(peers, p)
		}
	}
	return a.o.DB.ReplacePeers(ctx, b.j.Keys.ID, peers)
}
