// Package session resolves and starts seasoning sessions. The SessionStart
// hook and the MCP server (when hooks are missing) share it, so a session is
// described the same way whichever starts it.
package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/gitremote"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/project"
	"github.com/muratgozel/seasoning/internal/runtime"
)

// Joined is a session's project and identity.
type Joined struct {
	Project  config.Project
	Keys     keys.Project
	Person   string
	Identity keys.Identity
	Target   project.Target
	RepoID   string // the normalized remote that matched, if any
	Branch   string
}

// Resolve maps cwd to its project, or returns nil when cwd isn't linked or
// the person has no identity yet.
func Resolve(ctx context.Context, st config.Store, cwd string) (*Joined, error) {
	target, err := project.Inspect(ctx, cwd)
	if err != nil {
		return nil, err
	}
	ps, err := st.LoadProjects()
	if err != nil {
		return nil, err
	}
	hit, err := project.ResolveMarked(target, ps)
	if err != nil || hit == nil {
		return nil, err
	}
	return complete(ctx, st, *hit.Project, target)
}

// ForProject loads a known project for cwd without re-resolving it.
func ForProject(ctx context.Context, st config.Store, projectID, cwd string) (*Joined, error) {
	ps, err := st.LoadProjects()
	if err != nil {
		return nil, err
	}
	p := ps.Get(projectID)
	if p == nil {
		return nil, fmt.Errorf("project %s is no longer in projects.json", projectID)
	}
	target, err := project.Inspect(ctx, cwd)
	if err != nil {
		return nil, err
	}
	return complete(ctx, st, *p, target)
}

func complete(ctx context.Context, st config.Store, p config.Project, target project.Target) (*Joined, error) {
	cfg, err := st.Load()
	if err != nil {
		return nil, err
	}
	id, err := st.LoadIdentity()
	if errors.Is(err, config.ErrNoIdentity) || (err == nil && cfg.Handle == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k, _, err := p.Keys()
	if err != nil {
		return nil, err
	}
	j := &Joined{Project: p, Keys: k, Person: cfg.Handle, Identity: id, Target: target}
	for _, r := range target.Remotes {
		if slices.Contains(p.Repos, r) {
			j.RepoID = r
			break
		}
	}
	if target.InRepo {
		if b, err := gitremote.Branch(ctx, target.Repo.TopLevel); err == nil {
			j.Branch = b
		}
	}
	return j, nil
}

// Root is the directory file paths are made relative to.
func (j *Joined) Root() string {
	if j.Target.InRepo {
		return j.Target.Repo.TopLevel
	}
	return j.Target.Path
}

// Off reports whether seasoning is switched off for this process: by
// SEASONING=off in Claude Code's environment, or by /seasoning:off.
func Off(getenv func(string) string, rt runtime.Dir) bool {
	return getenv("SEASONING") == "off" || rt.Has(runtime.Off)
}

// Start records a starting session: it resolves cwd, writes the runtime
// binding, records the session (keeping label and status on resume) and
// sets the markers. It returns nil when the session isn't joined, or when
// off (an invisible session records its binding but joins nothing).
func Start(ctx context.Context, st config.Store, db *localdb.DB, rt runtime.Dir, localID, cwd, source string, headless, off bool, now time.Time) (*localdb.Session, *Joined, error) {
	if off {
		rt.Clear(runtime.Joined)
		return nil, nil, rt.WriteBinding(runtime.Binding{SessionID: localID, Cwd: cwd, Source: source, At: envelope.FormatTime(now)})
	}
	j, err := Resolve(ctx, st, cwd)
	if err != nil {
		rt.WriteBinding(runtime.Binding{SessionID: localID, Cwd: cwd, Source: source, At: envelope.FormatTime(now)})
		rt.Clear(runtime.Joined)
		return nil, nil, err
	}
	b := runtime.Binding{SessionID: localID, Cwd: cwd, Source: source, At: envelope.FormatTime(now)}
	if j == nil {
		if err := rt.WriteBinding(b); err != nil {
			return nil, nil, err
		}
		return nil, nil, rt.Clear(runtime.Joined)
	}
	label, err := freeLabel(ctx, db, j, localID)
	if err != nil {
		return nil, nil, err
	}
	s, err := db.StartSession(ctx, localdb.Session{
		LocalID: localID, Project: j.Project.ID, ProjectName: j.Project.Name, RelaySession: j.Keys.SessionID(localID),
		ClaudePID: rt.PID, Person: j.Person, Label: label, Repo: j.RepoID, RepoRoot: j.Root(), Branch: j.Branch,
		Worktree: filepath.Base(j.Root()), Headless: headless, StartedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(),
	})
	if err != nil {
		return nil, nil, err
	}
	b.Project, b.ProjectName = j.Project.ID, j.Project.Name
	if err := rt.WriteBinding(b); err != nil {
		return nil, nil, err
	}
	if err := rt.Set(runtime.Joined); err != nil {
		return nil, nil, err
	}
	// A resumed session may have messages that arrived while it was away.
	if err := SyncMarkers(ctx, db, rt, s.RelaySession); err != nil {
		return nil, nil, err
	}
	return s, j, nil
}

// HumanID is the local session ID of a person's human CLI session in a
// project (`seasoning send`, rotation notices). It is stable, so the person
// keeps one handle, <person>/human, across commands.
func HumanID(person string) string { return "human:" + person }

// StartHuman records and binds the person's human CLI session (label
// "human") in rt, for `seasoning send` and rotation notices.
func StartHuman(ctx context.Context, st config.Store, db *localdb.DB, rt runtime.Dir, projectID, cwd string, now time.Time) (*localdb.Session, error) {
	j, err := ForProject(ctx, st, projectID, cwd)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return nil, errors.New("you have no identity yet; run: seasoning setup")
	}
	id := HumanID(j.Person)
	label := "human"
	if prev, err := db.Session(ctx, id); err != nil {
		return nil, err
	} else if prev != nil && prev.Project == projectID {
		label = prev.Label
	} else {
		taken, err := TakenLabels(ctx, db, projectID, j.Person, id, j.Keys.SessionID(id))
		if err != nil {
			return nil, err
		}
		for i := 2; taken[label]; i++ {
			label = fmt.Sprintf("human-%d", i)
		}
	}
	s, err := db.StartSession(ctx, localdb.Session{
		LocalID: id, Project: projectID, ProjectName: j.Project.Name, RelaySession: j.Keys.SessionID(id),
		ClaudePID: rt.PID, Person: j.Person, Label: label, Status: "a person at the terminal (seasoning send)",
		Activity: envelope.ActivityWorking, Repo: j.RepoID, StartedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(),
	})
	if err != nil {
		return nil, err
	}
	if err := rt.WriteBinding(runtime.Binding{SessionID: id, Cwd: cwd, Source: "human", Project: projectID, ProjectName: j.Project.Name, At: envelope.FormatTime(now)}); err != nil {
		return nil, err
	}
	return s, rt.Set(runtime.Joined)
}

