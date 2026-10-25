package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/inject"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/project"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/relayclient"
	"github.com/muratgozel/seasoning/internal/session"
)

// current returns the joined session or an error that tells the agent what
// its human can do about it.
func (a *Agent) current(ctx context.Context) (*bound, *localdb.Session, error) {
	if session.Off(a.o.Getenv, a.o.Runtime) {
		return nil, nil, errors.New("seasoning is switched off for this session (by /seasoning:off or SEASONING=off), so peers can't see it; turn it back on with /seasoning:on")
	}
	b := a.bound()
	if b == nil {
		// The SessionStart hook may have just written the binding: have Run
		// pick it up now rather than fail a tool call made in the session's
		// first second.
		if bind, _ := a.o.Runtime.Binding(); bind != nil && bind.Project != "" {
			b = a.awaitBinding(ctx, 3*time.Second)
		}
	}
	if b == nil {
		bind, _ := a.o.Runtime.Binding()
		switch {
		case bind == nil:
			return nil, nil, errors.New("seasoning hasn't seen this session start yet; if this persists, the seasoning plugin's hooks are not loaded (ask your human to run `seasoning doctor`)")
		case bind.Project == "":
			return nil, nil, errors.New("this directory isn't linked to a seasoning project. Ask your human to run `seasoning join '<invite>'` (or `seasoning link`) in this repository, then /clear or restart the session")
		default:
			return nil, nil, errors.New("seasoning is still connecting this session; try again in a few seconds")
		}
	}
	s, err := a.o.DB.Session(ctx, b.localID)
	if err != nil {
		return nil, nil, err
	}
	if s == nil {
		return nil, nil, errors.New("this session is not recorded locally yet; try again in a few seconds")
	}
	return b, s, nil
}

// WaitBound waits up to max for the agent to bind its session and register
// it at the relay (the human CLI starts an agent and then acts at once).
// Binding alone isn't enough: registration is the presence loop's first
// PUT, and a send that beats it is refused as from an unknown session. A
// relay known to be down ends the wait early, so the action that follows
// fails fast with its own actionable error (SPEC D25).
func (a *Agent) WaitBound(ctx context.Context, max time.Duration) error {
	deadline := time.Now().Add(max)
	b := a.awaitBinding(ctx, max)
	for b != nil && !b.registered() && a.DownSince().IsZero() {
		if time.Now().After(deadline) {
			b = nil
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	if b == nil {
		return errors.New("seasoning could not start a session for this project in time; check that the relay is reachable (seasoning doctor)")
	}
	return nil
}

// awaitBinding nudges Run to re-read the binding and waits up to max for a
// bound session.
func (a *Agent) awaitBinding(ctx context.Context, max time.Duration) *bound {
	deadline := time.NewTimer(max)
	defer deadline.Stop()
	for {
		a.mu.Lock()
		b, next := a.cur, a.bindings
		a.mu.Unlock()
		if b != nil {
			return b
		}
		select {
		case a.kick <- struct{}{}:
		default:
		}
		select {
		case <-next:
		case <-time.After(100 * time.Millisecond):
		case <-deadline.C:
			return a.bound()
		case <-ctx.Done():
			return nil
		}
	}
}

// peerView is a peer with trusted presence, for resolution and listing.
type peerView struct {
	localdb.Peer
	p *envelope.Presence
}

func (a *Agent) peerViews(ctx context.Context, b *bound, fresh bool) ([]peerView, string, error) {
	note := ""
	if fresh {
		if err := a.refreshPeers(ctx, b); err != nil {
			var ue *relayclient.UnreachableError
			if !errors.As(err, &ue) {
				return nil, "", err
			}
			note = "The relay is unreachable; this is the last known list."
		}
	}
	peers, err := a.o.DB.Peers(ctx, b.j.Keys.ID)
	if err != nil {
		return nil, "", err
	}
	var out []peerView
	for _, p := range peers {
		if p.Session == b.sid || p.Presence == nil {
			continue
		}
		out = append(out, peerView{Peer: p, p: p.Presence})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].State == relayapi.StateEnded) != (out[j].State == relayapi.StateEnded) {
			return out[j].State == relayapi.StateEnded
		}
		return out[i].p.Handle < out[j].p.Handle
	})
	return out, note, nil
}

