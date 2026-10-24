package selfupdate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/config"
	"github.com/muratgozel/seasoning/internal/paths"
)

var (
	autoOn  = &config.Config{}
	autoOff = func() *config.Config { off := false; return &config.Config{AutoUpdate: &off} }()
)

func runnableScript(ver string) string { return "#!/bin/sh\necho seasoning " + ver + "\n" }

// installManaged makes the seasoning command a managed symlink to version
// ver (a runnable script), as install.sh leaves it, and pretends that binary
// is the one running.
func installManaged(t *testing.T, e env, ver string) string {
	t.Helper()
	p := filepath.Join(e.versions, ver)
	writeFile(t, p, runnableScript(ver), 0o755)
	link := filepath.Join(e.bin, "seasoning")
	if err := Activate(link, ver); err != nil {
		t.Fatal(err)
	}
	// newEnv restores paths.Executable.
	paths.Executable = func() (string, error) { return p, nil }
	return link
}

func linkTarget(t *testing.T, link string) string {
	t.Helper()
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Base(target)
}

func archiveHits(f *fakeReleases, tag string) int {
	return f.hitCount("/releases/download/" + tag + "/" + AssetName(strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH))
}

func mustAutoUpdate(t *testing.T, cfg *config.Config) {
	t.Helper()
	if err := AutoUpdateIfDue(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestAutoUpdateInstallsActivatesPrunesAndRecords(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	setNow(t, t0)
	// Older versions, oldest first; 0.2.1 is active and running. Prune
	// orders by mtime and the new file gets the wall clock's, so these are
	// relative to the wall clock, not the frozen one.
	link := installManaged(t, e, "0.2.1")
	for i, v := range []string{"0.1.0", "0.1.5", "0.2.0", "0.2.1"} {
		p := filepath.Join(e.versions, v)
		if v != "0.2.1" {
			writeFile(t, p, runnableScript(v), 0o755)
		}
		mt := time.Now().Add(time.Duration(i-10) * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")
	f.publish("v0.3.0", runnableScript("0.3.0"))

	mustAutoUpdate(t, autoOn)

	if got := linkTarget(t, link); got != "0.3.0" {
		t.Fatalf("link → %s, want 0.3.0", got)
	}
	if body := readFile(t, link); body != runnableScript("0.3.0") {
		t.Fatalf("through link = %q", body)
	}
	// Keeps the 3 newest (0.3.0, 0.2.1, 0.2.0); 0.2.1 is also the running one.
	entries, _ := os.ReadDir(e.versions)
	var left []string
	for _, en := range entries {
		left = append(left, en.Name())
	}
	sort.Strings(left)
	if got := strings.Join(left, ","); got != "0.2.0,0.2.1,0.3.0" {
		t.Fatalf("versions left = %q", got)
	}
	c, err := readCheck()
	if err != nil || !c.CheckedAt.Equal(t0) || c.Latest != "0.3.0" || c.Installed != "0.3.0" || !c.InstalledAt.Equal(t0) || c.Notified {
		t.Fatalf("cache = %+v, %v", c, err)
	}
	if _, err := os.Lstat(filepath.Join(e.state, LockFile)); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
	if left := dotFiles(t, e.versions); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	// Still-running 0.2.1 processes say nothing; the next command (0.3.0)
	// announces it once.
	var buf bytes.Buffer
	Hint(&buf)
	if buf.Len() != 0 {
		t.Fatalf("old process hinted %q", buf.String())
	}
	setVersion(t, "0.3.0")
	Hint(&buf)
	Hint(&buf)
	if buf.String() != "seasoning updated to 0.3.0\n" {
		t.Fatalf("Hint = %q", buf.String())
	}

	// Not due again for 24 h.
	setNow(t, t0.Add(23*time.Hour))
	mustAutoUpdate(t, autoOn)
	if n := f.hitCount("/releases/latest"); n != 1 {
		t.Fatalf("latest hits = %d", n)
	}
}

func TestAutoUpdateIsDaily(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	setNow(t, t0)
	link := installManaged(t, e, "0.2.1")
	f := newFakeReleases(t)
	f.setLatest("v0.2.1")

	// No cache → network, written with checked_at = now (UTC), 0600.
	mustAutoUpdate(t, autoOn)
	c, err := readCheck()
	if err != nil || c.Latest != "0.2.1" || !c.CheckedAt.Equal(t0) || c.Installed != "" {
		t.Fatalf("cache = %+v, %v", c, err)
	}
	if info, _ := os.Stat(filepath.Join(e.state, CheckFile)); info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %v", info.Mode())
	}

	// Fresh (23 h) → no network, even though a release came out.
	f.setLatest("v0.3.0")
	f.publish("v0.3.0", runnableScript("0.3.0"))
	setNow(t, t0.Add(23*time.Hour))
	mustAutoUpdate(t, autoOn)
	if n := f.hitCount("/releases/latest"); n != 1 {
		t.Fatalf("fresh cache hit the network (%d)", n)
	}
	if got := linkTarget(t, link); got != "0.2.1" {
		t.Fatalf("installed before due: link → %s", got)
	}

	// Stale (> 24 h) → checked and installed.
	setNow(t, t0.Add(24*time.Hour+time.Second))
	mustAutoUpdate(t, autoOn)
	if got := linkTarget(t, link); got != "0.3.0" {
		t.Fatalf("link → %s", got)
	}

	// checked_at in the future (clock moved back) → due again.
	writeCheck(t, e, t0.Add(72*time.Hour), "0.3.0")
	mustAutoUpdate(t, autoOn)
	if n := f.hitCount("/releases/latest"); n != 3 {
		t.Fatalf("future cache not refreshed (%d lookups)", n)
	}
}

// Off, unmanaged or a local build behind the link: the check still runs (it
// feeds the "is available" hint) but nothing is installed.
func TestAutoUpdateChecksOnlyWhenItMustNotInstall(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.Config
		env     string
		prepare func(t *testing.T, e env) string // returns the link to watch
		wantErr string
	}{
		{name: "off in config.json", cfg: autoOff, prepare: managed},
		{name: "env 0", cfg: autoOn, env: "0", prepare: managed},
		{name: "env false", cfg: autoOn, env: "false", prepare: managed},
		{name: "env not a boolean", cfg: autoOn, env: "nope", prepare: managed, wantErr: `SEASONING_AUTO_UPDATE="nope" is not a boolean`},
		{name: "regular file, not a symlink", cfg: autoOn, prepare: func(t *testing.T, e env) string {
			link := filepath.Join(e.bin, "seasoning")
			writeFile(t, link, "copied binary", 0o755)
			return link
		}},
		{name: "no command at all", cfg: autoOn, prepare: func(t *testing.T, e env) string { return "" }},
		{name: "make install dev build is active", cfg: autoOn, prepare: func(t *testing.T, e env) string {
			return installManaged(t, e, "dev")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			setVersion(t, "0.2.1")
			setNow(t, t0)
			t.Setenv(config.EnvAutoUpdate, c.env)
			link := c.prepare(t, e)
			var before string
			if link != "" {
				before, _ = os.Readlink(link)
			}
			f := newFakeReleases(t)
			f.setLatest("v0.3.0")
			f.publish("v0.3.0", runnableScript("0.3.0"))

			err := AutoUpdateIfDue(context.Background(), c.cfg)
			if c.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
			if n := archiveHits(f, "v0.3.0"); n != 0 {
				t.Fatalf("downloaded the release (%d)", n)
			}
			if _, err := os.Stat(filepath.Join(e.versions, "0.3.0")); !os.IsNotExist(err) {
				t.Fatalf("installed 0.3.0: %v", err)
			}
			if link != "" {
				if after, _ := os.Readlink(link); after != before {
					t.Fatalf("link changed: %q → %q", before, after)
				}
			}
			cache, err := readCheck()
			if err != nil || cache.Latest != "0.3.0" || !cache.CheckedAt.Equal(t0) || cache.Installed != "" {
				t.Fatalf("cache = %+v, %v", cache, err)
			}
			var buf bytes.Buffer
			Hint(&buf)
			if want := "seasoning 0.3.0 is available (you have 0.2.1): seasoning update\n"; buf.String() != want {
				t.Fatalf("Hint = %q, want %q", buf.String(), want)
			}
		})
	}
}

func managed(t *testing.T, e env) string { return installManaged(t, e, "0.2.1") }

func TestAutoUpdateSkipsDevBuilds(t *testing.T) {
	for _, v := range []string{"dev", "v0.2.1-3-gabc1234", "abc1234"} {
		t.Run(v, func(t *testing.T) {
			e := newEnv(t)
			setVersion(t, v)
			installManaged(t, e, "0.2.1")
			f := newFakeReleases(t)
			f.setLatest("v0.3.0")
			mustAutoUpdate(t, autoOn)
			if n := f.hitCount("/releases/latest"); n != 0 {
				t.Fatalf("dev build hit the network (%d)", n)
			}
			if _, err := os.Stat(filepath.Join(e.state, CheckFile)); !os.IsNotExist(err) {
				t.Fatalf("dev build wrote the cache: %v", err)
			}
		})
	}
}

func TestAutoUpdateUpToDateOrNewer(t *testing.T) {
	for _, active := range []string{"0.3.0", "0.4.0"} {
		t.Run(active, func(t *testing.T) {
			e := newEnv(t)
			setVersion(t, active)
			setNow(t, t0)
			link := installManaged(t, e, active)
			f := newFakeReleases(t)
			f.setLatest("v0.3.0")
			f.publish("v0.3.0", runnableScript("0.3.0"))
			mustAutoUpdate(t, autoOn)
			if n := archiveHits(f, "v0.3.0"); n != 0 || linkTarget(t, link) != active {
				t.Fatalf("downloads = %d, link → %s", n, linkTarget(t, link))
			}
			if c, err := readCheck(); err != nil || c.Latest != "0.3.0" || c.Installed != "" {
				t.Fatalf("cache = %+v, %v", c, err)
			}
		})
	}
}

// A newer active version than the running one counts: a child of an old
// process must not reinstall what another process already activated.
func TestAutoUpdateComparesWithTheActiveVersion(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	setNow(t, t0)
	writeFile(t, filepath.Join(e.versions, "0.2.1"), runnableScript("0.2.1"), 0o755)
	link := installManaged(t, e, "0.3.0")
	paths.Executable = func() (string, error) { return filepath.Join(e.versions, "0.2.1"), nil }
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")
	f.publish("v0.3.0", runnableScript("0.3.0"))
	mustAutoUpdate(t, autoOn)
	if n := archiveHits(f, "v0.3.0"); n != 0 || linkTarget(t, link) != "0.3.0" {
		t.Fatalf("downloads = %d, link → %s", n, linkTarget(t, link))
	}
}

// A failed install keeps the active version, is returned for errors.log,
// and is stamped so it is retried at the next daily check, not on every
// background run. Until then the manual hint shows.
func TestAutoUpdateFailureIsRetriedNextDay(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	setNow(t, t0)
	link := installManaged(t, e, "0.2.1")
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")
	archive := makeTarGz(t, tarEntry{name: "seasoning", body: runnableScript("0.3.0")})
	asset := AssetName("0.3.0", runtime.GOOS, runtime.GOARCH)
	f.publishArchive("v0.3.0", asset, archive, strings.Repeat("ab", 32)) // wrong checksum

	err := AutoUpdateIfDue(context.Background(), autoOn)
	if err == nil || !strings.Contains(err.Error(), "auto-update to 0.3.0: checksum mismatch") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "@") {
		t.Fatalf("error mentions an address: %v", err)
	}
	if got := linkTarget(t, link); got != "0.2.1" {
		t.Fatalf("link → %s after a failed install", got)
	}
	if _, err := os.Stat(filepath.Join(e.versions, "0.3.0")); !os.IsNotExist(err) {
		t.Fatalf("unverified binary installed: %v", err)
	}
	c, err := readCheck()
	if err != nil || !c.CheckedAt.Equal(t0) || c.Latest != "0.3.0" || c.Installed != "" {
		t.Fatalf("cache = %+v, %v", c, err)
	}
	if _, err := os.Lstat(filepath.Join(e.state, LockFile)); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
	var buf bytes.Buffer
	Hint(&buf)
	if want := "seasoning 0.3.0 is available (you have 0.2.1): seasoning update\n"; buf.String() != want {
		t.Fatalf("Hint = %q, want %q", buf.String(), want)
	}

	// Later background runs the same day do nothing.
	setNow(t, t0.Add(12*time.Hour))
	mustAutoUpdate(t, autoOn)
	if n := archiveHits(f, "v0.3.0"); n != 1 {
		t.Fatalf("retried before the next daily check (%d downloads)", n)
	}

	// The next day the fixed release goes in.
	f.publish("v0.3.0", runnableScript("0.3.0"))
	setNow(t, t0.Add(25*time.Hour))
	mustAutoUpdate(t, autoOn)
	if got := linkTarget(t, link); got != "0.3.0" {
		t.Fatalf("link → %s after the retry", got)
	}
	if c, _ := readCheck(); c.Installed != "0.3.0" || !c.InstalledAt.Equal(t0.Add(25*time.Hour)) {
		t.Fatalf("cache = %+v", c)
	}
}

// A failed lookup leaves the cache as it was, so the next background run
// retries the cheap lookup; "nothing published" is recorded.
func TestAutoUpdateLookupFailures(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	setNow(t, t0)
	installManaged(t, e, "0.2.1")
	f := newFakeReleases(t)

	mustAutoUpdate(t, autoOn)
	if c, err := readCheck(); err != nil || c.Latest != "" || !c.CheckedAt.Equal(t0) {
		t.Fatalf("nothing published: cache = %+v, %v", c, err)
	}

	writeCheck(t, e, t0.Add(-48*time.Hour), "0.2.5")
	f.setLatestCode(500)
	if err := AutoUpdateIfDue(context.Background(), autoOn); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v", err)
	}
	if c, _ := readCheck(); c.Latest != "0.2.5" || !c.CheckedAt.Equal(t0.Add(-48*time.Hour)) {
		t.Fatalf("cache changed on a failed lookup: %+v", c)
	}
}

