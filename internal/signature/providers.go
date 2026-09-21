package signature

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The other three schemes. Put together they are a short tour of how much
// four teams solving one problem can differ -- which is the argument for a
// tool that knows all of them, and the reason the canonical string is the
// field this package works hardest to expose.

const (
	GitHubHeader    = "X-Hub-Signature-256"
	GitHubLegacy    = "X-Hub-Signature"
	ShopifyHeader   = "X-Shopify-Hmac-Sha256"
	SlackSigHeader  = "X-Slack-Signature"
	SlackTimeHeader = "X-Slack-Request-Timestamp"
	slackVersion    = "v0"
	slackTolerance  = 5 * time.Minute
)

// VerifyGitHub checks X-Hub-Signature-256.
//
//	X-Hub-Signature-256: sha256=<hex>
//
// The signed string is the raw body and nothing else -- no timestamp, which
// means GitHub's scheme has NO REPLAY PROTECTION of its own. A captured
// delivery can be resent verbatim forever and will verify. GitHub's answer is
// the `X-GitHub-Delivery` id, which a receiver is expected to deduplicate on;
// that shifts the work to the application, and most applications do not do
// it. Worth knowing, and worth surfacing.
func VerifyGitHub(header string, body, secret []byte) Result {
	res := Result{Provider: "github"}

	scheme, tag, ok := strings.Cut(strings.TrimSpace(header), "=")
	if !ok || scheme == "" {
		res.Problem = fmt.Sprintf("expected `sha256=<hex>`, got %q", header)
		res.Hint = "GitHub sends X-Hub-Signature-256. Is this really a GitHub webhook?"
		return res
	}

	alg := SHA256
	if scheme == "sha1" {
		// The legacy header. Accepted so an older integration can still be
		// debugged, and flagged, because nobody should be adding one.
		alg = SHA1
		res.Problem = "using the deprecated sha1 scheme"
		res.Hint = "Switch to X-Hub-Signature-256. HMAC-SHA1 is not broken, but SHA-1 is " +
			"retired everywhere else and there is no reason to keep it here."
	} else if scheme != "sha256" {
		res.Problem = fmt.Sprintf("unknown scheme %q", scheme)
		return res
	}

	// GitHub signs the body and nothing else.
	res.Signed = string(body)
	res.Provided = tag
	res.Expected = Compute(alg, Hex, secret, body)

	// Lowercased before comparing because GitHub documents lowercase hex and
	// some clients send upper. Normalising is safe here for a specific
	// reason: the encoding is not the secret, and a case difference is a
	// formatting artefact rather than a failed authentication. Doing this
	// AFTER computing means the panel still shows what actually arrived.
	res.Valid = Equal(strings.ToLower(res.Expected), strings.ToLower(tag))
	if !res.Valid {
		res.Problem = "signature did not match"
		res.Hint = "Check the webhook secret, and make sure you are hashing the RAW body. " +
			"Note GitHub signs the body ALONE -- no timestamp, no method, no path."
	}
	return res
}

// VerifyShopify checks X-Shopify-Hmac-Sha256.
//
// Like GitHub, the signed string is the raw body. Unlike everyone else, the
// tag is BASE64 rather than hex -- which is the single most common reason a
// hand-written Shopify verifier fails, because the code was copied from a
// Stripe or GitHub example and the hex/base64 difference is invisible until
// you look at the header.
func VerifyShopify(header string, body, secret []byte) Result {
	res := Result{Provider: "shopify"}
	tag := strings.TrimSpace(header)
	if tag == "" {
		res.Problem = "no X-Shopify-Hmac-Sha256 header"
		return res
	}

	res.Signed = string(body)
	res.Provided = tag
	res.Expected = Compute(SHA256, Base64, secret, body)
	res.Valid = Equal(res.Expected, tag)

	if !res.Valid {
		res.Problem = "signature did not match"
		// The hex/base64 trap, detected rather than guessed at.
		if hexTag := Compute(SHA256, Hex, secret, body); Equal(strings.ToLower(hexTag), strings.ToLower(tag)) {
			res.Problem = "the signature is correct but HEX-encoded; Shopify uses base64"
			res.Hint = "Your code is probably copied from a Stripe or GitHub example. " +
				"Use base64 of the raw HMAC digest, not hex."
		} else {
			res.Hint = "Check the shared secret, and note Shopify encodes the tag as BASE64 " +
				"rather than hex."
		}
	}
	return res
}

// VerifySlack checks X-Slack-Signature against X-Slack-Request-Timestamp.
//
//	basestring = "v0:" + timestamp + ":" + body
//	X-Slack-Signature: v0=<hex>
//
// The most explicit of the four: the version is inside the signed string as
// well as on the tag, so a future v1 cannot be confused with a v0 even if an
// attacker rewrites the prefix. That is a small piece of good design worth
// noticing -- Stripe's `t.body` has no version in it at all.
func VerifySlack(sigHeader, tsHeader string, body, secret []byte, now time.Time, tolerance time.Duration) Result {
	res := Result{Provider: "slack"}
	if tolerance <= 0 {
		tolerance = slackTolerance
	}

	tsRaw := strings.TrimSpace(tsHeader)
	if tsRaw == "" {
		res.Problem = "no X-Slack-Request-Timestamp header"
		res.Hint = "Slack's signature covers the timestamp, so both headers are required."
		return res
	}
	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		res.Problem = fmt.Sprintf("timestamp %q is not a unix time", tsRaw)
		return res
	}
	res.Timestamp = time.Unix(ts, 0).UTC()
	res.Age = now.Sub(res.Timestamp)

	sig := strings.TrimSpace(sigHeader)
	version, tag, ok := strings.Cut(sig, "=")
	if !ok || version != slackVersion {
		res.Problem = fmt.Sprintf("expected `v0=<hex>`, got %q", sigHeader)
		return res
	}
	res.Provided = tag

	res.Signed = slackVersion + ":" + tsRaw + ":" + string(body)
	res.Expected = Compute(SHA256, Hex, secret, []byte(res.Signed))
	res.Valid = Equal(strings.ToLower(res.Expected), strings.ToLower(tag))

	if !res.Valid {
		res.Problem = "signature did not match"
		res.Hint = "The base string is `v0:<timestamp>:<raw body>` -- note the literal " +
			"`v0:` prefix and the colons. Missing either is the usual cause."
		return res
	}

	if res.Age > tolerance || res.Age < -tolerance {
		res.Valid = false
		res.Problem = fmt.Sprintf("signature is valid but the request is %s old (tolerance %s)",
			absDuration(res.Age).Round(time.Second), tolerance)
		res.Hint = "Slack recommends rejecting anything older than five minutes to limit replay."
	}
	return res
}
