package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/export"
	"github.com/ad3002/flylab/internal/interpret"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/ratelimit"
	"github.com/ad3002/flylab/internal/storage"
)

type Server struct {
	cfg       *config.Config
	store     *storage.Store
	validator *contracts.Validator
	registry  *contracts.Registry
	llmClient *llm.Client
	router    *http.ServeMux
	auth      *authGuard
	worker    WorkerStatus
	interp    *interpret.Service
}

// WorkerStatus is the background worker's health as the API reports it. LastError is nil
// while the worker is healthy; otherwise it describes a failure the user must see (e.g. a
// job's final status could not be saved), surfaced as /health 503 and capabilities.worker_error.
type WorkerStatus interface {
	LastError() error
}

// SetWorker connects the background worker's health to /health and /capabilities.
func (s *Server) SetWorker(w WorkerStatus) { s.worker = w }

func (s *Server) workerError() *string {
	if s.worker == nil {
		return nil
	}
	if err := s.worker.LastError(); err != nil {
		msg := err.Error()
		return &msg
	}
	return nil
}

func NewServer(
	cfg *config.Config,
	store *storage.Store,
	validator *contracts.Validator,
	registry *contracts.Registry,
	llmClient *llm.Client,
) *Server {
	s := &Server{
		cfg:       cfg,
		store:     store,
		validator: validator,
		registry:  registry,
		llmClient: llmClient,
		router:    http.NewServeMux(),
		auth:      newAuthGuard(cfg),
	}
	s.registerRoutes()
	return s
}

func (s *Server) Router() http.Handler {
	return s.middleware(s.router)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.New().String()
		}
		w.Header().Set("X-Request-ID", reqID)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key, X-Request-ID")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		if !s.csrfCheck(w, r) {
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes() {
	// Pages (contract section 1)
	s.router.HandleFunc("GET /{$}", s.handleIndex)
	s.router.HandleFunc("GET /app", s.handleApp)
	s.router.HandleFunc("GET /app/{path...}", s.handleApp)
	s.router.HandleFunc("GET /favicon.ico", s.handleFavicon)
	staticDir := filepath.Join(s.cfg.WebDir, "static")
	s.router.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))

	// Public base endpoints
	s.router.HandleFunc("GET /health", s.handleHealth)
	s.router.HandleFunc("GET /capabilities", s.handleCapabilities)
	s.router.HandleFunc("GET /datasets", s.handleDatasets)
	s.router.HandleFunc("GET /groups", s.handleGroups)
	s.router.HandleFunc("GET /neurons", s.handleNeurons)

	// Accounts (contract section 2)
	s.router.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	s.router.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	s.router.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)
	s.router.HandleFunc("GET /api/v1/me", s.requireAuth(s.handleMe))

	// API v1 and the legacy un-prefixed routes share handlers and auth rules (section 3).
	for _, prefix := range []string{"/api/v1", ""} {
		s.router.HandleFunc("POST "+prefix+"/plans/validate", s.handlePlanValidate)
		s.router.HandleFunc("POST "+prefix+"/plans/parse", s.requireAuth(s.handlePlanParse))
		s.router.HandleFunc("POST "+prefix+"/jobs", s.requireAuth(s.handleCreateJob))
		s.router.HandleFunc("GET "+prefix+"/jobs", s.requireAuth(s.handleListJobs))
		s.router.HandleFunc("GET "+prefix+"/jobs/{job_id}", s.requireAuth(s.handleGetJob))
		s.router.HandleFunc("POST "+prefix+"/jobs/{job_id}/cancel", s.requireAuth(s.handleCancelJob))
		s.router.HandleFunc("GET "+prefix+"/jobs/{job_id}/results", s.requireAuth(s.handleJobResults))
		s.router.HandleFunc("GET "+prefix+"/jobs/{job_id}/spikes", s.requireAuth(s.handleJobSpikes))
		s.router.HandleFunc("GET "+prefix+"/jobs/{job_id}/export", s.requireAuth(s.handleJobExport))
		s.router.HandleFunc("GET "+prefix+"/jobs/{job_id}/artifacts/{artifact_id}", s.requireAuth(s.handleJobArtifact))
		// v3 interpretation
		s.router.HandleFunc("GET "+prefix+"/jobs/{job_id}/digest", s.requireAuth(s.handleJobDigest))
		s.router.HandleFunc("GET "+prefix+"/jobs/{job_id}/interpretation", s.requireAuth(s.handleGetInterpretation))
		s.router.HandleFunc("POST "+prefix+"/jobs/{job_id}/interpretation", s.requireAuth(s.handlePostInterpretation))
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(s.cfg.WebDir, "templates", "index.html"))
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(s.cfg.WebDir, "templates", "app.html"))
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	http.ServeFile(w, r, filepath.Join(s.cfg.WebDir, "static", "img", "favicon.svg"))
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]interface{}) {
	reqID := w.Header().Get("X-Request-ID")
	errResp := domain.APIError{
		Error: domain.ErrorDetail{
			Code:      code,
			Message:   message,
			Details:   details,
			RequestID: reqID,
		},
	}
	s.writeJSON(w, status, errResp)
}

