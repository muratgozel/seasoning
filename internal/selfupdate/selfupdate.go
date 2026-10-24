// Package selfupdate installs and switches seasoning versions (see SPEC
// "Install layout and updates"). It talks to GitHub's public release pages
// only (no API, so no rate limit): the tag comes from the redirect of
// <base>/releases/latest and assets from <base>/releases/download/<tag>/.
// scripts/install.sh implements the same steps in shell; keep them in sync.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/muratgozel/seasoning/internal/version"
)

// Environment overrides. SEASONING_RELEASES_BASE exists for tests and
// mirrors; it replaces https://github.com/<repo> entirely.
const (
	EnvRepo         = "SEASONING_REPO"
	EnvReleasesBase = "SEASONING_RELEASES_BASE"
	DefaultRepo     = "muratgozel/seasoning"
)

// KeepVersions is how many most recent versions Prune keeps besides the
// active one.
const KeepVersions = 3

// ReleasesBase, when non-empty, replaces the releases base URL (tests). It
// takes precedence over SEASONING_RELEASES_BASE.
var ReleasesBase string

// Limits; vars so tests can shrink them. The archive (a few MB) has no short
// total timeout on purpose: slow links must still finish, so it is cut off
// only when it stalls or hits the generous overall cap.
var (
	tagLookupTimeout          = 10 * time.Second
	checksumsTimeout          = 30 * time.Second
	archiveStallTimeout       = 30 * time.Second
	archiveTimeout            = 10 * time.Minute
	verifyTimeout             = 10 * time.Second
	maxArchiveBytes     int64 = 100 << 20
	maxBinaryBytes      int64 = 100 << 20
	maxChecksumsBytes   int64 = 1 << 20
)

// ErrNoRelease means the repository has no published release yet.
var ErrNoRelease = errors.New("no published seasoning release")

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Repo is the GitHub owner/name releases come from.
func Repo() (string, error) {
	r := os.Getenv(EnvRepo)
	if r == "" {
		return DefaultRepo, nil
	}
	if !repoRe.MatchString(r) || strings.Contains(r, "..") {
		return "", fmt.Errorf("%s=%q is not a GitHub owner/name (e.g. %s)", EnvRepo, r, DefaultRepo)
	}
	return r, nil
}

// InstallerCommand is the one-liner that installs the managed layout.
func InstallerCommand() string {
	repo, err := Repo()
	if err != nil {
		repo = DefaultRepo
	}
	return "curl -fsSL https://raw.githubusercontent.com/" + repo + "/main/scripts/install.sh | bash"
}

func releasesBase() (string, error) {
	b, src := ReleasesBase, "selfupdate.ReleasesBase"
	if b == "" {
		b, src = os.Getenv(EnvReleasesBase), EnvReleasesBase
	}
	if b == "" {
		repo, err := Repo()
		if err != nil {
			return "", err
		}
		return "https://github.com/" + repo, nil
	}
	u, err := url.Parse(b)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s=%q must be an http(s) base URL such as https://github.com/%s", src, b, DefaultRepo)
	}
	return strings.TrimRight(b, "/"), nil
}

func userAgent() string { return "seasoning/" + DisplayVersion(version.Version) }

// LatestTag returns the tag of the latest release (e.g. v0.3.0) from the
// Location header of <base>/releases/latest, without following it.
// ErrNoRelease (wrapped) when nothing is published.
func LatestTag(ctx context.Context) (string, error) {
	base, err := releasesBase()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, tagLookupTimeout)
	defer cancel()
	reqURL := base + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("look up the latest release: %w", err)
	}
	req.Header.Set("User-Agent", userAgent())
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("look up the latest release at %s: %w", reqURL, netHint(err, tagLookupTimeout))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%w: %s not found (wrong %s, or the repository has no releases)", ErrNoRelease, reqURL, EnvRepo)
	case resp.StatusCode < 300 || resp.StatusCode > 399:
		return "", fmt.Errorf("look up the latest release: %s answered HTTP %d instead of a redirect", reqURL, resp.StatusCode)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("look up the latest release: %s redirected without a usable Location header", reqURL)
	}
	// GitHub redirects to .../releases/tag/<tag>, or to .../releases when
	// nothing is published.
	const marker = "/releases/tag/"
	i := strings.LastIndex(loc.Path, marker)
	if i < 0 {
		return "", fmt.Errorf("%w at %s/releases", ErrNoRelease, base)
	}
	tag := loc.Path[i+len(marker):]
	if strings.Contains(tag, "/") {
		return "", fmt.Errorf("latest release tag %q is not a semantic version (vX.Y.Z)", tag)
	}
	if _, err := parseVersion(tag); err != nil {
		return "", fmt.Errorf("latest release tag %q is not a semantic version (vX.Y.Z)", tag)
	}
	return tag, nil
}

// netHint drops the *url.Error wrapper (callers already name the URL) and
// turns a deadline into an actionable message.
func netHint(err error, limit time.Duration) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s; check your network connection and retry", limit)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
