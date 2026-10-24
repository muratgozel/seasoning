// Package localdb is the machine-local store (SPEC §15.1, local.db) shared by
// every seasoning MCP process and hook on the machine: sessions, the inbox,
// sent messages and receipts, the peers cache, and file activity.
//
// Several processes write concurrently (one MCP server per session plus
// short-lived hooks), so every write is a single SQLite transaction and
// claims use UPDATE … RETURNING, which SQLite serializes: two hooks can never
// surface the same message.
//
// The schema only grows (SPEC §15.3): a hook from a newer binary and an MCP
// server from an older one may share this database after an auto-update.
package localdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/paths"
)

// SchemaVersion is recorded in meta. Newer schemas are additive, so an older
// binary keeps working against them.
const SchemaVersion = 2

// migrations add what later schema versions introduced, for databases
// created by an older binary. Each must be idempotent: a newer and an older
// binary may both open the same database.
var migrations = []struct {
	table, column, ddl string
}{
	{"inbox", "from_key", `ALTER TABLE inbox ADD COLUMN from_key TEXT NOT NULL DEFAULT ''`}, // v2
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS sessions (
  local_id TEXT PRIMARY KEY,
  project TEXT NOT NULL,
  project_name TEXT NOT NULL DEFAULT '',
  relay_session TEXT NOT NULL,
  claude_pid INTEGER NOT NULL,
  person TEXT NOT NULL,
  label TEXT NOT NULL,
  aliases TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT '',
  activity TEXT NOT NULL DEFAULT 'idle',
  repo TEXT NOT NULL DEFAULT '',
  repo_root TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL DEFAULT '',
  worktree TEXT NOT NULL DEFAULT '',
  headless INTEGER NOT NULL DEFAULT 0,
  autonomous_turns INTEGER NOT NULL DEFAULT 0,
  started_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  cursor INTEGER NOT NULL DEFAULT 0,
  ended_at INTEGER
);
CREATE INDEX IF NOT EXISTS sessions_by_relay ON sessions (relay_session);
CREATE TABLE IF NOT EXISTS inbox (
  relay_session TEXT NOT NULL,
  seq INTEGER NOT NULL,
  project TEXT NOT NULL,
  message_id TEXT NOT NULL,
  thread TEXT NOT NULL,
  refs TEXT NOT NULL DEFAULT '[]',
  from_handle TEXT NOT NULL,
  from_person TEXT NOT NULL,
  from_session TEXT NOT NULL,
  to_json TEXT NOT NULL,
  subject TEXT NOT NULL,
  body TEXT NOT NULL,
  date TEXT NOT NULL,
  in_reply_to TEXT,
  reply_expected INTEGER NOT NULL,
  urgent INTEGER NOT NULL,
  wake INTEGER NOT NULL,
  received_at INTEGER NOT NULL,
  stored_at INTEGER NOT NULL,
  surfaced_at INTEGER,
  read_at INTEGER,
  replied INTEGER NOT NULL DEFAULT 0,
  quarantine TEXT NOT NULL DEFAULT '',
  from_key TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (relay_session, seq)
);
CREATE INDEX IF NOT EXISTS inbox_unsurfaced ON inbox (relay_session) WHERE surfaced_at IS NULL;
CREATE INDEX IF NOT EXISTS inbox_by_message ON inbox (message_id);
CREATE TABLE IF NOT EXISTS sent (
  relay_session TEXT NOT NULL,
  seq INTEGER NOT NULL,
  project TEXT NOT NULL,
  message_id TEXT NOT NULL,
  thread TEXT NOT NULL,
  refs TEXT NOT NULL DEFAULT '[]',
  to_json TEXT NOT NULL,
  subject TEXT NOT NULL,
  body TEXT NOT NULL,
  date TEXT NOT NULL,
  in_reply_to TEXT,
  reply_expected INTEGER NOT NULL,
  urgent INTEGER NOT NULL,
  recipients TEXT NOT NULL,
  stored_at INTEGER NOT NULL,
  PRIMARY KEY (relay_session, seq)
);
CREATE INDEX IF NOT EXISTS sent_by_message ON sent (message_id);
CREATE TABLE IF NOT EXISTS receipts (
  relay_session TEXT NOT NULL,
  ref_seq INTEGER NOT NULL,
  recipient TEXT NOT NULL,
  status TEXT NOT NULL,
  at INTEGER NOT NULL,
  PRIMARY KEY (relay_session, ref_seq, recipient)
);
CREATE TABLE IF NOT EXISTS peers (
  project TEXT NOT NULL,
  session TEXT NOT NULL,
  state TEXT NOT NULL,
  presence TEXT,
  last_seen_at INTEGER NOT NULL,
  ended_at INTEGER,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (project, session)
);
CREATE TABLE IF NOT EXISTS files (
  local_id TEXT NOT NULL,
  path TEXT NOT NULL,
  at INTEGER NOT NULL,
  via TEXT NOT NULL,
  PRIMARY KEY (local_id, path)
);
CREATE TABLE IF NOT EXISTS seen (message_id TEXT PRIMARY KEY, at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS warned (local_id TEXT NOT NULL, path TEXT NOT NULL, peer TEXT NOT NULL, PRIMARY KEY (local_id, path, peer));
CREATE TABLE IF NOT EXISTS notices (local_id TEXT NOT NULL, kind TEXT NOT NULL, at INTEGER NOT NULL, PRIMARY KEY (local_id, kind));
CREATE TABLE IF NOT EXISTS counters (project TEXT NOT NULL, kind TEXT NOT NULL, n INTEGER NOT NULL, last_at INTEGER NOT NULL, PRIMARY KEY (project, kind));
`

// DB is the local store.
type DB struct{ db *sql.DB }

// Path returns local.db's location.
func Path() (string, error) {
	d, err := paths.DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "local.db"), nil
}

// Open opens (creating if needed) local.db.
func Open() (*DB, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	return OpenPath(p)
}

// OpenPath opens the store at path.
func OpenPath(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	// It holds decrypted messages: create it private. SQLite gives its -wal
	// and -shm files the database file's mode.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close()
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &DB{db: db}, nil
}

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		has, err := hasColumn(db, m.table, m.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec(m.ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err // a concurrent opener may have just added it
		}
	}
	var v string
	err := db.QueryRow(`SELECT v FROM meta WHERE k = 'schema'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = db.Exec(`INSERT OR IGNORE INTO meta (k, v) VALUES ('schema', ?)`, strconv.Itoa(SchemaVersion))
	case err == nil:
		if n, _ := strconv.Atoi(v); n < SchemaVersion {
			_, err = db.Exec(`UPDATE meta SET v = ? WHERE k = 'schema'`, strconv.Itoa(SchemaVersion))
		}
		// A newer schema is additive by contract; an older binary keeps going.
	}
	return err
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close closes the store.
func (d *DB) Close() error { return d.db.Close() }

func ms(t time.Time) int64 { return t.UnixMilli() }

// --- sessions ------------------------------------------------------------

// Session is a local Claude Code session joined to a project.
type Session struct {
	LocalID         string
	Project         string
	ProjectName     string
	RelaySession    string
	ClaudePID       int
	Person          string
	Label           string
	Aliases         []string // previous labels, as labels
	Status          string
	Activity        string
	Repo            string
	RepoRoot        string
	Branch          string
	Worktree        string
	Headless        bool
	AutonomousTurns int
	StartedAt       int64
	UpdatedAt       int64
	Cursor          int64
	EndedAt         int64 // 0 while running
}

// Handle is person/label.
func (s *Session) Handle() string { return s.Person + "/" + s.Label }

// AliasHandles returns the previous labels as handles.
func (s *Session) AliasHandles() []string {
	out := make([]string, 0, len(s.Aliases))
	for _, a := range s.Aliases {
		out = append(out, s.Person+"/"+a)
	}
	return out
}

const sessionCols = `local_id, project, project_name, relay_session, claude_pid, person, label, aliases, status, activity,
	repo, repo_root, branch, worktree, headless, autonomous_turns, started_at, updated_at, cursor, ended_at`

func scanSession(row interface{ Scan(...any) error }) (*Session, error) {
	var s Session
	var aliases string
	var headless int
	var ended sql.NullInt64
	err := row.Scan(&s.LocalID, &s.Project, &s.ProjectName, &s.RelaySession, &s.ClaudePID, &s.Person, &s.Label, &aliases,
		&s.Status, &s.Activity, &s.Repo, &s.RepoRoot, &s.Branch, &s.Worktree, &headless, &s.AutonomousTurns,
		&s.StartedAt, &s.UpdatedAt, &s.Cursor, &ended)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(aliases), &s.Aliases)
	s.Headless = headless != 0
	s.EndedAt = ended.Int64
	return &s, nil
}

