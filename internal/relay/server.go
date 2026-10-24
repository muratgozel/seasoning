// Package relay is the seasoning relay server (SPEC §14): it stores
// ciphertext and opaque IDs in SQLite and routes envelopes between sessions
// over a REST API with a Server-Sent Events stream per session.
//
// The relay never holds a project key. It authenticates project calls with a
// bearer token derived from the project secret and keeps only its SHA-256.
package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/version"
)

// MinTokenLength is the shortest relay token the server accepts.
const MinTokenLength = 32

// Limits protect the relay's resources (SPEC §14.5). They are not
// conversation limits.
type Limits struct {
	MaxRequestBytes       int64
	MaxEnvelopeBytes      int
	MaxPresenceBytes      int
	MaxQueueMessages      int
	MaxQueueBytes         int64
	MaxLiveSessions       int // per project
	MaxProjects           int
	MaxRosterRecords      int // per project
	AuthFailuresPerMinute int // per client IP
}

// DefaultLimits are the SPEC §14.5 values.
func DefaultLimits() Limits {
	return Limits{
		MaxRequestBytes:       128 << 10,
		MaxEnvelopeBytes:      envelope.MaxEnvelopeBytes,
		MaxPresenceBytes:      envelope.MaxPresenceBytes,
		MaxQueueMessages:      1000,
		MaxQueueBytes:         16 << 20,
		MaxLiveSessions:       200,
		MaxProjects:           1000,
		MaxRosterRecords:      1000,
		AuthFailuresPerMinute: 30,
	}
}

// TTLs are the retention periods (SPEC §9.1, §14.4).
type TTLs struct {
	Hold        time.Duration // undelivered copies and receipts
	Session     time.Duration // sessions not seen for this long are purged
	Event       time.Duration // broadcast events kept for reconnect catch-up
	ProjectIdle time.Duration
	Away        time.Duration // no heartbeat for this long reads as "away"
	GCInterval  time.Duration
}

// DefaultTTLs are the SPEC values.
func DefaultTTLs() TTLs {
	return TTLs{
		Hold:        7 * 24 * time.Hour,
		Session:     7 * 24 * time.Hour,
		Event:       time.Hour,
		ProjectIdle: 90 * 24 * time.Hour,
		Away:        60 * time.Second,
		GCInterval:  10 * time.Minute,
	}
}

// Options configure a Server.
type Options struct {
	DBPath     string
	RelayToken string // required; gates project creation (SPEC D12)
	Limits     Limits
	TTLs       TTLs
	// TrustProxy takes the client IP from the last X-Forwarded-For entry,
	// as appended by a reverse proxy such as nginx. Only enable it behind
	// one, or clients can pick their own rate-limit bucket.
	TrustProxy bool
	Logger     *slog.Logger
	Now        func() time.Time
	KeepAlive  time.Duration // SSE comment interval
}

// Server is a relay instance.
type Server struct {
	opts     Options
	db       *store
	hub      *hub
	limiter  *limiter
	tokenSum [32]byte
	mux      *http.ServeMux
}

