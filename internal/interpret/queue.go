package interpret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/storage"
)

// The persisted interpretation queue (contract v4 section 4). POST .../interpretation only
// enqueues; INTERPRET_CONCURRENCY background workers take the oldest queued request, build
// the digest, call Claude (outside the planner's slots), store the result and mark the request.

// Request error codes stored in interpretation_requests.error_code.
const (
	CodeWorkerInterrupted   = "WORKER_INTERRUPTED"
	CodeBudgetExhausted     = "AI_BUDGET_EXHAUSTED"
	CodeBudgetCheckFailed   = "AI_BUDGET_CHECK_FAILED"
	CodeUsageRecordFailed   = "AI_USAGE_RECORD_FAILED"
	CodeDigestError         = "DIGEST_ERROR"
	CodeDigestBusy          = "DIGEST_BUSY"
	CodeRateLimited         = "RATE_LIMITED"
	CodeLLMError            = "LLM_ERROR"
	CodeStoreError          = "STORE_ERROR"
	CodeJobNotFound         = "JOB_NOT_FOUND"
	CodeJobNotCompleted     = "JOB_NOT_COMPLETED"
	QueueFullRetryAfterSecs = 30
)

// RequestView is the "request" object of the interpretation responses.
type RequestView struct {
	ID         int64      `json:"id"`
	JobID      string     `json:"job_id"`
	Status     string     `json:"status"`
	Position   int        `json:"position"`
	Language   string     `json:"language"`
	Regenerate bool       `json:"regenerate"`
	QueuedAt   time.Time  `json:"queued_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	// ErrorCode / ErrorMessage are set for a failed request, null otherwise.
	ErrorCode    *string `json:"error_code"`
	ErrorMessage *string `json:"error_message"`
}

// StateResponse is the body of GET .../interpretation and of POST's 200/202 answers: state,
// request, and the v3 result fields (interpretation, digest, meta, disclaimer,
// evidence_warnings) when a result exists. ResultError is set instead of the result fields when
// a stored result exists next to an active or failed request but cannot be read.
type StateResponse struct {
	State   string       `json:"state"`
	Request *RequestView `json:"request"`
	*Response
	ResultError *string `json:"result_error,omitempty"`
}

// InProgressError: the account already has a queued or running interpretation of another run
// (HTTP 409 INTERPRETATION_IN_PROGRESS).
type InProgressError struct {
	JobID     string
	RequestID int64
	Status    string
}

func (e *InProgressError) Error() string {
	return fmt.Sprintf("an interpretation of run %s is already %s for this account; wait for it to finish before requesting another one",
		e.JobID, e.Status)
}

// QueueFullError: INTERPRET_QUEUE_MAX requests are already queued (HTTP 503 QUEUE_FULL).
type QueueFullError struct{ Max, Queued int }

func (e *QueueFullError) Error() string {
	return fmt.Sprintf("the interpretation queue is full (%d requests waiting, limit %d); try again in a few minutes", e.Queued, e.Max)
}

// view re-reads the request so its status and position are one consistent snapshot.
func (s *Service) view(stale *storage.InterpretationRequest) (*RequestView, error) {
	r, pos, err := s.store.RequestWithPosition(stale.ID)
	if err != nil {
		return nil, err
	}
	return viewOf(r, pos), nil
}

func viewOf(r *storage.InterpretationRequest, pos int) *RequestView {
	return &RequestView{
		ID: r.ID, JobID: r.JobID, Status: r.Status, Position: pos, Language: r.Language, Regenerate: r.Regenerate,
		QueuedAt: r.QueuedAt, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		ErrorCode: r.ErrorCode, ErrorMessage: r.ErrorMessage,
	}
}

// ActiveRequest returns the job's queued or running request, or nil.
func (s *Service) ActiveRequest(jobID string) (*RequestView, error) {
	latest, err := s.store.LatestInterpretationRequest(jobID)
	if err != nil || latest == nil || !latest.Active() {
		return nil, err
	}
	return s.view(latest)
}

// Enqueue adds a request for job, or returns the job's active request. Errors:
// *InProgressError, *llm.BudgetError, *llm.BudgetCheckError, *RateLimitError, *QueueFullError,
// or a storage error.
func (s *Service) Enqueue(job *domain.Job, userID int64, clientIP, lang string, regenerate bool) (*RequestView, error) {
	s.enqueueMu.Lock()
	defer s.enqueueMu.Unlock()
	active, err := s.store.ActiveInterpretationRequestForUser(userID)
	if err != nil {
		return nil, err
	}
	if active != nil {
		if active.JobID == job.JobID {
			return s.view(active)
		}
		return nil, &InProgressError{JobID: active.JobID, RequestID: active.ID, Status: active.Status}
	}
	if err := s.budget.Check(userID); err != nil {
		return nil, err
	}
	if err := s.checkHourlyLimits(userID, clientIP); err != nil {
		return nil, err
	}
	queued, _, err := s.store.InterpretationQueueCounts()
	if err != nil {
		return nil, err
	}
	if queued >= s.cfg.InterpretQueueMax {
		return nil, &QueueFullError{Max: s.cfg.InterpretQueueMax, Queued: queued}
	}
	req, err := s.store.InsertInterpretationRequest(job.JobID, userID, lang, regenerate, time.Now())
	if err != nil {
		return nil, err
	}
	s.reqIPMu.Lock()
	s.reqIP[req.ID] = clientIP
	s.reqIPMu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return s.view(req)
}

// State answers GET .../interpretation. storage.ErrNotFound when the job has neither a request
// nor a result; *CorruptError when only an unreadable result exists.
func (s *Service) State(jobID string) (*StateResponse, error) {
	latest, err := s.store.LatestInterpretationRequest(jobID)
	if err != nil {
		return nil, err
	}
	pos := 0
	if latest != nil {
		// Status and position from one snapshot (the worker may have just claimed it).
		if latest, pos, err = s.store.RequestWithPosition(latest.ID); err != nil {
			return nil, err
		}
	}
	resp, resultAt, rerr := s.loadStored(jobID)
	var corrupt *CorruptError
	if rerr != nil && !errors.Is(rerr, storage.ErrNotFound) && !errors.As(rerr, &corrupt) {
		return nil, rerr
	}
	hasResult := !errors.Is(rerr, storage.ErrNotFound)
	state := storage.InterpretationState(latest, hasResult, resultAt)
	switch state {
	case "":
		if latest != nil {
			return nil, &CorruptError{Reason: fmt.Sprintf("interpretation request %d of %s is marked %s but no interpretation is stored",
				latest.ID, jobID, latest.Status)}
		}
		return nil, storage.ErrNotFound
	case storage.StateReady:
		if rerr != nil {
			return nil, rerr
		}
		return &StateResponse{State: state, Response: resp}, nil
	}
	out := &StateResponse{State: state, Request: viewOf(latest, pos)}
	if rerr == nil {
		out.Response = resp
	} else if hasResult {
		msg := rerr.Error()
		out.ResultError = &msg
	}
	return out, nil
}

// QueueCounts reports the queue for /capabilities.
func (s *Service) QueueCounts() (queued, running int, err error) {
	return s.store.InterpretationQueueCounts()
}

// Start marks requests left running by a previous process failed WORKER_INTERRUPTED (a
// failure here is a startup error) and starts INTERPRET_CONCURRENCY workers.
func (s *Service) Start() error {
	n, err := s.store.FailInterruptedInterpretationRequests(time.Now())
	if err != nil {
		return fmt.Errorf("mark interpretations interrupted by the previous shutdown as failed: %w", err)
	}
	if n > 0 {
		log.Printf("[interpret] %d interpretation(s) interrupted by the previous shutdown marked failed (WORKER_INTERRUPTED)", n)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	for i := 0; i < s.cfg.InterpretConcurrency; i++ {
		s.wg.Add(1)
		go s.loop()
	}
	return nil
}

// Stop stops the workers; a running Claude call is killed and its request marked
// WORKER_INTERRUPTED.
func (s *Service) Stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	s.wg.Wait()
}

// LastError is nil while the queue worker is healthy, else what users must see (surfaced as
// capabilities.interpret_worker_error and a degraded /health).
func (s *Service) LastError() error {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return errors.Join(s.persistErr, s.claimErr)
}

func (s *Service) loop() {
	defer s.wg.Done()
	for {
		if s.ctx.Err() != nil {
			return
		}
		req, err := s.store.ClaimNextInterpretationRequest(time.Now())
		s.statusMu.Lock()
		if err != nil {
			s.claimErr = fmt.Errorf("the interpretation queue cannot be read, queued interpretations will not start: %w", err)
		} else {
			s.claimErr = nil
		}
		s.statusMu.Unlock()
		if err != nil {
			log.Printf("[interpret] claim: %v", err)
		}
		if err == nil && req != nil {
			s.process(req)
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case <-time.After(s.PollInterval):
		}
	}
}

func (s *Service) takeIP(id int64) string {
	s.reqIPMu.Lock()
	defer s.reqIPMu.Unlock()
	ip := s.reqIP[id]
	delete(s.reqIP, id)
	return ip
}

func (s *Service) process(req *storage.InterpretationRequest) {
	code, msg := s.run(req)
	if code == "" {
		return
	}
	if s.ctx.Err() != nil && code != CodeWorkerInterrupted {
		code, msg = CodeWorkerInterrupted, "The server shut down while this interpretation was running; request it again. ("+msg+")"
	}
	s.persist(req, func() error { return s.store.FailInterpretationRequest(req.ID, code, msg, time.Now()) },
		"failed ("+code+")")
}

// persist saves a request's final status, retrying briefly; when every attempt fails the
// worker is degraded (LastError) until restart.
func (s *Service) persist(req *storage.InterpretationRequest, save func() error, what string) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = save(); err == nil {
			return nil
		}
		if attempt >= len(s.PersistRetryDelays) {
			break
		}
		log.Printf("[interpret] saving request %d as %s failed (attempt %d): %v", req.ID, what, attempt+1, err)
		time.Sleep(s.PersistRetryDelays[attempt])
	}
	perr := fmt.Errorf("interpretation request %d of %s finished as %s but its status could not be saved (it shows as running until the service restarts): %w",
		req.ID, req.JobID, what, err)
	log.Printf("[interpret] %v", perr)
	s.statusMu.Lock()
	s.persistErr = errors.Join(s.persistErr, perr)
	s.statusMu.Unlock()
	return perr
}

// run does one request; it returns ("", "") once the result and the succeeded status are
// stored, else the error code and user-facing message to mark the request failed with.
func (s *Service) run(req *storage.InterpretationRequest) (string, string) {
	ctx := s.ctx
	ip := s.takeIP(req.ID)
	job, err := s.store.GetJob(req.JobID)
	if errors.Is(err, storage.ErrNotFound) {
		return CodeJobNotFound, fmt.Sprintf("run %s no longer exists", req.JobID)
	}
	if err != nil {
		return CodeStoreError, fmt.Sprintf("run %s cannot be loaded: %v", req.JobID, err)
	}
	if job.Status != domain.StatusSucceeded {
		return CodeJobNotCompleted, fmt.Sprintf("run %s is %s; only succeeded runs can be interpreted", job.JobID, job.Status)
	}
	// The budget is checked again right before the interpretation starts.
	if err := s.budget.Check(req.UserID); err != nil {
		var be *llm.BudgetError
		if errors.As(err, &be) {
			return CodeBudgetExhausted, be.Error()
		}
		return CodeBudgetCheckFailed, err.Error()
	}
	digest, err := s.digestWith(ctx, job, false, s.WorkerDigestWait)
	if err != nil {
		var (
			de *DigestError
			db *DigestBusyError
		)
		switch {
		case errors.As(err, &db):
			return CodeDigestBusy, db.Error()
		case errors.As(err, &de):
			return CodeDigestError, de.Error()
		default:
			return CodeDigestError, err.Error()
		}
	}
	digestJSON, err := json.Marshal(digest)
	if err != nil {
		return CodeDigestError, fmt.Sprintf("digest cannot be encoded: %v", err)
	}
	if err := s.commitHourlyLimits(req.UserID, ip); err != nil {
		return CodeRateLimited, err.Error()
	}
	stdin, err := BuildUserMessage(job.Title, job.Prompt, req.Language, digestJSON)
	if err != nil {
		return CodeLLMError, (&LLMError{Reason: err.Error()}).Error()
	}
	res, err := s.llm.RunStructured(ctx, llm.StructuredCall{
		Model:        s.cfg.ClaudeInterpretModel,
		SystemPrompt: s.prompts[req.Language],
		Schema:       s.schema,
		Stdin:        stdin,
		Timeout:      time.Duration(s.cfg.ClaudeInterpretTimeoutSeconds) * time.Second,
		UserID:       req.UserID,
		Kind:         storage.UsageKindInterpreter,
	})
	if err != nil {
		var (
			ue *llm.UsageRecordError
			le *llm.Error
		)
		switch {
		case errors.As(err, &ue):
			return CodeUsageRecordFailed, ue.Error()
		case errors.As(err, &le):
			return CodeLLMError, (&LLMError{Reason: le.Reason}).Error()
		default:
			return CodeLLMError, (&LLMError{Reason: err.Error()}).Error()
		}
	}
	out, err := decodeOutput(res.Output)
	if err != nil {
		return CodeLLMError, (&LLMError{Reason: fmt.Sprintf("malformed structured_output: %v: %s", err, excerpt(string(res.Output)))}).Error()
	}
	if err := validateTests(out, s.validator, s.store, req.Language); err != nil {
		return CodeStoreError, err.Error()
	}
	qualityWarnings(out, digestJSON, req.Language)
	warnings := evidenceWarnings(out, digestJSON, digest.References)
	resultJSON, err := json.Marshal(stored{Interpretation: out, EvidenceWarnings: warnings})
	if err != nil {
		return CodeStoreError, fmt.Sprintf("interpretation cannot be encoded: %v", err)
	}
	rec := &storage.Interpretation{
		JobID: job.JobID, Language: req.Language, Model: s.cfg.ClaudeInterpretModel, CreatedAt: time.Now().UTC(),
		CostUSD: res.CostUSD, DurationMS: res.DurationMS, DigestJSON: string(digestJSON), ResultJSON: string(resultJSON),
	}
	if err := s.persist(req, func() error { return s.store.CompleteInterpretationRequest(req.ID, rec, time.Now()) }, "succeeded"); err != nil {
		return CodeStoreError, err.Error()
	}
	return "", ""
}
