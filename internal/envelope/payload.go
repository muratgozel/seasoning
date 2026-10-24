package envelope

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/muratgozel/seasoning/internal/keys"
)

// Limits from SPEC §8.3 and §14.5.
const (
	MaxSubjectRunes  = 200
	MaxBodyBytes     = 32 << 10
	MaxRecipients    = 20
	MaxReferences    = 20
	MaxEnvelopeBytes = 64 << 10
	MaxPresenceBytes = 16 << 10
)

var (
	personRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	labelRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	messageIDRe = regexp.MustCompile(`^<[0-9A-HJKMNP-TV-Z]{26}@seasoning>$`)
)

// ValidPerson reports whether s is a person handle.
func ValidPerson(s string) bool { return personRe.MatchString(s) }

// ValidLabel reports whether s is a session label.
func ValidLabel(s string) bool { return labelRe.MatchString(s) }

// PersonRule and LabelRule describe the grammars for error messages.
const (
	PersonRule = "1-32 characters of a-z, 0-9 and '-', starting with a letter or digit"
	LabelRule  = "1-40 characters of a-z, 0-9 and '-', starting with a letter or digit"
)

// Address is a message destination: one session (person/label), all of a
// person's live sessions (person/*), or every live session (*).
type Address struct {
	Person string
	Label  string // "*" for all of Person's sessions; empty when All
	All    bool
}

// ParseAddress parses "person/label", "person/*" or "*".
func ParseAddress(s string) (Address, error) {
	if s == "*" {
		return Address{All: true}, nil
	}
	person, label, ok := strings.Cut(s, "/")
	if !ok {
		return Address{}, fmt.Errorf("address %q must be person/label, person/* or *", s)
	}
	if !ValidPerson(person) {
		return Address{}, fmt.Errorf("address %q: person must be %s", s, PersonRule)
	}
	if label != "*" && !ValidLabel(label) {
		return Address{}, fmt.Errorf("address %q: label must be %s, or *", s, LabelRule)
	}
	return Address{Person: person, Label: label}, nil
}

// String renders the address in its parseable form.
func (a Address) String() string {
	if a.All {
		return "*"
	}
	return a.Person + "/" + a.Label
}

// IsSession reports whether the address names one specific session; only
// those messages are wake-eligible without "urgent" (SPEC D5, D23).
func (a Address) IsSession() bool { return !a.All && a.Label != "*" }

// crockford is the ULID alphabet.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewMessageID returns an RFC 5322 msg-id whose left part is a ULID (48-bit
// millisecond time plus 80 random bits). The right part is constant so that
// it never names a host.
func NewMessageID(now time.Time) (string, error) {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	if _, err := rand.Read(b[6:]); err != nil {
		return "", fmt.Errorf("message id: %w", err)
	}
	// 128 bits as 26 base32 digits, most significant first; the first digit
	// carries the top 3 bits.
	var out [26]byte
	var acc uint64
	bits := 2 // pad to 130 bits so the digits align at the end
	idx := 0
	for _, by := range b {
		acc = acc<<8 | uint64(by)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = crockford[(acc>>uint(bits))&31]
			idx++
		}
	}
	return "<" + string(out[:]) + "@seasoning>", nil
}

// ValidMessageID reports whether s is a seasoning msg-id.
func ValidMessageID(s string) bool { return messageIDRe.MatchString(s) }

// FormatTime renders t as RFC 3339 UTC with milliseconds, the protocol's
// only timestamp format (SPEC §18).
func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// ParseTime parses FormatTime's output; only UTC ("Z") is accepted.
func ParseTime(s string) (time.Time, error) {
	if !strings.HasSuffix(s, "Z") {
		return time.Time{}, fmt.Errorf("time %q must be RFC 3339 in UTC (ending in Z)", s)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("time %q is not RFC 3339", s)
	}
	return t, nil
}

// Message is the inner payload of a "message" envelope (SPEC §8.2).
type Message struct {
	MessageID     string   `json:"message_id"`
	Project       string   `json:"project"`
	From          string   `json:"from"`
	FromSession   string   `json:"from_session"`
	FromKey       string   `json:"from_key"`
	To            []string `json:"to"`
	Date          string   `json:"date"`
	Subject       string   `json:"subject"`
	InReplyTo     *string  `json:"in_reply_to"`
	References    []string `json:"references"`
	ReplyExpected bool     `json:"reply_expected"`
	Urgent        bool     `json:"urgent"`
	Body          string   `json:"body"`
}

