package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/paths"
)

func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(paths.EnvConfigDir, filepath.Join(dir, "config"))
	return filepath.Join(dir, "config")
}

func TestConfigRoundTrip(t *testing.T) {
	dir := sandbox(t)
	c, err := Load()
	if err != nil || c.Handle != "" {
		t.Fatalf("empty load: %+v %v", c, err)
	}
	c.Handle = "alice"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	back, err := Load()
	if err != nil || back.Handle != "alice" {
		t.Fatalf("load: %+v %v", back, err)
	}
	os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`{"version":9}`), 0o600)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "newer seasoning") {
		t.Fatalf("future version: %v", err)
	}
	os.WriteFile(filepath.Join(dir, ConfigFile), []byte(`{"version":1,"handle":"Alice"}`), 0o600)
	if _, err := Load(); err == nil {
		t.Fatal("invalid handle accepted")
	}
}

func TestIdentity(t *testing.T) {
	dir := sandbox(t)
	if _, err := LoadIdentity(); err != ErrNoIdentity {
		t.Fatalf("missing identity: %v", err)
	}
	id, _ := keys.GenerateIdentity()
	if err := SaveIdentity(id); err != nil {
		t.Fatal(err)
	}
	back, err := LoadIdentity()
	if err != nil || !back.Private.Equal(id.Private) {
		t.Fatalf("load identity: %v", err)
	}
	pub, _ := os.ReadFile(filepath.Join(dir, PublicFile))
	if strings.TrimSpace(string(pub)) != keys.FormatPublic(id.Public()) {
		t.Fatalf("identity.pub = %q", pub)
	}
	os.Chmod(filepath.Join(dir, IdentityFile), 0o644)
	if _, err := LoadIdentity(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable identity: %v", err)
	}
}

func newProject(t *testing.T, name string) Project {
	t.Helper()
	s, _ := keys.NewSecret()
	return Project{ID: keys.Derive(s).ID, Name: name, Relay: "https://relay.example.com", Secret: s.Encode(), JoinedAt: "2026-10-21T10:00:00.000Z"}
}

func TestProjects(t *testing.T) {
	dir := sandbox(t)
	ps, err := LoadProjects()
	if err != nil || len(ps.Projects) != 0 {
		t.Fatalf("empty: %v", err)
	}
	a, b, c := newProject(t, "acme"), newProject(t, "dup"), newProject(t, "dup")
	ps.Projects = append(ps.Projects, a, b, c)
	if err := ps.Save(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, ProjectsFile))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("projects.json mode %v", info.Mode().Perm())
	}
	ps, err = LoadProjects()
	if err != nil {
		t.Fatal(err)
	}
	if p, err := ps.Find("acme"); err != nil || p.ID != a.ID {
		t.Fatalf("find by name: %v", err)
	}
	if p, err := ps.Find(b.ID); err != nil || p.ID != b.ID {
		t.Fatalf("find by id: %v", err)
	}
	if _, err := ps.Find("dup"); err == nil || !strings.Contains(err.Error(), "use the ID") {
		t.Fatalf("ambiguous name: %v", err)
	}
	if _, err := ps.Find("nope"); err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("unknown name: %v", err)
	}

	// A secret that doesn't derive its ID means the file is corrupt.
	ps.Projects[0].Secret = ps.Projects[1].Secret
	ps.Save()
	if _, err := LoadProjects(); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("corrupt secret: %v", err)
	}
}

func TestPins(t *testing.T) {
	sandbox(t)
	p := newProject(t, "acme")
	pins, err := LoadPins(p.ID)
	if err != nil || len(pins.People) != 0 {
		t.Fatalf("empty pins: %v", err)
	}
	pins.People["bob"] = Pin{Key: "ed25519:x", FirstSeen: "2026-10-21T10:00:00.000Z"}
	if err := SavePins(p.ID, pins); err != nil {
		t.Fatal(err)
	}
	back, err := LoadPins(p.ID)
	if err != nil || back.People["bob"].Key != "ed25519:x" {
		t.Fatalf("pins: %+v %v", back, err)
	}
	if _, err := LoadPins("../escape"); err == nil {
		t.Fatal("path traversal in project ID accepted")
	}
}

func TestLockedSerializes(t *testing.T) {
	sandbox(t)
	(&Projects{}).Save()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := Locked(func() error {
				ps, err := LoadProjects()
				if err != nil {
					return err
				}
				ps.Projects = append(ps.Projects, newProject(t, "p"))
				return ps.Save()
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	ps, err := LoadProjects()
	if err != nil || len(ps.Projects) != 8 {
		t.Fatalf("lost updates: %d projects, %v", len(ps.Projects), err)
	}
}
