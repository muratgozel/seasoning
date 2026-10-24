package cli

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/gitremote"
	"github.com/muratgozel/seasoning/internal/project"
)

func init() {
	Register(&Command{
		Name:    "link",
		Summary: "link this repository (or folder) to a project you belong to",
		Usage:   "link [--project ID|NAME] [--remote URL] [--path DIR] [--marker]",
		Run:     runLink,
	})
	Register(&Command{
		Name:    "unlink",
		Summary: "remove this repository's (or folder's) link to its project",
		Usage:   "unlink [--path DIR]",
		Run:     runUnlink,
	})
}

func runLink(env Env) error {
	fs := NewFlagSet("link", env)
	ref := fs.String("project", "", "project ID or name (default: your only project)")
	remote := fs.String("remote", "", "map this remote URL instead of the repository's own remotes")
	path := fs.String("path", "", "directory to link (default: the current directory)")
	marker := fs.Bool("marker", false, "also write a committable .seasoning.json (relay URL and project ID, no secrets)")
	if _, err := parse(fs, env.Args, 0); err != nil {
		return err
	}
	ctx, cancel := netCtx()
	defer cancel()
	target, err := project.Inspect(ctx, dirArg(env, *path))
	if err != nil {
		return err
	}
	existing, markerPath, err := project.ReadMarker(target)
	if err != nil {
		return err
	}
	if *remote != "" {
		norm, err := gitremote.Normalize(*remote)
		if err != nil {
			return fmt.Errorf("--remote: %w", err)
		}
		target.Remotes = []string{norm}
	}
	return config.Locked(func() error {
		ps, err := config.LoadProjects()
		if err != nil {
			return err
		}
		var p *config.Project
		switch {
		case *ref != "":
			if p, err = ps.Find(*ref); err != nil {
				return err
			}
		case len(ps.Projects) == 1:
			p = &ps.Projects[0]
		case len(ps.Projects) == 0:
			return errors.New("you are in no projects yet; create one with `seasoning init` or join one with `seasoning join`")
		default:
			var labels []string
			for i := range ps.Projects {
				labels = append(labels, ps.Projects[i].Label())
			}
			return Usagef("you are in several projects; pass --project with one of: %s", strings.Join(labels, ", "))
		}
		if hit := project.Resolve(target, ps); hit != nil && hit.Project.ID != p.ID {
			return fmt.Errorf("%s is already linked to project %s (by %s); run `seasoning unlink` first", target.Describe(), hit.Project.Label(), hit.Via)
		}
		if existing != nil && existing.Project != p.ID {
			return fmt.Errorf("%s names project %s; remove it first if this repository belongs to %s now", markerPath, existing.Project, p.Label())
		}
		added := project.Link(p, target)
		if len(added) == 0 {
			fmt.Fprintf(env.Stdout, "%s is already linked to %s.\n", target.Describe(), p.Label())
		} else {
			if err := ps.Save(); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "Linked %s to %s.\n", strings.Join(added, ", "), p.Label())
		}
		if *marker {
			written, err := project.WriteMarker(target.MarkerDir(), project.Marker{Relay: p.Relay, Project: p.ID})
			if err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "Wrote %s (relay URL and project ID, no secrets). Commit it so teammates' sessions find the project. In a public repository it reveals your relay's address.\n", written)
		}
		return nil
	})
}

func runUnlink(env Env) error {
	fs := NewFlagSet("unlink", env)
	path := fs.String("path", "", "directory to unlink (default: the current directory)")
	if _, err := parse(fs, env.Args, 0); err != nil {
		return err
	}
	ctx, cancel := netCtx()
	defer cancel()
	target, err := project.Inspect(ctx, dirArg(env, *path))
	if err != nil {
		return err
	}
	return config.Locked(func() error {
		ps, err := config.LoadProjects()
		if err != nil {
			return err
		}
		removed := project.Unlink(ps, target)
		if len(removed) == 0 {
			return fmt.Errorf("nothing to unlink: %s is not linked to any project", target.Describe())
		}
		if err := ps.Save(); err != nil {
			return err
		}
		var ids []string
		for id := range removed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(env.Stdout, "Unlinked %s from %s.\n", strings.Join(removed[id], ", "), ps.Get(id).Label())
		}
		return nil
	})
}
