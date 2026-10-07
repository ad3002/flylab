package llm_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/storage"
)

func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// fakeClaude returns the absolute path of the fake CLI, making sure it is executable
// (a checkout or rsync may drop the mode bit).
func fakeClaude(t *testing.T) string {
	t.Helper()
	p := filepath.Join(projectRoot(t), "internal", "llm", "testdata", "fake_claude.sh")
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatalf("chmod fake claude: %v", err)
	}
	return p
}

// neuronSet loads the real v630 completeness table once per test binary.
var (
	neuronSetOnce sync.Once
	neuronSetVal  *contracts.NeuronIDSet
	neuronSetErr  error
)

func neuronSet(t *testing.T) *contracts.NeuronIDSet {
	t.Helper()
	neuronSetOnce.Do(func() { neuronSetVal, neuronSetErr = contracts.LoadNeuronIDs(filepath.Join(projectRoot(t), "data")) })
	if neuronSetErr != nil {
		t.Fatalf("load neuron ids: %v", neuronSetErr)
	}
	return neuronSetVal
}

// newStoreBudget returns a fresh store and the AI budget on it (global, per user).
func newStoreBudget(t *testing.T, global, user float64) (*storage.Store, *llm.Budget) {
	t.Helper()
	store, err := storage.OpenStore(filepath.Join(t.TempDir(), "llm.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	b, err := llm.NewBudget(store, global, user)
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	return store, b
}

func newClient(t *testing.T, mutate func(*config.Config)) *llm.Client {
	t.Helper()
	_, b := newStoreBudget(t, 1000, 1000)
	return newClientWithBudget(t, mutate, b)
}

func newClientWithBudget(t *testing.T, mutate func(*config.Config), budget *llm.Budget) *llm.Client {
	t.Helper()
	root := projectRoot(t)
	reg, err := contracts.LoadRegistry(filepath.Join(root, "registry"))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	val, err := contracts.NewValidator(filepath.Join(root, "contracts"), reg)
	if err != nil {
		t.Fatalf("new validator: %v", err)
	}
	val.SetNeuronIDs(neuronSet(t))
	cfg := &config.Config{
		ClaudeBin:             fakeClaude(t),
		ClaudeModel:           "claude-sonnet-5-5",
		ClaudeTimeoutSeconds:  10,
		LLMMaxConcurrency:     2,
		ParseRateLimitPerHour: 1000,

		ParseRateLimitPerIPPerHour: 1000,
		ParseGlobalLimitPerHour:    1000,
	}
	if mutate != nil {
		mutate(cfg)
	}
	c, err := llm.NewClient(cfg, val, reg, budget)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func setMode(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv("FAKE_CLAUDE_MODE", mode)
	logPath := filepath.Join(t.TempDir(), "claude.log")
	t.Setenv("FAKE_CLAUDE_LOG", logPath)
	return logPath
}

func wantLLMError(t *testing.T, err error, substr string) {
	t.Helper()
	var le *llm.Error
	if !errors.As(err, &le) {
		t.Fatalf("expected *llm.Error, got %T: %v", err, err)
	}
	if !strings.Contains(le.Reason, substr) {
		t.Fatalf("error reason %q does not contain %q", le.Reason, substr)
	}
}

func TestClaudeSuccessReturnsValidatedPlan(t *testing.T) {
	logPath := setMode(t, "ready_single")
	c := newClient(t, nil)

	prompt := "Using FlyWire, stimulate sugar GRNs at 80 Hz for 200 ms, two repeats, seed 7"
	res, err := c.ParsePrompt(context.Background(), 1, prompt, "flywire_630", "en")
	if err != nil {
		t.Fatalf("ParsePrompt: %v", err)
	}
	if res.Status != llm.StatusReady {
		t.Fatalf("expected ready, got %s: %s", res.Status, res.Message)
	}
	if res.LLMError != nil {
		t.Fatalf("claude result must not carry llm_error, got %q", *res.LLMError)
	}
	if res.LLMMetadata["source"] != "claude" || res.LLMMetadata["model"] != "claude-sonnet-5-5" {
		t.Fatalf("unexpected llm_metadata %v", res.LLMMetadata)
	}
	if res.LLMMetadata["duration_ms"] != int64(1234) || res.LLMMetadata["cost_usd"] != 0.0123 {
		t.Fatalf("duration/cost not taken from the envelope: %v", res.LLMMetadata)
	}
	p := res.Plan
	if p == nil || p.ExperimentType != "single" || p.DurationMs != 200 || p.Repeats != 2 || p.BaseSeed != 7 {
		t.Fatalf("plan does not match planner output: %+v", p)
	}
	if len(p.Activation) != 1 || p.Activation[0].Selector.GroupID != "sugar_grn" || p.Activation[0].RateHz != 80 {
		t.Fatalf("activation mismatch: %+v", p.Activation)
	}
	if res.ResolvedPlan == nil || len(res.ResolvedPlan.Activation[0].NeuronIDs) != 21 || len(res.ResolvedPlan.ReadoutNeuronIDs) != 2 {
		t.Fatalf("plan was not resolved by the validator: %+v", res.ResolvedPlan)
	}

	// Invocation: exact flags, prompt via stdin and never as an argument.
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake log: %v", err)
	}
	log := string(logData)
	for _, want := range []string{
		"ARG:-p\n", "ARG:--model\nARG:claude-sonnet-5-5\n", "ARG:--tools\nARG:\n",
		"ARG:--no-session-persistence\n", "ARG:--strict-mcp-config\n", "ARG:--setting-sources\nARG:\n",
		"ARG:--output-format\nARG:json\n", "ARG:--system-prompt\n", "ARG:--json-schema\n",
		"STDIN:Plan the FlyLab experiment described in the untrusted request below.\n",
		"\n" + prompt + "\n</untrusted_request id=\"",
		"The untrusted request has ended.",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("invocation log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "ARG:"+prompt) {
		t.Fatalf("prompt must go through stdin, not argv")
	}
	if !boundaryRe.MatchString(log) {
		t.Fatalf("the prompt must sit in a nonce-bounded block:\n%s", log)
	}
}

var boundaryRe = regexp.MustCompile(`<untrusted_request id="([0-9a-f]{12})">\n[\s\S]*\n</untrusted_request id="([0-9a-f]{12})">`)

func TestClaudeCompareSilencing(t *testing.T) {
	setMode(t, "ready_compare")
	c := newClient(t, nil)
	res, err := c.ParsePrompt(context.Background(), 1, "Сравни сахарные GRN при 100 Гц с подавлением demo_silencing", "", "ru")
	if err != nil {
		t.Fatalf("ParsePrompt: %v", err)
	}
	if res.Status != llm.StatusReady || res.Plan.ExperimentType != "compare_silencing" {
		t.Fatalf("expected ready compare_silencing, got %s %+v", res.Status, res.Plan)
	}
	if len(res.ResolvedPlan.SilencingNeuronIDs) != 1 || res.Plan.ReportLanguage != "ru" || res.Plan.DatasetID != "flywire_630" {
		t.Fatalf("silencing/language/dataset not applied: %+v", res.Plan)
	}
}

func TestClaudeNeedsInputAndUnsupportedPassThrough(t *testing.T) {
	setMode(t, "needs_input")
	c := newClient(t, nil)
	res, err := c.ParsePrompt(context.Background(), 1, "Turn off inhibitory neurons and see what happens", "", "en")
	if err != nil {
		t.Fatalf("ParsePrompt: %v", err)
	}
	if res.Status != llm.StatusNeedsInput || len(res.UnresolvedFields) != 1 || res.UnresolvedFields[0] != "activation.selector" {
		t.Fatalf("needs_input not passed through: %+v", res)
	}
	if res.Message != "Which neurons should be stimulated?" || res.Plan != nil {
		t.Fatalf("unexpected message/plan: %q %+v", res.Message, res.Plan)
	}

	setMode(t, "unsupported")
	res, err = c.ParsePrompt(context.Background(), 1, "Show me how the fly will walk", "", "en")
	if err != nil {
		t.Fatalf("ParsePrompt: %v", err)
	}
	if res.Status != llm.StatusUnsupported || !strings.Contains(res.Message, "whole-animal") {
		t.Fatalf("unsupported not passed through: %+v", res)
	}
}

func TestFlyWirePromptIsNotRejectedByKeywords(t *testing.T) {
	setMode(t, "ready_single")
	c := newClient(t, nil)
	res, err := c.ParsePrompt(context.Background(), 1, "FlyWire: activate sugar neurons of the fly at 80 Hz", "", "en")
	if err != nil {
		t.Fatalf("ParsePrompt: %v", err)
	}
	if res.Status != llm.StatusReady {
		t.Fatalf("a prompt mentioning FlyWire/fly must reach the planner, got %s: %s", res.Status, res.Message)
	}
}

func TestClaudeFailuresAreLLMErrors(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{"is_error", "claude reported an error (subtype=error_during_execution): API Error: 529 overloaded"},
		{"is_error_exit", "model refused"},
		{"malformed", "malformed structured_output"},
		{"bad_status", `status "maybe"`},
		{"missing_output", "no structured_output"},
		{"invalid_plan", "planner plan failed validation"},
		{"garbage", "non-JSON output"},
		{"nonzero", "authentication failed: please run /login"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			setMode(t, tc.mode)
			c := newClient(t, nil)
			res, err := c.ParsePrompt(context.Background(), 1, "stimulate sugar_grn", "", "en")
			if res != nil {
				t.Fatalf("a failed planner call must not return a result (no heuristic fallback), got %+v", res)
			}
			wantLLMError(t, err, tc.want)
		})
	}
}

func TestClaudeTimeout(t *testing.T) {
	setMode(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SLEEP", "10")
	c := newClient(t, func(cfg *config.Config) { cfg.ClaudeTimeoutSeconds = 1 })
	start := time.Now()
	_, err := c.ParsePrompt(context.Background(), 1, "stimulate sugar_grn", "", "en")
	wantLLMError(t, err, "timed out after 1 s")
	if time.Since(start) > 6*time.Second {
		t.Fatalf("timeout did not stop the process promptly: %v", time.Since(start))
	}
}

func TestMissingBinaryFallsBackToHeuristicWithLLMError(t *testing.T) {
	c := newClient(t, func(cfg *config.Config) { cfg.ClaudeBin = "flylab-no-such-claude-binary" })
	if c.Ready() {
		t.Fatalf("Ready() must be false for a missing binary")
	}
	res, err := c.ParsePrompt(context.Background(), 1, "FlyWire: compare sugar GRN at 100 Hz for 200 ms with silencing", "", "en")
	if err != nil {
		t.Fatalf("missing binary must fall back, got error %v", err)
	}
	if res.LLMError == nil || !strings.Contains(*res.LLMError, "claude CLI not found") {
		t.Fatalf("fallback result must carry llm_error, got %v", res.LLMError)
	}
	if res.LLMMetadata["source"] != "heuristic_fallback" {
		t.Fatalf("expected source heuristic_fallback, got %v", res.LLMMetadata)
	}
	if res.Status != llm.StatusReady || res.Plan.ExperimentType != "compare_silencing" ||
		res.Plan.Activation[0].RateHz != 100 || res.Plan.DurationMs != 200 {
		t.Fatalf("heuristic plan mismatch: %s %+v", res.Status, res.Plan)
	}

	// An absolute path that does not exist is the same "not found" case.
	c2 := newClient(t, func(cfg *config.Config) { cfg.ClaudeBin = "/nonexistent/dir/claude" })
	res2, err := c2.ParsePrompt(context.Background(), 1, "bitter neurons 30 Hz", "", "en")
	if err != nil || res2.LLMError == nil || res2.Plan.Activation[0].Selector.GroupID != "bitter_grn" {
		t.Fatalf("absolute missing path must fall back with llm_error, got %+v err=%v", res2, err)
	}
}

func TestHeuristicDoesNotGuessStimulusTarget(t *testing.T) {
	c := newClient(t, func(cfg *config.Config) { cfg.ClaudeBin = "flylab-no-such-claude-binary" })
	res, err := c.ParsePrompt(context.Background(), 1, "Turn off inhibitory neurons and see what happens", "", "en")
	if err != nil {
		t.Fatalf("ParsePrompt: %v", err)
	}
	if res.Status != llm.StatusNeedsInput || res.Plan != nil || res.LLMError == nil {
		t.Fatalf("heuristic must ask instead of guessing, got %+v", res)
	}
}

func TestNonExecutableBinaryIsError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := newClient(t, func(cfg *config.Config) { cfg.ClaudeBin = p })
	res, err := c.ParsePrompt(context.Background(), 1, "stimulate sugar_grn", "", "en")
	if res != nil {
		t.Fatalf("a broken install must not fall back silently, got %+v", res)
	}
	wantLLMError(t, err, "not usable")
}

func TestPromptLengthLimit(t *testing.T) {
	setMode(t, "ready_single")
	c := newClient(t, nil)
	res, err := c.ParsePrompt(context.Background(), 1, strings.Repeat("я", 4001), "", "en")
	if err != nil {
		t.Fatalf("ParsePrompt: %v", err)
	}
	if res.Status != llm.StatusInvalid || !strings.Contains(res.Message, "4000") {
		t.Fatalf("expected invalid for 4001 chars, got %+v", res)
	}
	// 4000 Cyrillic characters (8000 bytes) are within the limit.
	res, err = c.ParsePrompt(context.Background(), 1, strings.Repeat("я", 4000), "", "en")
	if err != nil || res.Status != llm.StatusReady {
		t.Fatalf("4000 characters must be accepted, got %+v err=%v", res, err)
	}
}

func TestRateLimitPerUser(t *testing.T) {
	setMode(t, "needs_input")
	c := newClient(t, func(cfg *config.Config) { cfg.ParseRateLimitPerHour = 2 })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := c.ParsePrompt(ctx, 1, "x", "", "en"); err != nil {
			t.Fatalf("call %d within limit failed: %v", i, err)
		}
	}
	_, err := c.ParsePrompt(ctx, 1, "x", "", "en")
	var rl *llm.RateLimitError
	if !errors.As(err, &rl) || rl.Limit != 2 || rl.RetryAfter <= 0 {
		t.Fatalf("third call must be rate limited, got %v", err)
	}
	if _, err := c.ParsePrompt(ctx, 2, "x", "", "en"); err != nil {
		t.Fatalf("another user must have an independent limit, got %v", err)
	}
}

