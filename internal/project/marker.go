package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/fsutil"
	"github.com/muratgozel/seasoning/internal/invite"
	"github.com/muratgozel/seasoning/internal/keys"
)

// MarkerFile is the opt-in, committable repo marker (SPEC §5.3). It names a
// relay and a project, never a secret.
const MarkerFile = ".seasoning.json"

// Marker is the repo marker's content.
type Marker struct {
	Version int    `json:"version"`
	Relay   string `json:"relay"`
	Project string `json:"project"`
}

// MarkerDir is where t's marker lives: the repository's top level, or for a
// folder outside git, the folder itself.
func (t Target) MarkerDir() string {
	if t.InRepo {
		return t.Repo.TopLevel
	}
	return t.Path
}

// ReadMarker finds and validates t's marker. Outside git it walks up from
// the folder. It returns nil when there is none.
func ReadMarker(t Target) (*Marker, string, error) {
	dirs := []string{t.MarkerDir()}
	if !t.InRepo {
		for d := filepath.Dir(t.Path); d != filepath.Dir(d); d = filepath.Dir(d) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		p := filepath.Join(d, MarkerFile)
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, p, err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var m Marker
		if err := dec.Decode(&m); err != nil {
			return nil, p, fmt.Errorf("%s is not a valid seasoning marker (%v); fix or remove it", p, err)
		}
		if m.Version != 1 {
			return nil, p, fmt.Errorf("%s has version %d; this seasoning understands version 1 (update seasoning)", p, m.Version)
		}
		if !keys.ValidProjectID(m.Project) {
			return nil, p, fmt.Errorf("%s: %q is not a project ID", p, m.Project)
		}
		if _, err := invite.NormalizeRelay(m.Relay); err != nil {
			return nil, p, fmt.Errorf("%s: %w", p, err)
		}
		return &m, p, nil
	}
	return nil, "", nil
}

// WriteMarker writes a marker into dir (world-readable: it's meant to be
// committed) and returns its path.
func WriteMarker(dir string, m Marker) (string, error) {
	m.Version = 1
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, MarkerFile)
	return p, fsutil.WriteFileAtomic(p, append(b, '\n'), 0o644)
}

// MarkerNoKeyError means a repo marker names a project the person holds no
// key for: they need an invite.
type MarkerNoKeyError struct {
	Marker Marker
	Path   string
}

func (e *MarkerNoKeyError) Error() string {
	return fmt.Sprintf("this repository is linked to project %s on %s (by %s), but you hold no key for it; ask a member for an invite, then run: seasoning join '<invite>'", e.Marker.Project, e.Marker.Relay, filepath.Base(e.Path))
}

// ResolveMarked is Resolve with the repo marker taking precedence (SPEC
// D17). A marker naming an unknown project yields *MarkerNoKeyError.
func ResolveMarked(t Target, ps *config.Projects) (*Match, error) {
	m, path, err := ReadMarker(t)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return Resolve(t, ps), nil
	}
	p := ps.Get(m.Project)
	if p == nil {
		return nil, &MarkerNoKeyError{Marker: *m, Path: path}
	}
	return &Match{Project: p, Via: "marker " + path}, nil
}