// Validate checks every field against SPEC §8.3. Errors name the field and
// the rule so an agent can fix its input.
func (m *Message) Validate() error {
	if !ValidMessageID(m.MessageID) {
		return fmt.Errorf("message_id %q is not a seasoning msg-id", m.MessageID)
	}
	if !keys.ValidProjectID(m.Project) {
		return fmt.Errorf("project %q is not a project ID", m.Project)
	}
	from, err := ParseAddress(m.From)
	if err != nil || !from.IsSession() {
		return fmt.Errorf("from %q must be a person/label handle", m.From)
	}
	if !keys.ValidSessionID(m.FromSession) {
		return fmt.Errorf("from_session %q is not a session ID", m.FromSession)
	}
	if _, err := keys.ParsePublic(m.FromKey); err != nil {
		return fmt.Errorf("from_key: %w", err)
	}
	if len(m.To) == 0 || len(m.To) > MaxRecipients {
		return fmt.Errorf("to lists %d addresses; it needs 1 to %d", len(m.To), MaxRecipients)
	}
	seen := map[string]bool{}
	for _, a := range m.To {
		if _, err := ParseAddress(a); err != nil {
			return fmt.Errorf("to: %w", err)
		}
		if seen[a] {
			return fmt.Errorf("to lists %q twice", a)
		}
		seen[a] = true
	}
	if _, err := ParseTime(m.Date); err != nil {
		return fmt.Errorf("date: %w", err)
	}
	if err := checkLine("subject", m.Subject, MaxSubjectRunes); err != nil {
		return err
	}
	if m.InReplyTo != nil && !ValidMessageID(*m.InReplyTo) {
		return fmt.Errorf("in_reply_to %q is not a seasoning msg-id", *m.InReplyTo)
	}
	if len(m.References) > MaxReferences {
		return fmt.Errorf("references has %d entries; the limit is %d", len(m.References), MaxReferences)
	}
	for _, r := range m.References {
		if !ValidMessageID(r) {
			return fmt.Errorf("references entry %q is not a seasoning msg-id", r)
		}
	}
	return checkBody(m.Body)
}

// Thread returns the message's thread ID: its root's message ID.
func (m *Message) Thread() string {
	if len(m.References) > 0 {
		return m.References[0]
	}
	return m.MessageID
}

// DecodeMessage parses and validates an opened message, and checks that it
// agrees with its envelope and its signer.
func DecodeMessage(o *Opened, e *Envelope) (*Message, error) {
	if o.Kind != KindMessage || e.Kind != KindMessage {
		return nil, fmt.Errorf("%w: not a message", ErrMalformed)
	}
	var m Message
	if err := json.Unmarshal(o.Plaintext, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if m.Project != e.Project || m.FromSession != e.FromSession {
		return nil, fmt.Errorf("%w: inner project or from_session differs from the envelope", ErrMalformed)
	}
	if !keys.SamePublic(m.FromKey, o.Signer) {
		return nil, fmt.Errorf("%w: from_key is not the signer", ErrMalformed)
	}
	return &m, nil
}

// RosterRecord announces a person's identity key to a project (SPEC §6.3).
type RosterRecord struct {
	Handle   string `json:"handle"`
	Key      string `json:"key"`
	JoinedAt string `json:"joined_at"`
}

// Validate checks the record's fields.
func (r *RosterRecord) Validate() error {
	if !ValidPerson(r.Handle) {
		return fmt.Errorf("handle %q must be %s", r.Handle, PersonRule)
	}
	if _, err := keys.ParsePublic(r.Key); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	if _, err := ParseTime(r.JoinedAt); err != nil {
		return fmt.Errorf("joined_at: %w", err)
	}
	return nil
}

// DecodeRoster parses and validates an opened roster record.
func DecodeRoster(o *Opened) (*RosterRecord, error) {
	if o.Kind != KindRoster {
		return nil, fmt.Errorf("%w: not a roster record", ErrMalformed)
	}
	var r RosterRecord
	if err := json.Unmarshal(o.Plaintext, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return &r, nil
}

// isControl covers C0, DEL, C1 and the Unicode line and paragraph
// separators, none of which belong in a one-line field.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x2028 || r == 0x2029
}

func checkLine(field, s string, maxRunes int) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	n := utf8.RuneCountInString(s)
	if n == 0 || strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s is empty", field)
	}
	if n > maxRunes {
		return fmt.Errorf("%s is %d characters; the limit is %d. Shorten it and put details in the body", field, n, maxRunes)
	}
	for _, r := range s {
		if isControl(r) {
			return fmt.Errorf("%s contains a line break or control character (U+%04X); it must be a single line", field, r)
		}
	}
	return nil
}

func checkBody(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("body is not valid UTF-8")
	}
	if strings.TrimSpace(s) == "" {
		return errors.New("body is empty")
	}
	if len(s) > MaxBodyBytes {
		return fmt.Errorf("body is %d bytes; the limit is %d. Point to files or commits instead of pasting them", len(s), MaxBodyBytes)
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && isControl(r) && r != 0x2028 && r != 0x2029 {
			return fmt.Errorf("body contains control character U+%04X; only line feeds and tabs are allowed", r)
		}
	}
	return nil
}