// interpretWorkerError is the interpretation queue worker's degraded state (nil when healthy).
func (s *Server) interpretWorkerError() *string {
	if s.interp == nil {
		return nil
	}
	if err := s.interp.LastError(); err != nil {
		msg := err.Error()
		return &msg
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	status, code := "ok", http.StatusOK
	werr := s.workerError()
	ierr := s.interpretWorkerError()
	if werr != nil || ierr != nil {
		status, code = "degraded", http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, map[string]interface{}{
		"status":                 status,
		"service":                "flylab",
		"domain":                 s.cfg.Domain,
		"version":                "1.0.0",
		"time":                   time.Now().UTC().Format(time.RFC3339),
		"worker_error":           werr,
		"interpret_worker_error": ierr,
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	_, dataErr := os.Stat(filepath.Join(s.cfg.DataDir, "dataset_manifest.json"))
	_, flysimErr := os.Stat(s.cfg.FlysimBin)

	annReady, annCount, interpModel := false, 0, s.cfg.ClaudeInterpretModel
	var queue interface{}
	var queueErr *string
	if s.interp != nil {
		annReady, annCount = s.interp.Annotations().Ready, s.interp.Annotations().Count()
		if queued, running, err := s.interp.QueueCounts(); err != nil {
			msg := err.Error()
			queueErr = &msg
		} else {
			queue = map[string]int{"queued": queued, "running": running}
		}
	}
	// The global AI budget: unreadable spend is reported, never shown as available.
	budgetAvailable := false
	var budgetErr *string
	if u, err := s.llmClient.Budget().GlobalUsage(); err != nil {
		msg := fmt.Sprintf("the AI budget cannot be checked: %v", err)
		budgetErr = &msg
	} else {
		budgetAvailable = !u.Exhausted
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"ai_budget_available":    budgetAvailable,
		"ai_budget_error":        budgetErr,
		"registration_mode":      s.cfg.RegistrationMode(),
		"interpret_queue":        queue,
		"interpret_queue_error":  queueErr,
		"interpret_worker_error": s.interpretWorkerError(),
		"annotations_ready":      annReady,
		"annotations_count":      annCount,
		"interpret_model":        interpModel,
		"interpret_ready":        s.interp != nil && s.llmClient.Ready(),
		"datasets_ready":         dataErr == nil,
		"worker_ready":           flysimErr == nil && s.workerError() == nil,
		"worker_error":           s.workerError(),
		"llm_provider":           "claude-cli",
		"llm_model":              s.cfg.ClaudeModel,
		"llm_ready":              s.llmClient.Ready(),
		"registration_open":      s.cfg.RegistrationOpen,
		"limits": map[string]interface{}{
			"max_wall_seconds":   s.cfg.MaxWallSeconds,
			"max_rss_bytes":      s.cfg.MaxRSSBytes,
			"max_artifact_bytes": s.cfg.MaxArtifactBytes,
			"max_prompt_chars":   llm.MaxPromptChars,
			"max_title_chars":    maxTitleChars,
		},
	})
}

func (s *Server) handleDatasets(w http.ResponseWriter, r *http.Request) {
	manifestPath := filepath.Join(s.cfg.DataDir, "dataset_manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "DATASET_MANIFEST_ERROR",
			fmt.Sprintf("Failed to load dataset manifest: %v", err), nil)
		return
	}
	var manifest interface{}
	if err := json.Unmarshal(data, &manifest); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "DATASET_MANIFEST_CORRUPT",
			fmt.Sprintf("dataset_manifest.json cannot be parsed: %v", err), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"datasets": []interface{}{manifest},
	})
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"groups": s.registry.Groups,
	})
}

