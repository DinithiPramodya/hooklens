package signature

import (
	"strings"
	"testing"
	"time"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name    string
		headers []Header
		want    Provider
		found   bool
	}{
		{"stripe", []Header{{"Stripe-Signature", "t=1,v1=x"}}, Stripe, true},
		{"github", []Header{{"X-Hub-Signature-256", "sha256=x"}}, GitHub, true},
		{"github legacy", []Header{{"X-Hub-Signature", "sha1=x"}}, GitHub, true},
		{"shopify", []Header{{"X-Shopify-Hmac-Sha256", "x"}}, Shopify, true},
		{"slack", []Header{{"X-Slack-Signature", "v0=x"}}, Slack, true},
		// Header names are case-insensitive on the wire, and providers are
		// not consistent about casing.
		{"lowercase", []Header{{"stripe-signature", "t=1,v1=x"}}, Stripe, true},
		{"UPPERCASE", []Header{{"X-SHOPIFY-HMAC-SHA256", "x"}}, Shopify, true},
		{"none", []Header{{"Content-Type", "application/json"}}, "", false},
		{"empty", nil, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Detect(tc.headers)
			if ok != tc.found || got != tc.want {
				t.Errorf("Detect = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.found)
			}
		})
	}
}

// TestGetTakesTheFirstValue: a signature header appearing twice is not a
// multi-value header, it is malformed or an attack. Concatenating would
// produce a string that matches nothing and explains nothing.
func TestGetTakesTheFirstValue(t *testing.T) {
	h := []Header{
		{"X-Shopify-Hmac-Sha256", "first"},
		{"X-Shopify-Hmac-Sha256", "second"},
	}
	if got := get(h, ShopifyHeader); got != "first" {
		t.Errorf("get = %q, want the first value", got)
	}
}

func TestVerifyRoutesToEveryProvider(t *testing.T) {
	secret := []byte("s")
	body := []byte(`{"a":1}`)
	now := time.Now()
	slackSig, slackTS := slackHeaders(t, secret, body, now)

	cases := []struct {
		provider Provider
		headers  []Header
	}{
		{Stripe, []Header{{StripeHeader, stripeHeader(t, secret, body, now)}}},
		{GitHub, []Header{{GitHubHeader, "sha256=" + Compute(SHA256, Hex, secret, body)}}},
		{Shopify, []Header{{ShopifyHeader, Compute(SHA256, Base64, secret, body)}}},
		{Slack, []Header{{SlackSigHeader, slackSig}, {SlackTimeHeader, slackTS}}},
	}

	for _, tc := range cases {
		t.Run(string(tc.provider), func(t *testing.T) {
			// Detection and verification must agree, or the UI offers a
			// provider that then fails to verify its own header.
			detected, ok := Detect(tc.headers)
			if !ok || detected != tc.provider {
				t.Fatalf("Detect = (%q, %v), want %q", detected, ok, tc.provider)
			}
			res := Verify(tc.provider, tc.headers, body, secret, now)
			if !res.Valid {
				t.Errorf("Verify failed: %s", res.Problem)
			}
		})
	}
}

func TestVerifyUnsupportedProvider(t *testing.T) {
	res := Verify("paypal", nil, nil, nil, time.Now())
	if res.Valid {
		t.Fatal("an unsupported provider verified")
	}
	if !strings.Contains(res.Hint, "stripe") {
		t.Errorf("hint = %q, want it to list what IS supported", res.Hint)
	}
}

// TestVerifyUsesReceivedAt: the whole reason receivedAt is a parameter. A
// capture from last week must still verify, or the panel is useless for
// anything but the last five minutes.
func TestVerifyUsesReceivedAt(t *testing.T) {
	secret := []byte("s")
	body := []byte(`{"a":1}`)
	lastWeek := time.Now().Add(-7 * 24 * time.Hour)

	headers := []Header{{StripeHeader, stripeHeader(t, secret, body, lastWeek)}}
	if res := Verify(Stripe, headers, body, secret, lastWeek); !res.Valid {
		t.Errorf("a week-old capture would not re-verify: %s", res.Problem)
	}
}
