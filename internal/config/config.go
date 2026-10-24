// Package config reads and writes seasoning's files in paths.ConfigDir()
// (SPEC §15.1):
//
//	config.json     handle and preferences
//	identity        Ed25519 identity key (0600, secret)
//	identity.pub    its public half
//	projects.json   project secrets and repo/path mappings (0600, secret)
//	pins/<id>.json  trust-on-first-use person→key pins per project
//
// Secret files that other users can read are refused, as ssh refuses keys.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/fsutil"
	"github.com/muratgozel/seasoning/internal/invite"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/paths"
)

// FileVersion is the schema version of every file in this package.
const FileVersion = 1

// File names in the config directory.
const (
	ConfigFile   = "config.json"
	IdentityFile = "identity"
	PublicFile   = "identity.pub"
	ProjectsFile = "projects.json"
	PinsDir      = "pins"
	lockFile     = ".lock"
)

// EnvAutoUpdate turns background auto-updates off at runtime when false
// (0, false, f; see strconv.ParseBool). A true value does not override
// "auto_update": false in config.json: the variable can only switch off.
const EnvAutoUpdate = "SEASONING_AUTO_UPDATE"

// ErrNoIdentity means `seasoning setup` has not run yet.
var ErrNoIdentity = errors.New("no identity yet; run: seasoning setup")

// Config is config.json.
type Config struct {
	Version    int    `json:"version"`
	Handle     string `json:"handle,omitempty"`
	AutoUpdate *bool  `json:"auto_update,omitempty"`
}

// Store is a config directory. The package-level functions use the default
// one (paths.ConfigDir); tests and multi-person harnesses use explicit ones.
type Store struct{ Dir string }

// Default returns the store at paths.ConfigDir().
func Default() (Store, error) {
	d, err := paths.ConfigDir()
	return Store{Dir: d}, err
}

// AutoUpdateEnabled reports whether background auto-update may install:
// the auto_update field (absent = on), switched off by a false
// EnvAutoUpdate. A value of EnvAutoUpdate that is not a boolean is an error
// and counts as off: whoever set it meant to configure updates. A nil
// receiver (not set up) consults the env only.
func (c *Config) AutoUpdateEnabled() (bool, error) {
	if v := strings.TrimSpace(os.Getenv(EnvAutoUpdate)); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("%s=%q is not a boolean (use 0 or 1)", EnvAutoUpdate, v)
		}
		if !on {
			return false, nil
		}
	}
	if c == nil || c.AutoUpdate == nil {
		return true, nil
	}
	return *c.AutoUpdate, nil
}

// Dir returns the default config directory.
func Dir() (string, error) { return paths.ConfigDir() }

func (s Store) file(name string) string { return filepath.Join(s.Dir, name) }

// Load reads the default store's config.json.
func Load() (*Config, error) { return withDefault(Store.Load) }

// LoadIdentity reads the default store's identity key.
func LoadIdentity() (keys.Identity, error) { return withDefault(Store.LoadIdentity) }

// SaveIdentity writes the default store's identity key.
func SaveIdentity(id keys.Identity) error {
	st, err := Default()
	if err != nil {
		return err
	}
	return st.SaveIdentity(id)
}

// LoadProjects reads the default store's projects.json.
func LoadProjects() (*Projects, error) { return withDefault(Store.LoadProjects) }

// LoadPins reads a project's pins from the default store.
func LoadPins(projectID string) (*Pins, error) {
	st, err := Default()
	if err != nil {
		return nil, err
	}
	return st.LoadPins(projectID)
}

// SavePins writes a project's pins to the default store.
func SavePins(projectID string, pins *Pins) error {
	st, err := Default()
	if err != nil {
		return err
	}
	return st.SavePins(projectID, pins)
}

// Locked runs fn under the default store's lock.
func Locked(fn func() error) error {
	st, err := Default()
	if err != nil {
		return err
	}
	return st.Locked(fn)
}

