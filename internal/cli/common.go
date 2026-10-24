package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/relayclient"
)

// EnvRelayToken holds the relay token: the relay reads it at start, and
// `seasoning init` sends it to create a project.
const EnvRelayToken = "SEASONING_RELAY_TOKEN"

// networkTimeout bounds one CLI command's relay calls.
const networkTimeout = 30 * time.Second

// now is the clock; tests replace it.
var now = time.Now

// newClient builds a relay client; tests replace it.
var newClient = relayclient.New

// me is the local person: handle and identity key.
type me struct {
	handle string
	id     keys.Identity
}

// requireMe loads the handle and identity that `seasoning setup` creates.
func requireMe() (me, error) {
	cfg, err := config.Load()
	if err != nil {
		return me{}, err
	}
	id, err := config.LoadIdentity()
	if errors.Is(err, config.ErrNoIdentity) || (err == nil && cfg.Handle == "") {
		return me{}, errors.New("you have no identity yet; run: seasoning setup")
	}
	if err != nil {
		return me{}, err
	}
	return me{handle: cfg.Handle, id: id}, nil
}

// dirArg resolves a --path flag against the working directory.
func dirArg(env Env, path string) string {
	if path == "" {
		return env.Dir
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(env.Dir, path)
}

// checkName validates a project display name.
func checkName(name string) error {
	n := utf8.RuneCountInString(name)
	if n == 0 || n > 64 || strings.TrimSpace(name) != name {
		return fmt.Errorf("project name %q must be 1-64 characters without leading or trailing spaces", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("project name %q contains a control character", name)
		}
	}
	return nil
}

// cliSession is the relay session ID a person's CLI uses for roster posts.
// It is never registered as a live session.
func cliSession(p keys.Project, handle string) string { return p.SessionID("cli:" + handle) }

// rosterRecord seals the person's roster record for p.
func rosterRecord(p keys.Project, m me) (*envelope.Envelope, error) {
	rec := envelope.RosterRecord{Handle: m.handle, Key: keys.FormatPublic(m.id.Public()), JoinedAt: envelope.FormatTime(now())}
	return envelope.Seal(p.EncKey, m.id, envelope.KindRoster, p.ID, cliSession(p, m.handle), envelope.Recipients{All: true}, rec)
}

// relayError rewrites relay errors that need command-specific advice.
func relayError(err error) error {
	var ue *relayclient.UnreachableError
	if errors.As(err, &ue) {
		return fmt.Errorf("%v; check the URL and your network, then retry", err)
	}
	return err
}

func netCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), networkTimeout)
}

// isTerminal reports whether r is an interactive terminal.
func isTerminal(r any) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
