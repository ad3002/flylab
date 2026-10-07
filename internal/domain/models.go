package domain

import (
	"encoding/json"
	"time"
)

// Selector specifies either a named group_id or an explicit array of neuron_ids.
type Selector struct {
	GroupID   string   `json:"group_id,omitempty"`
	NeuronIDs []string `json:"neuron_ids,omitempty"`
}

type ActivationSpec struct {
	Selector Selector `json:"selector"`
	RateHz   float64  `json:"rate_hz"`
}

type SilencingSpec struct {
	Selector Selector `json:"selector"`
}

type ReadoutSpec struct {
	Selector Selector `json:"selector"`
}

// ExperimentPlan is the canonical executable contract for user and LLM requests.
type ExperimentPlan struct {
	SchemaVersion  string           `json:"schema_version"`
	DatasetID      string           `json:"dataset_id"`
	ModelID        string           `json:"model_id"`
	ExperimentType string           `json:"experiment_type"` // "single" | "compare_silencing"
	Activation     []ActivationSpec `json:"activation"`
	Silencing      []SilencingSpec  `json:"silencing"`
	Readout        []ReadoutSpec    `json:"readout"`
	DurationMs     float64          `json:"duration_ms"`
	Repeats        int              `json:"repeats"`
	BaseSeed       uint64           `json:"base_seed"`
	ReportLanguage string           `json:"report_language"`
}

type ResolvedActivation struct {
	NeuronIDs []string `json:"neuron_ids"`
	RateHz    float64  `json:"rate_hz"`
}

// ResolvedPlan represents the frozen, resolved snapshot that is directly executed by Rust flysim.
type ResolvedPlan struct {
	SchemaVersion      string               `json:"schema_version"`
	PlanID             string               `json:"plan_id"`
	PlanHash           string               `json:"plan_hash"`
	DatasetID          string               `json:"dataset_id"`
	ModelID            string               `json:"model_id"`
	ExperimentType     string               `json:"experiment_type"`
	Activation         []ResolvedActivation `json:"activation"`
	SilencingNeuronIDs []string             `json:"silencing_neuron_ids"`
	ReadoutNeuronIDs   []string             `json:"readout_neuron_ids"`
	DurationMs         float64              `json:"duration_ms"`
	Repeats            int                  `json:"repeats"`
	BaseSeed           uint64               `json:"base_seed"`
	ReportLanguage     string               `json:"report_language"`
	DtMs               float64              `json:"dt_ms"`
	CreatedAt          time.Time            `json:"created_at"`
}

type JobStatus string

const (
	StatusQueued     JobStatus = "queued"
	StatusRunning    JobStatus = "running"
	StatusCancelling JobStatus = "cancelling"
	StatusSucceeded  JobStatus = "succeeded"
	StatusFailed     JobStatus = "failed"
	StatusCancelled  JobStatus = "cancelled"
)

type Job struct {
	JobID          string     `json:"job_id"`
	PlanID         string     `json:"plan_id"`
	PlanHash       string     `json:"plan_hash"`
	Status         JobStatus  `json:"status"`
	Stage          string     `json:"stage"` // loading, building, simulating, aggregating, exporting
	ProgressPct    float64    `json:"progress_pct"`
	IdempotencyKey *string    `json:"idempotency_key,omitempty"`
	ErrorMessage   *string    `json:"error_message,omitempty"`
	ErrorCode      *string    `json:"error_code,omitempty"`
	ArtifactsDir   string     `json:"artifacts_dir"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	// Prompt and Title are the user's natural-language request and a human label (nullable).
	Prompt *string `json:"prompt"`
	Title  *string `json:"title"`
	// UserID is the owner; never serialised (ownership is enforced, not exposed).
	UserID *int64 `json:"-"`
}

// User is the public view of an account. The password hash is never part of it.
type User struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// UserStats aggregates the caller's jobs for GET /api/v1/me.
// Running counts every non-terminal job (queued, running, cancelling).
type UserStats struct {
	TotalJobs int        `json:"total_jobs"`
	Succeeded int        `json:"succeeded"`
	Failed    int        `json:"failed"`
	Running   int        `json:"running"`
	LastJobAt *time.Time `json:"last_job_at"`
}

// JobSummary is the compact result shown in the history list. TotalSpikesB is null for
// "single" experiments (there is no condition B); flysim writes it as null there.
type JobSummary struct {
	TotalSpikesA        int64           `json:"total_spikes_A"`
	TotalSpikesB        *int64          `json:"total_spikes_B"`
	ActiveNeuronsCountA int64           `json:"active_neurons_count_A"`
	ActiveNeuronsCountB int64           `json:"active_neurons_count_B"`
	ReadoutSummary      json.RawMessage `json:"readout_summary"`
}

// HistoryJob is the job shape of GET /api/v1/jobs and GET /api/v1/jobs/{id}.
// SummaryError / PlanError are set when the authoritative source exists but cannot be
// read or parsed, so corruption is visible to the user instead of silently dropped.
type HistoryJob struct {
	*Job
	Plan         *ExperimentPlan `json:"plan"`
	PlanError    *string         `json:"plan_error"`
	Summary      *JobSummary     `json:"summary"`
	SummaryError *string         `json:"summary_error"`
	// HasInterpretation: an AI interpretation is stored for this job (contract v3).
	HasInterpretation bool `json:"has_interpretation"`
	// InterpretationLanguage is the stored interpretation's language (null without one), so
	// the UI labels a run in the language its hypotheses are written in.
	InterpretationLanguage *string `json:"interpretation_language"`
}

type NeuronGroup struct {
	GroupID         string   `json:"group_id"`
	NameEn          string   `json:"name_en"`
	NameRu          string   `json:"name_ru"`
	Aliases         []string `json:"aliases"`
	DatasetID       string   `json:"dataset_id"`
	SourceReference string   `json:"source_reference"`
	Description     string   `json:"description"`
	NeuronIDs       []string `json:"neuron_ids"`
}

type ErrorDetail struct {
	Code      string                 `json:"code"`
	Message   string                 `json:"message"`
	Details   map[string]interface{} `json:"details,omitempty"`
	RequestID string                 `json:"request_id,omitempty"`
}

type APIError struct {
	Error ErrorDetail `json:"error"`
}
