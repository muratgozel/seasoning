package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/muratgozel/seasoning/internal/agent"
	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/errlog"
	"github.com/muratgozel/seasoning/internal/hooks"
	"github.com/muratgozel/seasoning/internal/localdb"
	"github.com/muratgozel/seasoning/internal/mcpserver"
	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/runtime"
	"github.com/muratgozel/seasoning/internal/selfupdate"
	"github.com/muratgozel/seasoning/internal/version"
)

// autoUpdate is the background update; a var for tests.
var autoUpdate = selfupdate.AutoUpdateIfDue

// PluginProto is the plugin↔binary protocol this binary speaks (SPEC §15.3).
// The plugin passes --plugin-proto N; a newer N means the binary is too old.
const PluginProto = 1

func init() {
	Register(&Command{
		Name:    "mcp",
		Summary: "run the per-session MCP server (started by the Claude Code plugin)",
		Usage:   "mcp [--plugin-proto N]",
		Hidden:  true,
		Run:     runMCP,
	})
	Register(&Command{
		Name:    "hook",
		Summary: "handle a Claude Code hook event (called by the plugin)",
		Usage:   "hook EVENT [--plugin-proto N]   (EVENT: " + strings.Join(hooks.Events, ", ") + ")",
		Hidden:  true,
		Run:     runHook,
	})
}

// takeProto removes --plugin-proto from args and reports whether this
// binary supports it.
func takeProto(args []string) ([]string, bool, error) {
	var rest []string
	ok := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		var v string
		switch {
		case a == "--plugin-proto" && i+1 < len(args):
			v = args[i+1]
			i++
		case strings.HasPrefix(a, "--plugin-proto="):
			v = strings.TrimPrefix(a, "--plugin-proto=")
		default:
			rest = append(rest, a)
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, false, Usagef("--plugin-proto %q is not a number", v)
		}
		ok = n <= PluginProto
	}
	return rest, ok, nil
}

const updateHint = "the seasoning plugin is newer than the seasoning binary; update it with: seasoning update"

func runMCP(env Env) error {
	args, ok, err := takeProto(env.Args)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		return Usagef("unexpected arguments: %s", strings.Join(args, " "))
	}
	if !ok {
		return errors.New(updateHint)
	}
	st, err := config.Default()
	if err != nil {
		return err
	}
	db, err := localdb.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	state, err := paths.StateDir()
	if err != nil {
		return err
	}
	logger := errlog.Default()
	// Claude Code starts MCP servers as its direct children, so the parent
	// PID names the runtime directory its hooks use (as CLAUDE_PID).
	a := agent.New(agent.Options{
		Config: st, DB: db, Runtime: runtime.ForBase(state, os.Getppid()), Getenv: env.Getenv,
		Log: logger.Log, Version: version.Version,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	agentCtx, stopAgent := context.WithCancel(context.Background())
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		if err := a.Run(agentCtx); err != nil {
			logger.Log("mcp agent", err)
		}
	}()
	// Long-lived MCP servers are where the daily update check runs (SPEC
	// §15.2); it installs in the background and never blocks the session.
	go func() {
		cfg, err := config.Load()
		if err == nil {
			err = autoUpdate(context.Background(), cfg)
		}
		if err != nil {
			logger.Log("auto-update", err)
		}
	}()
	err = mcpserver.Run(ctx, mcpserver.New(a, version.Version))
	stopAgent() // ends the session at the relay
	select {
	case <-agentDone:
	case <-time.After(3 * time.Second):
	}
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func runHook(env Env) error {
	args, ok, err := takeProto(env.Args)
	if err != nil {
		return err
	}
	if len(args) != 1 || !slices.Contains(hooks.Events, args[0]) {
		return Usagef("expected one event: %s", strings.Join(hooks.Events, ", "))
	}
	logger := errlog.Default()
	if !ok {
		// Never break the session over a version skew; say it once in the log.
		logger.Log("hook "+args[0], errors.New(updateHint))
		io.Copy(io.Discard, env.Stdin)
		return nil
	}
	st, err := config.Default()
	if err != nil {
		logger.Log("hook "+args[0], err)
		return nil
	}
	state, err := paths.StateDir()
	if err != nil {
		logger.Log("hook "+args[0], err)
		return nil
	}
	dbPath, err := localdb.Path()
	if err != nil {
		logger.Log("hook "+args[0], err)
		return nil
	}
	code := hooks.Run(context.Background(), args[0], env.Stdin, env.Stdout, env.Stderr, hooks.Deps{
		Config: st, StateDir: state, DBPath: dbPath, Getenv: env.Getenv, Log: logger.Log,
	})
	if code != 0 {
		return Exit(code)
	}
	return nil
}
