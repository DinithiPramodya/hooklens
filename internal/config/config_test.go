package config

import (
	"testing"

	"github.com/DinithiPramodya/hooklens/internal/capture"
)

func TestEnvBytes(t *testing.T) {
	const def int64 = 1 << 20

	tests := []struct {
		name, value string
		want        int64
	}{
		{"unset", "", def},
		{"plain bytes", "4096", 4096},
		{"with B", "4096B", 4096},
		{"kilobytes", "512KB", 512 << 10},
		{"megabytes", "5MB", 5 << 20},
		{"lowercase", "5mb", 5 << 20},
		{"spaces", "  5 MB  ", 5 << 20},
		// A malformed value falls back rather than failing to start: there is
		// a safe default, and the blast radius of a typo is "captures are
		// 1MB" rather than "the routing is wrong".
		{"garbage", "big", def},
		{"negative", "-5", def},
		{"zero", "0", def},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value != "" {
				t.Setenv("HOOKLENS_TEST_BYTES", tc.value)
			}
			if got := envBytes("HOOKLENS_TEST_BYTES", def); got != tc.want {
				t.Errorf("envBytes(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestLoadDefaultsMaxBody(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxBody != capture.DefaultMaxBody {
		t.Errorf("MaxBody = %d, want the capture default %d", cfg.MaxBody, capture.DefaultMaxBody)
	}
}

func TestLoadReadsMaxBody(t *testing.T) {
	t.Setenv("HOOKLENS_MAX_BODY", "8MB")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxBody != 8<<20 {
		t.Errorf("MaxBody = %d, want %d", cfg.MaxBody, 8<<20)
	}
}
