package envelope

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/muratgozel/seasoning/internal/keys"
)

// Presence limits (SPEC §10.1).
const (
	MaxStatusRunes = 120
	MaxFiles       = 200
	MaxAliases     = 20
	maxPathBytes   = 512
)

// Activities.
const (
	ActivityWorking = "working"
	ActivityIdle    = "idle"
)

// Presence is a session's encrypted self-description (SPEC §10.1). It never
// carries a hostname, OS user, email or absolute path.
type Presence struct {
	Handle          string         `json:"handle"`
	Aliases         []string       `json:"aliases"`
	Person          string         `json:"person"`
	Status          string         `json:"status"`
	Activity        string         `json:"activity"`
	Repo            string         `json:"repo,omitempty"`
	Branch          string         `json:"branch,omitempty"`
	Worktree        string         `json:"worktree,omitempty"`
	Headless        bool           `json:"headless"`
	StartedAt       string         `json:"started_at"`
	UpdatedAt       string         `json:"updated_at"`
	AutonomousTurns int            `json:"autonomous_turns_since_human"`
	Files           []PresenceFile `json:"files"`
	Client          ClientInfo     `json:"client"`
	Key             string         `json:"key"`
}

// PresenceFile is a file the session changed, repo-relative.
type PresenceFile struct {
	Path string `json:"path"`
	At   string `json:"at"`
	Via  string `json:"via"` // "edit" (a file tool) or "git" (found by git status)
}

// ClientInfo names the software behind a session.
type ClientInfo struct {
	Seasoning  string `json:"seasoning"`
	ClaudeCode string `json:"claude_code,omitempty"`
}

// Label returns the label part of the handle.
func (p *Presence) Label() string {
	_, l, _ := strings.Cut(p.Handle, "/")
	return l
}

// Validate checks the presence document.
func (p *Presence) Validate() error {
	h, err := ParseAddress(p.Handle)
	if err != nil || !h.IsSession() {
		return fmt.Errorf("handle %q must be a person/label handle", p.Handle)
	}
	if h.Person != p.Person {
		return fmt.Errorf("handle %q does not belong to person %q", p.Handle, p.Person)
	}
	if len(p.Aliases) > MaxAliases {
		return fmt.Errorf("aliases has %d entries; the limit is %d", len(p.Aliases), MaxAliases)
	}
	for _, a := range p.Aliases {
		ad, err := ParseAddress(a)
		if err != nil || !ad.IsSession() || ad.Person != p.Person {
			return fmt.Errorf("alias %q must be a handle of %s", a, p.Person)
		}
	}
	if p.Status != "" {
		if err := checkLine("status", p.Status, MaxStatusRunes); err != nil {
			return err
		}
	}
	if p.Activity != ActivityWorking && p.Activity != ActivityIdle {
		return fmt.Errorf("activity %q must be working or idle", p.Activity)
	}
	for field, v := range map[string]string{"repo": p.Repo, "branch": p.Branch, "worktree": p.Worktree} {
		if v != "" {
			if err := checkLine(field, v, 256); err != nil {
				return err
			}
		}
	}
	if _, err := ParseTime(p.StartedAt); err != nil {
		return fmt.Errorf("started_at: %w", err)
	}
	if _, err := ParseTime(p.UpdatedAt); err != nil {
		return fmt.Errorf("updated_at: %w", err)
	}
	if p.AutonomousTurns < 0 {
		return fmt.Errorf("autonomous_turns_since_human must not be negative")
	}
	if len(p.Files) > MaxFiles {
		return fmt.Errorf("files has %d entries; the limit is %d", len(p.Files), MaxFiles)
	}
	for _, f := range p.Files {
		if err := CheckRepoPath(f.Path); err != nil {
			return err
		}
		if _, err := ParseTime(f.At); err != nil {
			return fmt.Errorf("files: %w", err)
		}
		if f.Via != "edit" && f.Via != "git" {
			return fmt.Errorf("files: via %q must be edit or git", f.Via)
		}
	}
	if _, err := keys.ParsePublic(p.Key); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	return nil
}

// CheckStatus validates a status line.
func CheckStatus(s string) error { return checkLine("status", s, MaxStatusRunes) }

// CheckRepoPath accepts clean, relative, slash-separated paths inside a repo.
func CheckRepoPath(p string) error {
	if p == "" || len(p) > maxPathBytes || !utf8.ValidString(p) {
		return fmt.Errorf("file path %q must be 1-%d bytes of UTF-8", p, maxPathBytes)
	}
	if strings.HasPrefix(p, "/") || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("file path %q must be a clean path relative to the repository", p)
	}
	for _, r := range p {
		if isControl(r) {
			return fmt.Errorf("file path %q contains a control character", p)
		}
	}
	return nil
}

// DecodePresence parses and validates an opened presence document and checks
// it describes its envelope's session.
func DecodePresence(o *Opened) (*Presence, error) {
	if o.Kind != KindPresence {
		return nil, fmt.Errorf("%w: not a presence document", ErrMalformed)
	}
	var p Presence
	if err := json.Unmarshal(o.Plaintext, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return &p, nil
}

// Word lists for generated labels (SPEC §6.2): short, neutral, easy to type.
var (
	adjectives = strings.Fields(`amber azure bold brave bright brisk calm clear cool coral crisp cozy dapper deft eager early fair fancy fast fresh gentle glad golden grand happy hazel humble ivory jolly keen kind lively lucky mellow merry mint misty modest neat nimble noble olive plucky proud quick quiet rapid ready rosy royal ruby rustic sage sandy shy silver sleek smart snowy solar spry steady sunny swift tidy topaz vivid warm wise witty`)
	nouns      = strings.Fields(`badger bear beaver bison crane crow deer dingo dolphin eagle egret falcon ferret finch fox gecko gull hare hawk heron ibis jay koala lark lemur lion lynx marten mink mole moose newt otter owl panda parrot pelican pika puffin quail rabbit raven robin salmon seal shrew sparrow stork swan tapir tern tiger toad trout turtle viper vole walrus weasel whale wolf wren yak zebra`)
)

// GenerateLabel returns a random "adjective-noun" label.
func GenerateLabel() string {
	pick := func(words []string) string {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
		if err != nil {
			return words[0]
		}
		return words[n.Int64()]
	}
	return pick(adjectives) + "-" + pick(nouns)
}
