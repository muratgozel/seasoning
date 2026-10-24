package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/relayapi"
)

// SchemaVersion is stored in meta and reported by /healthz. Migrations are
// forward-only and additive (SPEC §15.3).
const SchemaVersion = 2

// relayMigrations bring databases from older relays up to SchemaVersion.
var relayMigrations = []struct{ table, column, ddl string }{
	{"projects", "retire_at", `ALTER TABLE projects ADD COLUMN retire_at INTEGER`}, // v2: rotation
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  auth_hash BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  last_activity_at INTEGER NOT NULL,
  last_seq INTEGER NOT NULL DEFAULT 0,
  pruned_seq INTEGER NOT NULL DEFAULT 0,
  retire_at INTEGER
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS sessions (
  project TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  id TEXT NOT NULL,
  ended INTEGER NOT NULL DEFAULT 0,
  presence TEXT,
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  ended_at INTEGER,
  PRIMARY KEY (project, id)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS messages (
  project TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  from_session TEXT NOT NULL,
  envelope TEXT NOT NULL,
  received_at INTEGER NOT NULL,
  PRIMARY KEY (project, seq)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS deliveries (
  project TEXT NOT NULL,
  session TEXT NOT NULL,
  seq INTEGER NOT NULL,
  size INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (project, session, seq),
  FOREIGN KEY (project, seq) REFERENCES messages(project, seq) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS deliveries_by_age ON deliveries (created_at);
CREATE TABLE IF NOT EXISTS receipts (
  project TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  session TEXT NOT NULL,
  seq INTEGER NOT NULL,
  ref_seq INTEGER NOT NULL,
  recipient TEXT NOT NULL,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (project, session, seq)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS events (
  project TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  type TEXT NOT NULL,
  data TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (project, seq)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS roster (
  project TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  record TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (project, seq)
) WITHOUT ROWID;
`

// Errors the handlers map to API errors.
var (
	errProjectExists = errors.New("project exists with a different credential")
	errSessionGone   = errors.New("session not found")
	errSessionEnded  = errors.New("session has ended")
	errNoRecipients  = errors.New("no live sessions to deliver to")
	errLimit         = errors.New("limit reached")
)

type unknownSessionsError struct{ ids []string }

func (e *unknownSessionsError) Error() string {
	return "unknown sessions: " + strings.Join(e.ids, ", ")
}

// store wraps the relay database. All writes go through one connection, so
// they are serialized in-process; reads use a separate pool (WAL lets them
// run alongside the writer).
type store struct {
	w *sql.DB
	r *sql.DB
}

func openStore(path string) (*store, error) {
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close() // created private; SQLite gives -wal and -shm the same mode
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: create schema: %w", path, err)
	}
	var v string
	err = w.QueryRow(`SELECT v FROM meta WHERE k = 'schema'`).Scan(&v)
	stored, _ := strconv.Atoi(v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		stored = 0
	case err != nil:
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	case stored > SchemaVersion:
		w.Close()
		return nil, fmt.Errorf("%s has schema version %d, newer than this relay (%d); run the seasoning version that created it, or newer", path, stored, SchemaVersion)
	}
	for _, m := range relayMigrations {
		var n int
		if err := w.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, m.table, m.column).Scan(&n); err != nil {
			w.Close()
			return nil, fmt.Errorf("open %s: migrate: %w", path, err)
		}
		if n == 0 {
			if _, err := w.Exec(m.ddl); err != nil {
				w.Close()
				return nil, fmt.Errorf("open %s: migrate: %w", path, err)
			}
		}
	}
	if _, err := w.Exec(`INSERT INTO meta (k, v) VALUES ('schema', ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, fmt.Sprint(SchemaVersion)); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	r, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	r.SetMaxOpenConns(8)
	return &store{w: w, r: r}, nil
}

func (s *store) close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) string { return envelope.FormatTime(time.UnixMilli(v)) }

func (s *store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func nextSeq(ctx context.Context, tx *sql.Tx, project string) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `UPDATE projects SET last_seq = last_seq + 1 WHERE id = ? RETURNING last_seq`, project).Scan(&seq)
	return seq, err
}

func touch(ctx context.Context, tx *sql.Tx, project string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE projects SET last_activity_at = ? WHERE id = ?`, ms(now), project)
	return err
}

// createProject registers a project. Creating an existing project with the
// same credential is idempotent.
func (s *store) createProject(ctx context.Context, id string, authHash [32]byte, now time.Time, maxProjects int) (created bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var existing []byte
		err := tx.QueryRowContext(ctx, `SELECT auth_hash FROM projects WHERE id = ?`, id).Scan(&existing)
		if err == nil {
			if subtle.ConstantTimeCompare(existing, authHash[:]) != 1 {
				return errProjectExists
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM projects`).Scan(&n); err != nil {
			return err
		}
		if n >= maxProjects {
			return errLimit
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO projects (id, auth_hash, created_at, last_activity_at) VALUES (?, ?, ?, ?)`, id, authHash[:], ms(now), ms(now))
		created = err == nil
		return err
	})
	return created, err
}

// authProject reports whether token authenticates project. Unknown projects
// and wrong tokens are indistinguishable to the caller.
func (s *store) authProject(ctx context.Context, project string, token []byte) (bool, error) {
	var stored []byte
	err := s.r.QueryRowContext(ctx, `SELECT auth_hash FROM projects WHERE id = ?`, project).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		sum := sha256.Sum256(token) // same work either way
		subtle.ConstantTimeCompare(sum[:], sum[:])
		return false, nil
	}
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(token)
	return subtle.ConstantTimeCompare(stored, sum[:]) == 1, nil
}

// retireProject schedules a project's deletion (rotation, SPEC §7.4).
func (s *store) retireProject(ctx context.Context, project string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE projects SET retire_at = ? WHERE id = ?`, ms(at), project)
		return err
	})
}