func TestConcurrencySlotBusy(t *testing.T) {
	setMode(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SLEEP", "3")
	c := newClient(t, func(cfg *config.Config) { cfg.LLMMaxConcurrency = 1 })
	c.BusyWait = 200 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := c.ParsePrompt(context.Background(), 1, "first", "", "en")
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for c.InFlight() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("first call never took the slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err := c.ParsePrompt(context.Background(), 2, "second", "", "en")
	if !errors.Is(err, llm.ErrBusy) {
		t.Fatalf("second call must get ErrBusy, got %v", err)
	}
	<-done
}

func TestSystemPromptAndSchemaAreGenerated(t *testing.T) {
	c := newClient(t, nil)
	sp := c.SystemPrompt()
	for _, want := range []string{"sugar_grn", "Sugar Gustatory Receptor Neurons, 21 neurons", "ir94e",
		"10 to 1000 ms", "1 to 3", "from 0 to 200 Hz", `"single", "compare_silencing"`, "walking", "NOT by itself"} {
		if !strings.Contains(sp, want) {
			t.Fatalf("system prompt lacks %q", want)
		}
	}
	schema := c.PlannerSchema()
	for _, want := range []string{`"needs_input"`, `"maximum":1000`, `"maximum":200`, `"bitter_grn"`, `"maxItems":2`} {
		if !strings.Contains(schema, want) {
			t.Fatalf("planner schema lacks %s: %s", want, schema)
		}
	}
}

// TestRealClaudePlanner calls the real CLI. It runs only with FLYLAB_CLAUDE_E2E=1 (costs money).
func TestRealClaudePlanner(t *testing.T) {
	if os.Getenv("FLYLAB_CLAUDE_E2E") != "1" {
		t.Skip("set FLYLAB_CLAUDE_E2E=1 to call the real claude CLI")
	}
	c := newClient(t, func(cfg *config.Config) {
		cfg.ClaudeBin = "claude"
		cfg.ClaudeTimeoutSeconds = 120
	})
	res, err := c.ParsePrompt(context.Background(), 1,
		"Using FlyWire data, stimulate sugar GRNs at 120 Hz for 300 ms and compare with silencing demo_silencing, readout MN9", "", "en")
	if err != nil {
		t.Fatalf("real claude: %v", err)
	}
	t.Logf("status=%s message=%q meta=%v", res.Status, res.Message, res.LLMMetadata)
	if res.Status != llm.StatusReady || res.Plan.ExperimentType != "compare_silencing" ||
		res.Plan.Activation[0].RateHz != 120 || res.Plan.DurationMs != 300 {
		t.Fatalf("unexpected real planner result: %s %+v", res.Status, res.Plan)
	}
}

// A bare CLAUDE_BIN name (the default "claude") whose PATH entry exists but cannot run must be
// a 502-style *Error, not "not found" + heuristic fallback: exec.LookPath reports such a
// candidate as plain ErrNotFound.
func TestBareNameNonExecutableOnPathIsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PATH", dir)
	c := newClient(t, func(cfg *config.Config) { cfg.ClaudeBin = "claude" })
	if c.Ready() {
		t.Fatalf("a non-executable claude must not be reported ready")
	}
	res, err := c.ParsePrompt(context.Background(), 1, "stimulate sugar_grn", "", "en")
	if res != nil {
		t.Fatalf("a broken install must not fall back to the heuristic parser, got %+v", res)
	}
	wantLLMError(t, err, "not usable")
	wantLLMError(t, err, "is not executable")
}

func TestBareNameDanglingSymlinkOnPathIsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "gone", "cli.js"), filepath.Join(dir, "claude")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("PATH", dir)
	c := newClient(t, func(cfg *config.Config) { cfg.ClaudeBin = "claude" })
	res, err := c.ParsePrompt(context.Background(), 1, "stimulate sugar_grn", "", "en")
	if res != nil {
		t.Fatalf("a dangling symlink must not fall back to the heuristic parser, got %+v", res)
	}
	wantLLMError(t, err, "symlink whose target cannot be opened")
}

