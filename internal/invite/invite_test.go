package invite

import (
	"strings"
	"testing"

	"github.com/muratgozel/seasoning/internal/keys"
)

func TestRoundTrip(t *testing.T) {
	s, _ := keys.NewSecret()
	for _, relay := range []string{"https://relay.example.com", "https://example.com/seasoning", "http://127.0.0.1:8080"} {
		inv := Format(relay, s)
		gotRelay, gotSecret, err := Parse("  " + inv + "\n")
		if err != nil || gotRelay != relay || gotSecret != s {
			t.Fatalf("%s: %q %v", inv, gotRelay, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	s, _ := keys.NewSecret()
	enc := s.Encode()
	for _, inv := range []string{
		"https://relay.example.com/join",
		"https://relay.example.com/join#" + enc,
		"https://relay.example.com/join#k=" + enc[:40],
		"https://relay.example.com/other#k=" + enc,
		"http://relay.example.com/join#k=" + enc,
		"ftp://relay.example.com/join#k=" + enc,
		"https://user:pw@relay.example.com/join#k=" + enc,
	} {
		if _, _, err := Parse(inv); err == nil {
			t.Errorf("%q accepted", inv)
		}
	}
}

func TestNormalizeRelay(t *testing.T) {
	cases := map[string]string{
		"https://Relay.Example.com/":     "https://relay.example.com",
		"https://example.com/seasoning/": "https://example.com/seasoning",
		"http://localhost:8080":          "http://localhost:8080",
		"http://[::1]:9":                 "http://[::1]:9",
	}
	for in, want := range cases {
		got, err := NormalizeRelay(in)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"relay.example.com", "http://10.0.0.5", "https://x.example.com?a=1", ""} {
		if _, err := NormalizeRelay(bad); err == nil {
			t.Errorf("%q accepted", bad)
		} else if !strings.Contains(err.Error(), "relay URL") {
			t.Errorf("%q: error %q doesn't say what's wrong", bad, err)
		}
	}
}
