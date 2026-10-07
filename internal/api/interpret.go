package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

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
	resp, err := s.interp.Stored(job.JobID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "INTERPRETATION_NOT_FOUND",
				fmt.Sprintf("Job %s has no interpretation yet", job.JobID), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "INTERPRETATION_CORRUPT", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

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
	if !body.Regenerate {
		resp, err := s.interp.Stored(job.JobID)
		if err == nil {
			s.writeJSON(w, http.StatusOK, resp)
			return
		}
		if !errors.Is(err, storage.ErrNotFound) {
			s.writeError(w, r, http.StatusInternalServerError, "INTERPRETATION_CORRUPT",
				err.Error()+" (request it again with regenerate: true to replace it)", nil)
			return
		}
	}

	resp, err := s.interp.Generate(r.Context(), job, user.ID, clientIP(r), body.Language, body.Regenerate)
	if err != nil {
		var (
			rl *interpret.RateLimitError
			de *interpret.DigestError
			db *interpret.DigestBusyError
			le *interpret.LLMError
		)
		switch {
		case errors.As(err, &db):
			s.writeDigestBusy(w, r, db)
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
		case errors.As(err, &de):
			s.writeDigestError(w, r, de)
		case errors.Is(err, llm.ErrBusy):
			s.writeError(w, r, http.StatusServiceUnavailable, "LLM_BUSY", err.Error(), nil)
		case errors.As(err, &le):
			s.writeError(w, r, http.StatusBadGateway, "LLM_ERROR", le.Error(), nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "STORE_ERROR", err.Error(), nil)
		}
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}