// StartSession records a starting (or resuming) session. A resumed session
// keeps its label, aliases and status, and the relay cursor; everything that
// describes the current process and checkout is refreshed.
func (d *DB) StartSession(ctx context.Context, s Session) (*Session, error) {
	aliases, _ := json.Marshal(nonNil(s.Aliases))
	_, err := d.db.ExecContext(ctx, `INSERT INTO sessions (`+sessionCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL)
		ON CONFLICT (local_id) DO UPDATE SET project = excluded.project, project_name = excluded.project_name,
		  relay_session = excluded.relay_session, claude_pid = excluded.claude_pid, person = excluded.person,
		  repo = excluded.repo, repo_root = excluded.repo_root, branch = excluded.branch, worktree = excluded.worktree,
		  headless = excluded.headless, updated_at = excluded.updated_at, ended_at = NULL,
		  cursor = CASE WHEN sessions.project = excluded.project THEN sessions.cursor ELSE 0 END`,
		s.LocalID, s.Project, s.ProjectName, s.RelaySession, s.ClaudePID, s.Person, s.Label, string(aliases), s.Status,
		orDefault(s.Activity, envelope.ActivityIdle), s.Repo, s.RepoRoot, s.Branch, s.Worktree, boolInt(s.Headless),
		s.AutonomousTurns, s.StartedAt, s.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("record session: %w", err)
	}
	return d.Session(ctx, s.LocalID)
}

// Session returns the session with local ID id, or nil.
func (d *DB) Session(ctx context.Context, id string) (*Session, error) {
	s, err := scanSession(d.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE local_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// UpdateSession applies fn to the session inside one transaction.
func (d *DB) UpdateSession(ctx context.Context, id string, fn func(*Session) error) (*Session, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	s, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE local_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("session %s is not recorded", id)
	}
	if err != nil {
		return nil, err
	}
	if err := fn(s); err != nil {
		return nil, err
	}
	aliases, _ := json.Marshal(nonNil(s.Aliases))
	var ended any
	if s.EndedAt != 0 {
		ended = s.EndedAt
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET label = ?, aliases = ?, status = ?, activity = ?, branch = ?,
		autonomous_turns = ?, updated_at = ?, cursor = ?, ended_at = ? WHERE local_id = ?`,
		s.Label, string(aliases), s.Status, s.Activity, s.Branch, s.AutonomousTurns, s.UpdatedAt, s.Cursor, ended, id)
	if err != nil {
		return nil, err
	}
	return s, tx.Commit()
}

