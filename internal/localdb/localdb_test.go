package localdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func open(t *testing.T, path string) *DB {
	t.Helper()
	db, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func msg(seq int64, wake bool) Message {
	return Message{
		RelaySession: "s_bob", Seq: seq, Project: "p_x", MessageID: fmt.Sprintf("<%026d@seasoning>", seq), Thread: "t",
		FromHandle: "alice/work", FromPerson: "alice", FromSession: "s_alice", To: []string{"bob/x"},
		Subject: "s", Body: "b", Date: "2026-10-21T12:00:00.000Z", Wake: wake, ReceivedAt: ms(t0), StoredAt: ms(t0),
	}
}

func TestClaimIsExactlyOnceAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.db")
	db := open(t, path)
	ctx := context.Background()
	for i := int64(1); i <= 50; i++ {
		if ok, err := db.StoreMessage(ctx, msg(i, false)); err != nil || !ok {
			t.Fatalf("store %d: %v %v", i, ok, err)
		}
	}
	// Ten claimers with their own connections, like ten parallel PostToolUse
	// hook processes.
	var mu sync.Mutex
	got := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := OpenPath(path)
			if err != nil {
				t.Error(err)
				return
			}
			defer h.Close()
			for i := 0; i < 5; i++ {
				ms, err := h.Claim(ctx, "s_bob", false, t0)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				for _, m := range ms {
					got[m.Seq]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 50 {
		t.Fatalf("claimed %d distinct messages, want 50", len(got))
	}
	for seq, n := range got {
		if n != 1 {
			t.Fatalf("message %d claimed %d times", seq, n)
		}
	}
}

func TestStoreDedupesAndClaimWakeOnly(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "local.db"))
	ctx := context.Background()
	db.StoreMessage(ctx, msg(1, false))
	if ok, _ := db.StoreMessage(ctx, msg(1, false)); ok {
		t.Fatal("same stream item stored twice")
	}
	replay := msg(9, false)
	replay.MessageID = msg(1, false).MessageID
	if ok, _ := db.StoreMessage(ctx, replay); ok {
		t.Fatal("a replayed message ID was stored")
	}
	q := msg(2, true)
	q.Quarantine = "key_changed"
	db.StoreMessage(ctx, q)

	if u, w, _ := db.Pending(ctx, "s_bob"); u != 1 || w != 0 {
		t.Fatalf("pending = %d, %d (quarantine must not count)", u, w)
	}
	if ms, _ := db.Claim(ctx, "s_bob", true, t0); len(ms) != 0 {
		t.Fatalf("wake-only claim took non-wake messages: %v", ms)
	}
	db.StoreMessage(ctx, msg(3, true))
	ms, err := db.Claim(ctx, "s_bob", true, t0)
	if err != nil || len(ms) != 2 || ms[0].Seq != 1 || ms[1].Seq != 3 {
		t.Fatalf("wake claim should take everything waiting, in order: %+v %v", ms, err)
	}
	if u, _, _ := db.Pending(ctx, "s_bob"); u != 0 {
		t.Fatalf("pending after claim = %d", u)
	}
	unread, _ := db.Unread(ctx, "s_bob", 10)
	if len(unread) != 2 {
		t.Fatalf("surfaced but unread = %d", len(unread))
	}
	db.MarkRead(ctx, "s_bob", []int64{1, 3}, t0)
	if unread, _ := db.Unread(ctx, "s_bob", 10); len(unread) != 0 {
		t.Fatalf("unread after MarkRead = %v", unread)
	}
	if qs, _ := db.Quarantined(ctx, "s_bob"); qs["key_changed"] != 1 {
		t.Fatalf("quarantined = %v", qs)
	}
}

