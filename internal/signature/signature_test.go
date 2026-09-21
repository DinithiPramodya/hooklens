package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestComputeMatchesTheStandardLibrary is a sanity anchor: if this package's
// idea of HMAC ever diverges from crypto/hmac, everything downstream is
// wrong in a way no provider test would localise.
func TestComputeMatchesTheStandardLibrary(t *testing.T) {
	secret := []byte("whsec_test")
	payload := []byte(`{"id":"evt_1"}`)

	m := hmac.New(sha256.New, secret)
	m.Write(payload)
	want := hex.EncodeToString(m.Sum(nil))

	if got := Compute(SHA256, Hex, secret, payload); got != want {
		t.Errorf("Compute = %q, want %q", got, want)
	}
}

// TestComputeIsSensitiveToEveryByte. The whole value of a MAC is that it
// changes completely for any change at all -- including whitespace, which is
// the change a framework makes silently by re-serialising JSON.
func TestComputeIsSensitiveToEveryByte(t *testing.T) {
	secret := []byte("s")
	base := Compute(SHA256, Hex, secret, []byte(`{"a":1}`))

	for _, variant := range []string{
		`{"a": 1}`,    // one space
		`{"a":1} `,    // trailing space
		` {"a":1}`,    // leading space
		"{\"a\":1}\n", // trailing newline
		`{"a":2}`,     // a real change
	} {
		if got := Compute(SHA256, Hex, secret, []byte(variant)); got == base {
			t.Errorf("%q produced the same tag as the original; the MAC is not over exact bytes", variant)
		}
	}
}

func TestEqual(t *testing.T) {
	a := Compute(SHA256, Hex, []byte("k"), []byte("m"))

	if !Equal(a, a) {
		t.Error("identical tags compared unequal")
	}
	if Equal(a, a[:len(a)-1]) {
		t.Error("tags of different lengths compared equal")
	}
	// Differing in the LAST byte is the case a naive early-return comparison
	// gets right and a broken constant-time one can get wrong.
	flipped := a[:len(a)-1] + map[bool]string{true: "0", false: "1"}[a[len(a)-1] == '1']
	if Equal(a, flipped) {
		t.Error("tags differing in the last byte compared equal")
	}
	// Empty against empty. subtle.ConstantTimeCompare returns 1 here, which
	// cost a test in unit 08; Equal must not inherit that.
	if !Equal("", "") {
		t.Error("two empty strings should compare equal")
	}
	if Equal("", a) {
		t.Error("empty compared equal to a real tag")
	}
}

// ---- Stripe ----

// stripeHeader builds a valid header the way Stripe would.
func stripeHeader(t *testing.T, secret, body []byte, ts time.Time, extraV1 ...string) string {
	t.Helper()
	unix := strconv.FormatInt(ts.Unix(), 10)
	sig := Compute(SHA256, Hex, secret, []byte(unix+"."+string(body)))
	parts := []string{"t=" + unix, "v1=" + sig}
	for _, e := range extraV1 {
		parts = append(parts, "v1="+e)
	}
	return strings.Join(parts, ",")
}

func TestVerifyStripeValid(t *testing.T) {
	secret := []byte("whsec_abc")
	body := []byte(`{"id":"evt_1","amount":2500}`)
	now := time.Now()

	res := VerifyStripe(stripeHeader(t, secret, body, now), body, secret, now, 0)

	if !res.Valid {
		t.Fatalf("valid signature rejected: %s", res.Problem)
	}
	// The canonical string is the field a developer actually needs, so it is
	// asserted rather than assumed.
	want := strconv.FormatInt(now.Unix(), 10) + "." + string(body)
	if res.Signed != want {
		t.Errorf("Signed = %q, want %q", res.Signed, want)
	}
	if res.Problem != "" {
		t.Errorf("a valid result carried a problem: %q", res.Problem)
	}
}

// TestVerifyStripeRotation is the detail hand-rolled verifiers miss: during a
// secret rotation Stripe sends several v1 values, and ANY match is a pass.
// Getting this wrong fails only mid-rotation, which is the worst time to
// find out.
func TestVerifyStripeRotation(t *testing.T) {
	secret := []byte("whsec_new")
	body := []byte(`{"id":"evt_2"}`)
	now := time.Now()
	unix := strconv.FormatInt(now.Unix(), 10)

	// The old secret's tag comes FIRST, so a verifier that only checks the
	// first v1 fails this.
	oldSig := Compute(SHA256, Hex, []byte("whsec_old"), []byte(unix+"."+string(body)))
	newSig := Compute(SHA256, Hex, secret, []byte(unix+"."+string(body)))
	header := "t=" + unix + ",v1=" + oldSig + ",v1=" + newSig

	if res := VerifyStripe(header, body, secret, now, 0); !res.Valid {
		t.Errorf("a matching v1 that was not first was rejected: %s", res.Problem)
	}
}