func withDefault[T any](f func(Store) (T, error)) (T, error) {
	st, err := Default()
	if err != nil {
		var zero T
		return zero, err
	}
	return f(st)
}

func checkVersion(name string, v int) error {
	switch {
	case v == FileVersion:
		return nil
	case v > FileVersion:
		return fmt.Errorf("%s was written by a newer seasoning (version %d); update seasoning: seasoning update", name, v)
	default:
		return fmt.Errorf("%s has unsupported version %d", name, v)
	}
}

// Load reads config.json; a missing file yields an empty config.
func (s Store) Load() (*Config, error) {
	p := s.file(ConfigFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{Version: FileVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v); fix or remove it", p, err)
	}
	if err := checkVersion(p, c.Version); err != nil {
		return nil, err
	}
	if c.Handle != "" && !envelope.ValidPerson(c.Handle) {
		return nil, fmt.Errorf("%s: handle %q must be %s", p, c.Handle, envelope.PersonRule)
	}
	return &c, nil
}

// Save writes config.json to the default store.
func (c *Config) Save() error {
	st, err := Default()
	if err != nil {
		return err
	}
	return st.SaveConfig(c)
}

// SaveConfig writes config.json.
func (s Store) SaveConfig(c *Config) error {
	c.Version = FileVersion
	return writeJSON(s.file(ConfigFile), c, 0o600)
}

// LoadIdentity reads the identity key, or returns ErrNoIdentity.
func (s Store) LoadIdentity() (keys.Identity, error) {
	p := s.file(IdentityFile)
	b, err := fsutil.ReadSecretFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return keys.Identity{}, ErrNoIdentity
	}
	if err != nil {
		return keys.Identity{}, fmt.Errorf("read identity: %w", err)
	}
	id, err := keys.DecodeIdentity(b)
	if err != nil {
		return keys.Identity{}, fmt.Errorf("%s: %w", p, err)
	}
	return id, nil
}

// SaveIdentity writes the identity key and its public half.
func (s Store) SaveIdentity(id keys.Identity) error {
	if err := fsutil.WriteFileAtomic(s.file(IdentityFile), id.Encode(), 0o600); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.file(PublicFile), []byte(keys.FormatPublic(id.Public())+"\n"), 0o644)
}

// Project is one entry of projects.json.
type Project struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Relay    string   `json:"relay"`
	Secret   string   `json:"secret"`
	Repos    []string `json:"repos,omitempty"` // normalized remotes (gitremote.Normalize)
	Paths    []string `json:"paths,omitempty"` // git common dirs or folders, symlinks resolved
	JoinedAt string   `json:"joined_at"`
}

// Keys parses the project's secret and derives its keys.
func (p *Project) Keys() (keys.Project, keys.Secret, error) {
	s, err := keys.ParseSecret(p.Secret)
	if err != nil {
		return keys.Project{}, s, fmt.Errorf("project %s: %w", p.ID, err)
	}
	k := keys.Derive(s)
	if k.ID != p.ID {
		return keys.Project{}, s, fmt.Errorf("project %s: its secret derives %s; projects.json is corrupt", p.ID, k.ID)
	}
	return k, s, nil
}

// Label is how the CLI names a project: "name (p_…)".
func (p *Project) Label() string { return fmt.Sprintf("%s (%s)", p.Name, p.ID) }

// Projects is projects.json.
type Projects struct {
	Version  int       `json:"version"`
	Projects []Project `json:"projects"`
	store    Store
}

