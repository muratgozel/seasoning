package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/relay"
)

func init() {
	Register(&Command{
		Name:    "serve",
		Summary: "run a relay (needs SEASONING_RELAY_TOKEN)",
		Usage:   "serve [--listen ADDR] [--db PATH] [--trust-proxy] [--max-projects N] [--project-idle-ttl DURATION]",
		Run:     runServe,
	})
}

func runServe(env Env) error {
	fs := NewFlagSet("serve", env)
	listen := fs.String("listen", "127.0.0.1:8090", "address to listen on; put TLS in front with a reverse proxy")
	db := fs.String("db", "", "SQLite database path (default: <data dir>/relay.db)")
	trustProxy := fs.Bool("trust-proxy", false, "take client IPs from X-Forwarded-For (only behind a reverse proxy)")
	maxProjects := fs.Int("max-projects", relay.DefaultLimits().MaxProjects, "maximum number of projects")
	idle := fs.String("project-idle-ttl", "90d", "remove projects idle this long (e.g. 90d, 720h)")
	if _, err := parse(fs, env.Args, 0); err != nil {
		return err
	}
	idleTTL, err := parseDays(*idle)
	if err != nil {
		return Usagef("--project-idle-ttl: %v", err)
	}
	if *maxProjects < 1 {
		return Usagef("--max-projects must be at least 1")
	}
	token := env.Getenv(EnvRelayToken)
	if token == "" {
		return fmt.Errorf("%s is not set; generate a token with `openssl rand -base64 32`, set it in the relay's environment, and give it to people who may create projects", EnvRelayToken)
	}
	dbPath := *db
	if dbPath == "" {
		dir, err := paths.DataDir()
		if err != nil {
			return err
		}
		dbPath = filepath.Join(dir, "relay.db")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(dbPath), err)
	}

	limits := relay.DefaultLimits()
	limits.MaxProjects = *maxProjects
	ttls := relay.DefaultTTLs()
	ttls.ProjectIdle = idleTTL
	srv, err := relay.New(relay.Options{
		DBPath:     dbPath,
		RelayToken: token,
		Limits:     limits,
		TTLs:       ttls,
		TrustProxy: *trustProxy,
		Logger:     slog.New(slog.NewJSONHandler(env.Stderr, nil)),
	})
	if err != nil {
		return err
	}
	defer srv.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return relay.Serve(ctx, *listen, srv)
}

// parseDays accepts Go durations plus a whole-day suffix ("90d").
func parseDays(s string) (time.Duration, error) {
	if d, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 1 {
			return 0, fmt.Errorf("%q is not a number of days", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%q is not a duration such as 90d or 720h", s)
	}
	return d, nil
}