// EndSession marks a session ended.
func (d *DB) EndSession(ctx context.Context, id string, at time.Time) error {
	_, err := d.db.ExecContext(ctx, `UPDATE sessions SET ended_at = ?, activity = 'idle', updated_at = ? WHERE local_id = ? AND ended_at IS NULL`, ms(at), ms(at), id)
	return err
}

// SetCursor records the last relay stream item stored for a session.
func (d *DB) SetCursor(ctx context.Context, id string, seq int64) error {
	_, err := d.db.ExecContext(ctx, `UPDATE sessions SET cursor = max(cursor, ?) WHERE local_id = ?`, seq, id)
	return err
}

// LabelsInUse returns the labels and aliases of the person's other live
// sessions in the project on this machine.
func (d *DB) LabelsInUse(ctx context.Context, project, person, exceptLocalID string) (map[string]bool, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT label, aliases FROM sessions WHERE project = ? AND person = ? AND local_id != ? AND ended_at IS NULL`, project, person, exceptLocalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var label, aliases string
		if err := rows.Scan(&label, &aliases); err != nil {
			return nil, err
		}
		used[label] = true
		var as []string
		json.Unmarshal([]byte(aliases), &as)
		for _, a := range as {
			used[a] = true
		}
	}
	return used, rows.Err()
}

// --- inbox ---------------------------------------------------------------

// Message is a received message.
type Message struct {
	RelaySession  string // recipient
	Seq           int64
	Project       string
	MessageID     string
	Thread        string
	References    []string
	FromHandle    string
	FromPerson    string
	FromSession   string
	To            []string
	Subject       string
	Body          string
	Date          string
	InReplyTo     string
	ReplyExpected bool
	Urgent        bool
	Wake          bool
	ReceivedAt    int64
	StoredAt      int64
	SurfacedAt    int64
	ReadAt        int64
	Replied       bool
	Quarantine    string // why it is held back; empty when trusted
	FromKey       string // the signer's public key
}

const inboxCols = `relay_session, seq, project, message_id, thread, refs, from_handle, from_person, from_session, to_json,
	subject, body, date, in_reply_to, reply_expected, urgent, wake, received_at, stored_at, surfaced_at, read_at, replied, quarantine, from_key`

func scanMessage(row interface{ Scan(...any) error }) (*Message, error) {
	var m Message
	var refs, to string
	var inReply sql.NullString
	var surfaced, read sql.NullInt64
	var reply, urgent, wake, replied int
	err := row.Scan(&m.RelaySession, &m.Seq, &m.Project, &m.MessageID, &m.Thread, &refs, &m.FromHandle, &m.FromPerson,
		&m.FromSession, &to, &m.Subject, &m.Body, &m.Date, &inReply, &reply, &urgent, &wake, &m.ReceivedAt, &m.StoredAt,
		&surfaced, &read, &replied, &m.Quarantine, &m.FromKey)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(refs), &m.References)
	json.Unmarshal([]byte(to), &m.To)
	m.InReplyTo = inReply.String
	m.ReplyExpected, m.Urgent, m.Wake, m.Replied = reply != 0, urgent != 0, wake != 0, replied != 0
	m.SurfacedAt, m.ReadAt = surfaced.Int64, read.Int64
	return &m, nil
}

func scanMessages(rows *sql.Rows, err error) ([]Message, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// StoreMessage stores a received message. It reports false for a duplicate
// (same stream item, or a message ID seen before: a replay).
func (d *DB) StoreMessage(ctx context.Context, m Message) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO seen (message_id, at) VALUES (?, ?)`, m.MessageID, m.StoredAt)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, tx.Commit()
	}
	refs, _ := json.Marshal(nonNil(m.References))
	to, _ := json.Marshal(nonNil(m.To))
	var inReply any
	if m.InReplyTo != "" {
		inReply = m.InReplyTo
	}
	res, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox (`+inboxCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, 0, ?, ?)`,
		m.RelaySession, m.Seq, m.Project, m.MessageID, m.Thread, string(refs), m.FromHandle, m.FromPerson, m.FromSession,
		string(to), m.Subject, m.Body, m.Date, inReply, boolInt(m.ReplyExpected), boolInt(m.Urgent), boolInt(m.Wake),
		m.ReceivedAt, m.StoredAt, m.Quarantine, m.FromKey)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, tx.Commit()
}

