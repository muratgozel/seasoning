package selfupdate

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLatestTagFromRedirect(t *testing.T) {
	newEnv(t)
	setVersion(t, "0.2.1")
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")

	tag, err := LatestTag(context.Background())
	if err != nil || tag != "v0.3.0" {
		t.Fatalf("LatestTag = %q, %v", tag, err)
	}
	if f.hitCount("/releases/latest") != 1 {
		t.Fatalf("latest hits = %d", f.hitCount("/releases/latest"))
	}
	if ua := f.firstUserAgent(); ua != "seasoning/0.2.1" {
		t.Fatalf("User-Agent = %q", ua)
	}
}

func TestLatestTagRelativeLocation(t *testing.T) {
	newEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/o/r/releases/tag/v1.2.3")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	ReleasesBase = srv.URL + "/o/r/"
	t.Cleanup(func() { ReleasesBase = "" })

	if tag, err := LatestTag(context.Background()); err != nil || tag != "v1.2.3" {
		t.Fatalf("LatestTag = %q, %v", tag, err)
	}
}

func TestLatestTagNoRelease(t *testing.T) {
	newEnv(t)
	f := newFakeReleases(t)
	if _, err := LatestTag(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("redirect to /releases: err = %v, want ErrNoRelease", err)
	}
	f.setLatestCode(http.StatusNotFound)
	if _, err := LatestTag(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("404: err = %v, want ErrNoRelease", err)
	}
}

func TestLatestTagRejectsBadAnswers(t *testing.T) {
	newEnv(t)
	f := newFakeReleases(t)
	for _, tag := range []string{"nightly", "v1.2", "v1.2.3-", "1.2.3%2F..%2Fx"} {
		f.setLatest(tag)
		if got, err := LatestTag(context.Background()); err == nil || !strings.Contains(err.Error(), "not a semantic version") {
			t.Errorf("tag %q: got %q, %v", tag, got, err)
		}
	}
	f.setLatestCode(http.StatusOK)
	if _, err := LatestTag(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 200") {
		t.Fatalf("200: err = %v", err)
	}
}

func TestReleasesBaseValidation(t *testing.T) {
	newEnv(t)
	t.Setenv(EnvReleasesBase, "ftp://example.com/x")
	if _, err := LatestTag(context.Background()); err == nil || !strings.Contains(err.Error(), EnvReleasesBase) {
		t.Fatalf("ftp base: err = %v", err)
	}
	t.Setenv(EnvReleasesBase, "")
	t.Setenv(EnvRepo, "not a repo")
	if _, err := Install(context.Background(), "v1.0.0"); err == nil || !strings.Contains(err.Error(), EnvRepo) {
		t.Fatalf("bad repo: err = %v", err)
	}
	if got := InstallerCommand(); !strings.Contains(got, DefaultRepo) {
		t.Fatalf("InstallerCommand with bad repo = %q", got)
	}
	t.Setenv(EnvRepo, "acme/tool")
	if b, err := releasesBase(); err != nil || b != "https://github.com/acme/tool" {
		t.Fatalf("releasesBase = %q, %v", b, err)
	}
	if got := InstallerCommand(); got != "curl -fsSL https://raw.githubusercontent.com/acme/tool/main/scripts/install.sh | bash" {
		t.Fatalf("InstallerCommand = %q", got)
	}
}

func TestInstall(t *testing.T) {
	e := newEnv(t)
	f := newFakeReleases(t)
	f.publish("v0.3.0", "#!/bin/sh\necho seasoning 0.3.0\n")

	got, err := Install(context.Background(), "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(e.versions, "0.3.0")
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if body := readFile(t, want); body != "#!/bin/sh\necho seasoning 0.3.0\n" {
		t.Fatalf("body = %q", body)
	}
	fi, err := os.Stat(want)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, %v", fi.Mode(), err)
	}
	if left := dotFiles(t, e.versions); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	// Reinstalling replaces the file in place.
	f.publish("v0.3.0", "#!/bin/sh\necho new\n")
	if _, err := Install(context.Background(), "v0.3.0"); err != nil {
		t.Fatal(err)
	}
	if body := readFile(t, want); body != "#!/bin/sh\necho new\n" {
		t.Fatalf("reinstall body = %q", body)
	}
}

