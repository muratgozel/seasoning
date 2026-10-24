package keys

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func fixedSecret() Secret {
	var s Secret
	for i := range s {
		s[i] = byte(i)
	}
	return s
}

// The derivation is a wire contract: these vectors must never change. They
// were cross-checked against an independent Python HKDF/HMAC implementation.
func TestDeriveVectors(t *testing.T) {
	p := Derive(fixedSecret())
	if p.ID != "p_id77hmkyahvkkjek" {
		t.Errorf("ID = %s", p.ID)
	}
	if got := hex.EncodeToString(p.AuthToken[:]); got != "58dee4f4d835fa74cb1d0a45d64c1d80d60792c2bdecfc29f5cabdae65da9b42" {
		t.Errorf("AuthToken = %s", got)
	}
	if got := hex.EncodeToString(p.EncKey[:]); got != "33bca02d5d9b10cf1d3c74b358a32bc79dbf97fd1ad49a25ff007c95acb95a0b" {
		t.Errorf("EncKey = %s", got)
	}
	if got := p.SessionID("b3c89be3-3f7c-467d-b383-ab8671b2dd2f"); got != "s_bmyjyrbv4ggrvv2ncg2oalyc" {
		t.Errorf("SessionID = %s", got)
	}
}

func TestDeriveShapes(t *testing.T) {
	s, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	p := Derive(s)
	if !ValidProjectID(p.ID) {
		t.Fatalf("project ID %q", p.ID)
	}
	sid := p.SessionID("x")
	if !ValidSessionID(sid) || sid != p.SessionID("x") || sid == p.SessionID("y") {
		t.Fatalf("session IDs %q", sid)
	}
	if other := Derive(fixedSecret()); other.SessionID("x") == sid {
		t.Fatal("session IDs are linkable across projects")
	}
	if bytes.Equal(p.AuthToken[:], p.EncKey[:]) {
		t.Fatal("auth token equals encryption key")
	}
	back, err := ParseSecret(s.Encode())
	if err != nil || back != s {
		t.Fatalf("secret round trip: %v", err)
	}
	if _, err := ParseSecret(s.Encode()[:40]); err == nil {
		t.Fatal("short secret accepted")
	}
	tok, ok := DecodeBearer(p.Bearer())
	if !ok || AuthHash(tok) != AuthHash(p.AuthToken[:]) {
		t.Fatal("bearer round trip")
	}
	h, err := DecodeAuthHash(EncodeAuthHash(AuthHash(tok)))
	if err != nil || h != AuthHash(tok) {
		t.Fatalf("auth hash round trip: %v", err)
	}
}

func TestIdentity(t *testing.T) {
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeIdentity(id.Encode())
	if err != nil || !back.Private.Equal(id.Private) {
		t.Fatalf("identity round trip: %v", err)
	}
	pub := FormatPublic(id.Public())
	parsed, err := ParsePublic(pub)
	if err != nil || !parsed.Equal(id.Public()) {
		t.Fatalf("public round trip: %v", err)
	}
	if !SamePublic(pub, pub) || SamePublic(pub, "ed25519:AAAA") {
		t.Fatal("SamePublic")
	}
	if fp := Fingerprint(id.Public()); !strings.HasPrefix(fp, "SHA256:") || len(fp) != 50 {
		t.Fatalf("fingerprint %q", fp)
	}
	for _, bad := range []string{"", "seasoning-identity-v2 abc", "seasoning-identity-v1 !!!", "seasoning-identity-v1 AAAA"} {
		if _, err := DecodeIdentity([]byte(bad)); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	for _, bad := range []string{"ssh-ed25519 AAAA", "ed25519:", "ed25519:AAAA"} {
		if _, err := ParsePublic(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
