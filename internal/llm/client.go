// Package llm turns a natural-language request into a validated ExperimentPlan by calling the
// Claude Code CLI in print mode (`claude -p`) with a JSON schema (contract section 5).
//
// Failure policy (no silent fallback):
//   - CLAUDE_BIN not found          -> heuristic parser, result carries llm_error
//   - any other planner failure     -> *Error (HTTP 502 LLM_ERROR), never a heuristic result
//   - no concurrency slot in time   -> ErrBusy (HTTP 503 LLM_BUSY)
//   - per-user / per-IP / global hourly limit reached, or a parse already in flight for the
//     same account -> *RateLimitError (HTTP 429 RATE_LIMITED, Scope says which)
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/ratelimit"
)

type ParseStatus string

const (
	StatusReady       ParseStatus = "ready"
	StatusNeedsInput  ParseStatus = "needs_input"
	StatusUnsupported ParseStatus = "unsupported"
	StatusInvalid     ParseStatus = "invalid"

	MaxPromptChars = 4000

	SourceClaude    = "claude"
	SourceHeuristic = "heuristic_fallback"

	defaultBusyWait = 30 * time.Second
	excerptLimit    = 500
)

// ErrBusy means no planner slot became free within the busy wait (HTTP 503 LLM_BUSY).
var ErrBusy = errors.New("all planner slots are busy, try again shortly")

// Error is a planner failure that must be reported as 502 LLM_ERROR. Reason is user-facing.
type Error struct {
	Reason string
}

func (e *Error) Error() string { return "claude planner failed: " + e.Reason }

// Rate-limit scopes reported in RateLimitError.Scope (and in error.details.scope).
const (
	ScopeUser         = "user"           // PARSE_RATE_LIMIT_PER_HOUR per account
	ScopeIP           = "ip"             // PARSE_RATE_LIMIT_PER_IP_PER_HOUR per client address
	ScopeGlobal       = "global"         // PARSE_GLOBAL_LIMIT_PER_HOUR for the whole server
	ScopeUserInFlight = "user_in_flight" // one parse at a time per account
)

// userInFlightRetry is the Retry-After suggested when the account already has a parse running.
const userInFlightRetry = 5 * time.Second

// RateLimitError means a parse budget is exhausted (HTTP 429 RATE_LIMITED). Limit is per hour
// for the hourly scopes and the number of concurrent parses for ScopeUserInFlight.
type RateLimitError struct {
	Scope      string
	Limit      int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	retry := ratelimit.RetrySeconds(e.RetryAfter)
	switch e.Scope {
	case ScopeIP:
		return fmt.Sprintf("parse rate limit of %d requests per hour from this network address reached; retry in %d s", e.Limit, retry)
	case ScopeGlobal:
		return fmt.Sprintf("the server-wide planner budget of %d requests per hour is used up; retry in %d s", e.Limit, retry)
	case ScopeUserInFlight:
		return fmt.Sprintf("a plan for this account is already being generated; wait for it to finish (retry in %d s)", retry)
	default:
		return fmt.Sprintf("parse rate limit of %d requests per hour reached; retry in %d s", e.Limit, retry)
	}
}

type ParseResult struct {
	Status           ParseStatus            `json:"status"`
	Plan             *domain.ExperimentPlan `json:"plan,omitempty"`
	ResolvedPlan     *domain.ResolvedPlan   `json:"resolved_plan,omitempty"`
	DefaultsApplied  map[string]interface{} `json:"defaults_applied,omitempty"`
	UnresolvedFields []string               `json:"unresolved_fields,omitempty"`
	Message          string                 `json:"message"`
	LLMMetadata      map[string]interface{} `json:"llm_metadata,omitempty"`
	// LLMError is set when the result did not come from Claude (e.g. CLI missing), so the UI
	// can show a warning banner on the plan. null when Claude produced the result.
	LLMError *string `json:"llm_error"`
}

type Client struct {
	cfg           *config.Config
	validator     *contracts.Validator
	registry      *contracts.Registry
	systemPrompt  string
	plannerSchema string
	sem           chan struct{}

	userLimiter   *ratelimit.Window
	ipLimiter     *ratelimit.Window
	globalLimiter *ratelimit.Window
	limitMu       sync.Mutex
	inFlight      map[int64]bool

	// BusyWait is how long a request waits for a free planner slot (30 s per contract;
	// tests shorten it).
	BusyWait time.Duration
}

