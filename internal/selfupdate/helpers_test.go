package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/paths"
	"github.com/muratgozel/seasoning/internal/version"
)

// env points every seasoning location at temp dirs.
type env struct {
	home, state, bin, versions string
}

func newEnv(t *testing.T) env {
	t.Helper()
	root := t.TempDir()
	e := env{
		home:     filepath.Join(root, "home"),
		state:    filepath.Join(root, "home", ".local", "state", "seasoning"),
		bin:      filepath.Join(root, "bin"),
		versions: filepath.Join(root, "versions"),
	}
	if err := os.MkdirAll(e.home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", e.home)
	t.Setenv(paths.EnvStateDir, e.state)
	t.Setenv(paths.EnvBinDir, e.bin)
	t.Setenv(paths.EnvVersionsDir, e.versions)
	t.Setenv(EnvRepo, "")
	t.Setenv(EnvReleasesBase, "")
	t.Setenv(config.EnvAutoUpdate, "")
	// paths.FindLink scans PATH; keep the developer's real install out of it.
	t.Setenv("PATH", filepath.Join(root, "empty-path"))
	// The test binary is never a seasoning version file.
	oldExe := paths.Executable
	t.Cleanup(func() { paths.Executable = oldExe })
	paths.Executable = func() (string, error) { return filepath.Join(root, "not-installed"), nil }
	return e
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	t.Cleanup(func() { version.Version = old })
	version.Version = v
}

func setNow(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	t.Cleanup(func() { now = old })
	now = func() time.Time { return at }
}

type tarEntry struct {
	name     string
	body     string
	typ      byte
	linkname string
}

func makeTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Typeflag: typ, Linkname: e.linkname, ModTime: time.Unix(1700000000, 0)}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeReleases serves GitHub's release page shapes under /o/r.
type fakeReleases struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	latest     string            // "" → redirect to /releases (nothing published)
	latestCode int               // override status for /releases/latest
	files      map[string][]byte // "<tag>/<name>"
	hits       map[string]int
	userAgents []string
}

func newFakeReleases(t *testing.T) *fakeReleases {
	t.Helper()
	f := &fakeReleases{t: t, files: map[string][]byte{}, hits: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	t.Setenv(EnvReleasesBase, f.base())
	return f
}

func (f *fakeReleases) base() string { return f.srv.URL + "/o/r" }

func (f *fakeReleases) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[r.URL.Path]++
	f.userAgents = append(f.userAgents, r.UserAgent())
	p := strings.TrimPrefix(r.URL.Path, "/o/r")
	switch {
	case p == "/releases/latest":
		if f.latestCode != 0 {
			w.WriteHeader(f.latestCode)
			return
		}
		if f.latest == "" {
			http.Redirect(w, r, "/o/r/releases", http.StatusFound)
			return
		}
		http.Redirect(w, r, f.srv.URL+"/o/r/releases/tag/"+f.latest, http.StatusFound)
	case strings.HasPrefix(p, "/releases/tag/"):
		f.t.Errorf("client followed the /releases/latest redirect to %s", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	case strings.HasPrefix(p, "/releases/download/"):
		b, ok := f.files[strings.TrimPrefix(p, "/releases/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeReleases) setLatest(tag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest = tag
}

func (f *fakeReleases) setLatestCode(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestCode = code
}

func (f *fakeReleases) firstUserAgent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.userAgents) == 0 {
		return ""
	}
	return f.userAgents[0]
}

func (f *fakeReleases) remove(tag, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, tag+"/"+name)
}

func (f *fakeReleases) put(tag, name string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[tag+"/"+name] = b
}

func (f *fakeReleases) hitCount(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits["/o/r"+path]
}

// publish adds a well-formed release for this platform whose binary is body.
func (f *fakeReleases) publish(tag, body string) {
	ver := strings.TrimPrefix(tag, "v")
	archive := makeTarGz(f.t,
		tarEntry{name: "README.md", body: "readme"},
		tarEntry{name: "seasoning", body: body},
	)
	f.publishArchive(tag, AssetName(ver, runtime.GOOS, runtime.GOARCH), archive, sha(archive))
}

func (f *fakeReleases) publishArchive(tag, asset string, archive []byte, sum string) {
	f.put(tag, asset, archive)
	other := AssetName(strings.TrimPrefix(tag, "v"), "plan9", "mips")
	f.put(tag, "checksums.txt", []byte(fmt.Sprintf("%s  %s\n%s  %s\n", strings.Repeat("0", 64), other, sum, asset)))
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

// dotFiles lists hidden leftovers (temp files and links) in dir.
func dotFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}