func TestBareNameAbsentFromPathStillFallsBack(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	c := newClient(t, func(cfg *config.Config) { cfg.ClaudeBin = "claude" })
	res, err := c.ParsePrompt(context.Background(), 1, "stimulate sugar_grn at 50 Hz", "", "en")
	if err != nil {
		t.Fatalf("an absent CLI must use the heuristic parser, got %v", err)
	}
	if res.LLMError == nil || !strings.Contains(*res.LLMError, "claude CLI not found") || res.LLMMetadata["source"] != llm.SourceHeuristic {
		t.Fatalf("fallback must be visible: %+v", res)
	}
}

func TestRateLimitPerIPAcrossAccounts(t *testing.T) {
	setMode(t, "needs_input")
	c := newClient(t, func(cfg *config.Config) { cfg.ParseRateLimitPerIPPerHour = 2 })
	ctx := context.Background()
	// Throwaway accounts 1, 2 from the same address share the address budget.
	for i, uid := range []int64{1, 2} {
		if _, err := c.ParsePromptFrom(ctx, uid, "203.0.113.7", "x", "", "en"); err != nil {
			t.Fatalf("call %d within the IP limit failed: %v", i, err)
		}
	}
	_, err := c.ParsePromptFrom(ctx, 3, "203.0.113.7", "x", "", "en")
	var rl *llm.RateLimitError
	if !errors.As(err, &rl) || rl.Scope != llm.ScopeIP || rl.Limit != 2 || rl.RetryAfter <= 0 {
		t.Fatalf("a fresh account from the same address must hit the IP limit, got %v", err)
	}
	if !strings.Contains(err.Error(), "network address") {
		t.Fatalf("message must name the scope: %v", err)
	}
	if _, err := c.ParsePromptFrom(ctx, 3, "198.51.100.1", "x", "", "en"); err != nil {
		t.Fatalf("another address must have its own budget, got %v", err)
	}
}

