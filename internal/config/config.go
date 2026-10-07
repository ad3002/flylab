package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Host             string
	Port             int
	Domain           string
	DBPath           string
	DataDir          string
	ArtifactsDir     string
	RegistryDir      string
	ContractsDir     string
	WebDir           string
	FlysimBin        string
	MaxWallSeconds   int
	MaxRSSBytes      int64
	MaxArtifactBytes int64

	// Planner (claude -p) settings.
	ClaudeBin             string
	ClaudeModel           string
	ClaudeTimeoutSeconds  int
	LLMMaxConcurrency     int
	ParseRateLimitPerHour int
	// ParseRateLimitPerIPPerHour bounds parses from one client address across all accounts,
	// ParseGlobalLimitPerHour bounds paid Claude calls for the whole server.
	ParseRateLimitPerIPPerHour int
	ParseGlobalLimitPerHour    int

	// Interpretation (v3: spikes -> AI hypotheses) through the same claude CLI. The rate limits
	// are separate counters from the planner's. v4: interpretations run on their own queue
	// worker (InterpretConcurrency calls at a time), not on the planner's slots.
	ClaudeInterpretModel           string
	ClaudeInterpretTimeoutSeconds  int
	InterpretRateLimitPerHour      int
	InterpretRateLimitPerIPPerHour int
	InterpretGlobalLimitPerHour    int
	InterpretConcurrency           int
	InterpretQueueMax              int

	// AI budget (v4): rolling 24 h spend of every claude -p call, from the CLI's
	// total_cost_usd, for the whole server and per account.
	AIDailyBudgetUSD     float64
	AIUserDailyBudgetUSD float64

	// Accounts.
	RegistrationOpen bool
	// RegistrationInviteCode, when set (and RegistrationOpen), is required as invite_code to
	// register ("invite" registration mode).
	RegistrationInviteCode string
	// AuthRateLimitPerIP: login + register attempts per client address per 15 minutes.
	AuthRateLimitPerIP int
	// LoginFailuresPerUsername: failed logins per username per 15 minutes before that
	// username is locked until the window passes.
	LoginFailuresPerUsername int
	// RegisterRateLimitPerIPPerHour: accounts created per client address per hour.
	RegisterRateLimitPerIPPerHour int
	// PasswordHashConcurrency caps concurrent PBKDF2 computations (login + register).
	PasswordHashConcurrency int
}