// LoadProjects reads projects.json; a missing file yields no projects.
func (s Store) LoadProjects() (*Projects, error) {
	p := s.file(ProjectsFile)
	b, err := fsutil.ReadSecretFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Projects{Version: FileVersion, store: s}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read projects: %w", err)
	}
	ps := Projects{store: s}
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v); fix it or restore it from a backup", p, err)
	}
	if err := checkVersion(p, ps.Version); err != nil {
		return nil, err
	}
	for i := range ps.Projects {
		pr := &ps.Projects[i]
		if _, _, err := pr.Keys(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if _, err := invite.NormalizeRelay(pr.Relay); err != nil {
			return nil, fmt.Errorf("%s: project %s: %w", p, pr.ID, err)
		}
	}
	return &ps, nil
}

// Save writes projects.json back to the store it came from (the default
// store for a zero Projects).
func (ps *Projects) Save() error {
	st := ps.store
	if st.Dir == "" {
		var err error
		if st, err = Default(); err != nil {
			return err
		}
	}
	p := st.file(ProjectsFile)
	ps.Version = FileVersion
	if ps.Projects == nil {
		ps.Projects = []Project{}
	}
	return writeJSON(p, ps, 0o600)
}

// Get returns the project with ID id, or nil.
func (ps *Projects) Get(id string) *Project {
	for i := range ps.Projects {
		if ps.Projects[i].ID == id {
			return &ps.Projects[i]
		}
	}
	return nil
}

// Find returns the project whose ID or name is ref. A name shared by several
// projects is an error that lists their IDs.
func (ps *Projects) Find(ref string) (*Project, error) {
	if p := ps.Get(ref); p != nil {
		return p, nil
	}
	var hits []*Project
	for i := range ps.Projects {
		if ps.Projects[i].Name == ref {
			hits = append(hits, &ps.Projects[i])
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no project named %q; known projects: %s", ref, ps.list())
	case 1:
		return hits[0], nil
	default:
		var ids []string
		for _, h := range hits {
			ids = append(ids, h.ID)
		}
		return nil, fmt.Errorf("several projects are named %q (%v); use the ID instead", ref, ids)
	}
}

func (ps *Projects) list() string {
	if len(ps.Projects) == 0 {
		return "none (create one with seasoning init, or join one with seasoning join)"
	}
	s := ""
	for i := range ps.Projects {
		if i > 0 {
			s += ", "
		}
		s += ps.Projects[i].Label()
	}
	return s
}

// Pin records the identity key first seen for a person in a project.
type Pin struct {
	Key       string `json:"key"`
	FirstSeen string `json:"first_seen"`
}

// Pins is pins/<project>.json.
type Pins struct {
	Version int            `json:"version"`
	People  map[string]Pin `json:"people"`
}

func (s Store) pinsFile(projectID string) (string, error) {
	if !keys.ValidProjectID(projectID) {
		return "", fmt.Errorf("invalid project ID %q", projectID)
	}
	return filepath.Join(s.Dir, PinsDir, projectID+".json"), nil
}

// LoadPins reads a project's pins; a missing file yields none.
func (s Store) LoadPins(projectID string) (*Pins, error) {
	p, err := s.pinsFile(projectID)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Pins{Version: FileVersion, People: map[string]Pin{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pins: %w", err)
	}
	var pins Pins
	if err := json.Unmarshal(b, &pins); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v); fix or remove it (peers will be re-pinned on first sight)", p, err)
	}
	if err := checkVersion(p, pins.Version); err != nil {
		return nil, err
	}
	if pins.People == nil {
		pins.People = map[string]Pin{}
	}
	return &pins, nil
}

// SavePins writes a project's pins.
func (s Store) SavePins(projectID string, pins *Pins) error {
	p, err := s.pinsFile(projectID)
	if err != nil {
		return err
	}
	pins.Version = FileVersion
	return writeJSON(p, pins, 0o600)
}

// Locked runs fn while holding an exclusive lock on the config directory, so
// concurrent seasoning processes don't lose each other's read-modify-write
// updates of projects.json or pins.
func (s Store) Locked(fn func() error) error {
	p := s.file(lockFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock config: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func writeJSON(path string, v any, perm fs.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, append(b, '\n'), perm)
}