func NewClient(cfg *config.Config, validator *contracts.Validator, registry *contracts.Registry) (*Client, error) {
	if cfg.LLMMaxConcurrency < 1 {
		return nil, fmt.Errorf("LLM_MAX_CONCURRENCY must be >= 1 (got %d)", cfg.LLMMaxConcurrency)
	}
	if cfg.ParseRateLimitPerHour < 1 {
		return nil, fmt.Errorf("PARSE_RATE_LIMIT_PER_HOUR must be >= 1 (got %d)", cfg.ParseRateLimitPerHour)
	}
	if cfg.ParseRateLimitPerIPPerHour < 1 {
		return nil, fmt.Errorf("PARSE_RATE_LIMIT_PER_IP_PER_HOUR must be >= 1 (got %d)", cfg.ParseRateLimitPerIPPerHour)
	}
	if cfg.ParseGlobalLimitPerHour < 1 {
		return nil, fmt.Errorf("PARSE_GLOBAL_LIMIT_PER_HOUR must be >= 1 (got %d)", cfg.ParseGlobalLimitPerHour)
	}
	if cfg.ClaudeTimeoutSeconds < 1 {
		return nil, fmt.Errorf("CLAUDE_TIMEOUT_SECONDS must be >= 1 (got %d)", cfg.ClaudeTimeoutSeconds)
	}
	limits := validator.Limits()
	schema, err := buildPlannerSchema(registry, limits)
	if err != nil {
		return nil, fmt.Errorf("build planner schema: %w", err)
	}
	return &Client{
		cfg:           cfg,
		validator:     validator,
		registry:      registry,
		systemPrompt:  buildSystemPrompt(registry, limits),
		plannerSchema: schema,
		sem:           make(chan struct{}, cfg.LLMMaxConcurrency),
		userLimiter:   ratelimit.New(cfg.ParseRateLimitPerHour, time.Hour),
		ipLimiter:     ratelimit.New(cfg.ParseRateLimitPerIPPerHour, time.Hour),
		globalLimiter: ratelimit.New(cfg.ParseGlobalLimitPerHour, time.Hour),
		inFlight:      map[int64]bool{},
		BusyWait:      defaultBusyWait,
	}, nil
}

// SystemPrompt and PlannerSchema expose the generated planner inputs (for tests and docs).
func (c *Client) SystemPrompt() string  { return c.systemPrompt }
func (c *Client) PlannerSchema() string { return c.plannerSchema }

// InFlight is the number of claude processes currently holding a slot.
func (c *Client) InFlight() int { return len(c.sem) }

// Ready reports whether CLAUDE_BIN resolves to a usable executable (no Claude call).
func (c *Client) Ready() bool {
	_, missing, err := resolveClaude(c.cfg.ClaudeBin)
	return err == nil && !missing
}

