package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/interpret"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/ratelimit"
	"github.com/ad3002/flylab/internal/storage"
)

// SetInterpreter connects the v3 interpretation service (digest + Claude hypotheses).
func (s *Server) SetInterpreter(svc *interpret.Service) { s.interp = svc }

// interpreterOK writes 503 INTERPRETER_UNAVAILABLE when the service is not configured.
func (s *Server) interpreterOK(w http.ResponseWriter, r *http.Request) bool {
	if s.interp == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "INTERPRETER_UNAVAILABLE",
			"the interpretation service is not configured on this server", nil)
		return false
	}
	return true
}

// succeededJob loads an owned job and requires status succeeded (409 JOB_NOT_COMPLETED).
func (s *Server) succeededJob(w http.ResponseWriter, r *http.Request, user *domain.User) (*domain.Job, bool) {
	job, ok := s.loadOwnedJob(w, r, user)
	if !ok {
		return nil, false
	}
	if job.Status != domain.StatusSucceeded {
		s.writeError(w, r, http.StatusConflict, "JOB_NOT_COMPLETED",
			fmt.Sprintf("Job status is %s; only succeeded runs can be interpreted", job.Status), nil)
		return nil, false
	}
	return job, true
}

// writeDigestError reports a digest failure as 500 DIGEST_ERROR with the reason both in the
// error envelope and as a top-level digest_error field.
func (s *Server) writeDigestError(w http.ResponseWriter, r *http.Request, de *interpret.DigestError) {
	s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
		"error": domain.ErrorDetail{
			Code:      "DIGEST_ERROR",
			Message:   de.Error(),
			Details:   map[string]interface{}{"digest_error": de.Reason},
			RequestID: w.Header().Get("X-Request-ID"),
		},
		"digest_error": de.Reason,
	})
}

// writeDigestBusy reports a taken digest slot as 503 DIGEST_BUSY (temporary, retry shortly).
func (s *Server) writeDigestBusy(w http.ResponseWriter, r *http.Request, db *interpret.DigestBusyError) {
	retry := ratelimit.RetrySeconds(db.Wait)
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	s.writeError(w, r, http.StatusServiceUnavailable, "DIGEST_BUSY", db.Error(),
		map[string]interface{}{"retry_after_seconds": retry})
}

func (s *Server) handleJobDigest(w http.ResponseWriter, r *http.Request, user *domain.User) {
	if !s.interpreterOK(w, r) {
		return
	}
	job, ok := s.succeededJob(w, r, user)
	if !ok {
		return
	}
	d, err := s.interp.Digest(r.Context(), job, false)
	if err != nil {
		var (
			de *interpret.DigestError
			db *interpret.DigestBusyError
		)
		if errors.As(err, &db) {
			s.writeDigestBusy(w, r, db)
			return
		}
		if errors.As(err, &de) {
			s.writeDigestError(w, r, de)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "DIGEST_ERROR", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"job_id": job.JobID, "digest": d})
}