func TestInstallFailures(t *testing.T) {
	asset := AssetName("0.3.0", runtime.GOOS, runtime.GOARCH)
	good := func(t *testing.T) []byte {
		return makeTarGz(t, tarEntry{name: "seasoning", body: "bin"})
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, f *fakeReleases)
		want  string
	}{
		{"checksum mismatch", func(t *testing.T, f *fakeReleases) {
			f.publishArchive("v0.3.0", asset, good(t), strings.Repeat("ab", 32))
		}, "checksum mismatch"},
		{"missing asset", func(t *testing.T, f *fakeReleases) {
			f.publishArchive("v0.3.0", asset, good(t), sha(good(t)))
			f.remove("v0.3.0", asset)
		}, "has no build for " + runtime.GOOS + "/" + runtime.GOARCH},
		{"missing checksums", func(t *testing.T, f *fakeReleases) {
			f.put("v0.3.0", asset, good(t))
		}, "no checksums.txt"},
		{"no checksum entry", func(t *testing.T, f *fakeReleases) {
			f.put("v0.3.0", asset, good(t))
			f.put("v0.3.0", "checksums.txt", []byte(sha(good(t))+"  other.tar.gz\n"))
		}, "no entry for " + asset},
		{"traversal entry", func(t *testing.T, f *fakeReleases) {
			a := makeTarGz(t, tarEntry{name: "../evil", body: "x"}, tarEntry{name: "seasoning", body: "bin"})
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "unsafe path"},
		{"absolute entry", func(t *testing.T, f *fakeReleases) {
			a := makeTarGz(t, tarEntry{name: "seasoning", body: "bin"}, tarEntry{name: "/etc/evil", body: "x"})
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "unsafe path"},
		{"escaping link", func(t *testing.T, f *fakeReleases) {
			a := makeTarGz(t, tarEntry{name: "lib", typ: tar.TypeSymlink, linkname: "../../etc"}, tarEntry{name: "seasoning", body: "bin"})
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "links outside"},
		{"binary is a symlink", func(t *testing.T, f *fakeReleases) {
			a := makeTarGz(t, tarEntry{name: "seasoning", typ: tar.TypeSymlink, linkname: "README.md"})
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "not a regular file"},
		{"no binary", func(t *testing.T, f *fakeReleases) {
			a := makeTarGz(t, tarEntry{name: "README.md", body: "x"})
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "no \"seasoning\" binary"},
		{"binary in a subdir", func(t *testing.T, f *fakeReleases) {
			a := makeTarGz(t, tarEntry{name: "seasoning_0.3.0/seasoning", body: "x"})
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "no \"seasoning\" binary"},
		{"not gzip", func(t *testing.T, f *fakeReleases) {
			a := []byte("<html>not an archive</html>")
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "not a gzip archive"},
		{"binary over the cap", func(t *testing.T, f *fakeReleases) {
			old := maxBinaryBytes
			maxBinaryBytes = 8
			t.Cleanup(func() { maxBinaryBytes = old })
			a := makeTarGz(t, tarEntry{name: "seasoning", body: strings.Repeat("x", 9)})
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "over the 8 byte limit"},
		{"unpacked archive over the cap", func(t *testing.T, f *fakeReleases) {
			old := maxBinaryBytes
			maxBinaryBytes = 8
			t.Cleanup(func() { maxBinaryBytes = old })
			a := makeTarGz(t, tarEntry{name: "padding", body: strings.Repeat("0", 2<<20)}, tarEntry{name: "seasoning", body: "bin"})
			f.publishArchive("v0.3.0", asset, a, sha(a))
		}, "archive exceeds the size limit"},
		{"archive over the cap", func(t *testing.T, f *fakeReleases) {
			old := maxArchiveBytes
			maxArchiveBytes = 16
			t.Cleanup(func() { maxArchiveBytes = old })
			f.publishArchive("v0.3.0", asset, good(t), sha(good(t)))
		}, "exceeds the 16 byte limit"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			f := newFakeReleases(t)
			c.setup(t, f)
			_, err := Install(context.Background(), "v0.3.0")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			if _, err := os.Lstat(filepath.Join(e.versions, "0.3.0")); !os.IsNotExist(err) {
				t.Fatalf("a binary was installed despite the failure (%v)", err)
			}
			if left := dotFiles(t, e.versions); len(left) != 0 {
				t.Fatalf("temp files left behind: %v", left)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(e.versions), "evil")); !os.IsNotExist(err) {
				t.Fatal("traversal entry was written")
			}
		})
	}
}

func TestInstallRejectsInvalidTag(t *testing.T) {
	newEnv(t)
	newFakeReleases(t)
	for _, tag := range []string{"latest", "v1.0", "../v1.0.0"} {
		if _, err := Install(context.Background(), tag); err == nil || !strings.Contains(err.Error(), "invalid release tag") {
			t.Errorf("Install(%q) err = %v", tag, err)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("B", 64)
	got := parseChecksums([]byte(a + "  one.tar.gz\n" + b + " *two.tar.gz\nnot-a-sum  three.tar.gz\n\n" + strings.Repeat("c", 64) + "  one.tar.gz\n"))
	if len(got) != 2 || got["one.tar.gz"] != a || got["two.tar.gz"] != strings.ToLower(b) {
		t.Fatalf("parseChecksums = %v", got)
	}
}

// Like install.sh, a build that does not run `version` is never installed,
// and an existing file for that version is left alone.
func TestInstallRequiresBinaryThatRuns(t *testing.T) {
	cases := []struct {
		name, body, want string
		timeout          time.Duration // 0: the default
	}{
		{"exits non-zero", "#!/bin/sh\necho 'dyld: missing symbol' >&2\nexit 3\n", "`version` failed: exit status 3: dyld: missing symbol", 0},
		{"not a program", "just some text\n", "exec format error", 0},
		{"hangs", "#!/bin/sh\nexec /bin/sleep 30\n", "`version` did not finish within 1s", time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.timeout != 0 {
				setDuration(t, &verifyTimeout, c.timeout)
			}
			existing := filepath.Join(e.versions, "0.3.0")
			writeFile(t, existing, "working build", 0o755)
			f := newFakeReleases(t)
			f.publish("v0.3.0", c.body)

			_, err := Install(context.Background(), "v0.3.0")
			if err == nil || !strings.Contains(err.Error(), "seasoning 0.3.0 does not run on this machine") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			if strings.Contains(err.Error(), ".tmp") {
				t.Fatalf("err leaks the temp path: %v", err)
			}
			if body := readFile(t, existing); body != "working build" {
				t.Fatalf("existing version replaced by a broken build: %q", body)
			}
			if left := dotFiles(t, e.versions); len(left) != 0 {
				t.Fatalf("temp files left behind: %v", left)
			}
		})
	}
}

// sender writes body as the response to r.
type sender func(w http.ResponseWriter, r *http.Request, body []byte)

func sendAll(w http.ResponseWriter, _ *http.Request, body []byte) { w.Write(body) }

// trickle sends body in chunk-byte pieces, one every interval.
func trickle(chunk int, every time.Duration) sender {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		rc := http.NewResponseController(w)
		for len(body) > 0 {
			n := min(chunk, len(body))
			if _, err := w.Write(body[:n]); err != nil {
				return
			}
			rc.Flush()
			body = body[n:]
			select {
			case <-r.Context().Done():
				return
			case <-time.After(every):
			}
		}
	}
}

// hangAfter sends the first n bytes (n < 0: not even the headers), then
// blocks until the client goes away or the test ends.
func hangAfter(n int, release <-chan struct{}) sender {
	return func(w http.ResponseWriter, r *http.Request, body []byte) {
		if n >= 0 {
			w.Write(body[:n])
			http.NewResponseController(w).Flush()
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
}

// newPacedRelease publishes a runnable v0.3.0 whose checksums.txt and
// archive are sent by the given senders. It returns a channel closed at
// cleanup (before the server shuts down) to unblock hanging handlers.
func newPacedRelease(t *testing.T, sums, archive func(release <-chan struct{}) sender) {
	t.Helper()
	asset := AssetName("0.3.0", runtime.GOOS, runtime.GOARCH)
	a := makeTarGz(t, tarEntry{name: "seasoning", body: "#!/bin/sh\necho seasoning 0.3.0\n"})
	checksums := []byte(fmt.Sprintf("%s  %s\n", sha(a), asset))
	release := make(chan struct{})
	sendSums, sendArchive := sums(release), archive(release)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/o/r/releases/download/v0.3.0/checksums.txt":
			sendSums(w, r, checksums)
		case "/o/r/releases/download/v0.3.0/" + asset:
			sendArchive(w, r, a)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	t.Setenv(EnvReleasesBase, srv.URL+"/o/r")
}

func setDuration(t *testing.T, p *time.Duration, d time.Duration) {
	t.Helper()
	old := *p
	t.Cleanup(func() { *p = old })
	*p = d
}

func always(s sender) func(<-chan struct{}) sender {
	return func(<-chan struct{}) sender { return s }
}

// Regression: the archive shared checksums.txt's 30 s total timeout, so a
// few MB failed on slow links. A download that keeps moving must finish
// even when it takes longer than the checksums (or stall) timeout.
func TestInstallSlowArchiveFinishes(t *testing.T) {
	e := newEnv(t)
	setDuration(t, &checksumsTimeout, 300*time.Millisecond)
	setDuration(t, &archiveStallTimeout, 500*time.Millisecond)
	setDuration(t, &archiveTimeout, 10*time.Second)
	a := makeTarGz(t, tarEntry{name: "seasoning", body: "#!/bin/sh\necho seasoning 0.3.0\n"})
	chunk := len(a)/20 + 1 // ~20 pieces, 40 ms apart: ~800 ms in total
	newPacedRelease(t, always(sendAll), always(trickle(chunk, 40*time.Millisecond)))

	start := time.Now()
	got, err := Install(context.Background(), "v0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < checksumsTimeout {
		t.Fatalf("the download took only %s; the test no longer outlasts the checksums timeout", took)
	}
	if got != filepath.Join(e.versions, "0.3.0") {
		t.Fatalf("path = %q", got)
	}
}

func TestInstallTimeouts(t *testing.T) {
	cases := []struct {
		name          string
		sums, archive func(<-chan struct{}) sender
		stall, total  time.Duration
		want          string
	}{
		{"archive stalls before headers", always(sendAll), func(r <-chan struct{}) sender { return hangAfter(-1, r) },
			200 * time.Millisecond, 10 * time.Second, "stalled: no data received for 200ms"},
		{"archive stalls mid-body", always(sendAll), func(r <-chan struct{}) sender { return hangAfter(20, r) },
			200 * time.Millisecond, 10 * time.Second, "stalled: no data received for 200ms"},
		{"archive exceeds the overall cap", always(sendAll), always(trickle(1, 20*time.Millisecond)),
			5 * time.Second, 300 * time.Millisecond, "timed out after 300ms"},
		{"checksums.txt hangs", func(r <-chan struct{}) sender { return hangAfter(-1, r) }, always(sendAll),
			5 * time.Second, 10 * time.Second, "checksums.txt: timed out after 200ms"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			setDuration(t, &checksumsTimeout, 200*time.Millisecond)
			setDuration(t, &archiveStallTimeout, c.stall)
			setDuration(t, &archiveTimeout, c.total)
			newPacedRelease(t, c.sums, c.archive)

			start := time.Now()
			_, err := Install(context.Background(), "v0.3.0")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			if took := time.Since(start); took > 4*time.Second {
				t.Fatalf("gave up only after %s", took)
			}
			if _, err := os.Lstat(filepath.Join(e.versions, "0.3.0")); !os.IsNotExist(err) {
				t.Fatalf("installed despite the failure (%v)", err)
			}
			if left := dotFiles(t, e.versions); len(left) != 0 {
				t.Fatalf("temp files left behind: %v", left)
			}
		})
	}
}