// Peers lists the project's other sessions.
func (a *Agent) Peers(ctx context.Context) (string, error) {
	b, s, err := a.current(ctx)
	if err != nil {
		return "", err
	}
	views, note, err := a.peerViews(ctx, b, true)
	if err != nil {
		return "", err
	}
	unread, _, _ := a.o.DB.Pending(ctx, b.sid)
	var w strings.Builder
	fmt.Fprintf(&w, "You are %s in project %q", s.Handle(), b.j.Project.Name)
	if s.Status != "" {
		fmt.Fprintf(&w, " (status: %q)", s.Status)
	}
	fmt.Fprintf(&w, ". %d unsurfaced message(s).\n", unread)
	if s.AutonomousTurns > 0 {
		fmt.Fprintf(&w, "Peer messages have woken you %d time(s) since your human last prompted you.\n", s.AutonomousTurns)
	}
	if note != "" {
		w.WriteString(note + "\n")
	}
	now := a.o.Now()
	var live, ended []peerView
	for _, v := range views {
		if v.State == relayapi.StateEnded {
			ended = append(ended, v)
		} else {
			live = append(live, v)
		}
	}
	if len(live) == 0 {
		w.WriteString("No other sessions are live in this project.\n")
	} else {
		w.WriteString("Live peers:\n")
		for _, v := range live {
			fmt.Fprintf(&w, "- %s: %s, %s", v.p.Handle, v.State, v.p.Activity)
			if v.p.Branch != "" {
				fmt.Fprintf(&w, ", branch %s", v.p.Branch)
			}
			if v.p.Repo != "" {
				fmt.Fprintf(&w, " in %s", v.p.Repo)
			}
			fmt.Fprintf(&w, ", seen %s\n", inject.Age(now.Sub(time.UnixMilli(v.LastSeenAt))))
			if v.p.Status != "" {
				fmt.Fprintf(&w, "  status: %q\n", v.p.Status)
			}
			if len(v.p.Aliases) > 0 {
				fmt.Fprintf(&w, "  also addressable as: %s\n", strings.Join(v.p.Aliases, ", "))
			}
			if v.p.AutonomousTurns > 0 {
				fmt.Fprintf(&w, "  woken %d time(s) since its human last prompted it\n", v.p.AutonomousTurns)
			}
			if len(v.p.Files) > 0 {
				var fs []string
				for i, f := range v.p.Files {
					if i == 5 {
						fs = append(fs, fmt.Sprintf("and %d more", len(v.p.Files)-5))
						break
					}
					at, _ := envelope.ParseTime(f.At)
					fs = append(fs, fmt.Sprintf("%s (%s)", f.Path, inject.Age(now.Sub(at))))
				}
				fmt.Fprintf(&w, "  recently changed: %s\n", strings.Join(fs, ", "))
			}
		}
	}
	if len(ended) > 0 {
		w.WriteString("Ended (messages to them are held up to 7 days in case they resume):\n")
		for _, v := range ended {
			fmt.Fprintf(&w, "- %s: ended %s\n", v.p.Handle, inject.Age(now.Sub(time.UnixMilli(v.EndedAt))))
		}
	}
	return w.String(), nil
}

// SendInput is a send request.
type SendInput struct {
	To            []string
	Subject       string
	Body          string
	ReplyExpected *bool
	Urgent        bool
	// Wake overrides whether sessions named directly wake if idle; nil means
	// the default (SPEC D27): wake when a reply is expected, or when the
	// message answers a question still waiting for this session's reply.
	Wake      *bool
	InReplyTo string
}

