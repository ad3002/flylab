package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/export"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/storage"
)

type Server struct {
	cfg       *config.Config
	store     *storage.Store
	validator *contracts.Validator
	registry  *contracts.Registry
	llmClient *llm.Client
	router    *http.ServeMux
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
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, X-Request-ID")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) registerRoutes() {
	// Web UI
	s.router.HandleFunc("GET /{$}", s.handleIndex)
	staticDir := filepath.Join(s.cfg.WebDir, "static")
	s.router.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))

	// Base endpoints
	s.router.HandleFunc("GET /health", s.handleHealth)
	s.router.HandleFunc("GET /capabilities", s.handleCapabilities)
	s.router.HandleFunc("GET /datasets", s.handleDatasets)
	s.router.HandleFunc("GET /groups", s.handleGroups)
	s.router.HandleFunc("GET /neurons", s.handleNeurons)

	// API v1 endpoints
	s.router.HandleFunc("POST /api/v1/plans/parse", s.handlePlanParse)
	s.router.HandleFunc("POST /api/v1/plans/validate", s.handlePlanValidate)
	s.router.HandleFunc("POST /api/v1/jobs", s.handleCreateJob)
	s.router.HandleFunc("GET /api/v1/jobs", s.handleListJobs)
	s.router.HandleFunc("GET /api/v1/jobs/{job_id}", s.handleGetJob)
	s.router.HandleFunc("POST /api/v1/jobs/{job_id}/cancel", s.handleCancelJob)
	s.router.HandleFunc("GET /api/v1/jobs/{job_id}/results", s.handleJobResults)
	s.router.HandleFunc("GET /api/v1/jobs/{job_id}/spikes", s.handleJobSpikes)
	s.router.HandleFunc("GET /api/v1/jobs/{job_id}/export", s.handleJobExport)
	s.router.HandleFunc("GET /api/v1/jobs/{job_id}/artifacts/{artifact_id}", s.handleJobArtifact)

	// Direct routes without /api/v1 prefix
	s.router.HandleFunc("POST /plans/parse", s.handlePlanParse)
	s.router.HandleFunc("POST /plans/validate", s.handlePlanValidate)
	s.router.HandleFunc("POST /jobs", s.handleCreateJob)
	s.router.HandleFunc("GET /jobs", s.handleListJobs)
	s.router.HandleFunc("GET /jobs/{job_id}", s.handleGetJob)
	s.router.HandleFunc("POST /jobs/{job_id}/cancel", s.handleCancelJob)
	s.router.HandleFunc("GET /jobs/{job_id}/results", s.handleJobResults)
	s.router.HandleFunc("GET /jobs/{job_id}/spikes", s.handleJobSpikes)
	s.router.HandleFunc("GET /jobs/{job_id}/export", s.handleJobExport)
	s.router.HandleFunc("GET /jobs/{job_id}/artifacts/{artifact_id}", s.handleJobArtifact)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	indexPath := filepath.Join(s.cfg.WebDir, "templates", "index.html")
	http.ServeFile(w, r, indexPath)
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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"service": "flylab",
		"domain":  s.cfg.Domain,
		"version": "1.0.0",
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	// Check data presence
	dataReady := true
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "dataset_manifest.json")); os.IsNotExist(err) {
		dataReady = false
	}

	// Check flysim binary
	workerReady := true
	if _, err := os.Stat(s.cfg.FlysimBin); os.IsNotExist(err) {
		workerReady = false
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"datasets_ready": dataReady,
		"worker_ready":   workerReady,
		"llm_model":      s.cfg.OllamaModel,
		"llm_ready":      true,
		"limits": map[string]interface{}{
			"max_wall_seconds":   s.cfg.MaxWallSeconds,
			"max_rss_bytes":      s.cfg.MaxRSSBytes,
			"max_artifact_bytes": s.cfg.MaxArtifactBytes,
		},
	})
}

