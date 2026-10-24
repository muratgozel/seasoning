package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muratgozel/seasoning/internal/paths"
)

func TestMarkersAndBinding(t *testing.T) {
	t.Setenv(paths.EnvStateDir, t.TempDir())
	d, err := For(4242)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := d.Binding(); b != nil || err != nil {
		t.Fatalf("no binding yet: %v %v", b, err)
	}
	if err := d.WriteBinding(Binding{SessionID: "s1", Cwd: "/repo", Project: "p_x"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := d.Binding(); b == nil || b.SessionID != "s1" || b.Project != "p_x" {
		t.Fatalf("binding: %+v", b)
	}
	if d.Has(Unread) {
		t.Fatal("marker before Set")
	}
	d.Set(Unread)
	if !d.Has(Unread) {
		t.Fatal("marker after Set")
	}
	// A writer that raced the clear keeps its marker.
	d.ClearChecked(Unread, func() (bool, error) { return true, nil })
	if !d.Has(Unread) {
		t.Fatal("ClearChecked dropped a marker that should stay")
	}
	d.ClearChecked(Unread, func() (bool, error) { return false, nil })
	if d.Has(Unread) {
		t.Fatal("ClearChecked kept a stale marker")
	}
	d.WriteWaiter(77)
	if d.Waiter() != 77 {
		t.Fatalf("waiter = %d", d.Waiter())
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(paths.EnvStateDir, t.TempDir())
	if _, err := FromEnv(func(string) string { return "" }); err == nil {
		t.Fatal("missing CLAUDE_PID accepted")
	}
	d, err := FromEnv(func(string) string { return "123" })
	if err != nil || d.PID != 123 {
		t.Fatalf("%+v %v", d, err)
	}
}

func TestSweepRemovesDeadProcesses(t *testing.T) {
	state := t.TempDir()
	t.Setenv(paths.EnvStateDir, state)
	dead, _ := For(999999) // no such process
	live, _ := For(os.Getpid())
	dead.Set(Joined)
	live.Set(Joined)
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(dead.Path, old, old)
	os.Chtimes(live.Path, old, old)
	if err := Sweep(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead.Path); !os.IsNotExist(err) {
		t.Fatal("dead process's runtime dir survived")
	}
	if _, err := os.Stat(filepath.Join(live.Path, Joined)); err != nil {
		t.Fatal("live process's runtime dir was removed")
	}
}