// Send validates, resolves, seals and posts a message (SPEC §9.2).
func (a *Agent) Send(ctx context.Context, in SendInput) (string, error) {
	b, s, err := a.current(ctx)
	if err != nil {
		return "", err
	}
	if in.ReplyExpected == nil {
		return "", errors.New("reply_expected is required: true if you need an answer, false for FYI")
	}
	if in.Urgent && in.Wake != nil && !*in.Wake {
		return "", errors.New("urgent wakes every recipient, so it can't be combined with wake=false; drop one of them")
	}
	in.Body = strings.ReplaceAll(strings.ReplaceAll(in.Body, "\r\n", "\n"), "\r", "\n")
	in.Subject = strings.TrimSpace(in.Subject)
	if len(in.To) == 0 {
		return "", errors.New("to is empty; address a session (person/label), a person's sessions (person/*), or everyone (*)")
	}
	var addrs []envelope.Address
	seen := map[string]bool{}
	for _, raw := range in.To {
		raw = strings.TrimSpace(raw)
		ad, err := envelope.ParseAddress(raw)
		if err != nil {
			return "", err
		}
		if seen[ad.String()] {
			continue
		}
		seen[ad.String()] = true
		addrs = append(addrs, ad)
	}
	if seen["*"] && len(addrs) > 1 {
		return "", errors.New(`"*" already means every live session; send it alone`)
	}

	views, _, err := a.peerViews(ctx, b, true)
	if err != nil {
		return "", err
	}
	if !a.DownSince().IsZero() {
		return "", a.unreachable(b, nil)
	}
	recipients, handles, err := resolve(addrs, views, s)
	if err != nil {
		return "", err
	}

	msg := envelope.Message{
		Project: b.j.Keys.ID, From: s.Handle(), FromSession: b.sid, FromKey: envelopeKey(b),
		Date: envelope.FormatTime(a.o.Now()), Subject: in.Subject, References: []string{},
		ReplyExpected: *in.ReplyExpected, Urgent: in.Urgent, Body: in.Body,
	}
	for _, ad := range addrs {
		msg.To = append(msg.To, ad.String())
	}
	if msg.MessageID, err = envelope.NewMessageID(a.o.Now()); err != nil {
		return "", err
	}
	thread := msg.MessageID
	wake := msg.ReplyExpected
	if in.InReplyTo != "" {
		parent, err := a.o.DB.FindParent(ctx, b.sid, in.InReplyTo)
		if err != nil {
			return "", err
		}
		if parent == nil {
			return "", fmt.Errorf("in_reply_to %s is not a message this session received or sent; check the id with the inbox tool", in.InReplyTo)
		}
		// Its sender asked and is likely idle waiting for this answer.
		wake = wake || parent.AwaitsReply
		msg.InReplyTo = &parent.MessageID
		msg.References = append(append([]string{}, parent.References...), parent.MessageID)
		if len(msg.References) > envelope.MaxReferences {
			// Keep the root (the thread ID) and the most recent ancestors.
			msg.References = append([]string{msg.References[0]}, msg.References[len(msg.References)-envelope.MaxReferences+1:]...)
		}
		thread = msg.References[0]
	}
	if in.Wake != nil {
		wake = *in.Wake
	}
	wake = wake || msg.Urgent
	msg.Wake = &wake
	if err := msg.Validate(); err != nil {
		return "", err
	}
	env, err := envelope.Seal(b.j.Keys.EncKey, b.j.Identity, envelope.KindMessage, b.j.Keys.ID, b.sid, recipients, msg)
	if err != nil {
		return "", err
	}
	resp, err := b.client.PostMessage(ctx, b.j.Keys, env)
	var ue *relayclient.UnreachableError
	switch {
	case errors.As(err, &ue):
		return "", a.unreachable(b, err)
	case relayclient.IsCode(err, relayapi.CodeNoRecipients):
		return "", errors.New("nobody else is live in this project right now; the message was NOT sent")
	case relayclient.IsCode(err, relayapi.CodeUnknownSessions):
		a.refreshPeers(ctx, b)
		return "", errors.New("some recipients' sessions no longer exist on the relay; the message was NOT sent. Check the current list with the peers tool")
	case relayclient.IsCode(err, relayapi.CodeSessionEnded), relayclient.IsCode(err, relayapi.CodeNotFound):
		b.Poke()
		return "", errors.New("this session isn't registered at the relay right now; the message was NOT sent. Try again in a few seconds")
	case err != nil:
		return "", fmt.Errorf("the message was NOT sent: %w", err)
	}

	sent := localdb.Sent{
		RelaySession: b.sid, Seq: resp.Seq, Project: b.j.Keys.ID, MessageID: msg.MessageID, Thread: thread,
		References: msg.References, To: msg.To, Subject: msg.Subject, Body: msg.Body, Date: msg.Date,
		ReplyExpected: msg.ReplyExpected, Urgent: msg.Urgent, Recipients: map[string]string{}, StoredAt: a.o.Now().UnixMilli(),
	}
	for _, r := range resp.Recipients {
		sent.Recipients[r.Session] = handles[r.Session]
	}
	if err := a.o.DB.StoreSent(ctx, sent); err != nil {
		a.o.Log("store sent", err)
	}
	if in.InReplyTo != "" {
		a.o.DB.MarkReplied(ctx, b.sid, in.InReplyTo)
	}

	// Idle recipients this message won't wake, so the sender knows when it
	// will be read.
	asleep := map[string]bool{}
	for _, v := range views {
		if v.p.Activity == envelope.ActivityIdle && !msg.Urgent && !(wake && namedDirectly(addrs, v)) {
			asleep[v.Session] = true
		}
	}
	var w strings.Builder
	fmt.Fprintf(&w, "Sent %s %q (thread %s).\n", msg.MessageID, msg.Subject, thread)
	for _, r := range resp.Recipients {
		h := handles[r.Session]
		if h == "" {
			h = r.Session
		}
		switch {
		case r.Status == relayapi.StatusQueueFull:
			fmt.Fprintf(&w, "- %s: NOT delivered, its queue is full (it isn't reading messages)\n", h)
		case r.State == relayapi.StateEnded:
			fmt.Fprintf(&w, "- %s: session ended; held up to 7 days in case it resumes\n", h)
		case r.State == relayapi.StateAway:
			fmt.Fprintf(&w, "- %s: away (no recent heartbeat); queued\n", h)
		case asleep[r.Session]:
			fmt.Fprintf(&w, "- %s: live but idle; queued for its next turn, not woken\n", h)
		default:
			fmt.Fprintf(&w, "- %s: live; queued\n", h)
		}
	}
	if !msg.ReplyExpected {
		w.WriteString("No reply expected; don't wait for one.\n")
	}
	return w.String(), nil
}

