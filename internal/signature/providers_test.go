package signature

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- GitHub ----

func TestVerifyGitHub(t *testing.T) {
	secret := []byte("It's a Secret to Everybody")
	body := []byte("Hello, World!")

	header := "sha256=" + Compute(SHA256, Hex, secret, body)
	res := VerifyGitHub(header, body, secret)

	if !res.Valid {
		t.Fatalf("valid signature rejected: %s", res.Problem)
	}
	// GitHub signs the body ALONE -- no timestamp, no method, no path.
	if res.Signed != string(body) {
		t.Errorf("Signed = %q, want the bare body", res.Signed)
	}
}

// TestVerifyGitHubAcceptsUppercaseHex: some clients send uppercase. A case
// difference is a formatting artefact, not a failed authentication.
func TestVerifyGitHubAcceptsUppercaseHex(t *testing.T) {
	secret, body := []byte("s"), []byte("b")
	header := "sha256=" + strings.ToUpper(Compute(SHA256, Hex, secret, body))

	res := VerifyGitHub(header, body, secret)
	if !res.Valid {
		t.Error("an uppercase hex tag was rejected")
	}
	// But the panel must show what ARRIVED, not a normalised copy.
	if res.Provided != strings.ToUpper(res.Expected) {
		t.Errorf("Provided = %q; the panel should show the header verbatim", res.Provided)
	}
}

func TestVerifyGitHubFlagsLegacySHA1(t *testing.T) {
	secret, body := []byte("s"), []byte("b")
	header := "sha1=" + Compute(SHA1, Hex, secret, body)

	res := VerifyGitHub(header, body, secret)
	if !res.Valid {
		t.Fatalf("a valid sha1 signature was rejected: %s", res.Problem)
	}
	// Accepted so an old integration can be debugged, and flagged, because
	// nobody should be adding one.
	if !strings.Contains(res.Problem, "deprecated") {
		t.Errorf("problem = %q, want it to flag the deprecated scheme", res.Problem)
	}
}

func TestVerifyGitHubRejects(t *testing.T) {
	secret, body := []byte("s"), []byte("b")
	for _, tc := range []struct{ name, header, wantIn string }{
		{"empty", "", "expected `sha256="},
		{"no scheme", "abcdef", "expected `sha256="},
		{"unknown scheme", "md5=abcdef", "unknown scheme"},
		{"wrong tag", "sha256=" + strings.Repeat("0", 64), "did not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := VerifyGitHub(tc.header, body, secret)
			if res.Valid {
				t.Fatal("accepted")
			}
			if !strings.Contains(res.Problem, tc.wantIn) {
				t.Errorf("problem = %q, want %q in it", res.Problem, tc.wantIn)
			}
		})
	}
}

// ---- Shopify ----

func TestVerifyShopify(t *testing.T) {
	secret := []byte("hush")
	body := []byte(`{"id":820982911946154508}`)

	res := VerifyShopify(Compute(SHA256, Base64, secret, body), body, secret)
	if !res.Valid {
		t.Fatalf("valid signature rejected: %s", res.Problem)
	}
}

// TestVerifyShopifyDiagnosesHexEncoding is the payoff for this package
// existing. Copying a Stripe or GitHub example gives you a correct HMAC in
// the wrong encoding, and "invalid" would send the developer to check a
// secret that is perfectly fine.
func TestVerifyShopifyDiagnosesHexEncoding(t *testing.T) {
	secret := []byte("hush")
	body := []byte(`{"id":1}`)

	// The right digest, hex-encoded instead of base64.
	res := VerifyShopify(Compute(SHA256, Hex, secret, body), body, secret)

	if res.Valid {
		t.Fatal("a hex tag was accepted for a base64 scheme")
	}
	if !strings.Contains(res.Problem, "HEX") {
		t.Errorf("problem = %q; it should name the encoding, not blame the secret", res.Problem)
	}
	if !strings.Contains(res.Hint, "base64") {
		t.Errorf("hint = %q, want it to say base64", res.Hint)
	}
}

func TestVerifyShopifyWrongSecret(t *testing.T) {
	body := []byte(`{"id":1}`)
	res := VerifyShopify(Compute(SHA256, Base64, []byte("right"), body), body, []byte("wrong"))
	if res.Valid {
		t.Fatal("accepted a tag made with a different secret")
	}
	if strings.Contains(res.Problem, "HEX") {
		t.Error("a wrong secret was misdiagnosed as an encoding problem")
	}
}

// ---- Slack ----

func slackHeaders(t *testing.T, secret, body []byte, ts time.Time) (sig, tsHeader string) {
	t.Helper()
	tsHeader = strconv.FormatInt(ts.Unix(), 10)
	base := "v0:" + tsHeader + ":" + string(body)
	return "v0=" + Compute(SHA256, Hex, secret, []byte(base)), tsHeader
}