// Pending reports how many trusted messages for relaySession have not been
// surfaced, and how many of those are wake-eligible.
func (d *DB) Pending(ctx context.Context, relaySession string) (unsurfaced, wake int, err error) {
	err = d.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(wake), 0) FROM inbox
		WHERE relay_session = ? AND surfaced_at IS NULL AND quarantine = ''`, relaySession).Scan(&unsurfaced, &wake)
	return
}

// Claim atomically marks unsurfaced trusted messages as surfaced and returns
// them in stream order. With wakeOnly, it claims only if a wake-eligible
// message is among them, and then claims all of them (the woken turn should
// see everything waiting). Concurrent claimers never get the same message.
func (d *DB) Claim(ctx context.Context, relaySession string, wakeOnly bool, at time.Time) ([]Message, error) {
	cond := ""
	if wakeOnly {
		cond = ` AND EXISTS (SELECT 1 FROM inbox w WHERE w.relay_session = inbox.relay_session AND w.surfaced_at IS NULL AND w.quarantine = '' AND w.wake = 1)`
	}
	rows, err := d.db.QueryContext(ctx, `UPDATE inbox SET surfaced_at = ? WHERE relay_session = ? AND surfaced_at IS NULL AND quarantine = ''`+cond+`
		RETURNING `+inboxCols, ms(at), relaySession)
	msgs, err := scanMessages(rows, err)
	if err != nil {
		return nil, fmt.Errorf("claim messages: %w", err)
	}
	sortBySeq(msgs)
	return msgs, nil
}

// MarkRead records that the model saw these messages in full.
func (d *DB) MarkRead(ctx context.Context, relaySession string, seqs []int64, at time.Time) error {
	if len(seqs) == 0 {
		return nil
	}
	q := `UPDATE inbox SET read_at = ?, surfaced_at = coalesce(surfaced_at, ?) WHERE relay_session = ? AND seq IN (` + placeholders(len(seqs)) + `)`
	args := []any{ms(at), ms(at), relaySession}
	for _, s := range seqs {
		args = append(args, s)
	}
	_, err := d.db.ExecContext(ctx, q, args...)
	return err
}

// Unread returns trusted messages the model hasn't read in full, oldest first.
func (d *DB) Unread(ctx context.Context, relaySession string, limit int) ([]Message, error) {
	return scanMessages(d.db.QueryContext(ctx, `SELECT `+inboxCols+` FROM inbox WHERE relay_session = ? AND read_at IS NULL AND quarantine = '' ORDER BY seq LIMIT ?`, relaySession, limit))
}

// Recent returns the newest messages, newest first.
func (d *DB) Recent(ctx context.Context, relaySession string, limit int) ([]Message, error) {
	return scanMessages(d.db.QueryContext(ctx, `SELECT `+inboxCols+` FROM inbox WHERE relay_session = ? AND quarantine = '' ORDER BY seq DESC LIMIT ?`, relaySession, limit))
}

// ThreadMessages returns a thread's received messages for a session.
func (d *DB) ThreadMessages(ctx context.Context, relaySession, thread string) ([]Message, error) {
	return scanMessages(d.db.QueryContext(ctx, `SELECT `+inboxCols+` FROM inbox WHERE relay_session = ? AND (thread = ? OR message_id = ?) AND quarantine = '' ORDER BY seq`, relaySession, thread, thread))
}

// OpenReplies lists received messages that expect a reply and haven't got one.
func (d *DB) OpenReplies(ctx context.Context, relaySession string, limit int) ([]Message, error) {
	return scanMessages(d.db.QueryContext(ctx, `SELECT `+inboxCols+` FROM inbox WHERE relay_session = ? AND reply_expected = 1 AND replied = 0 AND quarantine = '' ORDER BY seq DESC LIMIT ?`, relaySession, limit))
}

// Quarantined counts held-back messages per reason.
func (d *DB) Quarantined(ctx context.Context, relaySession string) (map[string]int, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT quarantine, count(*) FROM inbox WHERE relay_session = ? AND quarantine != '' GROUP BY quarantine`, relaySession)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var why string
		var n int
		if err := rows.Scan(&why, &n); err != nil {
			return nil, err
		}
		out[why] = n
	}
	return out, rows.Err()
}

