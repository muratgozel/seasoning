package agent

import (
	"context"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/relayclient"
	"github.com/muratgozel/seasoning/internal/roster"
	"github.com/muratgozel/seasoning/internal/session"
)

// Snapshot lists a project's sessions with verified presence without
// registering a session (`seasoning peers`). Presence from a key that isn't
// pinned (after a roster refresh) is dropped.
func Snapshot(ctx context.Context, st config.Store, j *session.Joined, client *relayclient.Client, now time.Time) ([]localdb.Peer, error) {
	if entries, err := client.Roster(ctx, j.Keys); err == nil {
		members, _ := roster.Read(entries, j.Keys)
		roster.Pin(st, j.Keys.ID, members, now)
	} else {
		return nil, err
	}
	resp, err := client.Sessions(ctx, j.Keys)
	if err != nil {
		return nil, err
	}
	var out []localdb.Peer
	for _, info := range resp.Sessions {
		p := localdb.Peer{Project: j.Keys.ID, Session: info.Session, State: info.State}
		if t, err := envelope.ParseTime(info.LastSeenAt); err == nil {
			p.LastSeenAt = t.UnixMilli()
		}
		if t, err := envelope.ParseTime(info.EndedAt); err == nil {
			p.EndedAt = t.UnixMilli()
		}
		if len(info.Presence) > 0 {
			if env, err := envelope.ParseEnvelope(info.Presence, envelope.MaxPresenceBytes); err == nil && env.FromSession == info.Session {
				if o, err := envelope.Open(j.Keys.EncKey, env); err == nil {
					if pr, err := envelope.DecodePresence(o); err == nil {
						if v, _ := roster.Check(st, j.Keys.ID, pr.Person, pr.Key); v == roster.Trusted {
							p.Presence = pr
						}
					}
				}
			}
		}
		out = append(out, p)
	}
	return out, nil
}
