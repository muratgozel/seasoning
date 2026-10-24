// Package invite formats and parses project invites (SPEC §3):
// <relay URL>/join#k=<base64url project secret>. Browsers and HTTP clients
// never send the fragment, so the relay never sees the secret.
package invite

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/muratgozel/seasoning/internal/keys"
)

// Format builds an invite for relay (already normalized) and secret.
func Format(relay string, s keys.Secret) string {
	return relay + "/join#k=" + s.Encode()
}

// Parse splits an invite into its relay URL and project secret.
func Parse(raw string) (string, keys.Secret, error) {
	var zero keys.Secret
	raw = strings.TrimSpace(raw)
	base, frag, ok := strings.Cut(raw, "#")
	if !ok {
		return "", zero, errors.New("invite has no #k=… part; copy the whole invite, including everything after #")
	}
	enc, ok := strings.CutPrefix(frag, "k=")
	if !ok {
		return "", zero, errors.New("invite's part after # must start with k=")
	}
	secret, err := keys.ParseSecret(enc)
	if err != nil {
		return "", zero, fmt.Errorf("invite is damaged (%v); copy it again", err)
	}
	trimmed, ok := strings.CutSuffix(strings.TrimSuffix(base, "/"), "/join")
	if !ok {
		return "", zero, errors.New("invite URL must end in /join before the #")
	}
	relay, err := NormalizeRelay(trimmed)
	if err != nil {
		return "", zero, fmt.Errorf("invite: %w", err)
	}
	return relay, secret, nil
}

// NormalizeRelay validates a relay base URL and returns it without a
// trailing slash. It must be https, or http on a loopback host (local
// testing); it carries no credentials, query or fragment.
func NormalizeRelay(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("relay URL %q is not an absolute URL such as https://relay.example.com", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("relay URL %q must use https (plain http is allowed only for localhost)", raw)
		}
	default:
		return "", fmt.Errorf("relay URL %q must use https", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("relay URL %q must not contain credentials", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("relay URL %q must not have a query or fragment", raw)
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