// HeldKey is an untrusted key seen on held-back messages from a person.
type HeldKey struct {
	Key      string
	Messages int
	Last     int64 // received_at of the newest
}

// HeldKeys lists the keys of a person's held-back messages in a project.
func (d *DB) HeldKeys(ctx context.Context, project, person string) ([]HeldKey, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT from_key, count(*), max(received_at) FROM inbox
		WHERE project = ? AND from_person = ? AND quarantine IN ('key_changed', 'unknown_person') AND from_key != ''
		GROUP BY from_key ORDER BY max(received_at) DESC`, project, person)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldKey
	for rows.Next() {
		var h HeldKey
		if err := rows.Scan(&h.Key, &h.Messages, &h.Last); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Release trusts a person's held-back messages signed with key, and returns
// the relay sessions that received them.
func (d *DB) Release(ctx context.Context, project, person, key string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `UPDATE inbox SET quarantine = '' WHERE project = ? AND from_person = ? AND from_key = ?
		AND quarantine IN ('key_changed', 'unknown_person') RETURNING relay_session`, project, person, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var rs string
		if err := rows.Scan(&rs); err != nil {
			return nil, err
		}
		if !seen[rs] {
			seen[rs] = true
			out = append(out, rs)
		}
	}
	return out, rows.Err()
}

// SessionsByRelay returns the local sessions behind relay session IDs.
func (d *DB) SessionsByRelay(ctx context.Context, relaySessions []string) ([]Session, error) {
	var out []Session
	for _, rs := range relaySessions {
		rows, err := d.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE relay_session = ?`, rs)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			s, err := scanSession(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, *s)
		}
		rows.Close()
	}
	return out, nil
}

