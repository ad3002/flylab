package interpret

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/ratelimit"
	"github.com/ad3002/flylab/internal/storage"
)

const (
	// DigestTimeout bounds one `flysim digest` run (graph load from cache + BFS + one pass over
	// the edges for readout inputs; about 10 s on the production server).
	DigestTimeout = 60 * time.Second
	// DigestWait is how long a request waits for the single digest slot (then 503 DIGEST_BUSY).
	DigestWait = 20 * time.Second
	// ClaudeSlotWait is how long a request waits for a Claude slot (then 503 LLM_BUSY).
	ClaudeSlotWait = 30 * time.Second
	// ProxyReadTimeout is nginx's proxy_read_timeout (deploy/nginx-flylab.aglabx.com.conf).
	// The worst case of one POST .../interpretation must stay below it, or nginx answers an
	// HTML 504 while the server is still working; WorstCase is tested against it.
	ProxyReadTimeout = 300 * time.Second
	// stderrTail is how much of a failing flysim's stderr reaches digest_error.
	stderrTail = 800

	userInFlightRetry = 5 * time.Second
)

// DigestError means the deterministic digest cannot be produced for a succeeded run (missing
// spikes.parquet, flysim digest failure, corrupt rates.csv...). Shown as digest_error.
type DigestError struct{ Reason string }

func (e *DigestError) Error() string { return "digest cannot be built: " + e.Reason }

// DigestBusyError means the single digest slot stayed taken (HTTP 503 DIGEST_BUSY): a temporary
// condition, not a problem with the run.
type DigestBusyError struct{ Wait time.Duration }

func (e *DigestBusyError) Error() string {
	return fmt.Sprintf("another run's digest is being computed and no digest slot became free within %s; try again shortly", e.Wait)
}

// WorstCase is the longest one POST .../interpretation can take with these settings: digest
// slot wait + flysim digest + Claude slot wait + the Claude call.
func WorstCase(cfg *config.Config) time.Duration {
	return DigestWait + DigestTimeout + ClaudeSlotWait + time.Duration(cfg.ClaudeInterpretTimeoutSeconds)*time.Second
}

// LLMError is a failed Claude interpretation (HTTP 502 LLM_ERROR); nothing is cached.
type LLMError struct{ Reason string }

func (e *LLMError) Error() string { return "claude interpretation failed: " + e.Reason }

// RateLimitError means an interpretation budget is exhausted (HTTP 429 RATE_LIMITED).
type RateLimitError struct {
	Scope      string // user | ip | global | user_in_flight
	Limit      int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	retry := ratelimit.RetrySeconds(e.RetryAfter)
	switch e.Scope {
	case llm.ScopeIP:
		return fmt.Sprintf("interpretation limit of %d per hour from this network address reached; retry in %d s", e.Limit, retry)
	case llm.ScopeGlobal:
		return fmt.Sprintf("the server-wide interpretation budget of %d per hour is used up; retry in %d s", e.Limit, retry)
	case llm.ScopeUserInFlight:
		return fmt.Sprintf("an interpretation for this account is already being generated; wait for it to finish (retry in %d s)", retry)
	default:
		return fmt.Sprintf("interpretation limit of %d per hour reached; retry in %d s", e.Limit, retry)
	}
}

// Meta describes how an interpretation was produced.
type Meta struct {
	Model      string    `json:"model"`
	CostUSD    float64   `json:"cost_usd"`
	DurationMS int64     `json:"duration_ms"`
	CreatedAt  time.Time `json:"created_at"`
	Language   string    `json:"language"`
	Cached     bool      `json:"cached"`
}

// Response is the body of POST/GET /api/v1/jobs/{id}/interpretation.
type Response struct {
	Interpretation   *Interpretation   `json:"interpretation"`
	Digest           json.RawMessage   `json:"digest"`
	Meta             Meta              `json:"meta"`
	Disclaimer       string            `json:"disclaimer"`
	EvidenceWarnings []EvidenceWarning `json:"evidence_warnings"`
}

// stored is what result_json holds.
type stored struct {
	Interpretation   *Interpretation   `json:"interpretation"`
	EvidenceWarnings []EvidenceWarning `json:"evidence_warnings"`
}