func (s *store) deleteProject(ctx context.Context, project string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, project)
		return err
	})
}

type sessionRow struct {
	id         string
	ended      bool
	presence   sql.NullString
	createdAt  int64
	lastSeenAt int64
	endedAt    sql.NullInt64
}

func (r sessionRow) api(now time.Time, awayAfter time.Duration) relayapi.Session {
	out := relayapi.Session{Session: r.id, CreatedAt: fromMS(r.createdAt), LastSeenAt: fromMS(r.lastSeenAt)}
	switch {
	case r.ended:
		out.State = relayapi.StateEnded
	case now.Sub(time.UnixMilli(r.lastSeenAt)) > awayAfter:
		out.State = relayapi.StateAway
	default:
		out.State = relayapi.StateLive
	}
	if r.presence.Valid {
		out.Presence = json.RawMessage(r.presence.String)
	}
	if r.endedAt.Valid {
		out.EndedAt = fromMS(r.endedAt.Int64)
	}
	return out
}

func scanSession(row interface{ Scan(...any) error }) (sessionRow, error) {
	var r sessionRow
	var ended int
	err := row.Scan(&r.id, &ended, &r.presence, &r.createdAt, &r.lastSeenAt, &r.endedAt)
	r.ended = ended != 0
	return r, err
}

const sessionCols = `id, ended, presence, created_at, last_seen_at, ended_at`

