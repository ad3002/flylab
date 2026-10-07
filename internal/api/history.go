package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/storage"
)

// buildHistoryJob attaches the stored plan and, for succeeded jobs, the compact summary.
// When a source exists but cannot be read or parsed the error goes into plan_error /
// summary_error so the UI shows it; nothing is silently dropped.
func (s *Server) buildHistoryJob(job *domain.Job) *domain.HistoryJob {
	h := &domain.HistoryJob{Job: job}

	plan, err := s.store.GetPlan(job.PlanID)
	if err != nil {
		var msg string
		if errors.Is(err, storage.ErrNotFound) {
			msg = fmt.Sprintf("plan %s is missing from the store", job.PlanID)
		} else {
			msg = fmt.Sprintf("plan %s cannot be loaded: %v", job.PlanID, err)
		}
		h.PlanError = &msg
	} else {
		h.Plan = plan
	}

	if job.Status == domain.StatusSucceeded {
		summary, err := readJobSummary(filepath.Join(job.ArtifactsDir, "summary.json"))
		if err != nil {
			msg := err.Error()
			h.SummaryError = &msg
		} else {
			h.Summary = summary
		}
	}
	return h
}

func readJobSummary(path string) (*domain.JobSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("summary.json is missing for a succeeded job")
		}
		return nil, fmt.Errorf("summary.json cannot be read: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("summary.json cannot be parsed: %v", err)
	}

	var missing []string
	for _, k := range []string{"total_spikes_A", "total_spikes_B", "active_neurons_count_A", "active_neurons_count_B", "readout_summary"} {
		if _, ok := fields[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("summary.json lacks fields: %s", strings.Join(missing, ", "))
	}

	var expType string
	if raw, ok := fields["experiment_type"]; ok {
		if err := json.Unmarshal(raw, &expType); err != nil {
			return nil, fmt.Errorf("summary.json field experiment_type is invalid: %v", err)
		}
	}

	var out domain.JobSummary
	var problems []string
	decodeInt := func(key string, dst *int64) {
		raw := bytes.TrimSpace(fields[key])
		if bytes.Equal(raw, []byte("null")) {
			problems = append(problems, key+" is null")
			return
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			problems = append(problems, fmt.Sprintf("%s is not an integer (%s)", key, raw))
		}
	}
	decodeInt("total_spikes_A", &out.TotalSpikesA)
	decodeInt("active_neurons_count_A", &out.ActiveNeuronsCountA)
	decodeInt("active_neurons_count_B", &out.ActiveNeuronsCountB)

	rawB := bytes.TrimSpace(fields["total_spikes_B"])
	if bytes.Equal(rawB, []byte("null")) {
		// Only a single-condition run has no condition B.
		if expType == "compare_silencing" {
			problems = append(problems, "total_spikes_B is null for a compare_silencing run")
		}
	} else {
		var b int64
		decodeInt("total_spikes_B", &b)
		out.TotalSpikesB = &b
	}

	rs := bytes.TrimSpace(fields["readout_summary"])
	if len(rs) == 0 || bytes.Equal(rs, []byte("null")) {
		problems = append(problems, "readout_summary is null")
	}
	out.ReadoutSummary = rs

	if len(problems) > 0 {
		return nil, fmt.Errorf("summary.json has invalid fields: %s", strings.Join(problems, "; "))
	}
	return &out, nil
}