// resolveClaude finds the CLI. missing=true means it is genuinely absent (the heuristic
// fallback applies). A candidate that exists but cannot be executed (no exec bit, dangling
// symlink, directory) is a broken install and returns an error, also for a bare name such as
// the default "claude": exec.LookPath reports those as plain ErrNotFound, so PATH is scanned
// again here to tell "absent" from "broken".
func resolveClaude(name string) (bin string, missing bool, err error) {
	bin, lookErr := exec.LookPath(name)
	if lookErr == nil {
		return bin, false, nil
	}
	if !errors.Is(lookErr, exec.ErrNotFound) && !errors.Is(lookErr, fs.ErrNotExist) {
		return "", false, fmt.Errorf("%v", lookErr)
	}
	var candidates []string
	if strings.Contains(name, "/") {
		candidates = []string{name}
	} else {
		for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
			if dir == "" {
				dir = "."
			}
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	for _, cand := range candidates {
		if reason := brokenCandidate(cand); reason != "" {
			return "", false, fmt.Errorf("%s %s", cand, reason)
		}
	}
	return "", true, lookErr
}

// brokenCandidate returns why path exists but cannot run, or "" when nothing is there.
func brokenCandidate(path string) string {
	li, err := os.Lstat(path)
	if err != nil {
		// Nothing visible there (absent, or a PATH directory this user cannot search, which
		// says nothing about claude): not evidence of a broken install.
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		if li.Mode()&fs.ModeSymlink != 0 {
			return fmt.Sprintf("is a symlink whose target cannot be opened: %v", err)
		}
		return fmt.Sprintf("cannot be opened: %v", err)
	}
	if fi.IsDir() {
		return "is a directory"
	}
	if fi.Mode()&0o111 == 0 {
		return fmt.Sprintf("is not executable (mode %v)", fi.Mode().Perm())
	}
	// Has an exec bit but LookPath still refused it (e.g. not executable for this user).
	return "exists but is not executable by the server user"
}

// ParsePrompt plans an experiment for userID from an unspecified local caller (tests,
// scripts). HTTP handlers use ParsePromptFrom with the client address.
func (c *Client) ParsePrompt(ctx context.Context, userID int64, prompt, datasetID, lang string) (*ParseResult, error) {
	return c.ParsePromptFrom(ctx, userID, "local", prompt, datasetID, lang)
}

// admit applies the abuse limits in order: one in-flight parse per account, then the
// per-account, per-address and global hourly budgets. A request is counted against the
// hourly budgets only when all of them have room. The returned release must be called.
func (c *Client) admit(userID int64, clientIP string) (func(), error) {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	if c.inFlight[userID] {
		return nil, &RateLimitError{Scope: ScopeUserInFlight, Limit: 1, RetryAfter: userInFlightRetry}
	}
	now := time.Now()
	userKey := strconv.FormatInt(userID, 10)
	checks := []struct {
		w     *ratelimit.Window
		key   string
		scope string
	}{
		{c.userLimiter, userKey, ScopeUser},
		{c.ipLimiter, clientIP, ScopeIP},
		{c.globalLimiter, "global", ScopeGlobal},
	}
	for _, ch := range checks {
		if ok, retry := ch.w.Check(ch.key, now); !ok {
			return nil, &RateLimitError{Scope: ch.scope, Limit: ch.w.Limit(), RetryAfter: retry}
		}
	}
	for _, ch := range checks {
		ch.w.Record(ch.key, now)
	}
	c.inFlight[userID] = true
	return func() {
		c.limitMu.Lock()
		delete(c.inFlight, userID)
		c.limitMu.Unlock()
	}, nil
}

// ParsePromptFrom plans an experiment for userID calling from clientIP. See the package
// comment for the error policy.
func (c *Client) ParsePromptFrom(ctx context.Context, userID int64, clientIP, prompt, datasetID, lang string) (*ParseResult, error) {
	if utf8.RuneCountInString(prompt) > MaxPromptChars {
		return &ParseResult{
			Status:  StatusInvalid,
			Message: fmt.Sprintf("Prompt exceeds maximum length of %d characters", MaxPromptChars),
		}, nil
	}

	release, err := c.admit(userID, clientIP)
	if err != nil {
		return nil, err
	}
	defer release()

	bin, missing, err := resolveClaude(c.cfg.ClaudeBin)
	if missing {
		msg := fmt.Sprintf("claude CLI not found: %v", err)
		res := c.heuristicParse(prompt, datasetID, lang)
		res.LLMError = &msg
		return res, nil
	}
	if err != nil {
		// The binary exists but cannot be executed (permissions, dangling symlink): a broken
		// install, not an expected absence, so it is an error rather than a fallback.
		return nil, &Error{Reason: fmt.Sprintf("claude CLI is not usable: %v", err)}
	}

	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.sem }()

	out, meta, err := c.runClaude(ctx, bin, prompt)
	if err != nil {
		return nil, err
	}
	return c.interpret(out, meta, datasetID, lang)
}

func (c *Client) acquire(ctx context.Context) error {
	timer := time.NewTimer(c.BusyWait)
	defer timer.Stop()
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrBusy
	case <-ctx.Done():
		return &Error{Reason: fmt.Sprintf("request cancelled while waiting for a planner slot: %v", ctx.Err())}
	}
}

// claudeEnvelope is the JSON printed by `claude -p --output-format json`.
type claudeEnvelope struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	DurationMS       int64           `json:"duration_ms"`
}

// Args returns the exact argument list passed to the CLI (the prompt itself goes to stdin).
func (c *Client) Args() []string {
	return argsFor(c.cfg.ClaudeModel, c.systemPrompt, c.plannerSchema)
}

func argsFor(model, systemPrompt, schema string) []string {
	return []string{
		"-p",
		"--model", model,
		"--tools", "",
		"--no-session-persistence",
		"--strict-mcp-config",
		"--setting-sources", "",
		"--output-format", "json",
		"--system-prompt", systemPrompt,
		"--json-schema", schema,
	}
}

