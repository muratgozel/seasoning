package selfupdate

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.3.0", "0.2.1", 1},
		{"v0.3.0", "0.3.0", 0},
		{"0.2.9", "0.10.0", -1},
		{"1.0.0", "0.99.99", 1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-rc.1", "1.0.0-beta.11", 1},
		{"1.0.0+build.5", "1.0.0", 0},
		{"dev", "0.0.1", -1},
		{"0.0.1", "dev", 1},
		{"dev", "dev", 0},
	}
	for _, c := range cases {
		got, err := CompareVersions(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, %v; want %d", c.a, c.b, got, err, c.want)
		}
	}
}

func TestCompareVersionsRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "1.2", "1.2.3.4", "01.2.3", "1.2.3-", "1.2.3-01", "1.2.3+", "v", "latest", "1.2.x", "1.2.3/../x", "-1.2.3", "vv1.2.3", "1.2.3-a..b"} {
		if _, err := CompareVersions(bad, "1.0.0"); err == nil {
			t.Errorf("CompareVersions(%q, 1.0.0) accepted a malformed version", bad)
		}
		if _, err := CompareVersions("dev", bad); err == nil && bad != "dev" {
			t.Errorf("CompareVersions(dev, %q) accepted a malformed version", bad)
		}
	}
}

func TestIsDevBuild(t *testing.T) {
	for v, want := range map[string]bool{
		"dev":                     true,
		"":                        true,
		"abc1234":                 true, // git describe --always without tags
		"abc1234-dirty":           true,
		"v0.3.0-4-gabc1234":       true,
		"v0.3.0-4-gabc1234-dirty": true,
		"v0.3.0-dirty":            true,
		"0.3.0":                   false,
		"v0.3.0":                  false,
		"0.4.0-rc.1":              false,
	} {
		if got := IsDevBuild(v); got != want {
			t.Errorf("IsDevBuild(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestNormalizeTag(t *testing.T) {
	for in, want := range map[string]string{"0.3.0": "v0.3.0", "v0.3.0": "v0.3.0", " v1.0.0-rc.1 ": "v1.0.0-rc.1"} {
		if got, err := NormalizeTag(in); err != nil || got != want {
			t.Errorf("NormalizeTag(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"latest", "v1", "../v1.0.0", "1.0.0/x"} {
		if _, err := NormalizeTag(bad); err == nil {
			t.Errorf("NormalizeTag(%q) accepted", bad)
		}
	}
}

func TestValidVersionName(t *testing.T) {
	for name, want := range map[string]bool{"dev": true, "0.3.0": true, "1.0.0-rc.1": true, "v0.3.0": false, ".download-1.tar.gz": false, "notes.txt": false, "..": false} {
		if got := validVersionName(name); got != want {
			t.Errorf("validVersionName(%q) = %v, want %v", name, got, want)
		}
	}
}
