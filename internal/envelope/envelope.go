// Package envelope implements seasoning's wire format (SPEC §7, §8): the
// relay-visible outer envelope and the signed, encrypted inner payloads.
//
// Sealing signs the payload's canonical JSON (RFC 8785) with the sender's
// Ed25519 identity, then encrypts it with XChaCha20-Poly1305 under the
// project key. The outer routing fields are the AEAD's associated data, so a
// relay that rewrites them makes the envelope undecryptable.
package envelope

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/muratgozel/seasoning/internal/jcs"
	"github.com/muratgozel/seasoning/internal/keys"
)

// Version is the envelope format version this build writes and reads.
const Version = 1

// Kinds of sealed payloads.
const (
	KindMessage  = "message"
	KindPresence = "presence"
	KindRoster   = "roster"
)

// signerField names the payload field that carries the signer's public key.
var signerField = map[string]string{
	KindMessage:  "from_key",
	KindPresence: "key",
	KindRoster:   "key",
}

// Errors returned by Open, wrapped with detail.
var (
	ErrUndecryptable = errors.New("envelope does not decrypt with this project key")
	ErrBadSignature  = errors.New("envelope signature does not verify")
	ErrMalformed     = errors.New("envelope is malformed")
)

var b64 = base64.RawURLEncoding

// Recipients is the outer "to": every live session ("*") or a list of
// relay session IDs.
type Recipients struct {
	All      bool
	Sessions []string
}

// MarshalJSON renders "*" or an array of session IDs.
func (r Recipients) MarshalJSON() ([]byte, error) {
	if r.All {
		return []byte(`"*"`), nil
	}
	if r.Sessions == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(r.Sessions)
}

// UnmarshalJSON accepts "*" or an array of strings.
func (r *Recipients) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s != "*" {
			return fmt.Errorf(`"to" must be "*" or a list of session IDs, not %q`, s)
		}
		*r = Recipients{All: true}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New(`"to" must be "*" or a list of session IDs`)
	}
	*r = Recipients{Sessions: list}
	return nil
}

func (r Recipients) jsonValue() any {
	if r.All {
		return "*"
	}
	out := make([]any, len(r.Sessions))
	for i, s := range r.Sessions {
		out[i] = s
	}
	return out
}

// Envelope is the relay-visible outer layer.
type Envelope struct {
	V           int        `json:"v"`
	Kind        string     `json:"kind"`
	Project     string     `json:"project"`
	FromSession string     `json:"from_session"`
	To          Recipients `json:"to"`
	Nonce       string     `json:"nonce"`
	CT          string     `json:"ct"`
}