// StructuredCall is one `claude -p` invocation with a JSON schema. The planner and the
// interpreter use the same flags; Stdin carries the user message (never argv).
type StructuredCall struct {
	Model        string
	SystemPrompt string
	Schema       string
	Stdin        string
	Timeout      time.Duration
	// OnSlot, when set, runs once a concurrency slot is held and before claude starts; an
	// error from it releases the slot and is returned as is (claude never runs). The
	// interpreter records its rate-limit windows here, so a busy slot costs no quota.
	OnSlot func() error
}

// StructuredResult is the structured_output of a successful call plus its metadata.
type StructuredResult struct {
	Output     json.RawMessage
	CostUSD    float64
	DurationMS int64
	Wall       time.Duration
}

// RunStructured runs one call on the planner's shared concurrency slots (LLM_MAX_CONCURRENCY).
// There is no heuristic fallback here: a missing or broken CLI, a failed or timed-out run, an
// is_error envelope or a missing structured_output is an *Error; no slot in time is ErrBusy.
func (c *Client) RunStructured(ctx context.Context, call StructuredCall) (*StructuredResult, error) {
	bin, missing, err := resolveClaude(c.cfg.ClaudeBin)
	if missing {
		return nil, &Error{Reason: fmt.Sprintf("claude CLI not found: %v", err)}
	}
	if err != nil {
		return nil, &Error{Reason: fmt.Sprintf("claude CLI is not usable: %v", err)}
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.sem }()
	if call.OnSlot != nil {
		if err := call.OnSlot(); err != nil {
			return nil, err
		}
	}
	return c.invoke(ctx, bin, call)
}

func (c *Client) invoke(parent context.Context, bin string, call StructuredCall) (*StructuredResult, error) {
	ctx, cancel := context.WithTimeout(parent, call.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, argsFor(call.Model, call.SystemPrompt, call.Schema)...)
	cmd.Stdin = strings.NewReader(call.Stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start)

	if errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
		return nil, &Error{Reason: fmt.Sprintf("claude CLI timed out after %d s%s",
			int(call.Timeout.Seconds()), stderrSuffix(stderr.String()))}
	}
	if parent.Err() != nil {
		return nil, &Error{Reason: fmt.Sprintf("request cancelled while claude was running: %v", parent.Err())}
	}

	var env claudeEnvelope
	envErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env)

	if runErr != nil {
		if envErr == nil && env.IsError {
			return nil, &Error{Reason: fmt.Sprintf("claude reported an error (subtype=%s, %v): %s",
				orNone(env.Subtype), runErr, excerpt(env.Result))}
		}
		detail := stderr.String()
		if strings.TrimSpace(detail) == "" {
			detail = stdout.String()
		}
		return nil, &Error{Reason: fmt.Sprintf("claude CLI failed (%v): %s", runErr, excerpt(detail))}
	}
	if envErr != nil {
		return nil, &Error{Reason: fmt.Sprintf("claude CLI returned non-JSON output (%v): %s",
			envErr, excerpt(stdout.String()))}
	}
	if env.IsError {
		return nil, &Error{Reason: fmt.Sprintf("claude reported an error (subtype=%s): %s",
			orNone(env.Subtype), excerpt(env.Result))}
	}
	raw := bytes.TrimSpace(env.StructuredOutput)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, &Error{Reason: fmt.Sprintf("claude response has no structured_output (subtype=%s)%s",
			orNone(env.Subtype), resultSuffix(env.Result))}
	}
	return &StructuredResult{Output: raw, CostUSD: env.TotalCostUSD, DurationMS: env.DurationMS, Wall: wall}, nil
}

func (c *Client) runClaude(parent context.Context, bin, prompt string) (*plannerOutput, map[string]interface{}, error) {
	res, err := c.invoke(parent, bin, StructuredCall{
		Model:        c.cfg.ClaudeModel,
		SystemPrompt: c.systemPrompt,
		Schema:       c.plannerSchema,
		Stdin:        prompt,
		Timeout:      time.Duration(c.cfg.ClaudeTimeoutSeconds) * time.Second,
	})
	if err != nil {
		return nil, nil, err
	}
	out, err := decodePlannerOutput(res.Output)
	if err != nil {
		return nil, nil, &Error{Reason: fmt.Sprintf("malformed structured_output: %v: %s", err, excerpt(string(res.Output)))}
	}
	meta := map[string]interface{}{
		"source":      SourceClaude,
		"model":       c.cfg.ClaudeModel,
		"duration_ms": res.DurationMS,
		"cost_usd":    res.CostUSD,
		"wall_ms":     res.Wall.Milliseconds(),
	}
	return out, meta, nil
}