type Service struct {
	cfg         *config.Config
	store       *storage.Store
	registry    *contracts.Registry
	validator   *contracts.Validator
	llm         *llm.Client
	annotations *Annotations
	proxies     []Proxy
	schema      string
	prompts     map[string]string

	userLimiter   *ratelimit.Window
	ipLimiter     *ratelimit.Window
	globalLimiter *ratelimit.Window
	limitMu       sync.Mutex
	inFlight      map[int64]bool

	digestSem  chan struct{}
	digestWait time.Duration
	jobMu      sync.Mutex
	jobLocks   map[string]*sync.Mutex
}

// NewService loads data/annotations_630.tsv (missing = annotations_ready false; malformed =
// error) and registry/readout_proxies.json (required) and builds the prompt and schema.
func NewService(cfg *config.Config, store *storage.Store, registry *contracts.Registry,
	validator *contracts.Validator, llmClient *llm.Client) (*Service, error) {
	for name, v := range map[string]int{
		"CLAUDE_INTERPRET_TIMEOUT_SECONDS":     cfg.ClaudeInterpretTimeoutSeconds,
		"INTERPRET_RATE_LIMIT_PER_HOUR":        cfg.InterpretRateLimitPerHour,
		"INTERPRET_RATE_LIMIT_PER_IP_PER_HOUR": cfg.InterpretRateLimitPerIPPerHour,
		"INTERPRET_GLOBAL_LIMIT_PER_HOUR":      cfg.InterpretGlobalLimitPerHour,
	} {
		if v < 1 {
			return nil, fmt.Errorf("%s must be >= 1 (got %d)", name, v)
		}
	}
	if strings.TrimSpace(cfg.ClaudeInterpretModel) == "" {
		return nil, errors.New("CLAUDE_INTERPRET_MODEL is empty")
	}
	ann, err := LoadAnnotations(AnnotationsPath(cfg))
	if err != nil {
		return nil, err
	}
	proxies, err := LoadProxies(cfg.RegistryDir)
	if err != nil {
		return nil, err
	}
	schema, err := BuildSchema(registry, validator.Limits())
	if err != nil {
		return nil, fmt.Errorf("build interpretation schema: %w", err)
	}
	prompts := map[string]string{}
	for lang := range Languages {
		prompts[lang] = BuildSystemPrompt(registry, validator.Limits(), lang)
	}
	return &Service{
		cfg: cfg, store: store, registry: registry, validator: validator, llm: llmClient,
		annotations: ann, proxies: proxies, schema: schema, prompts: prompts,
		userLimiter:   ratelimit.New(cfg.InterpretRateLimitPerHour, time.Hour),
		ipLimiter:     ratelimit.New(cfg.InterpretRateLimitPerIPPerHour, time.Hour),
		globalLimiter: ratelimit.New(cfg.InterpretGlobalLimitPerHour, time.Hour),
		inFlight:      map[int64]bool{},
		digestSem:     make(chan struct{}, 1),
		digestWait:    DigestWait,
		jobLocks:      map[string]*sync.Mutex{},
	}, nil
}

// AnnotationsPath is where the derived annotation table lives.
func AnnotationsPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "annotations_630.tsv")
}

func (s *Service) Annotations() *Annotations { return s.annotations }

// SetDigestWait changes how long a request waits for the digest slot (tests shorten it).
func (s *Service) SetDigestWait(d time.Duration) { s.digestWait = d }
func (s *Service) SystemPrompt(lang string) string {
	return s.prompts[lang]
}
func (s *Service) Schema() string { return s.schema }

type limitCheck struct {
	w     *ratelimit.Window
	key   string
	scope string
}

func (s *Service) limitChecks(userID int64, clientIP string) []limitCheck {
	return []limitCheck{
		{s.userLimiter, strconv.FormatInt(userID, 10), llm.ScopeUser},
		{s.ipLimiter, clientIP, llm.ScopeIP},
		{s.globalLimiter, "global", llm.ScopeGlobal},
	}
}