// emit appends a broadcast event (session or roster change).
func emit(ctx context.Context, tx *sql.Tx, project, typ string, data any, now time.Time) (int64, error) {
	seq, err := nextSeq(ctx, tx, project)
	if err != nil {
		return 0, err
	}
	if m, ok := data.(interface{ setSeq(int64) }); ok {
		m.setSeq(seq)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events (project, seq, type, data, created_at) VALUES (?, ?, ?, ?, ?)`, project, seq, typ, string(b), ms(now))
	return seq, err
}

// putSession registers a session, revives an ended one (resume), or records
// a heartbeat. presence nil keeps the stored presence. Changes other than a
// bare heartbeat are broadcast as session events.
func (s *store) putSession(ctx context.Context, project, id string, presence *string, now time.Time, awayAfter time.Duration, maxLive int) (relayapi.Session, error) {
	var out relayapi.Session
	err := s.tx(ctx, func(tx *sql.Tx) error {
		r, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE project = ? AND id = ?`, project, id))
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !exists || r.ended {
			var live int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE project = ? AND ended = 0`, project).Scan(&live); err != nil {
				return err
			}
			if live >= maxLive {
				return errLimit
			}
		}
		changed := !exists || r.ended || (presence != nil && (!r.presence.Valid || r.presence.String != *presence))
		if !exists {
			r = sessionRow{id: id, createdAt: ms(now)}
		}
		r.ended = false
		r.endedAt = sql.NullInt64{}
		r.lastSeenAt = ms(now)
		if presence != nil {
			r.presence = sql.NullString{String: *presence, Valid: true}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO sessions (project, id, ended, presence, created_at, last_seen_at, ended_at)
			VALUES (?, ?, 0, ?, ?, ?, NULL)
			ON CONFLICT (project, id) DO UPDATE SET ended = 0, presence = excluded.presence, last_seen_at = excluded.last_seen_at, ended_at = NULL`,
			project, id, r.presence, r.createdAt, r.lastSeenAt)
		if err != nil {
			return err
		}
		if err := touch(ctx, tx, project, now); err != nil {
			return err
		}
		out = r.api(now, awayAfter)
		if changed {
			_, err = emit(ctx, tx, project, relayapi.EventSession, out, now)
		}
		return err
	})
	return out, err
}

// endSession marks a session ended. Its queued deliveries stay: messages to
// an ended session are held in case it resumes (SPEC D10).
func (s *store) endSession(ctx context.Context, project, id string, now time.Time, awayAfter time.Duration) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		r, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE project = ? AND id = ?`, project, id))
		if errors.Is(err, sql.ErrNoRows) {
			return errSessionGone
		}
		if err != nil {
			return err
		}
		if r.ended {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended = 1, ended_at = ? WHERE project = ? AND id = ?`, ms(now), project, id); err != nil {
			return err
		}
		r.ended = true
		r.endedAt = sql.NullInt64{Int64: ms(now), Valid: true}
		if err := touch(ctx, tx, project, now); err != nil {
			return err
		}
		_, err = emit(ctx, tx, project, relayapi.EventSession, r.api(now, awayAfter), now)
		return err
	})
}

func (s *store) sessions(ctx context.Context, project string, now time.Time, awayAfter time.Duration) ([]relayapi.Session, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE project = ? ORDER BY created_at, id`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []relayapi.Session{}
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r.api(now, awayAfter))
	}
	return out, rows.Err()
}

func (s *store) sessionExists(ctx context.Context, project, id string) (bool, error) {
	var one int
	err := s.r.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE project = ? AND id = ?`, project, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

type queueLimits struct {
	messages int
	bytes    int64
}

// postMessage stores an envelope and fans it out. A broadcast goes to every
// session that hasn't ended, except the sender; a list goes to exactly the
// listed sessions, ended ones included (held for resume).
func (s *store) postMessage(ctx context.Context, project, from string, to envelope.Recipients, raw string, now time.Time, awayAfter time.Duration, ql queueLimits) (relayapi.PostMessageResponse, error) {
	var resp relayapi.PostMessageResponse
	err := s.tx(ctx, func(tx *sql.Tx) error {
		sender, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE project = ? AND id = ?`, project, from))
		if errors.Is(err, sql.ErrNoRows) {
			return errSessionGone
		}
		if err != nil {
			return err
		}
		if sender.ended {
			return errSessionEnded
		}

		var targets []sessionRow
		if to.All {
			rows, err := tx.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE project = ? AND ended = 0 AND id != ? ORDER BY id`, project, from)
			if err != nil {
				return err
			}
			for rows.Next() {
				r, err := scanSession(rows)
				if err != nil {
					rows.Close()
					return err
				}
				targets = append(targets, r)
			}
			rows.Close()
			if len(targets) == 0 {
				return errNoRecipients
			}
		} else {
			var unknown []string
			for _, id := range to.Sessions {
				r, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE project = ? AND id = ?`, project, id))
				if errors.Is(err, sql.ErrNoRows) {
					unknown = append(unknown, id)
					continue
				}
				if err != nil {
					return err
				}
				targets = append(targets, r)
			}
			if len(unknown) > 0 {
				return &unknownSessionsError{ids: unknown}
			}
		}

		seq, err := nextSeq(ctx, tx, project)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages (project, seq, from_session, envelope, received_at) VALUES (?, ?, ?, ?, ?)`, project, seq, from, raw, ms(now)); err != nil {
			return err
		}
		size := int64(len(raw))
		queued := 0
		for _, t := range targets {
			st := relayapi.Recipient{Session: t.id, State: t.api(now, awayAfter).State, Status: relayapi.StatusQueued}
			var n int
			var bytes int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size), 0) FROM deliveries WHERE project = ? AND session = ?`, project, t.id).Scan(&n, &bytes); err != nil {
				return err
			}
			if n >= ql.messages || bytes+size > ql.bytes {
				st.Status = relayapi.StatusQueueFull
			} else {
				if _, err := tx.ExecContext(ctx, `INSERT INTO deliveries (project, session, seq, size, created_at) VALUES (?, ?, ?, ?, ?)`, project, t.id, seq, size, ms(now)); err != nil {
					return err
				}
				queued++
			}
			resp.Recipients = append(resp.Recipients, st)
		}
		if queued == 0 {
			// Nobody can take it; don't keep an orphan.
			if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE project = ? AND seq = ?`, project, seq); err != nil {
				return err
			}
		}
		resp.Seq = seq
		resp.ReceivedAt = fromMS(ms(now))
		return touch(ctx, tx, project, now)
	})
	return resp, err
}