func TestGlobalParseBudget(t *testing.T) {
	setMode(t, "needs_input")
	c := newClient(t, func(cfg *config.Config) { cfg.ParseGlobalLimitPerHour = 2 })
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := c.ParsePromptFrom(ctx, int64(10+i), fmt.Sprintf("192.0.2.%d", i), "x", "", "en"); err != nil {
			t.Fatalf("call %d within the global budget failed: %v", i, err)
		}
	}
	_, err := c.ParsePromptFrom(ctx, 99, "192.0.2.99", "x", "", "en")
	var rl *llm.RateLimitError
	if !errors.As(err, &rl) || rl.Scope != llm.ScopeGlobal || rl.Limit != 2 {
		t.Fatalf("the global budget must stop new accounts and addresses, got %v", err)
	}
}

func TestRefusedParseDoesNotConsumeOtherBudgets(t *testing.T) {
	setMode(t, "needs_input")
	c := newClient(t, func(cfg *config.Config) { cfg.ParseRateLimitPerHour = 1; cfg.ParseGlobalLimitPerHour = 2 })
	ctx := context.Background()
	if _, err := c.ParsePromptFrom(ctx, 1, "a", "x", "", "en"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	for i := 0; i < 3; i++ {
		var rl *llm.RateLimitError
		if _, err := c.ParsePromptFrom(ctx, 1, "a", "x", "", "en"); !errors.As(err, &rl) || rl.Scope != llm.ScopeUser {
			t.Fatalf("user over its limit must get scope=user, got %v", err)
		}
	}
	// The refused calls above must not have used the global budget (1 of 2 used).
	if _, err := c.ParsePromptFrom(ctx, 2, "b", "x", "", "en"); err != nil {
		t.Fatalf("refused calls consumed the global budget: %v", err)
	}
}

func TestOneParseInFlightPerUser(t *testing.T) {
	setMode(t, "sleep")
	t.Setenv("FAKE_CLAUDE_SLEEP", "2")
	c := newClient(t, nil) // 2 global slots
	done := make(chan error, 1)
	go func() {
		_, err := c.ParsePrompt(context.Background(), 1, "first", "", "en")
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for c.InFlight() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("first call never took a slot")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, err := c.ParsePrompt(context.Background(), 1, "second from the same account", "", "en")
	var rl *llm.RateLimitError
	if !errors.As(err, &rl) || rl.Scope != llm.ScopeUserInFlight || rl.RetryAfter <= 0 {
		t.Fatalf("a second concurrent parse of one account must be refused, got %v", err)
	}
	if c.InFlight() != 1 {
		t.Fatalf("the refused call must not hold the second planner slot, in flight=%d", c.InFlight())
	}
	<-done
}