func (s *Server) handleDatasets(w http.ResponseWriter, r *http.Request) {
	manifestPath := filepath.Join(s.cfg.DataDir, "dataset_manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "DATASET_MANIFEST_ERROR", "Failed to load dataset manifest", nil)
		return
	}
	var manifest interface{}
	_ = json.Unmarshal(data, &manifest)
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

func (s *Server) handlePlanParse(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Prompt         string `json:"prompt"`
		DatasetID      string `json:"dataset_id"`
		ReportLanguage string `json:"report_language"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REQUEST_BODY", err.Error(), nil)
		return
	}

	if body.Prompt == "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "EMPTY_PROMPT", "Prompt cannot be empty", nil)
		return
	}

	if body.ReportLanguage == "" {
		body.ReportLanguage = "en"
	}

	res, err := s.llmClient.ParsePrompt(r.Context(), body.Prompt, body.DatasetID, body.ReportLanguage)
	if err != nil {
		if errors.Is(err, llm.ErrLLMUnavailable) {
			s.writeError(w, r, http.StatusServiceUnavailable, "LLM_UNAVAILABLE", "Local LLM service is currently unavailable", nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "LLM_ERROR", err.Error(), nil)
		return
	}

	if res.ResolvedPlan != nil && res.Plan != nil {
		_ = s.store.SavePlan(res.Plan, res.ResolvedPlan)
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

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlanID string `json:"plan_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	if body.PlanID == "" {
		s.writeError(w, r, http.StatusUnprocessableEntity, "MISSING_PLAN_ID", "plan_id is required", nil)
		return
	}

	resolved, err := s.store.GetResolvedPlan(body.PlanID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "PLAN_NOT_FOUND", fmt.Sprintf("Plan %s not found", body.PlanID), nil)
		return
	}

	var idempKey *string
	if val := r.Header.Get("Idempotency-Key"); val != "" {
		idempKey = &val
	}

	jobID := fmt.Sprintf("job_%s", uuid.New().String()[:8])
	artifactsDir := filepath.Join(s.cfg.ArtifactsDir, jobID)

	job := &domain.Job{
		JobID:          jobID,
		PlanID:         resolved.PlanID,
		PlanHash:       resolved.PlanHash,
		Status:         domain.StatusQueued,
		Stage:          "queued",
		ProgressPct:    0.0,
		IdempotencyKey: idempKey,
		ArtifactsDir:   artifactsDir,
		CreatedAt:      time.Now().UTC(),
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
		"job":   createdJob,
		"links": map[string]string{
			"status":  fmt.Sprintf("/api/v1/jobs/%s", createdJob.JobID),
			"results": fmt.Sprintf("/api/v1/jobs/%s/results", createdJob.JobID),
			"export":  fmt.Sprintf("/api/v1/jobs/%s/export", createdJob.JobID),
		},
	})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 100 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
		offset = o
	}

	jobs, err := s.store.ListJobs(limit, offset)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "LIST_JOBS_ERROR", err.Error(), nil)
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobs":   jobs,
		"count":  len(jobs),
		"limit":  limit,
		"offset": offset,
	})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, err := s.store.GetJob(jobID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", fmt.Sprintf("Job %s not found", jobID), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "GET_JOB_ERROR", err.Error(), nil)
		return
	}

	s.writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, err := s.store.CancelJob(jobID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", fmt.Sprintf("Job %s not found", jobID), nil)
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

func (s *Server) handleJobResults(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, err := s.store.GetJob(jobID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", fmt.Sprintf("Job %s not found", jobID), nil)
		return
	}

	if job.Status != domain.StatusSucceeded {
		s.writeError(w, r, http.StatusConflict, "JOB_NOT_COMPLETED", fmt.Sprintf("Job status is %s", job.Status), nil)
		return
	}

	summaryPath := filepath.Join(job.ArtifactsDir, "summary.json")
	summaryData, err := os.ReadFile(summaryPath)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "RESULTS_NOT_FOUND", "Summary results missing", nil)
		return
	}

	var summaryObj interface{}
	_ = json.Unmarshal(summaryData, &summaryObj)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"job_id":  job.JobID,
		"summary": summaryObj,
	})
}

