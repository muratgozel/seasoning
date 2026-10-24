// Package keys derives everything a project needs from its 32-byte secret
// (SPEC §7.1) and manages a person's Ed25519 identity key (SPEC §6.1).
//
// The derivation labels below are protocol constants: changing one changes
// every project ID and key, so existing projects become unreachable.
package keys

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SecretSize is the size of a project secret in bytes.
const SecretSize = 32

const (
	hkdfSalt        = "seasoning/v1"
	infoProjectID   = "project-id"
	infoRelayAuth   = "relay-auth"
	infoEnc         = "enc"
	infoSessionID   = "session-id"
	projectIDBytes  = 10 // 16 base32 characters
	sessionIDBytes  = 15 // 24 base32 characters
	identityHeader  = "seasoning-identity-v1"
	publicKeyPrefix = "ed25519:"
)

var (
	// b32 is lowercase RFC 4648 base32 without padding, for IDs that appear
	// in URLs and file names.
	b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
	b64 = base64.RawURLEncoding

	projectIDRe = regexp.MustCompile(`^p_[a-z2-7]{16}$`)
	sessionIDRe = regexp.MustCompile(`^s_[a-z2-7]{24}$`)
)

// Secret is a project secret. It has no String method on purpose, so it is
// never printed by accident; use Encode where printing is intended.
type Secret [SecretSize]byte

// NewSecret returns a random project secret.
func NewSecret() (Secret, error) {
	var s Secret
	if _, err := rand.Read(s[:]); err != nil {
		return s, fmt.Errorf("generate project secret: %w", err)
	}
	return s, nil
}

// Encode returns the secret as unpadded base64url (43 characters).
func (s Secret) Encode() string { return b64.EncodeToString(s[:]) }

// ParseSecret decodes Encode's output.
func ParseSecret(s string) (Secret, error) {
	var out Secret
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != SecretSize {
		return out, errors.New("project secret must be 32 bytes of unpadded base64url")
	}
	copy(out[:], b)
	return out, nil
}

// Project holds the values derived from a project secret.
type Project struct {
	// ID is the relay-visible project ID, safe to commit in a repo marker.
	ID string
	// AuthToken authenticates project API calls. The relay stores only
	// AuthHash(AuthToken).
	AuthToken [32]byte
	// EncKey is the XChaCha20-Poly1305 key for messages, presence and roster.
	EncKey     [32]byte
	sessionKey [32]byte
}

// Derive computes a project's keys. Each output comes from an independent
// HKDF-SHA256 expansion, so the relay (which sees AuthToken) learns nothing
// about EncKey.
func Derive(s Secret) Project {
	var p Project
	p.ID = "p_" + b32.EncodeToString(expand(s, infoProjectID, projectIDBytes))
	copy(p.AuthToken[:], expand(s, infoRelayAuth, 32))
	copy(p.EncKey[:], expand(s, infoEnc, 32))
	copy(p.sessionKey[:], expand(s, infoSessionID, 32))
	return p
}

func expand(s Secret, info string, n int) []byte {
	out, err := hkdf.Key(sha256.New, s[:], []byte(hkdfSalt), info, n)
	if err != nil {
		// Only possible for lengths beyond 255*32 bytes, which we never ask for.
		panic("keys: hkdf: " + err.Error())
	}
	return out
}

// Bearer is the Authorization bearer value for project API calls.
func (p Project) Bearer() string { return b64.EncodeToString(p.AuthToken[:]) }

// AuthHash is what the relay stores to check a project's bearer token.
func AuthHash(token []byte) [32]byte { return sha256.Sum256(token) }

// EncodeAuthHash formats an AuthHash for the create-project request.
func EncodeAuthHash(h [32]byte) string { return b64.EncodeToString(h[:]) }

// DecodeAuthHash parses EncodeAuthHash's output.
func DecodeAuthHash(s string) ([32]byte, error) {
	var out [32]byte
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != len(out) {
		return out, errors.New("auth_hash must be 32 bytes of unpadded base64url")
	}
	copy(out[:], b)
	return out, nil
}

// DecodeBearer parses a bearer token; ok is false for anything malformed.
func DecodeBearer(s string) ([]byte, bool) {
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, false
	}
	return b, true
}

// SessionID maps a Claude Code session ID (or any local session key) to the
// relay's opaque session ID: stable across --resume, unlinkable across
// projects, and it doesn't reveal the local ID to the relay.
func (p Project) SessionID(local string) string {
	m := hmac.New(sha256.New, p.sessionKey[:])
	m.Write([]byte(local))
	return "s_" + b32.EncodeToString(m.Sum(nil)[:sessionIDBytes])
}

// ValidProjectID reports whether s has the shape of a project ID.
func ValidProjectID(s string) bool { return projectIDRe.MatchString(s) }

// ValidSessionID reports whether s has the shape of a relay session ID.
func ValidSessionID(s string) bool { return sessionIDRe.MatchString(s) }

// Identity is a person's signing key.
type Identity struct {
	Private ed25519.PrivateKey
}

// GenerateIdentity returns a new random identity.
func GenerateIdentity() (Identity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Identity{}, fmt.Errorf("generate identity key: %w", err)
	}
	return Identity{Private: priv}, nil
}

// Public returns the identity's public key.
func (id Identity) Public() ed25519.PublicKey {
	return id.Private.Public().(ed25519.PublicKey)
}

// Encode serializes the identity for its key file. The result is secret.
func (id Identity) Encode() []byte {
	return []byte(identityHeader + " " + b64.EncodeToString(id.Private.Seed()) + "\n")
}

// DecodeIdentity parses Encode's output.
func DecodeIdentity(b []byte) (Identity, error) {
	head, seed, ok := strings.Cut(strings.TrimSpace(string(b)), " ")
	if !ok || head != identityHeader {
		return Identity{}, fmt.Errorf("identity key file does not start with %q", identityHeader)
	}
	raw, err := b64.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		return Identity{}, errors.New("identity key file is corrupt (bad seed)")
	}
	return Identity{Private: ed25519.NewKeyFromSeed(raw)}, nil
}

// FormatPublic renders a public key as "ed25519:<base64url>".
func FormatPublic(pub ed25519.PublicKey) string {
	return publicKeyPrefix + b64.EncodeToString(pub)
}

// ParsePublic parses FormatPublic's output.
func ParsePublic(s string) (ed25519.PublicKey, error) {
	rest, ok := strings.CutPrefix(s, publicKeyPrefix)
	if !ok {
		return nil, fmt.Errorf("public key %q does not start with %q", s, publicKeyPrefix)
	}
	b, err := b64.DecodeString(rest)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key %q is not 32 bytes of base64url", s)
	}
	return ed25519.PublicKey(b), nil
}

// Fingerprint is the human-comparable form of a public key, in the style of
// OpenSSH: "SHA256:" plus unpadded standard base64 of the key's SHA-256.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// SamePublic reports whether two formatted public keys are equal.
func SamePublic(a, b string) bool {
	pa, errA := ParsePublic(a)
	pb, errB := ParsePublic(b)
	return errA == nil && errB == nil && bytes.Equal(pa, pb)
}