func checkLimits(checks []limitCheck, now time.Time) error {
	for _, c := range checks {
		if ok, retry := c.w.Check(c.key, now); !ok {
			return &RateLimitError{Scope: c.scope, Limit: c.w.Limit(), RetryAfter: retry}
		}
	}
	return nil
}

// admit checks the hourly limits and takes the account's in-flight slot, without using any
// quota. commit re-checks the limits and records one unit in each window; it is called only
// once the digest is built and a Claude slot is held, so digest failures and LLM_BUSY are free.
func (s *Service) admit(userID int64, clientIP string) (release func(), commit func() error, err error) {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	if s.inFlight[userID] {
		return nil, nil, &RateLimitError{Scope: llm.ScopeUserInFlight, Limit: 1, RetryAfter: userInFlightRetry}
	}
	checks := s.limitChecks(userID, clientIP)
	if err := checkLimits(checks, time.Now()); err != nil {
		return nil, nil, err
	}
	s.inFlight[userID] = true
	release = func() {
		s.limitMu.Lock()
		delete(s.inFlight, userID)
		s.limitMu.Unlock()
	}
	commit = func() error {
		s.limitMu.Lock()
		defer s.limitMu.Unlock()
		now := time.Now()
		if err := checkLimits(checks, now); err != nil {
			return err
		}
		for _, c := range checks {
			c.w.Record(c.key, now)
		}
		return nil
	}
	return release, commit, nil
}

func (s *Service) lockJob(jobID string) func() {
	s.jobMu.Lock()
	m, ok := s.jobLocks[jobID]
	if !ok {
		m = &sync.Mutex{}
		s.jobLocks[jobID] = m
	}
	s.jobMu.Unlock()
	m.Lock()
	return m.Unlock
}

// manifestWarnings compares the run's manifest.json with the server's dataset manifest. A
// different connectome is an error (the graph digest would describe another graph); a run
// without manifest.json gets a visible warning.
func (s *Service) manifestWarnings(dir string) ([]string, error) {
	type files struct {
		Files map[string]struct {
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	load := func(path string) (*files, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var f files
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("%s cannot be parsed: %v", filepath.Base(path), err)
		}
		return &f, nil
	}
	current, err := load(filepath.Join(s.cfg.DataDir, "dataset_manifest.json"))
	if err != nil {
		return nil, &DigestError{Reason: fmt.Sprintf("the server's dataset manifest cannot be loaded: %v", err)}
	}
	run, err := load(filepath.Join(dir, "manifest.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []string{"This run's artifact directory has no manifest.json; the graph facts assume it used the server's current connectome files."}, nil
		}
		return nil, &DigestError{Reason: fmt.Sprintf("the run's manifest.json cannot be used: %v", err)}
	}
	for _, key := range []string{"completeness", "connectivity"} {
		if run.Files[key].SHA256 != current.Files[key].SHA256 {
			return nil, &DigestError{Reason: fmt.Sprintf("the run used %s file %q but the server now has %q; graph facts would describe a different connectome",
				key, run.Files[key].SHA256, current.Files[key].SHA256)}
		}
	}
	return nil, nil
}

// ensureGraph makes sure digest_graph.json exists in the run's artifact directory, running
// `flysim digest` on the stored spikes.parquet when it does not (pre-v3 runs, or the first
// request). force deletes an existing file first (used when the cached file cannot be used).
func (s *Service) ensureGraph(ctx context.Context, job *domain.Job, force bool) error {
	dir := job.ArtifactsDir
	out := filepath.Join(dir, "digest_graph.json")
	if force {
		if err := os.Remove(out); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return &DigestError{Reason: fmt.Sprintf("old digest_graph.json cannot be removed: %v", err)}
		}
	}
	if _, err := os.Stat(out); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return &DigestError{Reason: fmt.Sprintf("digest_graph.json cannot be accessed: %v", err)}
	}

	spikes := filepath.Join(dir, "spikes.parquet")
	if _, err := os.Stat(spikes); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &DigestError{Reason: fmt.Sprintf("spikes.parquet is missing for this succeeded run (%s), so latency and graph facts cannot be computed", dir)}
		}
		return &DigestError{Reason: fmt.Sprintf("spikes.parquet cannot be accessed: %v", err)}
	}
	plan := filepath.Join(dir, "resolved_plan.json")
	if _, err := os.Stat(plan); err != nil {
		return &DigestError{Reason: fmt.Sprintf("resolved_plan.json of this run cannot be accessed: %v", err)}
	}

	timer := time.NewTimer(s.digestWait)
	defer timer.Stop()
	select {
	case s.digestSem <- struct{}{}:
	case <-timer.C:
		return &DigestBusyError{Wait: s.digestWait}
	case <-ctx.Done():
		return &DigestError{Reason: fmt.Sprintf("request cancelled while waiting for a digest slot: %v", ctx.Err())}
	}
	defer func() { <-s.digestSem }()

	runCtx, cancel := context.WithTimeout(ctx, DigestTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.cfg.FlysimBin, "digest",
		"--resolved-plan", plan,
		"--spikes", spikes,
		"--manifest", filepath.Join(s.cfg.DataDir, "dataset_manifest.json"),
		"--cache-dir", filepath.Join(s.cfg.DataDir, "cache"),
		"--output", out,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		reason := fmt.Sprintf("flysim digest failed (%v)", err)
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			reason = fmt.Sprintf("flysim digest timed out after %s", DigestTimeout)
		}
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			if len(tail) > stderrTail {
				tail = "…" + tail[len(tail)-stderrTail:]
			}
			reason += ": " + tail
		}
		return &DigestError{Reason: reason}
	}
	if _, err := os.Stat(out); err != nil {
		return &DigestError{Reason: fmt.Sprintf("flysim digest exited successfully but wrote no digest_graph.json: %v", err)}
	}
	return nil
}