// A later check keeps the record of the last auto-update (and whether it
// was announced).
func TestAutoUpdateCheckKeepsInstallRecord(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.3.0")
	setNow(t, t0)
	installManaged(t, e, "0.3.0")
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")
	at := t0.Add(-30 * time.Hour)
	if err := saveCheck(checkCache{CheckedAt: at, Latest: "0.3.0", Installed: "0.3.0", InstalledAt: at}); err != nil {
		t.Fatal(err)
	}
	mustAutoUpdate(t, autoOn)
	c, err := readCheck()
	if err != nil || !c.CheckedAt.Equal(t0) || c.Installed != "0.3.0" || !c.InstalledAt.Equal(at) || c.Notified {
		t.Fatalf("cache = %+v, %v", c, err)
	}
}

func TestAutoUpdateLock(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	setNow(t, t0)
	link := installManaged(t, e, "0.2.1")
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")
	f.publish("v0.3.0", runnableScript("0.3.0"))
	lock := filepath.Join(e.state, LockFile)

	// Held by a live process (9 minutes old): skipped without network.
	writeFile(t, lock, "999999\n", 0o600)
	held := t0.Add(-9 * time.Minute)
	if err := os.Chtimes(lock, held, held); err != nil {
		t.Fatal(err)
	}
	mustAutoUpdate(t, autoOn)
	if n := f.hitCount("/releases/latest"); n != 0 {
		t.Fatalf("ran while locked (%d lookups)", n)
	}
	if readFile(t, lock) != "999999\n" {
		t.Fatal("someone else's lock was released")
	}

	// Stale (over 10 minutes): its holder died; taken over.
	stale := t0.Add(-11 * time.Minute)
	if err := os.Chtimes(lock, stale, stale); err != nil {
		t.Fatal(err)
	}
	mustAutoUpdate(t, autoOn)
	if got := linkTarget(t, link); got != "0.3.0" {
		t.Fatalf("link → %s", got)
	}
	if _, err := os.Lstat(lock); !os.IsNotExist(err) {
		t.Fatalf("lock left behind: %v", err)
	}
}