// SyncMarkers makes the unread and wake markers match the database.
func SyncMarkers(ctx context.Context, db *localdb.DB, rt runtime.Dir, relaySession string) error {
	unread, wake, err := db.Pending(ctx, relaySession)
	if err != nil {
		return err
	}
	set := func(name string, on bool) error {
		if on {
			return rt.Set(name)
		}
		return rt.ClearChecked(name, func() (bool, error) {
			u, w, err := db.Pending(ctx, relaySession)
			if name == runtime.Wake {
				return w > 0, err
			}
			return u > 0, err
		})
	}
	if err := set(runtime.Unread, unread > 0); err != nil {
		return err
	}
	return set(runtime.Wake, wake > 0)
}

// freeLabel picks a generated label no other session of this person uses,
// locally or in the cached peer list.
func freeLabel(ctx context.Context, db *localdb.DB, j *Joined, localID string) (string, error) {
	if s, err := db.Session(ctx, localID); err != nil {
		return "", err
	} else if s != nil && s.Project == j.Project.ID {
		return s.Label, nil
	}
	used, err := TakenLabels(ctx, db, j.Project.ID, j.Person, localID, j.Keys.SessionID(localID))
	if err != nil {
		return "", err
	}
	for i := 0; i < 20; i++ {
		if l := envelope.GenerateLabel(); !used[l] {
			return l, nil
		}
	}
	return fmt.Sprintf("%s-%d", envelope.GenerateLabel(), time.Now().UnixNano()%1000), nil
}

// TakenLabels returns the labels and aliases of the person's other sessions
// in the project: local ones from the database, remote ones from the cache.
func TakenLabels(ctx context.Context, db *localdb.DB, projectID, person, localID, relaySession string) (map[string]bool, error) {
	used, err := db.LabelsInUse(ctx, projectID, person, localID)
	if err != nil {
		return nil, err
	}
	peers, err := db.Peers(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, p := range peers {
		if p.Session == relaySession || p.Presence == nil || p.Presence.Person != person || p.State == "ended" {
			continue
		}
		used[p.Presence.Label()] = true
		for _, a := range p.Presence.Aliases {
			used[a[len(person)+1:]] = true
		}
	}
	return used, nil
}
