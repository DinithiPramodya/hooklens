package signature

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Stripe's scheme.
//
//	Stripe-Signature: t=1614556800,v1=5257a8...,v1=... ,v0=...
//
// The signed string is `t + "." + body` -- the timestamp, a literal dot, and
// the raw body. Including the timestamp in the MAC is what makes the replay
// window enforceable: an attacker replaying an old request cannot change the
// timestamp without invalidating the tag.
//
// Multiple v1 values appear during a secret rotation, and ANY of them
// matching is a pass. That is the detail most hand-rolled verifiers miss, and
// it fails only during a rotation -- the worst possible time to discover it.
const (
	StripeHeader = "Stripe-Signature"
	// defaultTolerance matches Stripe's own libraries.
	defaultTolerance = 5 * time.Minute
)

// VerifyStripe checks a Stripe-Signature header against the raw body.
//
// now is a parameter rather than a call to time.Now() so the replay window is
// testable without sleeping, and so a stored capture can be re-verified later
// against the time it ARRIVED rather than the time someone opened the UI --
// otherwise every capture would read "expired" after five minutes, which is
// true and useless.
func VerifyStripe(header string, body, secret []byte, now time.Time, tolerance time.Duration) Result {
	res := Result{Provider: "stripe"}
	if tolerance <= 0 {
		tolerance = defaultTolerance
	}

	ts, sigs, err := parseStripeHeader(header)
	if err != nil {
		res.Problem = err.Error()
		res.Hint = "Expected a header like `t=1614556800,v1=<hex>`. Is this really a Stripe webhook?"
		return res
	}
	res.Timestamp = time.Unix(ts, 0).UTC()
	res.Age = now.Sub(res.Timestamp)
	res.Provided = strings.Join(sigs, ", ")

	// The canonical string, and it is shown whatever happens -- a developer
	// whose own code signs the wrong thing needs to compare against this.
	signed := strconv.FormatInt(ts, 10) + "." + string(body)
	res.Signed = signed
	res.Expected = Compute(SHA256, Hex, secret, []byte(signed))

	// Any v1 matching is a pass, for rotation.
	for _, s := range sigs {
		if Equal(res.Expected, s) {
			res.Valid = true
			break
		}
	}

	if !res.Valid {
		res.Problem = "no v1 signature matched"
		res.Hint = stripeHint(body)
		return res
	}

	// The window is checked AFTER the signature, deliberately. An expired
	// request with a valid tag and an expired request with a garbage tag are
	// different situations -- the first is a slow delivery or a clock skew,
	// the second is an attack -- and checking the cheap thing first would
	// report them identically.
	if res.Age > tolerance || res.Age < -tolerance {
		res.Valid = false
		res.Problem = fmt.Sprintf("signature is valid but the timestamp is %s outside the %s tolerance",
			absDuration(res.Age-tolerance).Round(time.Second), tolerance)
		if res.Age < 0 {
			res.Problem = fmt.Sprintf("signature is valid but the timestamp is %s in the FUTURE",
				absDuration(res.Age).Round(time.Second))
			res.Hint = "Your clock and the sender's disagree. Check NTP on this machine."
		} else {
			res.Hint = "This is normal for a replayed or retried delivery. " +
				"A live request this old means a slow queue or a clock problem."
		}
	}
	return res
}

// stripeHint guesses at the most common cause, which is almost never the
// secret being wrong.
func stripeHint(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) != len(body) {
		return "The body has leading or trailing whitespace. The signature covers the EXACT " +
			"bytes -- if your framework trimmed or re-serialised the body before you signed " +
			"it, the tag will never match. Sign the raw bytes."
	}
	return "Check the signing secret (whsec_...), and make sure you are signing the RAW " +
		"request body rather than a re-serialised copy of the parsed JSON."
}

// parseStripeHeader pulls the timestamp and every v1 tag out of the header.
func parseStripeHeader(header string) (ts int64, sigs []string, err error) {
	if strings.TrimSpace(header) == "" {
		return 0, nil, fmt.Errorf("no %s header", StripeHeader)
	}

	var haveTS bool
	for part := range strings.SplitSeq(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil {
				return 0, nil, fmt.Errorf("timestamp %q is not a unix time", v)
			}
			ts, haveTS = n, true
		case "v1":
			sigs = append(sigs, v)
			// v0 is Stripe's legacy test-mode scheme and is deliberately
			// ignored rather than treated as a signature. Accepting it would
			// mean accepting a weaker check we have not implemented.
		}
	}

	switch {
	case !haveTS:
		return 0, nil, fmt.Errorf("header has no t= timestamp")
	case len(sigs) == 0:
		return 0, nil, fmt.Errorf("header has no v1= signature")
	}
	return ts, sigs, nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