func (s *Server) handleJobSpikes(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, err := s.store.GetJob(jobID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", fmt.Sprintf("Job %s not found", jobID), nil)
		return
	}

	ratesPath := filepath.Join(job.ArtifactsDir, "rates.csv")
	ratesData, err := os.ReadFile(ratesPath)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "RATES_NOT_FOUND", "Rates data not found", nil)
		return
	}

	condFilter := strings.TrimSpace(r.URL.Query().Get("condition"))
	rootFilter := strings.TrimSpace(r.URL.Query().Get("root_id"))
	limit := 10000
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 10000 {
		limit = l
	}
	offset := 0
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && o >= 0 {
		offset = o
	}

	lines := strings.Split(string(ratesData), "\n")
	var matchedRows [][]string
	header := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if i == 0 {
			header = trimmed
			continue
		}
		parts := strings.Split(trimmed, ",")
		if len(parts) < 6 {
			continue
		}
		// condition,trial,root_id,spike_count,rate_hz,is_readout
		if condFilter != "" && !strings.EqualFold(parts[0], condFilter) {
			continue
		}
		if rootFilter != "" && parts[2] != rootFilter {
			continue
		}
		matchedRows = append(matchedRows, parts)
	}

	total := len(matchedRows)
	var paginatedRows [][]string
	if offset < total {
		end := offset + limit
		if end > total {
			end = total
		}
		paginatedRows = matchedRows[offset:end]
	}

	format := r.URL.Query().Get("format")
	acceptJSON := strings.Contains(r.Header.Get("Accept"), "application/json")

	if format == "json" || (format == "" && acceptJSON) {
		type SpikeRateRow struct {
			Condition  string  `json:"condition"`
			Trial      int     `json:"trial"`
			RootID     string  `json:"root_id"`
			SpikeCount int     `json:"spike_count"`
			RateHz     float64 `json:"rate_hz"`
			IsReadout  bool    `json:"is_readout"`
		}
		var rows []SpikeRateRow
		for _, parts := range paginatedRows {
			trial, _ := strconv.Atoi(parts[1])
			spikes, _ := strconv.Atoi(parts[3])
			rate, _ := strconv.ParseFloat(parts[4], 64)
			isRo := parts[5] == "true"
			rows = append(rows, SpikeRateRow{
				Condition:  parts[0],
				Trial:      trial,
				RootID:     parts[2],
				SpikeCount: spikes,
				RateHz:     rate,
				IsReadout:  isRo,
			})
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"job_id": job.JobID,
			"total":  total,
			"limit":  limit,
			"offset": offset,
			"spikes": rows,
		})
		return
	}

	// Default CSV response
	w.Header().Set("Content-Type", "text/csv")
	var sb strings.Builder
	sb.WriteString(header + "\n")
	for _, row := range paginatedRows {
		sb.WriteString(strings.Join(row, ",") + "\n")
	}
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Server) handleJobExport(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	job, err := s.store.GetJob(jobID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", fmt.Sprintf("Job %s not found", jobID), nil)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"flylab_%s_export.zip\"", job.JobID))

	if err := export.CreateJobArchive(job.ArtifactsDir, w); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "EXPORT_ERROR", err.Error(), nil)
	}
}

func (s *Server) handleJobArtifact(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	artifactID := filepath.Clean(r.PathValue("artifact_id"))
	if strings.Contains(artifactID, "..") || strings.Contains(artifactID, "/") {
		s.writeError(w, r, http.StatusForbidden, "INVALID_PATH", "Path traversal forbidden", nil)
		return
	}

	job, err := s.store.GetJob(jobID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "JOB_NOT_FOUND", fmt.Sprintf("Job %s not found", jobID), nil)
		return
	}

	filePath := filepath.Join(job.ArtifactsDir, artifactID)
	http.ServeFile(w, r, filePath)
}