// Parent is a message being replied to, received or sent by this machine.
type Parent struct {
	MessageID  string
	Thread     string
	References []string
}

// FindParent looks a message ID up in the session's inbox and sent mail.
func (d *DB) FindParent(ctx context.Context, relaySession, messageID string) (*Parent, error) {
	var p Parent
	var refs string
	err := d.db.QueryRowContext(ctx, `SELECT message_id, thread, refs FROM inbox WHERE relay_session = ? AND message_id = ? AND quarantine = ''
		UNION ALL SELECT message_id, thread, refs FROM sent WHERE relay_session = ? AND message_id = ? LIMIT 1`,
		relaySession, messageID, relaySession, messageID).Scan(&p.MessageID, &p.Thread, &refs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(refs), &p.References)
	return &p, nil
}

// MarkReplied flags a received message as answered.
func (d *DB) MarkReplied(ctx context.Context, relaySession, messageID string) error {
	_, err := d.db.ExecContext(ctx, `UPDATE inbox SET replied = 1 WHERE relay_session = ? AND message_id = ?`, relaySession, messageID)
	return err
}

// --- sent and receipts -----------------------------------------------------

// Sent is a message this session sent.
type Sent struct {
	RelaySession  string
	Seq           int64
	Project       string
	MessageID     string
	Thread        string
	References    []string
	To            []string
	Subject       string
	Body          string
	Date          string
	InReplyTo     string
	ReplyExpected bool
	Urgent        bool
	Recipients    map[string]string // relay session → handle at send time
	StoredAt      int64
	Receipts      []Receipt
}

// Receipt is a delivery outcome for one recipient.
type Receipt struct {
	Recipient string
	Status    string
	At        int64
}