func (a *Agent) unreachable(b *bound, err error) error {
	since := a.DownSince()
	if since.IsZero() {
		since = a.o.Now()
		a.relayDown(b, err)
	}
	detail := ""
	var ue *relayclient.UnreachableError
	if errors.As(err, &ue) {
		detail = fmt.Sprintf(" (%v)", ue.Err)
	}
	return fmt.Errorf("relay %s unreachable since %s UTC%s; the message was NOT sent. Retry later or tell your human", b.j.Project.Relay, since.UTC().Format("15:04"), detail)
}

// namedDirectly reports whether an address names v's session by its handle
// or an alias, rather than through person/* or *.
func namedDirectly(addrs []envelope.Address, v peerView) bool {
	for _, ad := range addrs {
		if h := ad.String(); ad.IsSession() && (v.p.Handle == h || slices.Contains(v.p.Aliases, h)) {
			return true
		}
	}
	return false
}

// resolve maps addresses to relay sessions using trusted presence (SPEC §9.2).
func resolve(addrs []envelope.Address, views []peerView, me *localdb.Session) (envelope.Recipients, map[string]string, error) {
	handles := map[string]string{}
	for _, v := range views {
		handles[v.Session] = v.p.Handle
	}
	var ids []string
	add := func(id string) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	liveList := func() string {
		var hs []string
		for _, v := range views {
			if v.State != relayapi.StateEnded {
				hs = append(hs, v.p.Handle)
			}
		}
		if len(hs) == 0 {
			return "none"
		}
		return strings.Join(hs, ", ")
	}
	for _, ad := range addrs {
		switch {
		case ad.All:
			return envelope.Recipients{All: true}, handles, nil
		case !ad.IsSession():
			// views never include this session, so person/* for yourself
			// reaches only your other sessions.
			n := 0
			var endedHandles []string
			for _, v := range views {
				if v.p.Person != ad.Person {
					continue
				}
				if v.State == relayapi.StateEnded {
					endedHandles = append(endedHandles, v.p.Handle)
					continue
				}
				add(v.Session)
				n++
			}
			if n == 0 {
				msg := fmt.Sprintf("%s has no live sessions in this project", ad.Person)
				if len(endedHandles) > 0 {
					msg += fmt.Sprintf("; address an ended one directly to have it held for a resume: %s", strings.Join(endedHandles, ", "))
				}
				return envelope.Recipients{}, nil, errors.New(msg)
			}
		default:
			h := ad.String()
			if h == me.Handle() || slices.Contains(me.AliasHandles(), h) {
				return envelope.Recipients{}, nil, fmt.Errorf("%s is this session; you can't message yourself", h)
			}
			var live, ended []peerView
			for _, v := range views {
				if v.p.Handle == h || slices.Contains(v.p.Aliases, h) {
					if v.State == relayapi.StateEnded {
						ended = append(ended, v)
					} else {
						live = append(live, v)
					}
				}
			}
			switch {
			case len(live) == 1:
				add(live[0].Session)
			case len(live) > 1:
				var hs []string
				for _, v := range live {
					hs = append(hs, fmt.Sprintf("%s (started %s)", v.p.Handle, v.p.StartedAt))
				}
				return envelope.Recipients{}, nil, fmt.Errorf("%s matches several live sessions: %s; ask them to rename with set_status, or address the person with %s/*", h, strings.Join(hs, "; "), ad.Person)
			case len(ended) > 0:
				add(ended[len(ended)-1].Session)
			default:
				return envelope.Recipients{}, nil, fmt.Errorf("no session is called %s; live peers: %s", h, liveList())
			}
		}
	}
	return envelope.Recipients{Sessions: ids}, handles, nil
}

