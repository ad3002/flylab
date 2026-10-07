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
	// WorkerDigestWait is how long the interpretation worker waits for the digest slot before
	// failing the request with DIGEST_BUSY (it has no client waiting on an HTTP response).
	WorkerDigestWait = 3 * time.Minute
	// ProxyReadTimeout is nginx's proxy_read_timeout (deploy/nginx-flylab.aglabx.com.conf).
	// The worst case of one request must stay below it, or nginx answers an HTML 504 while the
	// server is still working; WorstCase is tested against it.
	ProxyReadTimeout = 300 * time.Second
	// stderrTail is how much of a failing flysim's stderr reaches digest_error.
	stderrTail = 800
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

// WorstCase is the longest one interpretation-related request can take: GET .../digest waits
// for the digest slot and runs flysim digest. Since v4 POST .../interpretation only enqueues
// (the Claude call runs on the queue worker), so it no longer depends on the Claude timeout.
func WorstCase(cfg *config.Config) time.Duration {
	return DigestWait + DigestTimeout
}

// LLMError is a failed Claude interpretation (HTTP 502 LLM_ERROR); nothing is cached.
type LLMError struct{ Reason string }

func (e *LLMError) Error() string { return "claude interpretation failed: " + e.Reason }

// RateLimitError means an hourly interpretation limit is reached (HTTP 429 RATE_LIMITED).
type RateLimitError struct {
	Scope      string // user | ip | global
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

	budget        *llm.Budget
	userLimiter   *ratelimit.Window
	ipLimiter     *ratelimit.Window
	globalLimiter *ratelimit.Window
	limitMu       sync.Mutex

	digestSem  chan struct{}
	digestWait time.Duration
	jobMu      sync.Mutex
	jobLocks   map[string]*sync.Mutex

	// Interpretation queue (contract v4 section 4): see queue.go.
	enqueueMu sync.Mutex
	reqIPMu   sync.Mutex
	reqIP     map[int64]string
	wake      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	statusMu  sync.Mutex
	// persistErr is sticky (a request whose final status could not be saved stays 'running'
	// until a restart marks it WORKER_INTERRUPTED); claimErr clears on the next good claim.
	persistErr error
	claimErr   error

	// PersistRetryDelays are the waits between attempts to save a request's final status;
	// PollInterval is how often idle workers look at the queue; WorkerDigestWait is the
	// worker's digest slot wait. Tests shorten them.
	PersistRetryDelays []time.Duration
	PollInterval       time.Duration
	WorkerDigestWait   time.Duration
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
		"INTERPRET_CONCURRENCY":                cfg.InterpretConcurrency,
		"INTERPRET_QUEUE_MAX":                  cfg.InterpretQueueMax,
	} {
		if v < 1 {
			return nil, fmt.Errorf("%s must be >= 1 (got %d)", name, v)
		}
	}
	if strings.TrimSpace(cfg.ClaudeInterpretModel) == "" {
		return nil, errors.New("CLAUDE_INTERPRET_MODEL is empty")
	}
	if llmClient == nil || llmClient.Budget() == nil {
		return nil, errors.New("the interpretation service needs the claude client and its AI budget")
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
		budget:             llmClient.Budget(),
		userLimiter:        ratelimit.New(cfg.InterpretRateLimitPerHour, time.Hour),
		ipLimiter:          ratelimit.New(cfg.InterpretRateLimitPerIPPerHour, time.Hour),
		globalLimiter:      ratelimit.New(cfg.InterpretGlobalLimitPerHour, time.Hour),
		digestSem:          make(chan struct{}, 1),
		digestWait:         DigestWait,
		jobLocks:           map[string]*sync.Mutex{},
		reqIP:              map[int64]string{},
		wake:               make(chan struct{}, 1),
		PersistRetryDelays: []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second},
		PollInterval:       time.Second,
		WorkerDigestWait:   WorkerDigestWait,
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