// StoreSent records a sent message.
func (d *DB) StoreSent(ctx context.Context, s Sent) error {
	refs, _ := json.Marshal(nonNil(s.References))
	to, _ := json.Marshal(nonNil(s.To))
	recips, _ := json.Marshal(s.Recipients)
	var inReply any
	if s.InReplyTo != "" {
		inReply = s.InReplyTo
	}
	_, err := d.db.ExecContext(ctx, `INSERT OR REPLACE INTO sent (relay_session, seq, project, message_id, thread, refs, to_json, subject, body, date,
		in_reply_to, reply_expected, urgent, recipients, stored_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.RelaySession, s.Seq, s.Project, s.MessageID, s.Thread, string(refs), string(to), s.Subject, s.Body, s.Date, inReply,
		boolInt(s.ReplyExpected), boolInt(s.Urgent), string(recips), s.StoredAt)
	return err
}

// StoreReceipt records a receipt; a later status for the same copy wins.
func (d *DB) StoreReceipt(ctx context.Context, relaySession string, refSeq int64, r Receipt) error {
	_, err := d.db.ExecContext(ctx, `INSERT OR REPLACE INTO receipts (relay_session, ref_seq, recipient, status, at) VALUES (?, ?, ?, ?, ?)`,
		relaySession, refSeq, r.Recipient, r.Status, r.At)
	return err
}

// SentRecent returns the newest sent messages with their receipts.
func (d *DB) SentRecent(ctx context.Context, relaySession string, limit int) ([]Sent, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT seq, project, message_id, thread, refs, to_json, subject, body, date, coalesce(in_reply_to, ''),
		reply_expected, urgent, recipients, stored_at FROM sent WHERE relay_session = ? ORDER BY seq DESC LIMIT ?`, relaySession, limit)
	if err != nil {
		return nil, err
	}
	var out []Sent
	for rows.Next() {
		s := Sent{RelaySession: relaySession}
		var refs, to, recips string
		var reply, urgent int
		if err := rows.Scan(&s.Seq, &s.Project, &s.MessageID, &s.Thread, &refs, &to, &s.Subject, &s.Body, &s.Date, &s.InReplyTo,
			&reply, &urgent, &recips, &s.StoredAt); err != nil {
			rows.Close()
			return nil, err
		}
		json.Unmarshal([]byte(refs), &s.References)
		json.Unmarshal([]byte(to), &s.To)
		json.Unmarshal([]byte(recips), &s.Recipients)
		s.ReplyExpected, s.Urgent = reply != 0, urgent != 0
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		rr, err := d.db.QueryContext(ctx, `SELECT recipient, status, at FROM receipts WHERE relay_session = ? AND ref_seq = ? ORDER BY recipient`, relaySession, out[i].Seq)
		if err != nil {
			return nil, err
		}
		for rr.Next() {
			var r Receipt
			if err := rr.Scan(&r.Recipient, &r.Status, &r.At); err != nil {
				rr.Close()
				return nil, err
			}
			out[i].Receipts = append(out[i].Receipts, r)
		}
		rr.Close()
	}
	return out, nil
}

// --- peers cache ---------------------------------------------------------

// Peer is a cached view of another session in the project.
type Peer struct {
	Project    string
	Session    string
	State      string
	Presence   *envelope.Presence // nil when none or unverified
	LastSeenAt int64
	EndedAt    int64
	UpdatedAt  int64
}

