// Package secret generates and verifies the two unguessable values hooklens
// relies on: the inbox slug and the owner token.
//
// Nothing here talks to a database or to HTTP, which is what lets the entropy
// and hashing decisions be tested in isolation.
package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"strings"
)

const (
	// SlugBytes is 16 bytes = 128 bits, the standard bar for an unguessable
	// identifier. Encoded as base32 that is 26 characters, which fits inside
	// the 32-character CHECK constraint on endpoints.slug and inside a DNS
	// label with room to spare.
	SlugBytes = 16

	// TokenBytes is 32 bytes = 256 bits. Larger than the slug because the token
	// is the one that actually guards data, and the cost of extra bytes in a
	// header nobody types is zero.
	TokenBytes = 32
)

// slugEncoding is base32 without padding.
//
// base32 rather than base64 for three reasons, all about where this string
// ends up. It is case-insensitive, and DNS is too -- a base64 slug would break
// the moment a resolver or a provider's config normalised its case. It has no
// "+" or "/", which are not legal in a hostname. And it produces only [a-z2-7]
// once lowercased, which is a subset of the [a-z0-9-] whitelist in
// server.validSlug, so a generated slug is always a routable one.
//
// The cost is density: base32 carries 5 bits per character against base64's 6,
// so 128 bits needs 26 characters instead of 22. Worth it.
var slugEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSlug returns a fresh 128-bit inbox name.
func NewSlug() string {
	var b [SlugBytes]byte
	// crypto/rand, never math/rand. math/rand is a deterministic function of
	// its seed: observe a few outputs and every future value is predictable,
	// which would make every inbox findable.
	//
	// Since Go 1.24 crypto/rand.Read cannot fail -- it panics rather than
	// returning an error -- so there is no error path to handle here. That is a
	// deliberate change by the Go team: a silently ignored RNG error is worse
	// than a crash.
	_, _ = rand.Read(b[:])
	return strings.ToLower(slugEncoding.EncodeToString(b[:]))
}

// NewToken returns a fresh 256-bit owner token.
//
// base64url rather than base32: this value only ever travels in an
// Authorization header, where density matters more than case-insensitivity.
func NewToken() string {
	var b [TokenBytes]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Hash returns the SHA-256 of a token, for storage.
//
// A single fast hash, deliberately NOT bcrypt/argon2/scrypt. Slow hashes exist
// to make brute force expensive, which matters for user-chosen passwords
// holding perhaps 20-30 bits of entropy. This token holds 256 bits from a CSPRNG
// -- there is nothing to brute force, so a slow hash would buy no security and
// add latency to every authenticated request.
//
// (bcrypt would also silently truncate input at 72 bytes, which is the kind of
// detail that turns a "more secure" choice into a weaker one.)
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Equal compares two hashes in constant time.
//
// The timing leak from a byte-wise == on a 256-bit hash is not realistically
// exploitable -- an attacker would have to guess the hash, not the token, and
// cannot work backwards from one to the other. This is here because it costs
// one function call, and because reaching for the constant-time comparison
// every time is a habit worth having rather than a judgement worth making under
// pressure.
func Equal(a, b []byte) bool {
	// Guard the empty case first. ConstantTimeCompare returns 1 for nil vs nil,
	// because two empty slices genuinely are equal -- but here that would mean
	// an absent stored hash authenticates an absent token. Hash always returns
	// 32 bytes so a caller cannot reach this legitimately, which is exactly why
	// it must be handled: the only way to get here is something already wrong.
	//
	// The length check leaks nothing useful. ConstantTimeCompare itself already
	// returns 0 on a length mismatch without comparing contents.
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}
