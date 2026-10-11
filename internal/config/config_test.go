package config

import "testing"

func TestDefaultRateLimitMatchesUIBurstTest(t *testing.T) {
	t.Setenv("RATE_LIMIT_RPM", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimitRPM != 5 {
		t.Fatalf("RATE_LIMIT_RPM default = %d, want 5", cfg.RateLimitRPM)
	}
}