// PutPeer upserts one peer.
func (d *DB) PutPeer(ctx context.Context, p Peer) error {
	return putPeer(ctx, d.db, p)
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func putPeer(ctx context.Context, x execer, p Peer) error {
	var pres any
	if p.Presence != nil {
		b, _ := json.Marshal(p.Presence)
		pres = string(b)
	}
	var ended any
	if p.EndedAt != 0 {
		ended = p.EndedAt
	}
	_, err := x.ExecContext(ctx, `INSERT INTO peers (project, session, state, presence, last_seen_at, ended_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project, session) DO UPDATE SET state = excluded.state, presence = coalesce(excluded.presence, peers.presence),
		last_seen_at = excluded.last_seen_at, ended_at = excluded.ended_at, updated_at = excluded.updated_at`,
		p.Project, p.Session, p.State, pres, p.LastSeenAt, ended, p.UpdatedAt)
	return err
}

// ReplacePeers replaces the project's whole cache with a fresh listing.
func (d *DB) ReplacePeers(ctx context.Context, project string, peers []Peer) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM peers WHERE project = ?`, project); err != nil {
		return err
	}
	for _, p := range peers {
		if err := putPeer(ctx, tx, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Peers lists the cached sessions of a project.
func (d *DB) Peers(ctx context.Context, project string) ([]Peer, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT session, state, presence, last_seen_at, coalesce(ended_at, 0), updated_at FROM peers WHERE project = ? ORDER BY session`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		p := Peer{Project: project}
		var pres sql.NullString
		if err := rows.Scan(&p.Session, &p.State, &pres, &p.LastSeenAt, &p.EndedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		if pres.Valid {
			var pr envelope.Presence
			if json.Unmarshal([]byte(pres.String), &pr) == nil {
				p.Presence = &pr
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- files, warnings, notices, counters --------------------------------------

// TouchFile records that a session changed a repo-relative path.
func (d *DB) TouchFile(ctx context.Context, localID, path, via string, at time.Time) error {
	_, err := d.db.ExecContext(ctx, `INSERT INTO files (local_id, path, at, via) VALUES (?, ?, ?, ?)
		ON CONFLICT (local_id, path) DO UPDATE SET at = excluded.at, via = excluded.via`, localID, path, ms(at), via)
	return err
}

// NoteFile records a path found changed by git, keeping an earlier record
// (its time and how it was seen) when there is one.
func (d *DB) NoteFile(ctx context.Context, localID, path string, at time.Time) error {
	_, err := d.db.ExecContext(ctx, `INSERT OR IGNORE INTO files (local_id, path, at, via) VALUES (?, ?, ?, 'git')`, localID, path, ms(at))
	return err
}

// File is a touched path.
type File struct {
	Path string
	At   int64
	Via  string
}

// Files returns a session's touched files, newest first.
func (d *DB) Files(ctx context.Context, localID string, limit int) ([]File, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT path, at, via FROM files WHERE local_id = ? ORDER BY at DESC, path LIMIT ?`, localID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.At, &f.Via); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FirstWarning reports whether this (path, peer) warning is new for the
// session, recording it.
func (d *DB) FirstWarning(ctx context.Context, localID, path, peer string) (bool, error) {
	res, err := d.db.ExecContext(ctx, `INSERT OR IGNORE INTO warned (local_id, path, peer) VALUES (?, ?, ?)`, localID, path, peer)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// NoticeDue reports whether a notice of kind may be shown to the session
// now (at most once per every), recording it when it is.
func (d *DB) NoticeDue(ctx context.Context, localID, kind string, at time.Time, every time.Duration) (bool, error) {
	res, err := d.db.ExecContext(ctx, `INSERT INTO notices (local_id, kind, at) VALUES (?, ?, ?)
		ON CONFLICT (local_id, kind) DO UPDATE SET at = excluded.at WHERE notices.at <= ?`, localID, kind, ms(at), ms(at.Add(-every)))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Count increments a per-project counter of discarded or held-back items.
func (d *DB) Count(ctx context.Context, project, kind string, at time.Time) error {
	_, err := d.db.ExecContext(ctx, `INSERT INTO counters (project, kind, n, last_at) VALUES (?, ?, 1, ?)
		ON CONFLICT (project, kind) DO UPDATE SET n = n + 1, last_at = excluded.last_at`, project, kind, ms(at))
	return err
}

// Counters returns a project's counters.
func (d *DB) Counters(ctx context.Context, project string) (map[string]int, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT kind, n FROM counters WHERE project = ?`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// Prune removes local history older than before (SPEC: 30 days).
func (d *DB) Prune(ctx context.Context, before time.Time) error {
	cut := ms(before)
	for _, q := range []string{
		`DELETE FROM inbox WHERE stored_at < ?`,
		`DELETE FROM sent WHERE stored_at < ?`,
		`DELETE FROM receipts WHERE at < ?`,
		`DELETE FROM seen WHERE at < ?`,
		`DELETE FROM files WHERE local_id IN (SELECT local_id FROM sessions WHERE ended_at < ?)`,
		`DELETE FROM warned WHERE local_id IN (SELECT local_id FROM sessions WHERE ended_at < ?)`,
		`DELETE FROM notices WHERE local_id IN (SELECT local_id FROM sessions WHERE ended_at < ?)`,
		`DELETE FROM sessions WHERE ended_at < ?`,
	} {
		if _, err := d.db.ExecContext(ctx, q, cut); err != nil {
			return err
		}
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func sortBySeq(ms []Message) {
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && ms[j].Seq < ms[j-1].Seq; j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
}