// Digest returns the deterministic digest of a succeeded job, generating digest_graph.json
// first when needed. A cached digest_graph.json that cannot be used (written by an older
// flysim, unreadable, or not matching the run) is derived data: it is recomputed once from
// spikes.parquet, and unless it was merely outdated the digest carries a warning saying so.
// Errors are *DigestError or *DigestBusyError.
func (s *Service) Digest(ctx context.Context, job *domain.Job, force bool) (*Digest, error) {
	unlock := s.lockJob(job.JobID)
	defer unlock()
	warnings, err := s.manifestWarnings(job.ArtifactsDir)
	if err != nil {
		return nil, err
	}
	if err := s.ensureGraph(ctx, job, force); err != nil {
		return nil, err
	}
	in := DigestInput{
		JobID: job.JobID, Title: job.Title, ArtifactsDir: job.ArtifactsDir,
		Registry: s.registry, Annotations: s.annotations, Proxies: s.proxies, ExtraWarnings: warnings,
	}
	d, err := BuildDigest(in)
	var ge *GraphError
	if err != nil && errors.As(err, &ge) {
		if rerr := s.ensureGraph(ctx, job, true); rerr != nil {
			if de, ok := rerr.(*DigestError); ok {
				return nil, &DigestError{Reason: fmt.Sprintf("the cached digest_graph.json could not be used (%s) and recomputing it failed: %s", ge.Reason, de.Reason)}
			}
			return nil, rerr
		}
		if !ge.Outdated {
			in.ExtraWarnings = append(append([]string{}, warnings...), fmt.Sprintf(
				"The cached graph digest of this run could not be used (%s); it was recomputed from spikes.parquet for this digest.", ge.Reason))
		}
		d, err = BuildDigest(in)
	}
	if err != nil {
		return nil, &DigestError{Reason: err.Error()}
	}
	return d, nil
}