// event is one item of a session's stream.
type event struct {
	seq  int64
	typ  string
	data []byte
}

// eventsAfter returns up to limit stream items for session after seq, in seq
// order: its message deliveries and receipts, and the project's broadcast
// events. resync is true when broadcast events after seq were pruned.
func (s *store) eventsAfter(ctx context.Context, project, session string, after int64, limit int) (evs []event, resync bool, err error) {
	var pruned int64
	if err := s.r.QueryRowContext(ctx, `SELECT pruned_seq FROM projects WHERE id = ?`, project).Scan(&pruned); err != nil {
		return nil, false, err
	}
	resync = after > 0 && after < pruned

	type source struct {
		evs  []event
		full bool
	}
	var sources []source

	rows, err := s.r.QueryContext(ctx, `SELECT m.seq, m.received_at, m.envelope FROM deliveries d JOIN messages m ON m.project = d.project AND m.seq = d.seq
		WHERE d.project = ? AND d.session = ? AND d.seq > ? ORDER BY d.seq LIMIT ?`, project, session, after, limit)
	if err != nil {
		return nil, false, err
	}
	var src source
	for rows.Next() {
		var seq, at int64
		var env string
		if err := rows.Scan(&seq, &at, &env); err != nil {
			rows.Close()
			return nil, false, err
		}
		b, _ := json.Marshal(relayapi.MessageEvent{Seq: seq, ReceivedAt: fromMS(at), Envelope: json.RawMessage(env)})
		src.evs = append(src.evs, event{seq: seq, typ: relayapi.EventMessage, data: b})
	}
	rows.Close()
	src.full = len(src.evs) == limit
	sources = append(sources, src)

	rows, err = s.r.QueryContext(ctx, `SELECT seq, ref_seq, recipient, status, created_at FROM receipts WHERE project = ? AND session = ? AND seq > ? ORDER BY seq LIMIT ?`, project, session, after, limit)
	if err != nil {
		return nil, false, err
	}
	src = source{}
	for rows.Next() {
		var r relayapi.ReceiptEvent
		var at int64
		if err := rows.Scan(&r.Seq, &r.RefSeq, &r.Recipient, &r.Status, &at); err != nil {
			rows.Close()
			return nil, false, err
		}
		r.At = fromMS(at)
		b, _ := json.Marshal(r)
		src.evs = append(src.evs, event{seq: r.Seq, typ: relayapi.EventReceipt, data: b})
	}
	rows.Close()
	src.full = len(src.evs) == limit
	sources = append(sources, src)

	rows, err = s.r.QueryContext(ctx, `SELECT seq, type, data FROM events WHERE project = ? AND seq > ? ORDER BY seq LIMIT ?`, project, after, limit)
	if err != nil {
		return nil, false, err
	}
	src = source{}
	for rows.Next() {
		var e event
		var data string
		if err := rows.Scan(&e.seq, &e.typ, &data); err != nil {
			rows.Close()
			return nil, false, err
		}
		e.data = []byte(data)
		src.evs = append(src.evs, e)
	}
	rows.Close()
	src.full = len(src.evs) == limit
	sources = append(sources, src)

	// Merge. When a source hit the limit, items past its last seq may exist
	// in it, so the merged batch stops there to leave no gaps.
	boundary := int64(-1)
	for _, src := range sources {
		if src.full {
			last := src.evs[len(src.evs)-1].seq
			if boundary < 0 || last < boundary {
				boundary = last
			}
		}
		evs = append(evs, src.evs...)
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].seq < evs[j].seq })
	if boundary >= 0 {
		n := sort.Search(len(evs), func(i int) bool { return evs[i].seq > boundary })
		evs = evs[:n]
	}
	return evs, resync, nil
}

