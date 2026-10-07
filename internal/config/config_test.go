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

func TestLoadConfigInterpretation(t *testing.T) {
	for _, k := range []string{"CLAUDE_INTERPRET_MODEL", "CLAUDE_INTERPRET_TIMEOUT_SECONDS", "INTERPRET_RATE_LIMIT_PER_HOUR",
		"INTERPRET_RATE_LIMIT_PER_IP_PER_HOUR", "INTERPRET_GLOBAL_LIMIT_PER_HOUR"} {
		t.Setenv(k, "")
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ClaudeInterpretModel != "claude-opus-5-5" || cfg.ClaudeInterpretTimeoutSeconds != 180 ||
		cfg.InterpretRateLimitPerHour != 20 || cfg.InterpretRateLimitPerIPPerHour != 40 || cfg.InterpretGlobalLimitPerHour != 100 {
		t.Fatalf("unexpected interpretation defaults: model=%q timeout=%d user=%d ip=%d global=%d", cfg.ClaudeInterpretModel,
			cfg.ClaudeInterpretTimeoutSeconds, cfg.InterpretRateLimitPerHour, cfg.InterpretRateLimitPerIPPerHour, cfg.InterpretGlobalLimitPerHour)
	}
	t.Setenv("INTERPRET_RATE_LIMIT_PER_HOUR", "0")
	t.Setenv("CLAUDE_INTERPRET_TIMEOUT_SECONDS", "soon")
	_, err = config.LoadConfig()
	if err == nil {
		t.Fatalf("malformed interpretation settings must be a startup error")
	}
	for _, want := range []string{"INTERPRET_RATE_LIMIT_PER_HOUR=0 must be >= 1", `CLAUDE_INTERPRET_TIMEOUT_SECONDS="soon" is not an integer`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestLoadConfigV4GuardrailSettings(t *testing.T) {
	for _, k := range []string{"AI_DAILY_BUDGET_USD", "AI_USER_DAILY_BUDGET_USD", "INTERPRET_CONCURRENCY",
		"INTERPRET_QUEUE_MAX", "REGISTRATION_INVITE_CODE", "REGISTRATION_OPEN"} {
		t.Setenv(k, "")
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AIDailyBudgetUSD != 20 || cfg.AIUserDailyBudgetUSD != 3 || cfg.InterpretConcurrency != 1 ||
		cfg.InterpretQueueMax != 20 || cfg.RegistrationInviteCode != "" || cfg.RegistrationMode() != "open" {
		t.Fatalf("unexpected v4 defaults: global=%v user=%v conc=%d queue=%d invite=%q mode=%s", cfg.AIDailyBudgetUSD,
			cfg.AIUserDailyBudgetUSD, cfg.InterpretConcurrency, cfg.InterpretQueueMax, cfg.RegistrationInviteCode, cfg.RegistrationMode())
	}

	t.Setenv("AI_USER_DAILY_BUDGET_USD", "0.01")
	t.Setenv("REGISTRATION_INVITE_CODE", " fly-2026 ")
	cfg, err = config.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.AIUserDailyBudgetUSD != 0.01 || cfg.RegistrationInviteCode != "fly-2026" || cfg.RegistrationMode() != "invite" {
		t.Fatalf("budget/invite not applied: %v %q %s", cfg.AIUserDailyBudgetUSD, cfg.RegistrationInviteCode, cfg.RegistrationMode())
	}
	t.Setenv("REGISTRATION_OPEN", "false")
	if cfg, err = config.LoadConfig(); err != nil || cfg.RegistrationMode() != "closed" {
		t.Fatalf("REGISTRATION_OPEN=false must win over the invite code: %v %v", cfg, err)
	}

	t.Setenv("REGISTRATION_OPEN", "")
	t.Setenv("AI_DAILY_BUDGET_USD", "lots")
	t.Setenv("AI_USER_DAILY_BUDGET_USD", "0")
	t.Setenv("INTERPRET_QUEUE_MAX", "0")
	t.Setenv("INTERPRET_CONCURRENCY", "NaN")
	t.Setenv("REGISTRATION_INVITE_CODE", "   ")
	_, err = config.LoadConfig()
	if err == nil {
		t.Fatalf("malformed v4 settings must be a startup error")
	}
	for _, want := range []string{`AI_DAILY_BUDGET_USD="lots" is not a number`, "AI_USER_DAILY_BUDGET_USD=0 must be > 0",
		"INTERPRET_QUEUE_MAX=0 must be >= 1", `INTERPRET_CONCURRENCY="NaN" is not an integer`, "REGISTRATION_INVITE_CODE is set but blank"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}