// PlanSpec is the plan shape the LLM produces (planner output "plan", interpretation
// hypotheses' "test.plan"); ToExperimentPlan turns it into an ExperimentPlan for the validator.
type PlanSpecSelector struct {
	GroupID   string   `json:"group_id,omitempty"`
	NeuronIDs []string `json:"neuron_ids,omitempty"`
}

type PlanSpecActivation struct {
	GroupID   string   `json:"group_id,omitempty"`
	NeuronIDs []string `json:"neuron_ids,omitempty"`
	RateHz    float64  `json:"rate_hz"`
}

type PlanSpec struct {
	ExperimentType string               `json:"experiment_type"`
	Activation     []PlanSpecActivation `json:"activation"`
	Silencing      []PlanSpecSelector   `json:"silencing"`
	Readout        []PlanSpecSelector   `json:"readout"`
	DurationMs     float64              `json:"duration_ms"`
	Repeats        int                  `json:"repeats"`
	BaseSeed       uint64               `json:"base_seed"`
}

type plannerOutput struct {
	Status           string    `json:"status"`
	Message          string    `json:"message"`
	UnresolvedFields []string  `json:"unresolved_fields"`
	Plan             *PlanSpec `json:"plan"`
}

func decodePlannerOutput(raw []byte) (*plannerOutput, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out plannerOutput
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	switch ParseStatus(out.Status) {
	case StatusReady, StatusNeedsInput, StatusUnsupported:
	default:
		return nil, fmt.Errorf("status %q is not one of ready, needs_input, unsupported", out.Status)
	}
	if strings.TrimSpace(out.Message) == "" {
		return nil, errors.New("message is empty")
	}
	if ParseStatus(out.Status) == StatusReady && out.Plan == nil {
		return nil, errors.New(`status "ready" without a plan`)
	}
	return &out, nil
}

func (c *Client) interpret(out *plannerOutput, meta map[string]interface{}, datasetID, lang string) (*ParseResult, error) {
	status := ParseStatus(out.Status)
	if status != StatusReady {
		return &ParseResult{
			Status:           status,
			UnresolvedFields: out.UnresolvedFields,
			Message:          out.Message,
			LLMMetadata:      meta,
		}, nil
	}

	plan := ToExperimentPlan(out.Plan, datasetID, lang)
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, &Error{Reason: fmt.Sprintf("cannot encode planner plan: %v", err)}
	}
	valRes, err := c.validator.ValidateRawJSON(raw)
	if err != nil {
		return nil, &Error{Reason: fmt.Sprintf("planner plan failed validation: %v", err)}
	}
	return &ParseResult{
		Status:          StatusReady,
		Plan:            valRes.Plan,
		ResolvedPlan:    valRes.ResolvedPlan,
		DefaultsApplied: valRes.DefaultsApplied,
		Message:         out.Message,
		LLMMetadata:     meta,
	}, nil
}

// ToExperimentPlan converts an LLM plan into the canonical plan (dataset defaults to flywire_630).
func ToExperimentPlan(p *PlanSpec, datasetID, lang string) *domain.ExperimentPlan {
	if datasetID == "" {
		datasetID = "flywire_630"
	}
	plan := &domain.ExperimentPlan{
		SchemaVersion:  "1.0",
		DatasetID:      datasetID,
		ModelID:        "shiu_lif_rust",
		ExperimentType: p.ExperimentType,
		Activation:     []domain.ActivationSpec{},
		Silencing:      []domain.SilencingSpec{},
		Readout:        []domain.ReadoutSpec{},
		DurationMs:     p.DurationMs,
		Repeats:        p.Repeats,
		BaseSeed:       p.BaseSeed,
		ReportLanguage: lang,
	}
	for _, a := range p.Activation {
		plan.Activation = append(plan.Activation, domain.ActivationSpec{
			Selector: domain.Selector{GroupID: a.GroupID, NeuronIDs: a.NeuronIDs},
			RateHz:   a.RateHz,
		})
	}
	for _, s := range p.Silencing {
		plan.Silencing = append(plan.Silencing, domain.SilencingSpec{
			Selector: domain.Selector{GroupID: s.GroupID, NeuronIDs: s.NeuronIDs},
		})
	}
	for _, r := range p.Readout {
		plan.Readout = append(plan.Readout, domain.ReadoutSpec{
			Selector: domain.Selector{GroupID: r.GroupID, NeuronIDs: r.NeuronIDs},
		})
	}
	return plan
}

