package config_test

import (
	"strings"
	"testing"

	"github.com/ad3002/flylab/internal/config"
)

func TestLoadConfigDefaults(t *testing.T) {
	for _, k := range []string{"PORT", "CLAUDE_BIN", "CLAUDE_MODEL", "CLAUDE_TIMEOUT_SECONDS",
		"LLM_MAX_CONCURRENCY", "PARSE_RATE_LIMIT_PER_HOUR", "REGISTRATION_OPEN", "MAX_WALL_SECONDS"} {
		t.Setenv(k, "")
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with empty env failed: %v", err)
	}
	if cfg.Port != 8080 || cfg.ClaudeBin != "claude" || cfg.ClaudeModel != "claude-sonnet-5-5" {
		t.Fatalf("unexpected defaults: port=%d bin=%q model=%q", cfg.Port, cfg.ClaudeBin, cfg.ClaudeModel)
	}
	if cfg.ClaudeTimeoutSeconds != 90 || cfg.LLMMaxConcurrency != 2 || cfg.ParseRateLimitPerHour != 60 {
		t.Fatalf("unexpected planner defaults: timeout=%d conc=%d rate=%d",
			cfg.ClaudeTimeoutSeconds, cfg.LLMMaxConcurrency, cfg.ParseRateLimitPerHour)
	}
	if !cfg.RegistrationOpen {
		t.Fatalf("REGISTRATION_OPEN must default to true")
	}
}

func TestLoadConfigRejectsMalformedValues(t *testing.T) {
	t.Setenv("PORT", "abc")
	t.Setenv("REGISTRATION_OPEN", "maybe")
	t.Setenv("LLM_MAX_CONCURRENCY", "0")
	_, err := config.LoadConfig()
	if err == nil {
		t.Fatalf("expected an error for malformed env values, got nil")
	}
	for _, want := range []string{`PORT="abc" is not an integer`, `REGISTRATION_OPEN="maybe" is not a boolean`, "LLM_MAX_CONCURRENCY=0 must be >= 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestLoadConfigRegistrationClosed(t *testing.T) {
	t.Setenv("REGISTRATION_OPEN", "false")
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.RegistrationOpen {
		t.Fatalf("REGISTRATION_OPEN=false must close registration")
	}
}

func TestLoadConfigAbuseLimits(t *testing.T) {
	for _, k := range []string{"PARSE_RATE_LIMIT_PER_IP_PER_HOUR", "PARSE_GLOBAL_LIMIT_PER_HOUR", "AUTH_RATE_LIMIT_PER_IP",
		"LOGIN_FAILURES_PER_USERNAME", "REGISTER_RATE_LIMIT_PER_IP_PER_HOUR", "PASSWORD_HASH_CONCURRENCY"} {
		t.Setenv(k, "")
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ParseRateLimitPerIPPerHour != 120 || cfg.ParseGlobalLimitPerHour != 300 || cfg.AuthRateLimitPerIP != 30 ||
		cfg.LoginFailuresPerUsername != 10 || cfg.RegisterRateLimitPerIPPerHour != 5 || cfg.PasswordHashConcurrency != 4 {
		t.Fatalf("unexpected abuse-limit defaults: %+v", cfg)
	}

	t.Setenv("PASSWORD_HASH_CONCURRENCY", "0")
	t.Setenv("PARSE_GLOBAL_LIMIT_PER_HOUR", "lots")
	_, err = config.LoadConfig()
	if err == nil {
		t.Fatalf("malformed abuse limits must be a startup error")
	}
	for _, want := range []string{"PASSWORD_HASH_CONCURRENCY=0 must be >= 1", `PARSE_GLOBAL_LIMIT_PER_HOUR="lots" is not an integer`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}