// ack removes session's deliveries and receipts up to seq. Each removed
// delivery produces a "delivered" receipt for its sender, if the sender is
// still around to read it.
func (s *store) ack(ctx context.Context, project, session string, upTo int64, now time.Time) (notify bool, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT d.seq, m.from_session FROM deliveries d JOIN messages m ON m.project = d.project AND m.seq = d.seq
			WHERE d.project = ? AND d.session = ? AND d.seq <= ?`, project, session, upTo)
		if err != nil {
			return err
		}
		type acked struct {
			seq  int64
			from string
		}
		var done []acked
		for rows.Next() {
			var a acked
			if err := rows.Scan(&a.seq, &a.from); err != nil {
				rows.Close()
				return err
			}
			done = append(done, a)
		}
		rows.Close()
		if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE project = ? AND session = ? AND seq <= ?`, project, session, upTo); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM receipts WHERE project = ? AND session = ? AND seq <= ?`, project, session, upTo); err != nil {
			return err
		}
		for _, a := range done {
			if a.from == session {
				continue
			}
			ok, err := liveSession(ctx, tx, project, a.from)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := addReceipt(ctx, tx, project, a.from, a.seq, session, relayapi.ReceiptDelivered, now); err != nil {
				return err
			}
			notify = true
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE project = ? AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.project = messages.project AND d.seq = messages.seq)`, project); err != nil {
			return err
		}
		return touch(ctx, tx, project, now)
	})
	return notify, err
}

func liveSession(ctx context.Context, tx *sql.Tx, project, id string) (bool, error) {
	var ended int
	err := tx.QueryRowContext(ctx, `SELECT ended FROM sessions WHERE project = ? AND id = ?`, project, id).Scan(&ended)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil && ended == 0, err
}

func addReceipt(ctx context.Context, tx *sql.Tx, project, sender string, refSeq int64, recipient, status string, now time.Time) error {
	seq, err := nextSeq(ctx, tx, project)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO receipts (project, session, seq, ref_seq, recipient, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		project, sender, seq, refSeq, recipient, status, ms(now))
	return err
}

// rosterEntry gets its seq when emitted.
type rosterEntry struct{ relayapi.RosterEntry }

func (r *rosterEntry) setSeq(seq int64) { r.Seq = seq }

func (s *store) addRoster(ctx context.Context, project, raw string, now time.Time, max int) (int64, error) {
	var seq int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM roster WHERE project = ?`, project).Scan(&n); err != nil {
			return err
		}
		if n >= max {
			return errLimit
		}
		e := &rosterEntry{relayapi.RosterEntry{Record: json.RawMessage(raw), CreatedAt: fromMS(ms(now))}}
		var err error
		seq, err = emit(ctx, tx, project, relayapi.EventRoster, e, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO roster (project, seq, record, created_at) VALUES (?, ?, ?, ?)`, project, seq, raw, ms(now)); err != nil {
			return err
		}
		return touch(ctx, tx, project, now)
	})
	return seq, err
}