// ---- heuristic parser (only used when the claude CLI is not installed) ----

var (
	rateRe     = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*(?:hz|гц)`)
	durationRe = regexp.MustCompile(`(\d+(?:[.,]\d+)?)\s*(?:ms\b|мс)`)
)

// heuristic keyword -> group mapping for stimulation targets.
var heuristicActivation = []struct {
	keywords []string
	groupID  string
}{
	{[]string{"sugar", "сахар", "сладк"}, "sugar_grn"},
	{[]string{"bitter", "горьк"}, "bitter_grn"},
	{[]string{"ir94e"}, "ir94e"},
}

func (c *Client) heuristicParse(prompt, datasetID, lang string) *ParseResult {
	lower := strings.ToLower(prompt)
	meta := map[string]interface{}{"source": SourceHeuristic, "model": "keyword-heuristic"}

	var groups []string
	for _, h := range heuristicActivation {
		matched := strings.Contains(lower, h.groupID)
		for _, kw := range h.keywords {
			if strings.Contains(lower, kw) {
				matched = true
			}
		}
		if matched {
			if _, ok := c.registry.GroupsMap[h.groupID]; ok {
				groups = append(groups, h.groupID)
			}
		}
	}
	if len(groups) == 0 {
		return &ParseResult{
			Status:           StatusNeedsInput,
			UnresolvedFields: []string{"activation.selector"},
			Message:          "The keyword parser could not identify which neurons to stimulate. Name a registered group (sugar_grn, bitter_grn, ir94e) or FlyWire root IDs.",
			LLMMetadata:      meta,
		}
	}
	if len(groups) > 2 {
		groups = groups[:2]
	}

	expType := "single"
	for _, kw := range []string{"compare", "silenc", "сравни", "подавл", "заглуш"} {
		if strings.Contains(lower, kw) {
			expType = "compare_silencing"
		}
	}

	rate := 50.0
	if m := rateRe.FindStringSubmatch(lower); m != nil {
		if v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64); err == nil {
			rate = v
		}
	}
	duration := 100.0
	if m := durationRe.FindStringSubmatch(lower); m != nil {
		if v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64); err == nil {
			duration = v
		}
	}

	if datasetID == "" {
		datasetID = "flywire_630"
	}
	plan := &domain.ExperimentPlan{
		SchemaVersion:  "1.0",
		DatasetID:      datasetID,
		ModelID:        "shiu_lif_rust",
		ExperimentType: expType,
		Silencing:      []domain.SilencingSpec{},
		Readout:        []domain.ReadoutSpec{{Selector: domain.Selector{GroupID: "mn9"}}},
		DurationMs:     duration,
		Repeats:        1,
		BaseSeed:       42,
		ReportLanguage: lang,
	}
	for _, g := range groups {
		plan.Activation = append(plan.Activation, domain.ActivationSpec{
			Selector: domain.Selector{GroupID: g}, RateHz: rate,
		})
	}
	if expType == "compare_silencing" {
		plan.Silencing = append(plan.Silencing, domain.SilencingSpec{
			Selector: domain.Selector{GroupID: "demo_silencing"},
		})
	}

	raw, err := json.Marshal(plan)
	if err != nil {
		return &ParseResult{Status: StatusInvalid, Message: err.Error(), LLMMetadata: meta}
	}
	valRes, err := c.validator.ValidateRawJSON(raw)
	if err != nil {
		return &ParseResult{Status: StatusInvalid, Message: err.Error(), LLMMetadata: meta}
	}
	return &ParseResult{
		Status:          StatusReady,
		Plan:            valRes.Plan,
		ResolvedPlan:    valRes.ResolvedPlan,
		DefaultsApplied: valRes.DefaultsApplied,
		Message:         "Plan built by the keyword parser (Claude planner unavailable); review every parameter before running.",
		LLMMetadata:     meta,
	}
}

// ---- helpers ----

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output)"
	}
	if utf8.RuneCountInString(s) <= excerptLimit {
		return s
	}
	r := []rune(s)
	return string(r[:excerptLimit-1]) + "…"
}

func stderrSuffix(stderr string) string {
	if strings.TrimSpace(stderr) == "" {
		return ""
	}
	return "; stderr: " + excerpt(stderr)
}

func resultSuffix(result string) string {
	if strings.TrimSpace(result) == "" {
		return ""
	}
	return ": " + excerpt(result)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