// LoadConfig reads the environment. A variable that is unset or empty gets its default;
// a variable that is set but malformed (e.g. PORT=abc, REGISTRATION_OPEN=maybe) is a
// startup error instead of being silently replaced by the default.
func LoadConfig() (*Config, error) {
	var errs []string

	intVar := func(key string, def, min int) int {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return def
		}
		v, err := strconv.Atoi(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s=%q is not an integer", key, raw))
			return def
		}
		if v < min {
			errs = append(errs, fmt.Sprintf("%s=%d must be >= %d", key, v, min))
			return def
		}
		return v
	}

	boolVar := func(key string, def bool) bool {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return def
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s=%q is not a boolean (use true/false)", key, raw))
			return def
		}
		return v
	}

	// floatVar is a positive, finite amount (budgets in USD).
	floatVar := func(key string, def float64) float64 {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			return def
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			errs = append(errs, fmt.Sprintf("%s=%q is not a number", key, raw))
			return def
		}
		if v <= 0 {
			errs = append(errs, fmt.Sprintf("%s=%s must be > 0", key, raw))
			return def
		}
		return v
	}

	// An invite code that is set but blank would silently open registration: refuse it.
	invite := os.Getenv("REGISTRATION_INVITE_CODE")
	if invite != "" && strings.TrimSpace(invite) == "" {
		errs = append(errs, "REGISTRATION_INVITE_CODE is set but blank")
	}
	invite = strings.TrimSpace(invite)

	cfg := &Config{
		Host:             getEnv("HOST", "0.0.0.0"),
		Port:             intVar("PORT", 8080, 1),
		Domain:           getEnv("DOMAIN", "flylab.aglabx.com"),
		DBPath:           getEnv("DB_PATH", "flylab.db"),
		DataDir:          getEnv("DATA_DIR", "data"),
		ArtifactsDir:     getEnv("ARTIFACTS_DIR", "artifacts"),
		RegistryDir:      getEnv("REGISTRY_DIR", "registry"),
		ContractsDir:     getEnv("CONTRACTS_DIR", "contracts"),
		WebDir:           getEnv("WEB_DIR", "web"),
		FlysimBin:        getEnv("FLYSIM_BIN", "bin/flysim"),
		MaxWallSeconds:   intVar("MAX_WALL_SECONDS", 3600, 1),
		MaxRSSBytes:      24 * 1024 * 1024 * 1024, // 24 GB
		MaxArtifactBytes: 2 * 1024 * 1024 * 1024,  // 2 GB

		ClaudeBin:             getEnv("CLAUDE_BIN", "claude"),
		ClaudeModel:           getEnv("CLAUDE_MODEL", "claude-sonnet-5-5"),
		ClaudeTimeoutSeconds:  intVar("CLAUDE_TIMEOUT_SECONDS", 90, 1),
		LLMMaxConcurrency:     intVar("LLM_MAX_CONCURRENCY", 2, 1),
		ParseRateLimitPerHour: intVar("PARSE_RATE_LIMIT_PER_HOUR", 60, 1),

		ParseRateLimitPerIPPerHour: intVar("PARSE_RATE_LIMIT_PER_IP_PER_HOUR", 120, 1),
		ParseGlobalLimitPerHour:    intVar("PARSE_GLOBAL_LIMIT_PER_HOUR", 300, 1),

		ClaudeInterpretModel:           getEnv("CLAUDE_INTERPRET_MODEL", "claude-opus-5-5"),
		ClaudeInterpretTimeoutSeconds:  intVar("CLAUDE_INTERPRET_TIMEOUT_SECONDS", 180, 1),
		InterpretRateLimitPerHour:      intVar("INTERPRET_RATE_LIMIT_PER_HOUR", 20, 1),
		InterpretRateLimitPerIPPerHour: intVar("INTERPRET_RATE_LIMIT_PER_IP_PER_HOUR", 40, 1),
		InterpretGlobalLimitPerHour:    intVar("INTERPRET_GLOBAL_LIMIT_PER_HOUR", 100, 1),
		InterpretConcurrency:           intVar("INTERPRET_CONCURRENCY", 1, 1),
		InterpretQueueMax:              intVar("INTERPRET_QUEUE_MAX", 20, 1),

		AIDailyBudgetUSD:     floatVar("AI_DAILY_BUDGET_USD", 20),
		AIUserDailyBudgetUSD: floatVar("AI_USER_DAILY_BUDGET_USD", 3),

		RegistrationOpen:              boolVar("REGISTRATION_OPEN", true),
		RegistrationInviteCode:        invite,
		AuthRateLimitPerIP:            intVar("AUTH_RATE_LIMIT_PER_IP", 30, 1),
		LoginFailuresPerUsername:      intVar("LOGIN_FAILURES_PER_USERNAME", 10, 1),
		RegisterRateLimitPerIPPerHour: intVar("REGISTER_RATE_LIMIT_PER_IP_PER_HOUR", 5, 1),
		PasswordHashConcurrency:       intVar("PASSWORD_HASH_CONCURRENCY", 4, 1),
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return cfg, nil
}

// Registration modes reported by GET /capabilities (contract v4 section 3).
const (
	RegistrationModeOpen   = "open"
	RegistrationModeInvite = "invite"
	RegistrationModeClosed = "closed"
)

// RegistrationMode is "closed" when REGISTRATION_OPEN=false, "invite" when an invite code is
// configured, else "open".
func (c *Config) RegistrationMode() string {
	switch {
	case !c.RegistrationOpen:
		return RegistrationModeClosed
	case c.RegistrationInviteCode != "":
		return RegistrationModeInvite
	default:
		return RegistrationModeOpen
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