func TestSessionsResumeKeepsIdentity(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "local.db"))
	ctx := context.Background()
	s := Session{LocalID: "claude-1", Project: "p_x", RelaySession: "s_a", ClaudePID: 100, Person: "alice", Label: "amber-fox", StartedAt: ms(t0), UpdatedAt: ms(t0)}
	if _, err := db.StartSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	db.UpdateSession(ctx, "claude-1", func(s *Session) error {
		s.Aliases = append(s.Aliases, s.Label)
		s.Label, s.Status = "auth-refactor", "working on auth"
		return nil
	})
	db.SetCursor(ctx, "claude-1", 42)
	db.EndSession(ctx, "claude-1", t0)

	s.ClaudePID, s.Label, s.Branch = 200, "ignored-on-resume", "feature"
	got, err := db.StartSession(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "auth-refactor" || got.Status != "working on auth" || got.Cursor != 42 || got.ClaudePID != 200 || got.Branch != "feature" || got.EndedAt != 0 {
		t.Fatalf("resumed session: %+v", got)
	}
	if got.Handle() != "alice/auth-refactor" || got.AliasHandles()[0] != "alice/amber-fox" {
		t.Fatalf("handles: %s %v", got.Handle(), got.AliasHandles())
	}
	used, _ := db.LabelsInUse(ctx, "p_x", "alice", "other")
	if !used["auth-refactor"] || !used["amber-fox"] {
		t.Fatalf("labels in use: %v", used)
	}
	// Joining another project starts the relay stream from scratch.
	s.Project = "p_y"
	if got, _ := db.StartSession(ctx, s); got.Cursor != 0 {
		t.Fatalf("cursor carried across projects: %d", got.Cursor)
	}
}

func TestNoticesWarningsAndPrune(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "local.db"))
	ctx := context.Background()
	if ok, _ := db.NoticeDue(ctx, "c", "relay_down", t0, time.Hour); !ok {
		t.Fatal("first notice not due")
	}
	if ok, _ := db.NoticeDue(ctx, "c", "relay_down", t0.Add(59*time.Minute), time.Hour); ok {
		t.Fatal("notice repeated within the hour")
	}
	if ok, _ := db.NoticeDue(ctx, "c", "relay_down", t0.Add(61*time.Minute), time.Hour); !ok {
		t.Fatal("notice not due after an hour")
	}
	if first, _ := db.FirstWarning(ctx, "c", "a.go", "s_p"); !first {
		t.Fatal("first warning")
	}
	if first, _ := db.FirstWarning(ctx, "c", "a.go", "s_p"); first {
		t.Fatal("repeated warning")
	}
	db.StoreMessage(ctx, msg(1, false))
	db.Prune(ctx, t0.Add(time.Hour))
	if u, _, _ := db.Pending(ctx, "s_bob"); u != 0 {
		t.Fatal("old message not pruned")
	}
}

func TestDatabaseIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.db")
	open(t, path)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("local.db mode: %v %v", info, err)
	}
}

func TestMigrationAddsColumnsToOldDatabases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.db")
	// A v1 database: inbox without from_key.
	old, err := OpenPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`ALTER TABLE inbox DROP COLUMN from_key`); err != nil {
		t.Fatal(err)
	}
	old.db.Exec(`UPDATE meta SET v = '1' WHERE k = 'schema'`)
	old.Close()

	db := open(t, path)
	ctx := context.Background()
	m := msg(1, false)
	m.FromKey, m.Quarantine = "ed25519:abc", "key_changed"
	if _, err := db.StoreMessage(ctx, m); err != nil {
		t.Fatalf("store after migration: %v", err)
	}
	held, err := db.HeldKeys(ctx, "p_x", "alice")
	if err != nil || len(held) != 1 || held[0].Key != "ed25519:abc" || held[0].Messages != 1 {
		t.Fatalf("held keys: %+v %v", held, err)
	}
	released, err := db.Release(ctx, "p_x", "alice", "ed25519:abc")
	if err != nil || len(released) != 1 || released[0] != "s_bob" {
		t.Fatalf("release: %v %v", released, err)
	}
	if u, _, _ := db.Pending(ctx, "s_bob"); u != 1 {
		t.Fatalf("released message not pending: %d", u)
	}
	var v string
	db.db.QueryRow(`SELECT v FROM meta WHERE k = 'schema'`).Scan(&v)
	if v != "2" {
		t.Fatalf("schema version = %s", v)
	}
}