// Stored returns the stored interpretation (storage.ErrNotFound when there is none).
func (s *Service) Stored(jobID string) (*Response, error) {
	rec, err := s.store.GetInterpretation(jobID)
	if err != nil {
		return nil, err
	}
	var st stored
	if err := json.Unmarshal([]byte(rec.ResultJSON), &st); err != nil || st.Interpretation == nil {
		if err == nil {
			err = errors.New("no interpretation object")
		}
		return nil, fmt.Errorf("stored interpretation of %s is corrupt: %v", jobID, err)
	}
	if !json.Valid([]byte(rec.DigestJSON)) {
		return nil, fmt.Errorf("stored digest of %s is corrupt (invalid JSON)", jobID)
	}
	if st.EvidenceWarnings == nil {
		st.EvidenceWarnings = []EvidenceWarning{}
	}
	// Deterministic checks of test plans and confidence also apply to interpretations stored
	// before these checks existed.
	qualityWarnings(st.Interpretation, []byte(rec.DigestJSON), rec.Language)
	return &Response{
		Interpretation: st.Interpretation,
		Digest:         json.RawMessage(rec.DigestJSON),
		Meta: Meta{Model: rec.Model, CostUSD: rec.CostUSD, DurationMS: rec.DurationMS,
			CreatedAt: rec.CreatedAt, Language: rec.Language, Cached: true},
		Disclaimer:       Disclaimers[rec.Language],
		EvidenceWarnings: st.EvidenceWarnings,
	}, nil
}

// Generate builds the digest, calls Claude, validates and stores the interpretation.
// Errors: *RateLimitError, *DigestError, *LLMError, llm.ErrBusy, or a storage error.
//
// Once admitted, the work no longer depends on the client connection: ctx is detached from
// cancellation (its values are kept), so a reload or a closed tab does not kill a paid Claude
// call, and the stored result is found by the next GET. The timeouts still bound every step.
// Quota is used only when a Claude slot is held (see admit).
func (s *Service) Generate(ctx context.Context, job *domain.Job, userID int64, clientIP, lang string, regenerate bool) (*Response, error) {
	release, commit, err := s.admit(userID, clientIP)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx = context.WithoutCancel(ctx)

	digest, err := s.Digest(ctx, job, false)
	if err != nil {
		return nil, err
	}
	digestJSON, err := json.Marshal(digest)
	if err != nil {
		return nil, &DigestError{Reason: fmt.Sprintf("digest cannot be encoded: %v", err)}
	}

	res, err := s.llm.RunStructured(ctx, llm.StructuredCall{
		Model:        s.cfg.ClaudeInterpretModel,
		SystemPrompt: s.prompts[lang],
		Schema:       s.schema,
		Stdin:        BuildUserMessage(job.Title, job.Prompt, lang, digestJSON),
		Timeout:      time.Duration(s.cfg.ClaudeInterpretTimeoutSeconds) * time.Second,
		OnSlot:       commit,
	})
	if err != nil {
		var le *llm.Error
		if errors.As(err, &le) {
			return nil, &LLMError{Reason: le.Reason}
		}
		return nil, err
	}
	out, err := decodeOutput(res.Output)
	if err != nil {
		return nil, &LLMError{Reason: fmt.Sprintf("malformed structured_output: %v: %s", err, excerpt(string(res.Output)))}
	}
	if err := validateTests(out, s.validator, s.store, lang); err != nil {
		return nil, err
	}
	qualityWarnings(out, digestJSON, lang)
	warnings := evidenceWarnings(out, digestJSON, digest.References)

	resultJSON, err := json.Marshal(stored{Interpretation: out, EvidenceWarnings: warnings})
	if err != nil {
		return nil, fmt.Errorf("interpretation cannot be encoded: %w", err)
	}
	now := time.Now().UTC()
	rec := &storage.Interpretation{
		JobID: job.JobID, Language: lang, Model: s.cfg.ClaudeInterpretModel, CreatedAt: now,
		CostUSD: res.CostUSD, DurationMS: res.DurationMS, DigestJSON: string(digestJSON), ResultJSON: string(resultJSON),
	}
	if err := s.store.SaveInterpretation(rec); err != nil {
		return nil, err
	}
	return &Response{
		Interpretation: out,
		Digest:         json.RawMessage(digestJSON),
		Meta: Meta{Model: rec.Model, CostUSD: rec.CostUSD, DurationMS: rec.DurationMS,
			CreatedAt: now, Language: lang, Cached: false},
		Disclaimer:       Disclaimers[lang],
		EvidenceWarnings: warnings,
	}, nil
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= 500 {
		return s
	}
	return string(r[:499]) + "…"
}