func (s *store) roster(ctx context.Context, project string) ([]relayapi.RosterEntry, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT seq, record, created_at FROM roster WHERE project = ? ORDER BY seq`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []relayapi.RosterEntry{}
	for rows.Next() {
		var e relayapi.RosterEntry
		var rec string
		var at int64
		if err := rows.Scan(&e.Seq, &rec, &at); err != nil {
			return nil, err
		}
		e.Record = json.RawMessage(rec)
		e.CreatedAt = fromMS(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// gcResult counts what one GC pass removed; touched lists projects whose
// streams have new items (expiry receipts).
type gcResult struct {
	expired, messages, receipts, events, sessions, projects int64
	touched                                                 []string
}

// gc removes everything past its retention (SPEC §14.4). Undelivered copies
// expire after hold and produce "expired" receipts for live senders.
func (s *store) gc(ctx context.Context, now time.Time, t TTLs) (gcResult, error) {
	var res gcResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		holdCut := ms(now.Add(-t.Hold))
		rows, err := tx.QueryContext(ctx, `SELECT d.project, d.session, d.seq, m.from_session FROM deliveries d JOIN messages m ON m.project = d.project AND m.seq = d.seq WHERE d.created_at < ?`, holdCut)
		if err != nil {
			return err
		}
		type exp struct {
			project, session, from string
			seq                    int64
		}
		var expired []exp
		for rows.Next() {
			var e exp
			if err := rows.Scan(&e.project, &e.session, &e.seq, &e.from); err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, e)
		}
		rows.Close()
		touched := map[string]bool{}
		for _, e := range expired {
			if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE project = ? AND session = ? AND seq = ?`, e.project, e.session, e.seq); err != nil {
				return err
			}
			res.expired++
			ok, err := liveSession(ctx, tx, e.project, e.from)
			if err != nil {
				return err
			}
			if ok && e.from != e.session {
				if err := addReceipt(ctx, tx, e.project, e.from, e.seq, e.session, relayapi.ReceiptExpired, now); err != nil {
					return err
				}
				touched[e.project] = true
			}
		}
		for p := range touched {
			res.touched = append(res.touched, p)
		}

		r, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.project = messages.project AND d.seq = messages.seq)`)
		if err != nil {
			return err
		}
		res.messages, _ = r.RowsAffected()

		r, err = tx.ExecContext(ctx, `DELETE FROM receipts WHERE created_at < ?`, holdCut)
		if err != nil {
			return err
		}
		res.receipts, _ = r.RowsAffected()

		eventCut := ms(now.Add(-t.Event))
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET pruned_seq = max(pruned_seq, (SELECT max(seq) FROM events e WHERE e.project = projects.id AND e.created_at < ?))
			WHERE EXISTS (SELECT 1 FROM events e WHERE e.project = projects.id AND e.created_at < ?)`, eventCut, eventCut); err != nil {
			return err
		}
		r, err = tx.ExecContext(ctx, `DELETE FROM events WHERE created_at < ?`, eventCut)
		if err != nil {
			return err
		}
		res.events, _ = r.RowsAffected()

		sessionCut := ms(now.Add(-t.Session))
		if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.project = deliveries.project AND s.id = deliveries.session AND s.last_seen_at < ?)`, sessionCut); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM receipts WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.project = receipts.project AND s.id = receipts.session AND s.last_seen_at < ?)`, sessionCut); err != nil {
			return err
		}
		r, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE last_seen_at < ?`, sessionCut)
		if err != nil {
			return err
		}
		res.sessions, _ = r.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.project = messages.project AND d.seq = messages.seq)`); err != nil {
			return err
		}

		r, err = tx.ExecContext(ctx, `DELETE FROM projects WHERE last_activity_at < ? OR retire_at < ?`, ms(now.Add(-t.ProjectIdle)), ms(now))
		if err != nil {
			return err
		}
		res.projects, _ = r.RowsAffected()
		return nil
	})
	return res, err
}
