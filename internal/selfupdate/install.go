package selfupdate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/muratgozel/seasoning/internal/paths"
)

// binaryName is the archive entry (at the archive root) that gets installed.
const binaryName = "seasoning"

// AssetName is the goreleaser archive name for a version (no leading "v").
func AssetName(ver, goos, goarch string) string {
	return fmt.Sprintf("seasoning_%s_%s_%s.tar.gz", ver, goos, goarch)
}

// Install downloads the release tag for this platform, verifies it against
// the release's checksums.txt (fail closed), checks that the binary runs
// (`version` must succeed) and only then writes it to VersionsDir/<version>
// (0755, temp file + rename), so a build that does not run on this machine
// never replaces an existing version file. It does not activate it.
// Returns the installed path.
func Install(ctx context.Context, tag string) (string, error) {
	if _, err := parseVersion(tag); err != nil {
		return "", fmt.Errorf("invalid release tag: %w", err)
	}
	ver := DisplayVersion(tag)
	base, err := releasesBase()
	if err != nil {
		return "", err
	}
	dir, err := paths.VersionsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create versions dir: %w", err)
	}

	asset := AssetName(ver, runtime.GOOS, runtime.GOARCH)
	relURL := base + "/releases/download/" + url.PathEscape(tag) + "/"

	sums, err := fetchChecksums(ctx, relURL+"checksums.txt", tag)
	if err != nil {
		return "", err
	}
	want, ok := sums[asset]
	if !ok {
		return "", fmt.Errorf("checksums.txt of release %s has no entry for %s; refusing to install an unverified binary", tag, asset)
	}

	archive, err := os.CreateTemp(dir, ".download-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	defer func() {
		archive.Close()
		os.Remove(archive.Name())
	}()
	lim := fetchLimits{total: archiveTimeout, stall: archiveStallTimeout, maxBytes: maxArchiveBytes}
	got, err := downloadTo(ctx, relURL+asset, archive, lim, func(status int) error {
		if status == http.StatusNotFound {
			return fmt.Errorf("release %s has no build for %s/%s (%s not found)", tag, runtime.GOOS, runtime.GOARCH, asset)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("checksum mismatch for %s (expected %s, got %s): the download is corrupt or was tampered with; nothing was installed", asset, want, got)
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind %s: %w", asset, err)
	}

	dest := filepath.Join(dir, ver)
	if err := writeBinary(ctx, archive, dest, asset, ver); err != nil {
		return "", err
	}
	return dest, nil
}

// fetchChecksums downloads and parses a goreleaser checksums.txt.
func fetchChecksums(ctx context.Context, rawURL, tag string) (map[string]string, error) {
	var buf bytes.Buffer
	lim := fetchLimits{total: checksumsTimeout, maxBytes: maxChecksumsBytes}
	_, err := downloadTo(ctx, rawURL, &buf, lim, func(status int) error {
		if status == http.StatusNotFound {
			return fmt.Errorf("release %s not found or it has no checksums.txt (%s); refusing to install an unverified binary", tag, rawURL)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return parseChecksums(buf.Bytes()), nil
}

// parseChecksums reads "<sha256>  <file>" lines (also "<sha256> *<file>").
// Malformed lines are skipped; the first entry for a file wins.
func parseChecksums(data []byte) map[string]string {
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		sum, name := strings.ToLower(f[0]), strings.TrimPrefix(f[1], "*")
		if b, err := hex.DecodeString(sum); err != nil || len(b) != sha256.Size {
			continue
		}
		if _, dup := sums[name]; !dup {
			sums[name] = sum
		}
	}
	return sums
}

var downloadClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect from https to %s", req.URL.Scheme)
	}
	return nil
}}

// fetchLimits bounds one download. total caps the whole request; stall,
// when non-zero, aborts it once no bytes (response headers included) arrived
// for that long, so a slow but moving download is not cut off early.
type fetchLimits struct {
	total, stall time.Duration
	maxBytes     int64
}

var (
	errStalled      = errors.New("download stalled")
	errTotalTimeout = errors.New("download timed out")
)