// New opens the database and builds the server.
func New(opts Options) (*Server, error) {
	if len(opts.RelayToken) < MinTokenLength {
		return nil, fmt.Errorf("the relay token must be at least %d characters; generate one with: openssl rand -base64 32", MinTokenLength)
	}
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	if opts.TTLs == (TTLs{}) {
		opts.TTLs = DefaultTTLs()
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.KeepAlive == 0 {
		opts.KeepAlive = 15 * time.Second
	}
	db, err := openStore(opts.DBPath)
	if err != nil {
		return nil, err
	}
	s := &Server{
		opts:     opts,
		db:       db,
		hub:      newHub(),
		limiter:  newLimiter(opts.Limits.AuthFailuresPerMinute, time.Minute),
		tokenSum: sha256.Sum256([]byte(opts.RelayToken)),
		mux:      http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

// Close releases the database.
func (s *Server) Close() error { return s.db.close() }

// Handler returns the HTTP handler with request logging.
func (s *Server) Handler() http.Handler { return s.logRequests(s.mux) }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /join", s.joinPage)
	s.mux.HandleFunc("POST /v1/projects", s.createProject)
	s.mux.HandleFunc("DELETE /v1/projects/{p}", s.project(s.deleteProject))
	s.mux.HandleFunc("POST /v1/projects/{p}/retire", s.project(s.retireProject))
	s.mux.HandleFunc("GET /v1/projects/{p}/roster", s.project(s.getRoster))
	s.mux.HandleFunc("POST /v1/projects/{p}/roster", s.project(s.postRoster))
	s.mux.HandleFunc("GET /v1/projects/{p}/sessions", s.project(s.listSessions))
	s.mux.HandleFunc("PUT /v1/projects/{p}/sessions/{s}", s.project(s.session(s.putSession)))
	s.mux.HandleFunc("DELETE /v1/projects/{p}/sessions/{s}", s.project(s.session(s.endSession)))
	s.mux.HandleFunc("GET /v1/projects/{p}/sessions/{s}/events", s.project(s.session(s.events)))
	s.mux.HandleFunc("POST /v1/projects/{p}/sessions/{s}/ack", s.project(s.session(s.ack)))
	s.mux.HandleFunc("POST /v1/projects/{p}/messages", s.project(s.postMessage))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, relayapi.CodeNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path, "the API lives under /v1; check the client and relay versions")
	})
}

// RunGC runs a GC pass every TTLs.GCInterval until ctx ends.
func (s *Server) RunGC(ctx context.Context) {
	t := time.NewTicker(s.opts.TTLs.GCInterval)
	defer t.Stop()
	for {
		if err := s.GC(ctx); err != nil && ctx.Err() == nil {
			s.opts.Logger.Error("gc failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// GC runs one retention pass.
func (s *Server) GC(ctx context.Context) error {
	res, err := s.db.gc(ctx, s.opts.Now(), s.opts.TTLs)
	if err != nil {
		return err
	}
	for _, p := range res.touched {
		s.hub.notify(p)
	}
	if res.expired+res.messages+res.receipts+res.events+res.sessions+res.projects > 0 {
		s.opts.Logger.Info("gc", "expired", res.expired, "messages", res.messages, "receipts", res.receipts,
			"events", res.events, "sessions", res.sessions, "projects", res.projects)
	}
	return nil
}

// --- errors and JSON -------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg, hint string) {
	writeJSON(w, status, relayapi.ErrorBody{Error: relayapi.ErrorDetail{Code: code, Message: msg, Hint: hint}})
}

func (s *Server) internal(w http.ResponseWriter, r *http.Request, err error) {
	s.opts.Logger.Error("internal error", "route", r.Pattern, "error", err.Error())
	writeError(w, http.StatusInternalServerError, relayapi.CodeInternal, "the relay hit an internal error", "retry shortly; if it persists, the relay operator can find it in the relay's log")
}

// readBody reads a request body within the request size limit.
func (s *Server) readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.Limits.MaxRequestBytes))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, relayapi.CodePayloadTooLarge,
			fmt.Sprintf("request body exceeds %d bytes", s.opts.Limits.MaxRequestBytes), "send less per request")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, "could not read the request body", "")
		return nil, false
	}
	return b, true
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	b, ok := s.readBody(w, r)
	if !ok {
		return false
	}
	if err := json.Unmarshal(b, v); err != nil {
		writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, "request body is not valid JSON: "+err.Error(), "")
		return false
	}
	return true
}

