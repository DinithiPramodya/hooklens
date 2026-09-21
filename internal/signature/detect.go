package signature

import (
	"strings"
	"time"
)

// Provider names a supported scheme.
type Provider string

const (
	Stripe  Provider = "stripe"
	GitHub  Provider = "github"
	Shopify Provider = "shopify"
	Slack   Provider = "slack"
)

// Providers is every scheme this package knows, in detection order.
var Providers = []Provider{Stripe, GitHub, Shopify, Slack}

// Header is one captured header. Declared here rather than imported from
// capture or tunnel for the reason those two also have their own: this
// package is a leaf, and a signature verifier that drags in the storage
// layer is one that cannot be reused or tested cheaply.
type Header struct {
	Name  string
	Value string
}

// Detect identifies the scheme from the headers alone.
//
// By header name, not by body shape or user-agent. A header name is what the
// provider's own documentation guarantees; everything else about a webhook
// is content the sender chose.
func Detect(headers []Header) (Provider, bool) {
	has := func(name string) bool {
		for _, h := range headers {
			if strings.EqualFold(h.Name, name) {
				return true
			}
		}
		return false
	}

	switch {
	case has(StripeHeader):
		return Stripe, true
	case has(GitHubHeader), has(GitHubLegacy):
		return GitHub, true
	case has(ShopifyHeader):
		return Shopify, true
	case has(SlackSigHeader):
		return Slack, true
	}
	return "", false
}

// get returns the first value for a header name, case-insensitively.
//
// First, not joined: a signature header appearing twice is not a multi-value
// header like Accept, it is a malformed request or an attack. Taking the
// first and ignoring the rest is the same thing every HTTP stack does, and
// concatenating them would produce a string that matches nothing and
// explains nothing.
func get(headers []Header, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// Verify checks a captured request against a secret, choosing the scheme
// from its headers.
//
// The secret is a parameter and is never stored. It belongs to the user's
// integration, we have no use for it beyond this call, and holding other
// people's webhook secrets is a liability with no matching benefit --
// storing one would make this service worth attacking for a reason unrelated
// to what it does.
//
// receivedAt, not time.Now(): a stored capture must be re-verified against
// the time it ARRIVED, or every capture reads "expired" a few minutes later,
// which is true and useless.
func Verify(p Provider, headers []Header, body, secret []byte, receivedAt time.Time) Result {
	switch p {
	case Stripe:
		return VerifyStripe(get(headers, StripeHeader), body, secret, receivedAt, 0)

	case GitHub:
		h := get(headers, GitHubHeader)
		if h == "" {
			h = get(headers, GitHubLegacy)
		}
		return VerifyGitHub(h, body, secret)

	case Shopify:
		return VerifyShopify(get(headers, ShopifyHeader), body, secret)

	case Slack:
		return VerifySlack(get(headers, SlackSigHeader), get(headers, SlackTimeHeader),
			body, secret, receivedAt, 0)
	}

	return Result{
		Provider: string(p),
		Problem:  "unsupported provider " + string(p),
		Hint: "Supported: stripe, github, shopify, slack. " +
			"For anything else, compare the canonical string by hand.",
	}
}