// limitChecks are the hourly windows of one request; clientIP "" (unknown after a restart)
// skips the per-address window.
func (s *Service) limitChecks(userID int64, clientIP string) []limitCheck {
	checks := []limitCheck{{s.userLimiter, strconv.FormatInt(userID, 10), llm.ScopeUser}}
	if clientIP != "" {
		checks = append(checks, limitCheck{s.ipLimiter, clientIP, llm.ScopeIP})
	}
	return append(checks, limitCheck{s.globalLimiter, "global", llm.ScopeGlobal})
}

func checkLimits(checks []limitCheck, now time.Time) error {
	for _, c := range checks {
		if ok, retry := c.w.Check(c.key, now); !ok {
			return &RateLimitError{Scope: c.scope, Limit: c.w.Limit(), RetryAfter: retry}
		}
	}
	return nil
}

// checkHourlyLimits checks the hourly limits at enqueue time without using any quota.
func (s *Service) checkHourlyLimits(userID int64, clientIP string) error {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	return checkLimits(s.limitChecks(userID, clientIP), time.Now())
}

// commitHourlyLimits re-checks and records one unit in each window. The worker calls it once
// the digest is built, right before Claude starts, so digest failures cost no quota.
func (s *Service) commitHourlyLimits(userID int64, clientIP string) error {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	now := time.Now()
	checks := s.limitChecks(userID, clientIP)
	if err := checkLimits(checks, now); err != nil {
		return err
	}
	for _, c := range checks {
		c.w.Record(c.key, now)
	}
	return nil
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
func (s *Service) ensureGraph(ctx context.Context, job *domain.Job, force bool, wait time.Duration) error {
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

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case s.digestSem <- struct{}{}:
	case <-timer.C:
		return &DigestBusyError{Wait: wait}
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
	return s.digestWith(ctx, job, force, s.digestWait)
}

func (s *Service) digestWith(ctx context.Context, job *domain.Job, force bool, wait time.Duration) (*Digest, error) {
	unlock := s.lockJob(job.JobID)
	defer unlock()
	warnings, err := s.manifestWarnings(job.ArtifactsDir)
	if err != nil {
		return nil, err
	}
	if err := s.ensureGraph(ctx, job, force, wait); err != nil {
		return nil, err
	}
	in := DigestInput{
		JobID: job.JobID, Title: job.Title, ArtifactsDir: job.ArtifactsDir,
		Registry: s.registry, Annotations: s.annotations, Proxies: s.proxies, ExtraWarnings: warnings,
	}
	d, err := BuildDigest(in)
	var ge *GraphError
	if err != nil && errors.As(err, &ge) {
		if rerr := s.ensureGraph(ctx, job, true, wait); rerr != nil {
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

// CorruptError means the stored interpretation exists but cannot be read (HTTP 500
// INTERPRETATION_CORRUPT, or result_error next to an active/failed request).
type CorruptError struct{ Reason string }

func (e *CorruptError) Error() string { return e.Reason }

// Stored returns the stored interpretation: storage.ErrNotFound when there is none,
// *CorruptError when the row cannot be read or parsed.
func (s *Service) Stored(jobID string) (*Response, error) {
	resp, _, err := s.loadStored(jobID)
	return resp, err
}

// loadStored also returns the row's created_at (nil when unknown), which decides whether a
// failed request is newer than the result.
func (s *Service) loadStored(jobID string) (*Response, *time.Time, error) {
	rec, err := s.store.GetInterpretation(jobID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, nil, err
		}
		return nil, nil, &CorruptError{Reason: fmt.Sprintf("stored interpretation of %s cannot be read: %v", jobID, err)}
	}
	created := rec.CreatedAt
	var st stored
	if err := json.Unmarshal([]byte(rec.ResultJSON), &st); err != nil || st.Interpretation == nil {
		if err == nil {
			err = errors.New("no interpretation object")
		}
		return nil, &created, &CorruptError{Reason: fmt.Sprintf("stored interpretation of %s is corrupt: %v", jobID, err)}
	}
	if !json.Valid([]byte(rec.DigestJSON)) {
		return nil, &created, &CorruptError{Reason: fmt.Sprintf("stored digest of %s is corrupt (invalid JSON)", jobID)}
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
	}, &created, nil
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= 500 {
		return s
	}
	return string(r[:499]) + "…"
}