func TestVerifySlack(t *testing.T) {
	secret := []byte("8f742231b10e8888abcd99yyyzzz85a5")
	body := []byte("token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J")
	now := time.Now()

	sig, ts := slackHeaders(t, secret, body, now)
	res := VerifySlack(sig, ts, body, secret, now, 0)

	if !res.Valid {
		t.Fatalf("valid signature rejected: %s", res.Problem)
	}
	// The version is INSIDE the signed string as well as on the tag, which
	// is what stops a future v1 being confused with a v0.
	if !strings.HasPrefix(res.Signed, "v0:") {
		t.Errorf("Signed = %q, want the v0: prefix inside the base string", res.Signed)
	}
	if strings.Count(res.Signed, ":") < 2 {
		t.Errorf("Signed = %q, want `v0:<ts>:<body>`", res.Signed)
	}
}

func TestVerifySlackReplayWindow(t *testing.T) {
	secret := []byte("s")
	body := []byte("a=b")
	old := time.Now().Add(-30 * time.Minute)

	sig, ts := slackHeaders(t, secret, body, old)
	res := VerifySlack(sig, ts, body, secret, time.Now(), 5*time.Minute)

	if res.Valid {
		t.Fatal("a 30-minute-old request was accepted")
	}
	if !strings.Contains(res.Problem, "valid") {
		t.Errorf("problem = %q; the signature itself was fine and should say so", res.Problem)
	}
}

func TestVerifySlackRejects(t *testing.T) {
	secret, body := []byte("s"), []byte("a=b")
	now := time.Now()
	goodSig, goodTS := slackHeaders(t, secret, body, now)

	for _, tc := range []struct{ name, sig, ts, wantIn string }{
		{"no timestamp", goodSig, "", "no X-Slack-Request-Timestamp"},
		{"bad timestamp", goodSig, "soon", "not a unix time"},
		{"no version", strings.TrimPrefix(goodSig, "v0="), goodTS, "expected `v0="},
		{"wrong version", "v1=" + strings.TrimPrefix(goodSig, "v0="), goodTS, "expected `v0="},
		{"wrong tag", "v0=" + strings.Repeat("0", 64), goodTS, "did not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := VerifySlack(tc.sig, tc.ts, body, secret, now, 0)
			if res.Valid {
				t.Fatal("accepted")
			}
			if !strings.Contains(res.Problem, tc.wantIn) {
				t.Errorf("problem = %q, want %q in it", res.Problem, tc.wantIn)
			}
		})
	}
}

// TestEveryProviderShowsItsCanonicalString is the property the whole package
// is built around: whatever the outcome, the developer can see the exact
// bytes that were signed. A verifier that only returns a boolean is barely
// better than the provider's own dashboard.
func TestEveryProviderShowsItsCanonicalString(t *testing.T) {
	secret := []byte("s")
	body := []byte(`{"a":1}`)
	now := time.Now()

	sig, ts := slackHeaders(t, secret, body, now)
	results := map[string]Result{
		"stripe":  VerifyStripe(stripeHeader(t, secret, body, now), body, secret, now, 0),
		"github":  VerifyGitHub("sha256="+Compute(SHA256, Hex, secret, body), body, secret),
		"shopify": VerifyShopify(Compute(SHA256, Base64, secret, body), body, secret),
		"slack":   VerifySlack(sig, ts, body, secret, now, 0),
	}

	for name, res := range results {
		if !res.Valid {
			t.Errorf("%s: valid signature rejected: %s", name, res.Problem)
		}
		if res.Signed == "" {
			t.Errorf("%s: no canonical string", name)
		}
		if !strings.Contains(res.Signed, string(body)) {
			t.Errorf("%s: canonical string %q does not contain the body", name, res.Signed)
		}
		if res.Expected == "" || res.Provided == "" {
			t.Errorf("%s: missing expected/provided tags", name)
		}
		if res.Provider != name {
			t.Errorf("provider = %q, want %q", res.Provider, name)
		}
	}
}

// TestProvidersDisagree: the same body and secret produce four different
// tags, because the canonical strings differ. This is the argument for a
// tool that knows all of them -- and a guard against a refactor that
// accidentally makes two providers share a code path.
func TestProvidersDisagree(t *testing.T) {
	secret := []byte("s")
	body := []byte(`{"a":1}`)
	now := time.Now()
	sig, ts := slackHeaders(t, secret, body, now)

	signed := map[string]string{
		"stripe":  VerifyStripe(stripeHeader(t, secret, body, now), body, secret, now, 0).Signed,
		"github":  VerifyGitHub("sha256="+Compute(SHA256, Hex, secret, body), body, secret).Signed,
		"shopify": VerifyShopify(Compute(SHA256, Base64, secret, body), body, secret).Signed,
		"slack":   VerifySlack(sig, ts, body, secret, now, 0).Signed,
	}

	// GitHub and Shopify both sign the bare body -- they differ only in
	// encoding, which is exactly the trap VerifyShopify diagnoses.
	if signed["github"] != signed["shopify"] {
		t.Error("github and shopify should sign the same string and differ only in encoding")
	}
	if signed["stripe"] == signed["github"] {
		t.Error("stripe prefixes a timestamp; its canonical string cannot equal the bare body")
	}
	if signed["slack"] == signed["stripe"] {
		t.Error("slack and stripe use different separators and prefixes")
	}
}