func (s *Server) handleGetInterpretation(w http.ResponseWriter, r *http.Request, user *domain.User) {
	if !s.interpreterOK(w, r) {
		return
	}
	job, ok := s.loadOwnedJob(w, r, user)
	if !ok {
		return
	}
	resp, err := s.interp.State(job.JobID)
	if err != nil {
		var ce *interpret.CorruptError
		switch {
		case errors.Is(err, storage.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "INTERPRETATION_NOT_FOUND",
				fmt.Sprintf("Job %s has no interpretation yet", job.JobID), nil)
		case errors.As(err, &ce):
			s.writeError(w, r, http.StatusInternalServerError, "INTERPRETATION_CORRUPT",
				ce.Error()+" (request it again with regenerate: true to replace it)", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		}
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handlePostInterpretation (contract v4 section 4): a stored result without regenerate is
// 200 ready; otherwise the job's active request, or a newly queued one, is 202.
func (s *Server) handlePostInterpretation(w http.ResponseWriter, r *http.Request, user *domain.User) {
	if !s.interpreterOK(w, r) {
		return
	}
	var body struct {
		Language   string `json:"language"`
		Regenerate bool   `json:"regenerate"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REQUEST_BODY", err.Error(), nil)
		return
	}
	if strings.TrimSpace(string(raw)) != "" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REQUEST_BODY", err.Error(), nil)
			return
		}
	}
	if body.Language == "" {
		body.Language = "en"
	}
	if _, ok := interpret.Languages[body.Language]; !ok {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_LANGUAGE",
			fmt.Sprintf("language must be \"en\" or \"ru\" (got %q)", body.Language), nil)
		return
	}

	job, ok := s.succeededJob(w, r, user)
	if !ok {
		return
	}
	// A request already queued or running for this job is returned as is (also with
	// regenerate), so a repeat POST never queues a second paid call.
	active, err := s.interp.ActiveRequest(job.JobID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		return
	}
	if active != nil {
		s.writeJSON(w, http.StatusAccepted, &interpret.StateResponse{State: active.Status, Request: active})
		return
	}
	if !body.Regenerate {
		resp, err := s.interp.Stored(job.JobID)
		if err == nil {
			s.writeJSON(w, http.StatusOK, &interpret.StateResponse{State: storage.StateReady, Response: resp})
			return
		}
		var ce *interpret.CorruptError
		switch {
		case errors.As(err, &ce):
			s.writeError(w, r, http.StatusInternalServerError, "INTERPRETATION_CORRUPT",
				ce.Error()+" (request it again with regenerate: true to replace it)", nil)
			return
		case !errors.Is(err, storage.ErrNotFound):
			s.writeError(w, r, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
			return
		}
	}

	req, err := s.interp.Enqueue(job, user.ID, clientIP(r), body.Language, body.Regenerate)
	if err != nil {
		var (
			ip *interpret.InProgressError
			qf *interpret.QueueFullError
			rl *interpret.RateLimitError
		)
		switch {
		case s.writeBudgetError(w, r, err):
		case errors.As(err, &ip):
			s.writeError(w, r, http.StatusConflict, "INTERPRETATION_IN_PROGRESS", ip.Error(),
				map[string]interface{}{"job_id": ip.JobID, "request_id": ip.RequestID, "status": ip.Status})
		case errors.As(err, &qf):
			w.Header().Set("Retry-After", strconv.Itoa(interpret.QueueFullRetryAfterSecs))
			s.writeError(w, r, http.StatusServiceUnavailable, "QUEUE_FULL", qf.Error(),
				map[string]interface{}{"queue_max": qf.Max, "queued": qf.Queued, "retry_after_seconds": interpret.QueueFullRetryAfterSecs})
		case errors.As(err, &rl):
			retry := ratelimit.RetrySeconds(rl.RetryAfter)
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			s.writeError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", rl.Error(),
				map[string]interface{}{"scope": rl.Scope, "limit_per_hour": rl.Limit, "retry_after_seconds": retry})
		default:
			s.writeError(w, r, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		}
		return
	}
	s.writeJSON(w, http.StatusAccepted, &interpret.StateResponse{State: req.Status, Request: req})
}

// writeBudgetError answers the AI budget errors shared by parse and interpret: 429
// AI_BUDGET_EXHAUSTED with details {scope, spent_usd, budget_usd, resets_at} and Retry-After,
// or 500 AI_BUDGET_CHECK_FAILED when the spend cannot be read. It reports whether it wrote.
func (s *Server) writeBudgetError(w http.ResponseWriter, r *http.Request, err error) bool {
	var (
		be *llm.BudgetError
		ce *llm.BudgetCheckError
	)
	switch {
	case errors.As(err, &be):
		retry := ratelimit.RetrySeconds(time.Until(be.ResetsAt))
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		s.writeError(w, r, http.StatusTooManyRequests, "AI_BUDGET_EXHAUSTED", be.Error(), map[string]interface{}{
			"scope":      be.Scope,
			"spent_usd":  be.SpentUSD,
			"budget_usd": be.BudgetUSD,
			"resets_at":  be.ResetsAt.UTC().Format(time.RFC3339),
		})
		return true
	case errors.As(err, &ce):
		s.writeError(w, r, http.StatusInternalServerError, "AI_BUDGET_CHECK_FAILED", ce.Error(), nil)
		return true
	}
	return false
}
