// Package relayclient talks to a seasoning relay's HTTP API (SPEC §14.2).
package relayclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/muratgozel/seasoning/internal/envelope"
	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/relayapi"
	"github.com/muratgozel/seasoning/internal/version"
)

// APIError is an error answer from the relay.
type APIError struct {
	Status int
	relayapi.ErrorDetail
}

func (e *APIError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("relay: %s (%s)", e.Message, e.Hint)
	}
	return "relay: " + e.Message
}

// IsCode reports whether err is an APIError with the given code.
func IsCode(err error, code string) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == code
}

// UnreachableError means the relay could not be reached at all.
type UnreachableError struct {
	Base string
	Err  error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("relay %s is unreachable (%v)", e.Base, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// Client is a relay client. Base is the normalized relay URL.
type Client struct {
	Base string
	// HTTP serves ordinary calls; Stream uses StreamHTTP, which must not
	// have a total timeout.
	HTTP       *http.Client
	StreamHTTP *http.Client
}

// New returns a client with sensible timeouts.
func New(base string) *Client {
	return &Client{
		Base:       base,
		HTTP:       &http.Client{Timeout: 15 * time.Second},
		StreamHTTP: &http.Client{},
	}
}

func (c *Client) request(ctx context.Context, method, path, bearer string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "seasoning/"+version.Version)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *Client) do(ctx context.Context, method, path, bearer string, in, out any) error {
	var body io.Reader
	switch v := in.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := c.request(ctx, method, path, bearer, body)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &UnreachableError{Base: c.Base, Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &UnreachableError{Base: c.Base, Err: err}
	}
	if resp.StatusCode >= 300 {
		return decodeError(resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("relay %s sent an unexpected answer (%v); is that URL a seasoning relay?", c.Base, err)
		}
	}
	return nil
}

func decodeError(status int, data []byte) error {
	var eb relayapi.ErrorBody
	if json.Unmarshal(data, &eb) == nil && eb.Error.Code != "" {
		return &APIError{Status: status, ErrorDetail: eb.Error}
	}
	return &APIError{Status: status, ErrorDetail: relayapi.ErrorDetail{
		Code:    relayapi.CodeInternal,
		Message: fmt.Sprintf("unexpected HTTP %d", status),
		Hint:    "check that the URL points at a seasoning relay and that its reverse proxy forwards /v1",
	}}
}

func projectPath(p keys.Project, rest string) string { return "/v1/projects/" + p.ID + rest }

// Health checks the relay is up.
func (c *Client) Health(ctx context.Context) (*relayapi.Health, error) {
	var h relayapi.Health
	return &h, c.do(ctx, http.MethodGet, "/healthz", "", nil, &h)
}

// CreateProject registers p, authorized by the relay token.
func (c *Client) CreateProject(ctx context.Context, relayToken string, p keys.Project) (bool, error) {
	var out relayapi.CreateProjectResponse
	err := c.do(ctx, http.MethodPost, "/v1/projects", relayToken, relayapi.CreateProjectRequest{
		ProjectID: p.ID,
		AuthHash:  keys.EncodeAuthHash(keys.AuthHash(p.AuthToken[:])),
	}, &out)
	return out.Created, err
}

// DeleteProject removes p and everything stored for it.
func (c *Client) DeleteProject(ctx context.Context, p keys.Project) error {
	return c.do(ctx, http.MethodDelete, projectPath(p, ""), p.Bearer(), nil, nil)
}

// RetireProject schedules p's deletion after a delay (rotation).
func (c *Client) RetireProject(ctx context.Context, p keys.Project, after time.Duration) (string, error) {
	var out relayapi.RetireResponse
	err := c.do(ctx, http.MethodPost, projectPath(p, "/retire"), p.Bearer(), relayapi.RetireRequest{AfterSeconds: int64(after / time.Second)}, &out)
	return out.RetireAt, err
}

// Roster lists p's roster records.
func (c *Client) Roster(ctx context.Context, p keys.Project) ([]relayapi.RosterEntry, error) {
	var out relayapi.RosterResponse
	err := c.do(ctx, http.MethodGet, projectPath(p, "/roster"), p.Bearer(), nil, &out)
	return out.Entries, err
}

// PostRoster appends a roster record.
func (c *Client) PostRoster(ctx context.Context, p keys.Project, e *envelope.Envelope) (int64, error) {
	var out relayapi.PostRosterResponse
	err := c.do(ctx, http.MethodPost, projectPath(p, "/roster"), p.Bearer(), e, &out)
	return out.Seq, err
}

// PutSession registers session or sends its heartbeat; presence nil keeps
// the stored presence.
func (c *Client) PutSession(ctx context.Context, p keys.Project, session string, presence *envelope.Envelope) (*relayapi.Session, error) {
	var req relayapi.PutSessionRequest
	if presence != nil {
		b, err := json.Marshal(presence)
		if err != nil {
			return nil, err
		}
		req.Presence = b
	}
	var out relayapi.Session
	err := c.do(ctx, http.MethodPut, projectPath(p, "/sessions/"+session), p.Bearer(), req, &out)
	return &out, err
}

// EndSession marks session ended.
func (c *Client) EndSession(ctx context.Context, p keys.Project, session string) error {
	return c.do(ctx, http.MethodDelete, projectPath(p, "/sessions/"+session), p.Bearer(), nil, nil)
}

// Sessions lists p's sessions.
func (c *Client) Sessions(ctx context.Context, p keys.Project) (*relayapi.SessionsResponse, error) {
	var out relayapi.SessionsResponse
	err := c.do(ctx, http.MethodGet, projectPath(p, "/sessions"), p.Bearer(), nil, &out)
	return &out, err
}

// PostMessage stores and fans out a message envelope.
func (c *Client) PostMessage(ctx context.Context, p keys.Project, e *envelope.Envelope) (*relayapi.PostMessageResponse, error) {
	var out relayapi.PostMessageResponse
	err := c.do(ctx, http.MethodPost, projectPath(p, "/messages"), p.Bearer(), e, &out)
	return &out, err
}

// Ack acknowledges session's stream items up to upTo.
func (c *Client) Ack(ctx context.Context, p keys.Project, session string, upTo int64) error {
	return c.do(ctx, http.MethodPost, projectPath(p, "/sessions/"+session+"/ack"), p.Bearer(), relayapi.AckRequest{UpToSeq: upTo}, nil)
}

// Event is one Server-Sent Event. ID is 0 for events without an id.
type Event struct {
	ID   int64
	Type string
	Data []byte
}

// Stream opens session's event stream after seq and calls fn for each event
// until the stream ends, ctx is cancelled, or fn returns an error. It does
// not reconnect; the caller decides when to.
func (c *Client) Stream(ctx context.Context, p keys.Project, session string, after int64, fn func(Event) error) error {
	req, err := c.request(ctx, http.MethodGet, projectPath(p, "/sessions/"+session+"/events"), p.Bearer(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if after > 0 {
		req.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &UnreachableError{Base: c.Base, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return decodeError(resp.StatusCode, data)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var ev Event
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 || ev.Type != "" {
				ev.Data = []byte(strings.Join(data, "\n"))
				if ev.Type == "" {
					ev.Type = "message"
				}
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev, data = Event{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				ev.ID = n
			}
		case "event":
			ev.Type = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &UnreachableError{Base: c.Base, Err: err}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &UnreachableError{Base: c.Base, Err: io.ErrUnexpectedEOF}
}