// A holder whose lock was broken as stale must not delete its successor's.
func TestLockReleaseKeepsSuccessorsLock(t *testing.T) {
	e := newEnv(t)
	setNow(t, t0)
	release, ok, err := acquireLock()
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	if _, ok, _ := acquireLock(); ok {
		t.Fatal("lock taken twice")
	}
	lock := filepath.Join(e.state, LockFile)
	writeFile(t, lock, "999999\n", 0o600)
	release()
	if readFile(t, lock) != "999999\n" {
		t.Fatal("released a lock that is no longer ours")
	}
}

// Concurrent background processes download and switch exactly once.
func TestAutoUpdateConcurrentRunsInstallOnce(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	// The wall clock: a lock file carries the real creation time until its
	// holder stamps it, and a racer must read that as held.
	setNow(t, time.Now())
	link := installManaged(t, e, "0.2.1")
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")
	f.publish("v0.3.0", runnableScript("0.3.0"))

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- AutoUpdateIfDue(context.Background(), autoOn)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := archiveHits(f, "v0.3.0"); n != 1 {
		t.Fatalf("archive downloaded %d times", n)
	}
	if n := f.hitCount("/releases/latest"); n != 1 {
		t.Fatalf("latest looked up %d times", n)
	}
	if got := linkTarget(t, link); got != "0.3.0" {
		t.Fatalf("link → %s", got)
	}
}
