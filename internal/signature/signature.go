// Package signature verifies webhook signatures, and shows its work.
//
// The showing is half the point. A tool that says "INVALID" and stops is
// barely better than the provider's own dashboard; what a developer needs is
// the exact bytes that were signed, so they can see that their canonical
// string is missing a newline or that they signed the parsed body instead of
// the raw one. See docs/learn/26-hmac.md.
package signature

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"time"
)

// Algorithm names the hash under the HMAC.
type Algorithm string

const (
	SHA256 Algorithm = "sha256"
	// SHA1 is here because Shopify and older GitHub webhooks use it, not
	// because it is a good idea. SHA-1 is broken for collision resistance,
	// which matters for signatures over attacker-chosen documents -- but
	// HMAC-SHA1 remains unbroken, because HMAC's security rests on the hash
	// being a decent PRF rather than on collision resistance. Knowing which
	// property a construction actually needs is the difference between
	// "SHA-1 is broken so this is broken" and the truth.
	SHA1 Algorithm = "sha1"
)

func (a Algorithm) new() func() hash.Hash {
	if a == SHA1 {
		return sha1.New
	}
	return sha256.New
}

// Encoding is how the tag is rendered in the header.
type Encoding string

const (
	Hex    Encoding = "hex"
	Base64 Encoding = "base64"
)

// Compute returns the HMAC of payload under secret, encoded for the wire.
func Compute(alg Algorithm, enc Encoding, secret, payload []byte) string {
	m := hmac.New(alg.new(), secret)
	// Write to a hash never returns an error; the interface says so
	// explicitly. Ignored rather than checked, because a checked error here
	// would be dead code that implies a failure mode that does not exist.
	m.Write(payload)
	sum := m.Sum(nil)

	if enc == Base64 {
		return base64.StdEncoding.EncodeToString(sum)
	}
	return hex.EncodeToString(sum)
}

// Equal compares two encoded tags in constant time.
//
// The comparison must not return early on the first differing byte. How long
// a comparison takes would otherwise leak how much of the prefix was correct,
// and an attacker measuring that recovers a tag one byte at a time -- turning
// an infeasible search into a few thousand requests.
//
// hmac.Equal rather than subtle.ConstantTimeCompare directly: it is the same
// thing with a clearer name at the call site, and it handles the
// length-mismatch case, which ConstantTimeCompare returns 1 for when both
// inputs are empty. (That surprise cost a test in unit 08.)
func Equal(a, b string) bool {
	// Decoding first would be better cryptographic hygiene -- comparing
	// bytes rather than their textual encoding -- but it breaks the thing
	// this package exists for. Providers differ in hex case, and a developer
	// whose tag differs only in case needs to SEE that rather than be told
	// "invalid". So the comparison is over the encoded form, and callers
	// normalise case deliberately where the provider's spec allows it.
	if len(a) != len(b) {
		// Length is not secret: it is fixed by the algorithm and visible in
		// the header. Rejecting here avoids comparing a short tag against a
		// long one, which no constant-time primitive defines behaviour for.
		return false
	}
	return hmac.Equal([]byte(a), []byte(b))
}

// Result is the outcome of verifying one request, built to be displayed.
type Result struct {
	// Provider is the scheme that was used, e.g. "stripe".
	Provider string
	// Valid is the answer. Everything else exists to explain it.
	Valid bool

	// Signed is the exact byte string the HMAC was computed over.
	//
	// The single most useful field in this struct. Nearly every real
	// signature bug is a canonical-string bug -- a missing newline, a
	// re-serialised body, the wrong separator -- and none of them is
	// diagnosable from a boolean.
	Signed string

	// Expected is the tag we computed; Provided is what the header carried.
	Expected string
	Provided string

	// Timestamp and Age describe the replay window, when the scheme has one.
	Timestamp time.Time
	Age       time.Duration

	// Problem explains a failure in a sentence, or is empty when Valid.
	Problem string

	// Hint is what to try next. Separate from Problem because "what is
	// wrong" and "what do I do" are different questions and a developer
	// staring at a red panel needs both.
	Hint string
}
