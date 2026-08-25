package server

import "testing"

func TestResolve(t *testing.T) {
	const base = "hooklens.dev"

	tests := []struct {
		name string
		host string
		path string
		want Route
	}{
		// --- the app ---
		{"bare domain", "hooklens.dev", "/", Route{TargetApp, "", "/"}},
		{"www alias", "www.hooklens.dev", "/", Route{TargetApp, "", "/"}},
		{"domain with port", "hooklens.dev:8080", "/healthz", Route{TargetApp, "", "/healthz"}},
		{"uppercase host", "HookLens.DEV", "/", Route{TargetApp, "", "/"}},
		{"trailing dot is the same host", "hooklens.dev.", "/", Route{TargetApp, "", "/"}},
		{"api subdomain is reserved, not an inbox", "api.hooklens.dev", "/", Route{TargetApp, "", "/"}},

		// --- subdomain capture ---
		{"subdomain inbox", "a7f3.hooklens.dev", "/webhook", Route{TargetIngest, "a7f3", "/webhook"}},
		{"subdomain with port", "a7f3.hooklens.dev:8080", "/webhook", Route{TargetIngest, "a7f3", "/webhook"}},
		{"subdomain uppercased", "A7F3.HookLens.dev", "/webhook", Route{TargetIngest, "a7f3", "/webhook"}},
		{"hyphenated slug", "my-inbox.hooklens.dev", "/x", Route{TargetIngest, "my-inbox", "/x"}},
		{"deep path preserved", "a7f3.hooklens.dev", "/a/b/c", Route{TargetIngest, "a7f3", "/a/b/c"}},

		// --- path-form capture (the wildcard-DNS fallback) ---
		{"path form", "hooklens.dev", "/e/a7f3/webhook", Route{TargetIngest, "a7f3", "/webhook"}},
		{"path form at inbox root", "hooklens.dev", "/e/a7f3", Route{TargetIngest, "a7f3", "/"}},
		{"path form with trailing slash", "hooklens.dev", "/e/a7f3/", Route{TargetIngest, "a7f3", "/"}},
		{"path form nested", "hooklens.dev", "/e/a7f3/a/b", Route{TargetIngest, "a7f3", "/a/b"}},
		{"path form works on any host", "localhost:8080", "/e/a7f3/webhook", Route{TargetIngest, "a7f3", "/webhook"}},
		{"path form beats subdomain", "b8c2.hooklens.dev", "/e/a7f3/x", Route{TargetIngest, "a7f3", "/x"}},

		// --- things that must NOT become inboxes ---
		{"nested subdomain", "a.b.hooklens.dev", "/", Route{TargetApp, "", "/"}},
		{"slug too short", "ab.hooklens.dev", "/", Route{TargetApp, "", "/"}},
		{"slug leading hyphen", "-abc.hooklens.dev", "/", Route{TargetApp, "", "/"}},
		{"slug trailing hyphen", "abc-.hooklens.dev", "/", Route{TargetApp, "", "/"}},
		{"slug with underscore", "a_b3.hooklens.dev", "/", Route{TargetApp, "", "/"}},
		{"unrelated domain", "evil.example.com", "/", Route{TargetApp, "", "/"}},
		{"suffix collision", "nothooklens.dev", "/", Route{TargetApp, "", "/"}},
		{"bare IP", "203.0.113.9", "/healthz", Route{TargetApp, "", "/healthz"}},
		{"IPv6 literal with port", "[::1]:8080", "/healthz", Route{TargetApp, "", "/healthz"}},
		{"empty host", "", "/", Route{TargetApp, "", "/"}},
		{"path form with bad slug is not capture", "hooklens.dev", "/e/xx/webhook", Route{TargetApp, "", "/e/xx/webhook"}},
		{"path form with reserved slug is not capture", "hooklens.dev", "/e/api/webhook", Route{TargetApp, "", "/e/api/webhook"}},
		{"path prefix lookalike", "hooklens.dev", "/events/a7f3", Route{TargetApp, "", "/events/a7f3"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(base, tt.host, tt.path)
			if got != tt.want {
				t.Errorf("Resolve(%q, %q, %q)\n got %+v\nwant %+v", base, tt.host, tt.path, got, tt.want)
			}
		})
	}
}

// TestResolveLocalhostBase covers the development configuration, where the base
// domain has a single label. "a7f3.localhost" is a legal Host header even where
// the OS will not resolve it, so the subdomain form still has to work.
func TestResolveLocalhostBase(t *testing.T) {
	const base = "localhost"

	tests := []struct {
		host string
		path string
		want Route
	}{
		{"localhost:8080", "/healthz", Route{TargetApp, "", "/healthz"}},
		{"a7f3.localhost:8080", "/webhook", Route{TargetIngest, "a7f3", "/webhook"}},
		{"localhost:8080", "/e/a7f3/webhook", Route{TargetIngest, "a7f3", "/webhook"}},
		{"127.0.0.1:8080", "/e/a7f3/webhook", Route{TargetIngest, "a7f3", "/webhook"}},
		{"127.0.0.1:8080", "/healthz", Route{TargetApp, "", "/healthz"}},
	}

	for _, tt := range tests {
		got := Resolve(base, tt.host, tt.path)
		if got != tt.want {
			t.Errorf("Resolve(%q, %q, %q)\n got %+v\nwant %+v", base, tt.host, tt.path, got, tt.want)
		}
	}
}

func TestValidSlug(t *testing.T) {
	valid := []string{"a7f3", "abc", "my-inbox", "a-b-c", "0123456789", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for _, s := range valid {
		if !validSlug(s) {
			t.Errorf("validSlug(%q) = false, want true", s)
		}
	}

	invalid := []string{
		"",                                  // empty
		"ab",                                // too short
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", // 33 chars, too long
		"-abc", "abc-",                      // hyphen at an edge
		"AbC",    // uppercase (normalizeHost lowercases, but the path form does not)
		"a_bc",   // underscore
		"a.bc",   // dot: this is what stops nested subdomains
		"a bc",   // space
		"a/bc",   // slash
		"café",   // multi-byte rune
		"www",    // reserved
		"api",    // reserved
		"status", // reserved
	}
	for _, s := range invalid {
		if validSlug(s) {
			t.Errorf("validSlug(%q) = true, want false", s)
		}
	}
}