func TestVerifyStripeWrongSecret(t *testing.T) {
	body := []byte(`{"id":"evt_3"}`)
	now := time.Now()
	header := stripeHeader(t, []byte("whsec_right"), body, now)

	res := VerifyStripe(header, body, []byte("whsec_wrong"), now, 0)

	if res.Valid {
		t.Fatal("a signature made with a different secret was accepted")
	}
	// Failing is not enough: the panel has to be usable.
	if res.Signed == "" {
		t.Error("no canonical string shown; the developer cannot see what was compared")
	}
	if res.Expected == "" || res.Provided == "" {
		t.Error("expected/provided tags missing from a failure")
	}
	if res.Hint == "" {
		t.Error("no hint on a failure")
	}
}

// TestVerifyStripeDetectsMangledBody covers the single most common real
// cause: a framework re-serialised or trimmed the body before signing.
func TestVerifyStripeDetectsMangledBody(t *testing.T) {
	secret := []byte("whsec_abc")
	raw := []byte(` {"id":"evt_4"} `)
	now := time.Now()
	// The header was made over the TRIMMED body, as a framework would.
	header := stripeHeader(t, secret, []byte(strings.TrimSpace(string(raw))), now)

	res := VerifyStripe(header, raw, secret, now, 0)

	if res.Valid {
		t.Fatal("a signature over a different byte sequence was accepted")
	}
	if !strings.Contains(res.Hint, "whitespace") {
		t.Errorf("hint = %q; it should point at the whitespace, not at the secret", res.Hint)
	}
}

func TestVerifyStripeReplayWindow(t *testing.T) {
	secret := []byte("whsec_abc")
	body := []byte(`{"id":"evt_5"}`)
	signedAt := time.Now().Add(-10 * time.Minute)
	header := stripeHeader(t, secret, body, signedAt)

	res := VerifyStripe(header, body, secret, time.Now(), 5*time.Minute)

	if res.Valid {
		t.Fatal("a ten-minute-old request passed a five-minute tolerance")
	}
	// The distinction that matters: the SIGNATURE was fine. Reporting this
	// identically to a bad tag would send a developer to check their secret.
	if !strings.Contains(res.Problem, "valid") {
		t.Errorf("problem = %q; it should say the signature itself was valid", res.Problem)
	}
	if res.Age < 9*time.Minute {
		t.Errorf("Age = %v, want about 10m", res.Age)
	}
}

func TestVerifyStripeFutureTimestamp(t *testing.T) {
	secret := []byte("whsec_abc")
	body := []byte(`{"id":"evt_6"}`)
	header := stripeHeader(t, secret, body, time.Now().Add(1*time.Hour))

	res := VerifyStripe(header, body, secret, time.Now(), 5*time.Minute)

	if res.Valid {
		t.Fatal("a timestamp an hour in the future was accepted")
	}
	if !strings.Contains(res.Problem, "FUTURE") {
		t.Errorf("problem = %q; a future timestamp is a clock problem and should say so", res.Problem)
	}
	if !strings.Contains(res.Hint, "clock") {
		t.Errorf("hint = %q, want it to mention clocks", res.Hint)
	}
}

// TestVerifyStripeOldRequestStillVerifiable: re-checking a stored capture
// must use the time it ARRIVED, not the time someone opened the UI --
// otherwise every capture reads "expired" after five minutes, which is true
// and useless. `now` being a parameter is what makes that possible, so it is
// worth a test.
func TestVerifyStripeOldRequestStillVerifiable(t *testing.T) {
	secret := []byte("whsec_abc")
	body := []byte(`{"id":"evt_7"}`)
	arrived := time.Now().Add(-72 * time.Hour)
	header := stripeHeader(t, secret, body, arrived)

	if res := VerifyStripe(header, body, secret, arrived, 5*time.Minute); !res.Valid {
		t.Errorf("a three-day-old capture could not be re-verified against its arrival time: %s",
			res.Problem)
	}
}

func TestParseStripeHeaderRejects(t *testing.T) {
	tests := []struct{ name, header, wantIn string }{
		{"empty", "", "no Stripe-Signature"},
		{"no timestamp", "v1=abc", "no t="},
		{"no signature", "t=1614556800", "no v1="},
		{"bad timestamp", "t=yesterday,v1=abc", "not a unix time"},
		// v0 is Stripe's legacy test-mode scheme; accepting it would mean
		// accepting a weaker check this package does not implement.
		{"only v0", "t=1614556800,v0=abc", "no v1="},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := VerifyStripe(tc.header, []byte("{}"), []byte("s"), time.Now(), 0)
			if res.Valid {
				t.Fatal("accepted")
			}
			if !strings.Contains(res.Problem, tc.wantIn) {
				t.Errorf("problem = %q, want it to contain %q", res.Problem, tc.wantIn)
			}
		})
	}
}
