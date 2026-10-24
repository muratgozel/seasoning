package selfupdate

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

func writeCheck(t *testing.T, e env, checkedAt time.Time, latest string) {
	t.Helper()
	raw, _ := json.Marshal(checkCache{CheckedAt: checkedAt, Latest: latest})
	writeFile(t, filepath.Join(e.state, CheckFile), string(raw), 0o600)
}

func TestHint(t *testing.T) {
	cases := []struct {
		name, current, cache, want string
	}{
		{"newer", "0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, "seasoning 0.3.0 is available (you have 0.2.1): seasoning update\n"},
		{"tag-style current", "v0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, "seasoning 0.3.0 is available (you have 0.2.1): seasoning update\n"},
		{"stale cache still hints", "0.2.1", `{"checked_at":"2020-01-01T00:00:00Z","latest":"0.3.0"}`, "seasoning 0.3.0 is available (you have 0.2.1): seasoning update\n"},
		{"same", "0.3.0", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, ""},
		{"older", "0.4.0", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, ""},
		{"dev", "dev", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, ""},
		{"git describe", "v0.2.1-3-gabc1234", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, ""},
		{"nothing published", "0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":""}`, ""},
		{"malformed latest", "0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"banana"}`, ""},
		{"corrupt cache", "0.2.1", `{not json`, ""},
		{"no cache", "0.2.1", "", ""},

		// Background auto-update.
		{"auto-updated", "0.3.0", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0","installed":"0.3.0","installed_at":"2026-09-29T09:00:01Z"}`, "seasoning updated to 0.3.0\n"},
		{"auto-updated, tag-style current", "v0.3.0", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0","installed":"0.3.0"}`, "seasoning updated to 0.3.0\n"},
		{"auto-update already announced", "0.3.0", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0","installed":"0.3.0","notified":true}`, ""},
		{"process started before the auto-update", "0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0","installed":"0.3.0"}`, ""},
		{"auto-update behind latest", "0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.1","installed":"0.3.0","notified":true}`, "seasoning 0.3.1 is available (you have 0.2.1): seasoning update\n"},
		{"auto-updated version no longer running", "0.3.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.1","installed":"0.3.0"}`, ""},
		{"dev build with a pending notice", "dev", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0","installed":"0.3.0"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			setVersion(t, c.current)
			if c.cache != "" {
				writeFile(t, filepath.Join(e.state, CheckFile), c.cache, 0o600)
			}
			var buf bytes.Buffer
			Hint(&buf)
			if buf.String() != c.want {
				t.Fatalf("Hint = %q, want %q", buf.String(), c.want)
			}
		})
	}
}

// The notice shows exactly once; the other fields stay as they were.
func TestHintAnnouncesAutoUpdateOnce(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.3.0")
	at := t0.Add(-time.Hour)
	saved := checkCache{CheckedAt: t0.Add(-2 * time.Hour), Latest: "0.3.0", Installed: "0.3.0", InstalledAt: at}
	if err := saveCheck(saved); err != nil {
		t.Fatal(err)
	}
	raw := readFile(t, filepath.Join(e.state, CheckFile))
	if strings.Contains(raw, "notified") {
		t.Fatalf("pending notice written as %s", raw)
	}

	var first, second bytes.Buffer
	Hint(&first)
	Hint(&second)
	if first.String() != "seasoning updated to 0.3.0\n" || second.String() != "" {
		t.Fatalf("Hint = %q then %q", first.String(), second.String())
	}
	c, err := readCheck()
	saved.Notified = true
	if err != nil || !c.CheckedAt.Equal(saved.CheckedAt) || !c.InstalledAt.Equal(at) || c.Latest != "0.3.0" || c.Installed != "0.3.0" || !c.Notified {
		t.Fatalf("cache = %+v, %v; want %+v", c, err, saved)
	}
}

// A manual update rewrites the file: no stale auto-update notice follows it.
func TestRecordLatestDropsAutoUpdateFields(t *testing.T) {
	newEnv(t)
	setNow(t, t0)
	if err := saveCheck(checkCache{CheckedAt: t0.Add(-time.Hour), Latest: "0.3.0", Installed: "0.3.0", InstalledAt: t0}); err != nil {
		t.Fatal(err)
	}
	if err := RecordLatest("v0.3.1"); err != nil {
		t.Fatal(err)
	}
	c, err := readCheck()
	if err != nil || !c.CheckedAt.Equal(t0) || c.Latest != "0.3.1" || c.Installed != "" || !c.InstalledAt.IsZero() || c.Notified {
		t.Fatalf("cache = %+v, %v", c, err)
	}
}

func TestRecordLatestRejectsMalformed(t *testing.T) {
	newEnv(t)
	if err := RecordLatest("nightly"); err == nil {
		t.Fatal("accepted a malformed tag")
	}
}