// ParseEnvelope decodes raw JSON (at most maxBytes) and validates its outer
// shape. Unknown fields are tolerated so newer senders stay readable.
func ParseEnvelope(raw []byte, maxBytes int) (*Envelope, error) {
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("envelope is %d bytes; the limit is %d", len(raw), maxBytes)
	}
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("envelope is not valid JSON: %w", err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// Validate checks the outer fields. It does not need the project key.
func (e *Envelope) Validate() error {
	if e.V != Version {
		if e.V > Version {
			return fmt.Errorf("envelope version %d is newer than this seasoning understands (%d); update seasoning", e.V, Version)
		}
		return fmt.Errorf("envelope version %d is not supported", e.V)
	}
	if _, ok := signerField[e.Kind]; !ok {
		return fmt.Errorf("envelope kind %q is not one of message, presence, roster", e.Kind)
	}
	if !keys.ValidProjectID(e.Project) {
		return fmt.Errorf("envelope project %q is not a project ID", e.Project)
	}
	if !keys.ValidSessionID(e.FromSession) {
		return fmt.Errorf("envelope from_session %q is not a session ID", e.FromSession)
	}
	if !e.To.All {
		if len(e.To.Sessions) == 0 || len(e.To.Sessions) > MaxRecipients {
			return fmt.Errorf(`envelope "to" lists %d sessions; it needs 1 to %d, or "*"`, len(e.To.Sessions), MaxRecipients)
		}
		seen := map[string]bool{}
		for _, s := range e.To.Sessions {
			if !keys.ValidSessionID(s) {
				return fmt.Errorf(`envelope "to" entry %q is not a session ID`, s)
			}
			if seen[s] {
				return fmt.Errorf(`envelope "to" lists %s twice`, s)
			}
			seen[s] = true
		}
	}
	if n, err := b64.DecodeString(e.Nonce); err != nil || len(n) != chacha20poly1305.NonceSizeX {
		return errors.New("envelope nonce must be 24 bytes of unpadded base64url")
	}
	if ct, err := b64.DecodeString(e.CT); err != nil || len(ct) < chacha20poly1305.Overhead {
		return errors.New("envelope ct must be unpadded base64url ciphertext")
	}
	return nil
}

func (e *Envelope) aad() ([]byte, error) {
	return jcs.Value(map[string]any{
		"v":            json.Number(fmt.Sprint(e.V)),
		"kind":         e.Kind,
		"project":      e.Project,
		"from_session": e.FromSession,
		"to":           e.To.jsonValue(),
	})
}

// sigInput separates signatures by kind, so a signed presence document can
// never pass as a signed message.
func sigInput(kind string, canonical []byte) []byte {
	return append([]byte("seasoning/v1/"+kind+"\x00"), canonical...)
}

// Seal signs payload with id and encrypts it under key. payload must marshal
// to a JSON object whose signer field (from_key for messages, key otherwise)
// holds id's public key; any "sig" field is replaced.
func Seal(key [32]byte, id keys.Identity, kind, project, fromSession string, to Recipients, payload any) (*Envelope, error) {
	field, ok := signerField[kind]
	if !ok {
		return nil, fmt.Errorf("seal: unknown kind %q", kind)
	}
	obj, err := toObject(payload)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	if got, _ := obj[field].(string); !keys.SamePublic(got, keys.FormatPublic(id.Public())) {
		return nil, fmt.Errorf("seal: payload %s does not match the signing identity", field)
	}
	delete(obj, "sig")
	canonical, err := jcs.Value(obj)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	obj["sig"] = b64.EncodeToString(ed25519.Sign(id.Private, sigInput(kind, canonical)))
	plaintext, err := jcs.Value(obj)
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}

	e := &Envelope{V: Version, Kind: kind, Project: project, FromSession: fromSession, To: to}
	aad, err := e.aad()
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("seal: nonce: %w", err)
	}
	e.Nonce = b64.EncodeToString(nonce)
	e.CT = b64.EncodeToString(aead.Seal(nil, nonce, plaintext, aad))
	if err := e.Validate(); err != nil {
		return nil, fmt.Errorf("seal: %w", err)
	}
	return e, nil
}

// Opened is a decrypted payload whose signature verified.
type Opened struct {
	Kind      string
	Plaintext []byte // canonical JSON, including "sig"
	Signer    string // "ed25519:<base64url>"
}

// Open decrypts e under key and verifies the payload's signature against the
// public key the payload names. Whether that key belongs to the claimed
// person is the caller's check (TOFU pins, SPEC §6.4).
func Open(key [32]byte, e *Envelope) (*Opened, error) {
	if err := e.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	nonce, _ := b64.DecodeString(e.Nonce)
	ct, _ := b64.DecodeString(e.CT)
	aad, err := e.aad()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	aead, err := chacha20poly1305.NewX(key[:])
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrUndecryptable
	}
	obj, err := toObject(json.RawMessage(plaintext))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	sigText, _ := obj["sig"].(string)
	sig, err := b64.DecodeString(sigText)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: missing or malformed sig", ErrMalformed)
	}
	signer, _ := obj[signerField[e.Kind]].(string)
	pub, err := keys.ParsePublic(signer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	delete(obj, "sig")
	canonical, err := jcs.Value(obj)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if !ed25519.Verify(pub, sigInput(e.Kind, canonical), sig) {
		return nil, ErrBadSignature
	}
	return &Opened{Kind: e.Kind, Plaintext: plaintext, Signer: signer}, nil
}

// toObject turns a struct, map or raw JSON into a decoded JSON object with
// json.Number numbers, ready for canonicalization.
func toObject(v any) (map[string]any, error) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}
	if obj == nil {
		return nil, errors.New("payload is not a JSON object")
	}
	return obj, nil
}