// downloadTo GETs rawURL into w (at most lim.maxBytes) and returns the
// SHA-256 of what was written. statusErr may map a status to a friendlier
// error; returning nil falls back to a generic one for non-200 answers.
func downloadTo(ctx context.Context, rawURL string, w io.Writer, lim fetchLimits, statusErr func(int) error) (string, error) {
	ctx, cancelTotal := context.WithTimeoutCause(ctx, lim.total, errTotalTimeout)
	defer cancelTotal()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	progress := func() {}
	if lim.stall > 0 {
		// Reset after the timer fired only schedules another (no-op) cancel.
		stall := time.AfterFunc(lim.stall, func() { cancel(errStalled) })
		defer stall.Stop()
		progress = func() { stall.Reset(lim.stall) }
	}
	fail := func(err error) error {
		return fmt.Errorf("download %s: %w", rawURL, fetchErr(ctx, err, lim))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := downloadClient.Do(req)
	if err != nil {
		return "", fail(err)
	}
	defer resp.Body.Close()
	progress()
	if resp.StatusCode != http.StatusOK {
		if e := statusErr(resp.StatusCode); e != nil {
			return "", e
		}
		return "", fmt.Errorf("download %s: HTTP %d; try again later", rawURL, resp.StatusCode)
	}
	if resp.ContentLength > lim.maxBytes {
		return "", fmt.Errorf("download %s: %d bytes exceeds the %d byte limit", rawURL, resp.ContentLength, lim.maxBytes)
	}
	h := sha256.New()
	body := &progressReader{r: io.LimitReader(resp.Body, lim.maxBytes+1), progress: progress}
	n, err := io.Copy(io.MultiWriter(w, h), body)
	if err != nil {
		return "", fail(err)
	}
	if n > lim.maxBytes {
		return "", fmt.Errorf("download %s: response exceeds the %d byte limit", rawURL, lim.maxBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchErr explains why a download failed. The HTTP client reports a
// cancelled context without its cause, so the cause is read from ctx.
func fetchErr(ctx context.Context, err error, lim fetchLimits) error {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errStalled):
		return fmt.Errorf("stalled: no data received for %s; check your network connection and retry", lim.stall)
	case errors.Is(cause, errTotalTimeout):
		return fmt.Errorf("timed out after %s; check your network connection and retry", lim.total)
	}
	return netHint(err, lim.total)
}

type progressReader struct {
	r        io.Reader
	progress func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress()
	}
	return n, err
}

// writeBinary extracts the seasoning entry of a tar.gz into dest atomically,
// after the extracted binary ran `version` successfully.
func writeBinary(ctx context.Context, archive io.Reader, dest, asset, ver string) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := extractBinary(archive, tmp); err != nil {
		return fail(fmt.Errorf("%s: %w", asset, err))
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fail(fmt.Errorf("chmod %s: %w", tmpName, err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("write %s: %w", tmpName, err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// Like install.sh, the new binary must run before anything points at it.
	// It runs from the temp name so a broken build never replaces dest.
	if err := verifyRuns(ctx, tmpName); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("seasoning %s does not run on this machine (%v); nothing was installed and the active version is unchanged", ver, err)
	}
	// rename, not write-in-place: dest may be the running binary ("text file
	// busy" on Linux), and readers must never see a partial file.
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("install %s: %w", dest, err)
	}
	return nil
}

// verifyRuns runs `bin version` (stdin /dev/null, output discarded except a
// little stderr for the error) and fails unless it exits 0 in time.
func verifyRuns(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "version")
	stderr := &headBuffer{max: 512}
	cmd.Stdout, cmd.Stderr = io.Discard, stderr
	// Bounds Wait when a child of the binary keeps the output pipes open.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	switch {
	case err == nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("`version` did not finish within %s", verifyTimeout)
	case ctx.Err() != nil:
		return fmt.Errorf("`version` was interrupted: %w", ctx.Err())
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err // e.g. exec format error; the temp path means nothing to users
	}
	if line := firstLine(stderr.String()); line != "" {
		return fmt.Errorf("`version` failed: %v: %s", err, line)
	}
	return fmt.Errorf("`version` failed: %v", err)
}

// headBuffer keeps the first max bytes written to it and drops the rest.
type headBuffer struct {
	buf []byte
	max int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (b *headBuffer) String() string { return string(b.buf) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

var errTooLarge = errors.New("archive exceeds the size limit")

// cappedReader fails once more than n bytes were read (gzip bomb guard).
type cappedReader struct {
	r io.Reader
	n int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		var one [1]byte
		if k, _ := c.r.Read(one[:]); k > 0 {
			return 0, errTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	k, err := c.r.Read(p)
	c.n -= int64(k)
	return k, err
}

// extractBinary copies the single regular-file entry named "seasoning" at
// the archive root into w. The whole archive is scanned, and any entry with
// an absolute or parent-escaping path (or link target) rejects it: nothing
// else is extracted, but such an archive is not one goreleaser produced.
func extractBinary(r io.Reader, w io.Writer) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	// Headers and the README add a little on top of the binary.
	tr := tar.NewReader(&cappedReader{r: gz, n: 2*maxBinaryBytes + 1<<20})
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, errTooLarge) {
				return err
			}
			return fmt.Errorf("read archive: %w", err)
		}
		name, err := safeEntryPath(hdr.Name)
		if err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			if _, err := safeEntryPath(hdr.Linkname); err != nil {
				return fmt.Errorf("entry %q links outside the archive", hdr.Name)
			}
		}
		if name != binaryName {
			continue
		}
		if found {
			return fmt.Errorf("archive has more than one %q entry", binaryName)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive entry %q is not a regular file", binaryName)
		}
		if hdr.Size > maxBinaryBytes {
			return fmt.Errorf("archive entry %q is %d bytes, over the %d byte limit", binaryName, hdr.Size, maxBinaryBytes)
		}
		n, err := io.Copy(w, io.LimitReader(tr, maxBinaryBytes+1))
		if err != nil {
			if errors.Is(err, errTooLarge) {
				return err
			}
			return fmt.Errorf("extract %s: %w", binaryName, err)
		}
		if n > maxBinaryBytes {
			return fmt.Errorf("archive entry %q exceeds the %d byte limit", binaryName, maxBinaryBytes)
		}
		found = true
	}
	if !found {
		return fmt.Errorf("archive has no %q binary at its root", binaryName)
	}
	return nil
}

// safeEntryPath cleans an archive path and rejects absolute or escaping ones.
func safeEntryPath(p string) (string, error) {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
		return "", fmt.Errorf("archive entry %q has an unsafe path", p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("archive entry %q has an unsafe path", p)
	}
	return c, nil
}

// randSuffix names temporary files and links. crypto/rand.Text never fails
// (Go 1.24+).
func randSuffix() string { return strings.ToLower(rand.Text()[:12]) }
