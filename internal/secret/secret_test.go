package secret

import (
	"strings"
	"testing"
)

// TestNewSlugShape pins the properties the rest of the system depends on: it
// must fit the CHECK constraint on endpoints.slug, and it must satisfy
// server.validSlug, or a generated inbox could be created and never routed to.
func TestNewSlugShape(t *testing.T) {
	const want = 26 // ceil(128 bits / 5 bits per base32 char)

	for range 200 {
		s := NewSlug()

		if len(s) != want {
			t.Fatalf("NewSlug() = %q, length %d, want %d", s, len(s), want)
		}
		if len(s) > 32 {
			t.Fatalf("slug %q exceeds the 32-char CHECK constraint", s)
		}
		if s[0] == '-' || s[len(s)-1] == '-' {
			t.Fatalf("slug %q starts or ends with a hyphen", s)
		}
		for i := range len(s) {
			c := s[i]
			ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
			if !ok {
				t.Fatalf("slug %q contains %q, outside the [a-z0-9-] whitelist", s, c)
			}
		}
	}
}

// TestNewSlugUnique is a smoke test, not a statistical one. With 128 bits a
// collision in 10,000 draws is so improbable that seeing one means the RNG is
// broken, not that we were unlucky.
func TestNewSlugUnique(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for range 10000 {
		s := NewSlug()
		if seen[s] {
			t.Fatalf("duplicate slug %q in 10k draws -- the RNG is not random", s)
		}
		seen[s] = true
	}
}

// TestNewSlugNotConstant catches the specific catastrophic bug of an
// uninitialised or all-zero buffer being encoded, which would produce a
// perfectly well-formed slug that is identical every time.
func TestNewSlugNotConstant(t *testing.T) {
	first := NewSlug()
	if strings.Count(first, "a") == len(first) {
		t.Fatalf("slug %q is all one character -- zero buffer?", first)
	}
	if NewSlug() == first {
		t.Fatal("two consecutive slugs are identical")
	}
}

func TestNewToken(t *testing.T) {
	// 32 bytes base64url without padding = ceil(32*8/6) = 43 chars.
	const want = 43

	seen := make(map[string]bool, 1000)
	for range 1000 {
		tok := NewToken()
		if len(tok) != want {
			t.Fatalf("NewToken() = %q, length %d, want %d", tok, len(tok), want)
		}
		if strings.ContainsAny(tok, "+/=") {
			t.Fatalf("token %q contains a character base64url should not emit", tok)
		}
		if seen[tok] {
			t.Fatalf("duplicate token in 1k draws")
		}
		seen[tok] = true
	}
}

func TestHash(t *testing.T) {
	tok := NewToken()

	h1, h2 := Hash(tok), Hash(tok)
	if len(h1) != 32 {
		t.Errorf("Hash returned %d bytes, want 32 (SHA-256)", len(h1))
	}
	if !Equal(h1, h2) {
		t.Error("Hash is not deterministic")
	}
	if Equal(h1, Hash(NewToken())) {
		t.Error("different tokens hashed to the same value")
	}

	// The property the whole scheme rests on: the stored value must not
	// contain the token. Anyone reading the table has a hash and nothing else.
	if strings.Contains(string(h1), tok) {
		t.Error("the hash contains the plaintext token")
	}
}

func TestEqual(t *testing.T) {
	a := Hash("one")
	if !Equal(a, Hash("one")) {
		t.Error("Equal said identical hashes differ")
	}
	if Equal(a, Hash("two")) {
		t.Error("Equal said different hashes match")
	}
	// Length mismatch must be false, not a panic -- a truncated value from the
	// database must not take the process down.
	if Equal(a, a[:16]) {
		t.Error("Equal matched a truncated hash")
	}
	if Equal(nil, nil) {
		t.Error("Equal matched two empty values; an absent token must never authenticate")
	}
}