// InboxInput selects an inbox view.
type InboxInput struct {
	View  string // unread (default), thread, recent, sent
	ID    string // thread or message ID for view=thread
	Limit int
}

// Inbox renders a view of the session's messages. Reading marks messages
// read and clears the markers they set.
func (a *Agent) Inbox(ctx context.Context, in InboxInput) (string, error) {
	b, _, err := a.current(ctx)
	if err != nil {
		return "", err
	}
	limit := in.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	now := a.o.Now()
	var w strings.Builder
	switch in.View {
	case "", "unread":
		if _, err := a.o.DB.Claim(ctx, b.sid, false, now); err != nil {
			return "", err
		}
		msgs, err := a.o.DB.Unread(ctx, b.sid, limit)
		if err != nil {
			return "", err
		}
		if len(msgs) == 0 {
			w.WriteString("No unread messages.\n")
		}
		var seqs []int64
		for _, m := range msgs {
			writeMessage(&w, m, now)
			seqs = append(seqs, m.Seq)
		}
		if err := a.o.DB.MarkRead(ctx, b.sid, seqs, now); err != nil {
			return "", err
		}
	case "thread":
		if !envelope.ValidMessageID(in.ID) {
			return "", fmt.Errorf("id %q is not a message ID such as <01JB8Z…@seasoning>", in.ID)
		}
		parent, err := a.o.DB.FindParent(ctx, b.sid, in.ID)
		if err != nil {
			return "", err
		}
		thread := in.ID
		if parent != nil {
			thread = parent.Thread
		}
		msgs, err := a.o.DB.ThreadMessages(ctx, b.sid, thread)
		if err != nil {
			return "", err
		}
		sent, err := a.o.DB.SentRecent(ctx, b.sid, 200)
		if err != nil {
			return "", err
		}
		type item struct {
			at   int64
			text string
		}
		var items []item
		var seqs []int64
		for _, m := range msgs {
			var sb strings.Builder
			writeMessage(&sb, m, now)
			items = append(items, item{m.ReceivedAt, sb.String()})
			seqs = append(seqs, m.Seq)
		}
		for _, s := range sent {
			if s.Thread == thread || s.MessageID == thread {
				var sb strings.Builder
				fmt.Fprintf(&sb, "[you sent] %s to %s, %s\nSubject: %s\n%s\n\n", s.MessageID, strings.Join(s.To, ", "), inject.Age(now.Sub(time.UnixMilli(s.StoredAt))), s.Subject, s.Body)
				items = append(items, item{s.StoredAt, sb.String()})
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].at < items[j].at })
		if len(items) == 0 {
			return "", fmt.Errorf("no messages in thread %s on this machine", thread)
		}
		fmt.Fprintf(&w, "Thread %s:\n\n", thread)
		for _, it := range items {
			w.WriteString(it.text)
		}
		a.o.DB.MarkRead(ctx, b.sid, seqs, now)
	case "recent":
		msgs, err := a.o.DB.Recent(ctx, b.sid, limit)
		if err != nil {
			return "", err
		}
		if len(msgs) == 0 {
			w.WriteString("No messages yet.\n")
		}
		for _, m := range msgs {
			writeMessage(&w, m, now)
		}
	case "sent":
		sent, err := a.o.DB.SentRecent(ctx, b.sid, limit)
		if err != nil {
			return "", err
		}
		if len(sent) == 0 {
			w.WriteString("You haven't sent anything in this session.\n")
		}
		for _, s := range sent {
			fmt.Fprintf(&w, "%s to %s, %s: %q\n", s.MessageID, strings.Join(s.To, ", "), inject.Age(now.Sub(time.UnixMilli(s.StoredAt))), s.Subject)
			status := map[string]string{}
			for _, r := range s.Receipts {
				status[r.Recipient] = r.Status
			}
			for sid, h := range s.Recipients {
				st := status[sid]
				if st == "" {
					st = "pending"
				}
				if h == "" {
					h = sid
				}
				fmt.Fprintf(&w, "  - %s: %s\n", h, st)
			}
		}
	default:
		return "", fmt.Errorf("view %q must be unread, thread, recent or sent", in.View)
	}
	if err := session.SyncMarkers(ctx, a.o.DB, a.o.Runtime, b.sid); err != nil {
		a.o.Log("sync markers", err)
	}
	if q, _ := a.o.DB.Quarantined(ctx, b.sid); len(q) > 0 {
		writeQuarantine(&w, q)
	}
	return w.String(), nil
}