// compactEnvelope validates an envelope's outer shape and returns it as one
// line of JSON (SSE data must not contain newlines). The envelope is stored
// as the sender wrote it, so fields this relay doesn't know survive.
func (s *Server) compactEnvelope(w http.ResponseWriter, raw []byte, kind, project, session string, max int) (*envelope.Envelope, string, bool) {
	e, err := envelope.ParseEnvelope(raw, max)
	if err != nil {
		status, code := http.StatusBadRequest, relayapi.CodeBadRequest
		if len(raw) > max {
			status, code = http.StatusRequestEntityTooLarge, relayapi.CodePayloadTooLarge
		}
		writeError(w, status, code, err.Error(), "")
		return nil, "", false
	}
	if e.Kind != kind || e.Project != project || (session != "" && e.FromSession != session) {
		writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest,
			fmt.Sprintf("envelope must be kind %q for project %s%s", kind, project, map[bool]string{true: " from session " + session}[session != ""]), "")
		return nil, "", false
	}
	var buf strings.Builder
	if err := compactJSON(&buf, raw); err != nil {
		writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, "envelope is not valid JSON", "")
		return nil, "", false
	}
	return e, buf.String(), true
}

// --- middleware --------------------------------------------------------------

type ctxKey int

const projectKey ctxKey = 0

// project authenticates a project route. Unknown projects and wrong
// credentials get the same 404, so project IDs can't be probed.
func (s *Server) project(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		if s.limiter.blocked(ip, s.opts.Now()) {
			writeError(w, http.StatusTooManyRequests, relayapi.CodeRateLimited, "too many failed authentication attempts from your address", "wait a minute, then check that you are using a current invite")
			return
		}
		p := r.PathValue("p")
		tok, ok := bearer(r)
		var token []byte
		if ok {
			token, ok = keys.DecodeBearer(tok)
		}
		if ok && keys.ValidProjectID(p) {
			valid, err := s.db.authProject(r.Context(), p, token)
			if err != nil {
				s.internal(w, r, err)
				return
			}
			if valid {
				next(w, r.WithContext(context.WithValue(r.Context(), projectKey, p)))
				return
			}
		}
		s.limiter.fail(ip, s.opts.Now())
		writeError(w, http.StatusNotFound, relayapi.CodeNotFound, "project not found, or its credential is wrong",
			"the project may have been rotated or removed; ask a member for a fresh invite")
	}
}

// session validates the {s} path segment.
func (s *Server) session(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !keys.ValidSessionID(r.PathValue("s")) {
			writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, fmt.Sprintf("%q is not a session ID", r.PathValue("s")), "")
			return
		}
		next(w, r)
	}
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	return strings.TrimSpace(tok), ok && tok != ""
}

func (s *Server) clientIP(r *http.Request) string {
	if s.opts.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logRequests logs one line per request: no tokens, no bodies, and project
// IDs truncated (SPEC §14.6).
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		attrs := []any{"method", r.Method, "route", r.Pattern, "status", sw.status, "ms", time.Since(start).Milliseconds()}
		if p := r.PathValue("p"); len(p) > 6 {
			attrs = append(attrs, "project", p[:6]+"…")
		}
		s.opts.Logger.Info("request", attrs...)
	})
}

// --- handlers ----------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, relayapi.Health{OK: true, Version: version.Version, Schema: SchemaVersion})
}

// joinScript fills the join command with the invite from the address bar.
// Showing it is safe: the fragment never left the browser; the CSP forbids
// every network request (default-src 'none') and lets only this exact
// script run (by hash); and the link must match the invite format and is set
// as text, never as HTML.
const joinScript = `(function () {
  if (!/^#k=[A-Za-z0-9_-]{43}$/.test(location.hash) || !/\/join$/.test(location.pathname)) return;
  var cmd = document.getElementById("cmd");
  cmd.textContent = "seasoning join '" + location.origin + location.pathname + location.hash + "'";
  document.getElementById("filled").hidden = false;
  if (!navigator.clipboard) return;
  var copy = document.getElementById("copy");
  copy.hidden = false;
  copy.onclick = function () {
    navigator.clipboard.writeText(cmd.textContent).then(function () { copy.textContent = "Copied"; });
  };
})();`

const joinHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Join a seasoning project</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem;color:#222;background:#fff}code,pre{background:#f3f3f3;padding:.1rem .3rem;border-radius:4px}pre{padding:.75rem;white-space:pre-wrap;word-break:break-all}.note{font-size:.9rem;color:#666;margin:.25rem 0}button{font:inherit;padding:.3rem .9rem;border:1px solid #bbb;border-radius:6px;background:#fff;color:inherit;cursor:pointer}@media (prefers-color-scheme:dark){body{color:#ddd;background:#161616}code,pre{background:#262626}.note{color:#999}button{background:#262626;border-color:#555}}</style>
</head><body>
<h1>You have a seasoning invite</h1>
<p>This link is an invite to a seasoning project: a relay that lets coding agents on the same project see and message each other.</p>
<p>The part of the link after <code>#</code> is the project's secret. Your browser did not send it to this server. Keep it private.</p>
<ol>
<li>Install seasoning: <pre>curl -fsSL https://raw.githubusercontent.com/muratgozel/seasoning/main/scripts/install.sh | bash</pre></li>
<li>Set up your identity: <pre>seasoning setup</pre></li>
<li>In your clone of the repository, join with the full link, in quotes: <pre id="cmd">seasoning join '&lt;the whole link, including everything after #&gt;'</pre>
<p id="filled" class="note" hidden>Filled in from your address bar, in this browser only; nothing was sent anywhere.</p>
<button id="copy" type="button" hidden>Copy command</button></li>
</ol>
<script>` + joinScript + `</script>
</body></html>
`

// joinCSP allows no network access at all, inline styles, and exactly
// joinScript.
var joinCSP = func() string {
	sum := sha256.Sum256([]byte(joinScript))
	return "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}()

func (s *Server) joinPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", joinCSP)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	io.WriteString(w, joinHTML)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.limiter.blocked(ip, s.opts.Now()) {
		writeError(w, http.StatusTooManyRequests, relayapi.CodeRateLimited, "too many failed authentication attempts from your address", "wait a minute, then check the relay token")
		return
	}
	tok, _ := bearer(r)
	sum := sha256.Sum256([]byte(tok))
	if subtle.ConstantTimeCompare(sum[:], s.tokenSum[:]) != 1 {
		s.limiter.fail(ip, s.opts.Now())
		writeError(w, http.StatusUnauthorized, relayapi.CodeUnauthorized, "the relay token is missing or wrong",
			"creating a project needs the relay's token in SEASONING_RELAY_TOKEN; ask the relay operator for it")
		return
	}
	var req relayapi.CreateProjectRequest
	if !s.decode(w, r, &req) {
		return
	}
	if !keys.ValidProjectID(req.ProjectID) {
		writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, fmt.Sprintf("%q is not a project ID", req.ProjectID), "")
		return
	}
	hash, err := keys.DecodeAuthHash(req.AuthHash)
	if err != nil {
		writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, err.Error(), "")
		return
	}
	created, err := s.db.createProject(r.Context(), req.ProjectID, hash, s.opts.Now(), s.opts.Limits.MaxProjects)
	switch {
	case errors.Is(err, errProjectExists):
		writeError(w, http.StatusConflict, relayapi.CodeConflict, "a project with this ID already exists with a different credential", "generate a new project")
		return
	case errors.Is(err, errLimit):
		writeError(w, http.StatusInsufficientStorage, relayapi.CodeLimitReached, fmt.Sprintf("this relay already holds its maximum of %d projects", s.opts.Limits.MaxProjects), "ask the relay operator to raise --max-projects or remove idle projects")
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, relayapi.CreateProjectResponse{ProjectID: req.ProjectID, Created: created})
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("p")
	if err := s.db.deleteProject(r.Context(), p); err != nil {
		s.internal(w, r, err)
		return
	}
	s.hub.notify(p)
	w.WriteHeader(http.StatusNoContent)
}

// maxRetire bounds how long a retired project may linger.
const maxRetire = 7 * 24 * time.Hour

func (s *Server) retireProject(w http.ResponseWriter, r *http.Request) {
	var req relayapi.RetireRequest
	if !s.decode(w, r, &req) {
		return
	}
	after := time.Duration(req.AfterSeconds) * time.Second
	if after < 0 || after > maxRetire {
		writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, fmt.Sprintf("after_seconds must be between 0 and %d", int64(maxRetire/time.Second)), "")
		return
	}
	at := s.opts.Now().Add(after)
	if err := s.db.retireProject(r.Context(), r.PathValue("p"), at); err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, relayapi.RetireResponse{RetireAt: envelope.FormatTime(at)})
}

func (s *Server) getRoster(w http.ResponseWriter, r *http.Request) {
	entries, err := s.db.roster(r.Context(), r.PathValue("p"))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, relayapi.RosterResponse{Entries: entries})
}

func (s *Server) postRoster(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("p")
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	_, compact, ok := s.compactEnvelope(w, raw, envelope.KindRoster, p, "", s.opts.Limits.MaxPresenceBytes)
	if !ok {
		return
	}
	seq, err := s.db.addRoster(r.Context(), p, compact, s.opts.Now(), s.opts.Limits.MaxRosterRecords)
	if errors.Is(err, errLimit) {
		writeError(w, http.StatusInsufficientStorage, relayapi.CodeLimitReached, fmt.Sprintf("the project's roster is full (%d records)", s.opts.Limits.MaxRosterRecords), "rotate the project to start a fresh roster")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.hub.notify(p)
	writeJSON(w, http.StatusCreated, relayapi.PostRosterResponse{Seq: seq})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	now := s.opts.Now()
	list, err := s.db.sessions(r.Context(), r.PathValue("p"), now, s.opts.TTLs.Away)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, relayapi.SessionsResponse{Now: envelope.FormatTime(now), Sessions: list})
}

func (s *Server) putSession(w http.ResponseWriter, r *http.Request) {
	p, sid := r.PathValue("p"), r.PathValue("s")
	var req relayapi.PutSessionRequest
	if !s.decode(w, r, &req) {
		return
	}
	var presence *string
	if len(req.Presence) > 0 && string(req.Presence) != "null" {
		_, compact, ok := s.compactEnvelope(w, req.Presence, envelope.KindPresence, p, sid, s.opts.Limits.MaxPresenceBytes)
		if !ok {
			return
		}
		presence = &compact
	}
	out, err := s.db.putSession(r.Context(), p, sid, presence, s.opts.Now(), s.opts.TTLs.Away, s.opts.Limits.MaxLiveSessions)
	if errors.Is(err, errLimit) {
		writeError(w, http.StatusInsufficientStorage, relayapi.CodeLimitReached, fmt.Sprintf("the project already has its maximum of %d live sessions", s.opts.Limits.MaxLiveSessions), "end unused sessions, or ask the relay operator to raise the limit")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.hub.notify(p)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("p")
	err := s.db.endSession(r.Context(), p, r.PathValue("s"), s.opts.Now(), s.opts.TTLs.Away)
	if errors.Is(err, errSessionGone) {
		writeError(w, http.StatusNotFound, relayapi.CodeNotFound, "session not found", "it may have been purged after 7 days without activity")
		return
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	s.hub.notify(p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("p")
	raw, ok := s.readBody(w, r)
	if !ok {
		return
	}
	e, compact, ok := s.compactEnvelope(w, raw, envelope.KindMessage, p, "", s.opts.Limits.MaxEnvelopeBytes)
	if !ok {
		return
	}
	resp, err := s.db.postMessage(r.Context(), p, e.FromSession, e.To, compact, s.opts.Now(), s.opts.TTLs.Away,
		queueLimits{messages: s.opts.Limits.MaxQueueMessages, bytes: s.opts.Limits.MaxQueueBytes})
	var unknown *unknownSessionsError
	switch {
	case errors.Is(err, errSessionGone):
		writeError(w, http.StatusNotFound, relayapi.CodeNotFound, "the sending session is not registered", "register it with PUT …/sessions/{s} before sending")
		return
	case errors.Is(err, errSessionEnded):
		writeError(w, http.StatusConflict, relayapi.CodeSessionEnded, "the sending session has ended", "register it again with PUT …/sessions/{s} before sending")
		return
	case errors.Is(err, errNoRecipients):
		writeError(w, http.StatusConflict, relayapi.CodeNoRecipients, "there are no other live sessions in this project", "nobody would receive a broadcast right now")
		return
	case errors.As(err, &unknown):
		writeError(w, http.StatusUnprocessableEntity, relayapi.CodeUnknownSessions, err.Error(), "refresh the peer list; those sessions were purged or never existed")
		return
	case err != nil:
		s.internal(w, r, err)
		return
	}
	queued := 0
	for _, rc := range resp.Recipients {
		if rc.Status == relayapi.StatusQueued {
			queued++
		}
	}
	if queued == 0 {
		writeError(w, http.StatusTooManyRequests, relayapi.CodeQueueFull, "every recipient's queue is full; the message was not stored",
			"the recipients are not reading their messages; try again later")
		return
	}
	s.hub.notify(p)
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("p")
	var req relayapi.AckRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.UpToSeq <= 0 {
		writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, "up_to_seq must be a positive sequence number", "")
		return
	}
	notify, err := s.db.ack(r.Context(), p, r.PathValue("s"), req.UpToSeq, s.opts.Now())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if notify {
		s.hub.notify(p)
	}
	w.WriteHeader(http.StatusNoContent)
}

// events streams a session's items as Server-Sent Events. The id of each
// event is its seq; clients resume with Last-Event-ID (or ?after=).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	p, sid := r.PathValue("p"), r.PathValue("s")
	ok, err := s.db.sessionExists(r.Context(), p, sid)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, relayapi.CodeNotFound, "session not found", "register it with PUT …/sessions/{s} before streaming")
		return
	}
	after := int64(0)
	cursor := r.Header.Get("Last-Event-ID")
	if cursor == "" {
		cursor = r.URL.Query().Get("after")
	}
	if cursor != "" {
		after, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || after < 0 {
			writeError(w, http.StatusBadRequest, relayapi.CodeBadRequest, "Last-Event-ID must be a non-negative integer", "")
			return
		}
	}

	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // a stream outlives the server's write timeout
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, ": seasoning\n\n"); err != nil {
		return
	}
	rc.Flush()

	keepAlive := time.NewTicker(s.opts.KeepAlive)
	defer keepAlive.Stop()
	for {
		wake := s.hub.wait(p) // taken before the query, so no notify is missed
		evs, resync, err := s.db.eventsAfter(r.Context(), p, sid, after, 500)
		if err != nil {
			if r.Context().Err() == nil {
				s.opts.Logger.Error("event query failed", "error", err.Error())
			}
			return
		}
		if resync {
			b, _ := json.Marshal(relayapi.ResyncEvent{Reason: "events you missed were pruned; refetch sessions and roster"})
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", relayapi.EventResync, b); err != nil {
				return
			}
		}
		for _, e := range evs {
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.seq, e.typ, e.data); err != nil {
				return
			}
			after = e.seq
		}
		if len(evs) > 0 || resync {
			rc.Flush()
		}
		if len(evs) == 500 {
			continue // more pending
		}
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			rc.Flush()
		}
	}
}

// --- hub and limiter ---------------------------------------------------------

// hub wakes a project's open streams when something new is stored.
type hub struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func newHub() *hub { return &hub{ch: map[string]chan struct{}{}} }

func (h *hub) wait(project string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.ch[project]
	if !ok {
		c = make(chan struct{})
		h.ch[project] = c
	}
	return c
}

func (h *hub) notify(project string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.ch[project]; ok {
		close(c)
		delete(h.ch, project)
	}
}

// limiter blocks an IP for the rest of a window after too many failures.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*bucket
}

type bucket struct {
	start time.Time
	n     int
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string]*bucket{}}
}

func (l *limiter) fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 {
		for k, b := range l.hits {
			if now.Sub(b.start) > l.window {
				delete(l.hits, k)
			}
		}
	}
	b, ok := l.hits[ip]
	if !ok || now.Sub(b.start) > l.window {
		b = &bucket{start: now}
		l.hits[ip] = b
	}
	b.n++
}

func (l *limiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.hits[ip]
	return ok && now.Sub(b.start) <= l.window && b.n >= l.max
}
