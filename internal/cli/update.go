package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	goruntime "runtime"
	"syscall"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/selfupdate"
	"github.com/muratgozel/seasoning/internal/version"
)

func init() {
	Register(&Command{
		Name:    "update",
		Summary: "update seasoning to the latest release (or --version vX.Y.Z)",
		Usage:   "update [--version vX.Y.Z]",
		Run:     runUpdate,
	})
}

func runUpdate(env Env) error {
	fs := NewFlagSet("update", env)
	want := fs.String("version", "", "release tag to install instead of the latest")
	if _, err := parse(fs, env.Args, 0); err != nil {
		return err
	}
	var tag string
	if *want != "" {
		t, err := selfupdate.NormalizeTag(*want)
		if err != nil {
			return Usagef("invalid --version: %v", err)
		}
		tag = t
	}
	// Only a managed install (a seasoning symlink → VersionsDir/<version>)
	// can be switched safely; anything else was put there by someone else.
	link, _, err := selfupdate.ActiveLink()
	if err != nil {
		var nm *selfupdate.NotManagedError
		if errors.As(err, &nm) {
			return fmt.Errorf("%v, so `seasoning update` cannot manage this installation.\nInstall with the installer instead (it sets up the updatable layout and replaces this copy):\n  %s", nm, selfupdate.InstallerCommand())
		}
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cur := version.Version
	latest := ""
	if tag == "" {
		t, err := selfupdate.LatestTag(ctx)
		if err != nil {
			return err
		}
		tag, latest = t, t
	}
	target := selfupdate.DisplayVersion(tag)
	// Dev builds (make install) always move to the release.
	if !selfupdate.IsDevBuild(cur) {
		cmp, err := selfupdate.CompareVersions(target, cur)
		if err != nil {
			return err
		}
		if cmp == 0 || (cmp < 0 && *want == "") {
			recordLatest(ctx, env, latest)
			if cmp == 0 {
				fmt.Fprintf(env.Stdout, "seasoning %s is up to date.\n", target)
			} else {
				fmt.Fprintf(env.Stdout, "seasoning %s is up to date (newer than the latest release %s).\n", selfupdate.DisplayVersion(cur), target)
			}
			return nil
		}
	}
	fmt.Fprintf(env.Stderr, "Downloading seasoning %s (%s/%s)...\n", target, goruntime.GOOS, goruntime.GOARCH)
	// Install fails (and nothing is activated) unless the new binary runs
	// `version` successfully, like install.sh.
	if _, err := selfupdate.Install(ctx, tag); err != nil {
		return err
	}
	if err := selfupdate.Activate(link, target); err != nil {
		return err
	}
	if _, err := selfupdate.Prune(selfupdate.KeepVersions, link); err != nil {
		fmt.Fprintf(env.Stderr, "seasoning: warning: could not remove old versions: %v\n", err)
	}
	latest = recordLatest(ctx, env, latest)
	fmt.Fprintf(env.Stdout, "Updated %s → %s.\n", selfupdate.DisplayVersion(cur), target)
	fmt.Fprintln(env.Stdout, "Running Claude Code sessions keep the old version until they restart; that is fine.")
	noteAutoUpdate(env, latest, target)
	return nil
}

// recordLatest refreshes update-check.json so no stale hint follows the
// update, and returns the latest tag ("" when unknown).
func recordLatest(ctx context.Context, env Env, latest string) string {
	if latest == "" {
		t, err := selfupdate.LatestTag(ctx)
		if err != nil {
			fmt.Fprintf(env.Stderr, "seasoning: warning: could not refresh the update check: %v\n", err)
			return ""
		}
		latest = t
	}
	if err := selfupdate.RecordLatest(latest); err != nil {
		fmt.Fprintf(env.Stderr, "seasoning: warning: could not refresh the update check: %v\n", err)
	}
	return latest
}

// noteAutoUpdate warns after `--version` installed something older than the
// latest release: the daily background auto-update would move back to it.
func noteAutoUpdate(env Env, latest, installed string) {
	if latest == "" {
		return
	}
	if cmp, err := selfupdate.CompareVersions(latest, installed); err != nil || cmp <= 0 {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		return
	}
	if on, _ := cfg.AutoUpdateEnabled(); !on {
		return
	}
	dir, _ := config.Dir()
	fmt.Fprintf(env.Stderr, "seasoning: note: auto-update will move to %s again within a day; to stay on %s, set \"auto_update\": false in %s/%s (or export %s=0)\n",
		selfupdate.DisplayVersion(latest), installed, dir, config.ConfigFile, config.EnvAutoUpdate)
}