func writeMessage(w *strings.Builder, m localdb.Message, now time.Time) {
	fmt.Fprintf(w, "From %s to %s, %s (%s)\nid %s, thread %s, reply-expected %s, urgent %s\n",
		m.FromHandle, strings.Join(m.To, ", "), time.UnixMilli(m.ReceivedAt).UTC().Format("2006-01-02T15:04:05Z"),
		inject.Age(now.Sub(time.UnixMilli(m.ReceivedAt))), m.MessageID, m.Thread, yesNo(m.ReplyExpected), yesNo(m.Urgent))
	if m.InReplyTo != "" {
		fmt.Fprintf(w, "in reply to %s\n", m.InReplyTo)
	}
	fmt.Fprintf(w, "Subject: %s\n%s\n\n", m.Subject, strings.TrimRight(m.Body, "\n"))
}

func writeQuarantine(w *strings.Builder, q map[string]int) {
	for why, n := range q {
		switch why {
		case QuarantineKeyChanged:
			fmt.Fprintf(w, "Note: %d message(s) were held back because the sender's identity key changed. Ask your human to verify it with them, then run `seasoning trust <handle>`.\n", n)
		case QuarantineUnknownPerson:
			fmt.Fprintf(w, "Note: %d message(s) were held back because their sender isn't in the project's roster.\n", n)
		case QuarantineClock:
			fmt.Fprintf(w, "Note: %d message(s) were held back because their dates are far from when they arrived (a possible replay, or a wrong clock).\n", n)
		}
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// SetStatus renames the session's label (keeping the old one as an alias)
// and/or sets its one-line status, then publishes presence (SPEC §6.2).
func (a *Agent) SetStatus(ctx context.Context, label, status *string) (string, error) {
	b, s, err := a.current(ctx)
	if err != nil {
		return "", err
	}
	if label == nil && status == nil {
		return "", errors.New("pass label, status, or both")
	}
	if label != nil {
		l := strings.ToLower(strings.TrimSpace(*label))
		if !envelope.ValidLabel(l) {
			return "", fmt.Errorf("label %q must be %s", *label, envelope.LabelRule)
		}
		*label = l
	}
	if status != nil {
		st := strings.TrimSpace(*status)
		if st != "" {
			if err := envelope.CheckStatus(st); err != nil {
				return "", err
			}
		}
		*status = st
	}
	note := ""
	if label != nil && *label != s.Label {
		taken, err := session.TakenLabels(ctx, a.o.DB, b.j.Keys.ID, s.Person, b.localID, b.sid)
		if err != nil {
			return "", err
		}
		want := *label
		for i := 2; taken[want] && i < 100; i++ {
			want = fmt.Sprintf("%s-%d", *label, i)
			if len(want) > 40 {
				want = fmt.Sprintf("%s-%d", (*label)[:40-len(fmt.Sprint(i))-1], i)
			}
		}
		if want != *label {
			note = fmt.Sprintf(" (%s was taken by another of your sessions)", *label)
		}
		*label = want
	}
	updated, err := a.o.DB.UpdateSession(ctx, b.localID, func(x *localdb.Session) error {
		if label != nil && *label != x.Label {
			if !slices.Contains(x.Aliases, x.Label) {
				x.Aliases = append(x.Aliases, x.Label)
			}
			x.Aliases = slices.DeleteFunc(x.Aliases, func(a string) bool { return a == *label })
			if len(x.Aliases) > envelope.MaxAliases {
				x.Aliases = x.Aliases[len(x.Aliases)-envelope.MaxAliases:]
			}
			x.Label = *label
		}
		if status != nil {
			x.Status = *status
		}
		x.UpdatedAt = a.o.Now().UnixMilli()
		return nil
	})
	if err != nil {
		return "", err
	}
	b.Poke()
	out := fmt.Sprintf("You are now %s%s.", updated.Handle(), note)
	if len(updated.Aliases) > 0 {
		out += fmt.Sprintf(" Earlier names still reach you: %s.", strings.Join(updated.AliasHandles(), ", "))
	}
	if updated.Status != "" {
		out += fmt.Sprintf(" Status: %q.", updated.Status)
	}
	return out, nil
}

// LinkRepo adds the repository's remotes to the session's project mapping
// and, with writeMarker, writes the committable .seasoning.json (SPEC §5.3).
// Agents call it only on their human's explicit request.
func (a *Agent) LinkRepo(ctx context.Context, writeMarker bool) (string, error) {
	b, _, err := a.current(ctx)
	if err != nil {
		return "", err
	}
	target := b.j.Target
	if m, path, err := project.ReadMarker(target); err != nil {
		return "", err
	} else if m != nil && m.Project != b.j.Keys.ID {
		return "", fmt.Errorf("%s names another project (%s); ask your human to resolve it", path, m.Project)
	}
	var added []string
	err = a.o.Config.Locked(func() error {
		ps, err := a.o.Config.LoadProjects()
		if err != nil {
			return err
		}
		p := ps.Get(b.j.Keys.ID)
		if p == nil {
			return fmt.Errorf("project %s is no longer in projects.json", b.j.Keys.ID)
		}
		if added = project.Link(p, target); len(added) > 0 {
			return ps.Save()
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("%s is linked to project %q.", target.Describe(), b.j.Project.Name)
	if !writeMarker {
		return out, nil
	}
	path, err := project.WriteMarker(target.MarkerDir(), project.Marker{Relay: b.j.Project.Relay, Project: b.j.Keys.ID})
	if err != nil {
		return "", err
	}
	return out + fmt.Sprintf(" Wrote %s (relay URL and project ID, no secrets). It is not committed: commit it with your human's other changes so teammates' sessions find the project. In a public repository it reveals the relay's address.", path), nil
}