func (s *Server) handleNeurons(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	var matches []map[string]interface{}

	for _, g := range s.registry.Groups {
		for _, id := range g.NeuronIDs {
			if query == "" || strings.Contains(id, query) || strings.Contains(strings.ToLower(g.NameEn), strings.ToLower(query)) {
				matches = append(matches, map[string]interface{}{
					"root_id":  id,
					"group_id": g.GroupID,
					"name_en":  g.NameEn,
				})
				if len(matches) >= 100 {
					break
				}
			}
		}
		if len(matches) >= 100 {
			break
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"neurons": matches,
		"total":   len(matches),
	})
}

func (s *Server) handlePlanParse(w http.ResponseWriter, r *http.Request, user *domain.User) {
	var body struct {
		Prompt         string `json:"prompt"`
		DatasetID      string `json:"dataset_id"`
		ReportLanguage string `json:"report_language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REQUEST_BODY", err.Error(), nil)
		return
	}
	if strings.TrimSpace(body.Prompt) == "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "EMPTY_PROMPT", "Prompt cannot be empty", nil)
		return
	}
	if body.ReportLanguage == "" {
		body.ReportLanguage = "en"
	}
	if body.ReportLanguage != "en" && body.ReportLanguage != "ru" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REPORT_LANGUAGE",
			fmt.Sprintf("report_language must be \"en\" or \"ru\" (got %q)", body.ReportLanguage), nil)
		return
	}
	if body.DatasetID != "" && body.DatasetID != "flywire_630" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "UNKNOWN_DATASET",
			fmt.Sprintf("dataset_id %q is not available (only flywire_630)", body.DatasetID), nil)
		return
	}

	res, err := s.llmClient.ParsePromptFrom(r.Context(), user.ID, clientIP(r), body.Prompt, body.DatasetID, body.ReportLanguage)
	if err != nil {
		var rl *llm.RateLimitError
		var le *llm.Error
		var ue *llm.UsageRecordError
		switch {
		case s.writeBudgetError(w, r, err):
		case errors.As(err, &ue):
			s.writeError(w, r, http.StatusInternalServerError, "AI_USAGE_RECORD_FAILED", ue.Error(), nil)
		case errors.As(err, &rl):
			retry := ratelimit.RetrySeconds(rl.RetryAfter)
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			details := map[string]interface{}{"scope": rl.Scope, "retry_after_seconds": retry}
			if rl.Scope == llm.ScopeUserInFlight {
				details["limit"] = rl.Limit
			} else {
				details["limit_per_hour"] = rl.Limit
			}
			s.writeError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", rl.Error(), details)
		case errors.Is(err, llm.ErrBusy):
			s.writeError(w, r, http.StatusServiceUnavailable, "LLM_BUSY", err.Error(), nil)
		case errors.As(err, &le):
			s.writeError(w, r, http.StatusBadGateway, "LLM_ERROR", le.Error(), nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "PARSE_FAILED", err.Error(), nil)
		}
		return
	}

	if res.ResolvedPlan != nil && res.Plan != nil {
		if err := s.store.SavePlan(res.Plan, res.ResolvedPlan); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "STORE_ERROR",
				fmt.Sprintf("plan was parsed but could not be saved: %v", err), nil)
			return
		}
	}

	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handlePlanValidate(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "READ_ERROR", err.Error(), nil)
		return
	}

	valRes, err := s.validator.ValidateRawJSON(raw)
	if err != nil {
		var unk *contracts.UnknownNeuronsError
		if errors.As(err, &unk) {
			s.writeError(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", err.Error(), map[string]interface{}{
				"unknown_neuron_ids":       unk.ReportedIDs(),
				"unknown_neuron_ids_total": len(unk.IDs),
				"fields":                   unk.Paths(),
			})
			return
		}
		s.writeError(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", err.Error(), nil)
		return
	}

	// Persist plan in store
	if err := s.store.SavePlan(valRes.Plan, valRes.ResolvedPlan); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"plan_id":          valRes.ResolvedPlan.PlanID,
		"plan_hash":        valRes.ResolvedPlan.PlanHash,
		"plan":             valRes.Plan,
		"resolved_plan":    valRes.ResolvedPlan,
		"defaults_applied": valRes.DefaultsApplied,
		"budget": map[string]interface{}{
			"estimated_wall_seconds": 2.5 * float64(valRes.Plan.Repeats),
			"max_wall_seconds":       s.cfg.MaxWallSeconds,
		},
	})
}

const maxTitleChars = 120

func optionalText(raw string, max int, field string) (*string, error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return nil, nil
	}
	if n := utf8.RuneCountInString(t); n > max {
		return nil, fmt.Errorf("%s must be at most %d characters (got %d)", field, max, n)
	}
	return &t, nil
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request, user *domain.User) {
	var body struct {
		PlanID string `json:"plan_id"`
		Prompt string `json:"prompt"`
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	if body.PlanID == "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "MISSING_PLAN_ID", "plan_id is required", nil)
		return
	}
	prompt, err := optionalText(body.Prompt, llm.MaxPromptChars, "prompt")
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "PROMPT_TOO_LONG", err.Error(), nil)
		return
	}
	title, err := optionalText(body.Title, maxTitleChars, "title")
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "TITLE_TOO_LONG", err.Error(), nil)
		return
	}

	resolved, err := s.store.GetResolvedPlan(body.PlanID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "PLAN_NOT_FOUND", fmt.Sprintf("Plan %s not found", body.PlanID), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "PLAN_LOAD_ERROR",
			fmt.Sprintf("Plan %s cannot be loaded: %v", body.PlanID, err), nil)
		return
	}

	var idempKey *string
	if val := r.Header.Get("Idempotency-Key"); val != "" {
		idempKey = &val
	}

	jobID := fmt.Sprintf("job_%s", uuid.New().String()[:8])
	uid := user.ID
	job := &domain.Job{
		JobID:          jobID,
		PlanID:         resolved.PlanID,
		PlanHash:       resolved.PlanHash,
		Status:         domain.StatusQueued,
		Stage:          "queued",
		ProgressPct:    0.0,
		IdempotencyKey: idempKey,
		ArtifactsDir:   filepath.Join(s.cfg.ArtifactsDir, jobID),
		CreatedAt:      time.Now().UTC(),
		Prompt:         prompt,
		Title:          title,
		UserID:         &uid,
	}

	createdJob, isNew, err := s.store.CreateJob(job, idempKey)
	if err != nil {
		if errors.Is(err, storage.ErrConflict) {
			s.writeError(w, r, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency-Key already used for a different plan", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "JOB_CREATION_FAILED", err.Error(), nil)
		return
	}

	status := http.StatusAccepted
	if !isNew {
		status = http.StatusOK
	}

	s.writeJSON(w, status, map[string]interface{}{
		"job": createdJob,
		"links": map[string]string{
			"status":  fmt.Sprintf("/api/v1/jobs/%s", createdJob.JobID),
			"results": fmt.Sprintf("/api/v1/jobs/%s/results", createdJob.JobID),
			"export":  fmt.Sprintf("/api/v1/jobs/%s/export", createdJob.JobID),
		},
	})
}

var validJobStatuses = map[string]bool{
	string(domain.StatusQueued): true, string(domain.StatusRunning): true,
	string(domain.StatusCancelling): true, string(domain.StatusSucceeded): true,
	string(domain.StatusFailed): true, string(domain.StatusCancelled): true,
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request, user *domain.User) {
	q := r.URL.Query()
	limit := 24
	if raw := q.Get("limit"); raw != "" {
		l, err := strconv.Atoi(raw)
		if err != nil || l < 1 || l > 100 {
			s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_LIMIT",
				fmt.Sprintf("limit must be an integer from 1 to 100 (got %q)", raw), nil)
			return
		}
		limit = l
	}
	offset := 0
	if raw := q.Get("offset"); raw != "" {
		o, err := strconv.Atoi(raw)
		if err != nil || o < 0 {
			s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_OFFSET",
				fmt.Sprintf("offset must be a non-negative integer (got %q)", raw), nil)
			return
		}
		offset = o
	}
	status := q.Get("status")
	if status != "" && !validJobStatuses[status] {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_STATUS",
			fmt.Sprintf("status %q is not a job status", status), nil)
		return
	}

	jobs, total, err := s.store.ListJobsForUser(user.ID, status, limit, offset)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "LIST_JOBS_ERROR", err.Error(), nil)
		return
	}
	out, err := s.buildHistoryJobs(jobs)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "LIST_JOBS_ERROR", err.Error(), nil)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobs":   out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// loadOwnedJob writes 404 JOB_NOT_FOUND for missing jobs and for jobs of other users (or
// ownerless jobs), so existence is never leaked. ok=false means a response was written.
func (s *Server) loadOwnedJob(w http.ResponseWriter, r *http.Request, user *domain.User) (*domain.Job, bool) {
	jobID := r.PathValue("job_id")
	job, err := s.store.GetJobForUser(jobID, user.ID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", fmt.Sprintf("Job %s not found", jobID), nil)
			return nil, false
		}
		s.writeError(w, r, http.StatusInternalServerError, "GET_JOB_ERROR", err.Error(), nil)
		return nil, false
	}
	return job, true
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request, user *domain.User) {
	job, ok := s.loadOwnedJob(w, r, user)
	if !ok {
		return
	}
	out, err := s.buildHistoryJobs([]*domain.Job{job})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "GET_JOB_ERROR", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, out[0])
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request, user *domain.User) {
	owned, ok := s.loadOwnedJob(w, r, user)
	if !ok {
		return
	}
	job, err := s.store.CancelJob(owned.JobID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", fmt.Sprintf("Job %s not found", owned.JobID), nil)
			return
		}
		if errors.Is(err, storage.ErrInvalidState) {
			s.writeError(w, r, http.StatusConflict, "CANNOT_CANCEL", "Cannot cancel job in terminal state", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "CANCEL_ERROR", err.Error(), nil)
		return
	}

	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleJobResults(w http.ResponseWriter, r *http.Request, user *domain.User) {
	job, ok := s.loadOwnedJob(w, r, user)
	if !ok {
		return
	}

	if job.Status != domain.StatusSucceeded {
		s.writeError(w, r, http.StatusConflict, "JOB_NOT_COMPLETED", fmt.Sprintf("Job status is %s", job.Status), nil)
		return
	}

	summaryData, err := os.ReadFile(filepath.Join(job.ArtifactsDir, "summary.json"))
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "RESULTS_NOT_FOUND",
			fmt.Sprintf("Summary results cannot be read: %v", err), nil)
		return
	}

	var summaryObj interface{}
	if err := json.Unmarshal(summaryData, &summaryObj); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "RESULTS_CORRUPT",
			fmt.Sprintf("summary.json cannot be parsed: %v", err), nil)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"job_id":  job.JobID,
		"summary": summaryObj,
	})
}

// ratesHeader is the exact header flysim writes to rates.csv.
const ratesHeader = "condition,trial,root_id,spike_count,rate_hz,is_readout"

type spikeRateRow struct {
	Condition  string  `json:"condition"`
	Trial      int     `json:"trial"`
	RootID     string  `json:"root_id"`
	SpikeCount int     `json:"spike_count"`
	RateHz     float64 `json:"rate_hz"`
	IsReadout  bool    `json:"is_readout"`
	raw        string
}

// parseRatesRow validates one rates.csv data line (exactly 6 columns, typed values).
func parseRatesRow(line string) (spikeRateRow, error) {
	parts := strings.Split(line, ",")
	if len(parts) != 6 {
		return spikeRateRow{}, fmt.Errorf("has %d columns, expected 6", len(parts))
	}
	row := spikeRateRow{Condition: parts[0], RootID: parts[2], raw: line}
	if row.Condition == "" || row.RootID == "" {
		return row, errors.New("condition and root_id must not be empty")
	}
	var err error
	if row.Trial, err = strconv.Atoi(parts[1]); err != nil || row.Trial < 0 {
		return row, fmt.Errorf("trial %q is not a non-negative integer", parts[1])
	}
	if row.SpikeCount, err = strconv.Atoi(parts[3]); err != nil || row.SpikeCount < 0 {
		return row, fmt.Errorf("spike_count %q is not a non-negative integer", parts[3])
	}
	row.RateHz, err = strconv.ParseFloat(parts[4], 64)
	if err != nil || math.IsNaN(row.RateHz) || math.IsInf(row.RateHz, 0) || row.RateHz < 0 {
		return row, fmt.Errorf("rate_hz %q is not a finite non-negative number", parts[4])
	}
	switch parts[5] {
	case "true":
		row.IsReadout = true
	case "false":
	default:
		return row, fmt.Errorf("is_readout %q is not true/false", parts[5])
	}
	return row, nil
}

func (s *Server) handleJobSpikes(w http.ResponseWriter, r *http.Request, user *domain.User) {
	job, ok := s.loadOwnedJob(w, r, user)
	if !ok {
		return
	}

	condFilter := strings.TrimSpace(r.URL.Query().Get("condition"))
	rootFilter := strings.TrimSpace(r.URL.Query().Get("root_id"))
	limit := 10000
	if raw := r.URL.Query().Get("limit"); raw != "" {
		l, err := strconv.Atoi(raw)
		if err != nil || l < 1 || l > 10000 {
			s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_LIMIT",
				fmt.Sprintf("limit must be an integer from 1 to 10000 (got %q)", raw), nil)
			return
		}
		limit = l
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		o, err := strconv.Atoi(raw)
		if err != nil || o < 0 {
			s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_OFFSET",
				fmt.Sprintf("offset must be a non-negative integer (got %q)", raw), nil)
			return
		}
		offset = o
	}

	ratesPath := filepath.Join(job.ArtifactsDir, "rates.csv")
	ratesData, err := os.ReadFile(ratesPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeError(w, r, http.StatusNotFound, "RATES_NOT_FOUND", fmt.Sprintf("Job %s has no rates.csv", job.JobID), nil)
			return
		}
		// Exists but unreadable (permissions, I/O error): a server-side fault, not "not found".
		s.writeError(w, r, http.StatusInternalServerError, "RATES_UNREADABLE",
			fmt.Sprintf("rates.csv of job %s cannot be read: %v", job.JobID, err), nil)
		return
	}

	// Every row is validated before filtering and paging, in both output formats: a corrupt
	// row on another page or in CSV mode must not pass as data.
	lines := strings.Split(string(ratesData), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != ratesHeader {
		s.writeError(w, r, http.StatusInternalServerError, "RATES_CORRUPT",
			fmt.Sprintf("rates.csv line 1 is %q, expected the header %q", strings.TrimSpace(lines[0]), ratesHeader), nil)
		return
	}
	var matched []spikeRateRow
	for i, line := range lines[1:] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		row, err := parseRatesRow(trimmed)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "RATES_CORRUPT",
				fmt.Sprintf("rates.csv line %d %v", i+2, err), map[string]interface{}{"line": i + 2})
			return
		}
		if condFilter != "" && !strings.EqualFold(row.Condition, condFilter) {
			continue
		}
		if rootFilter != "" && row.RootID != rootFilter {
			continue
		}
		matched = append(matched, row)
	}

	total := len(matched)
	page := []spikeRateRow{}
	if offset < total {
		end := offset + limit
		if end > total {
			end = total
		}
		page = matched[offset:end]
	}

	format := r.URL.Query().Get("format")
	acceptJSON := strings.Contains(r.Header.Get("Accept"), "application/json")

	if format == "json" || (format == "" && acceptJSON) {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"job_id": job.JobID,
			"total":  total,
			"limit":  limit,
			"offset": offset,
			"spikes": page,
		})
		return
	}

	// Default CSV response
	w.Header().Set("Content-Type", "text/csv")
	var sb strings.Builder
	sb.WriteString(ratesHeader + "\n")
	for _, row := range page {
		sb.WriteString(row.raw + "\n")
	}
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Server) handleJobExport(w http.ResponseWriter, r *http.Request, user *domain.User) {
	job, ok := s.loadOwnedJob(w, r, user)
	if !ok {
		return
	}
	// Check the directory before streaming: once the zip headers are sent an error can no
	// longer be reported as a JSON envelope.
	if _, err := os.ReadDir(job.ArtifactsDir); err != nil {
		code, status := "EXPORT_ERROR", http.StatusInternalServerError
		if os.IsNotExist(err) {
			code, status = "ARTIFACTS_NOT_FOUND", http.StatusNotFound
		}
		s.writeError(w, r, status, code, fmt.Sprintf("Artifacts of job %s cannot be read: %v", job.JobID, err), nil)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"flylab_%s_export.zip\"", job.JobID))

	if err := export.CreateJobArchive(job.ArtifactsDir, w); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "EXPORT_ERROR", err.Error(), nil)
	}
}

func (s *Server) handleJobArtifact(w http.ResponseWriter, r *http.Request, user *domain.User) {
	artifactID := filepath.Clean(r.PathValue("artifact_id"))
	if strings.Contains(artifactID, "..") || strings.Contains(artifactID, "/") {
		s.writeError(w, r, http.StatusForbidden, "INVALID_PATH", "Path traversal forbidden", nil)
		return
	}

	job, ok := s.loadOwnedJob(w, r, user)
	if !ok {
		return
	}

	filePath := filepath.Join(job.ArtifactsDir, artifactID)
	http.ServeFile(w, r, filePath)
}
