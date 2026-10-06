package domain

import "time"

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
	JobID          string    `json:"job_id"`
	PlanID         string    `json:"plan_id"`
	PlanHash       string    `json:"plan_hash"`
	Status         JobStatus `json:"status"`
	Stage          string    `json:"stage"` // loading, building, simulating, aggregating, exporting
	ProgressPct    float64   `json:"progress_pct"`
	IdempotencyKey *string   `json:"idempotency_key,omitempty"`
	ErrorMessage   *string   `json:"error_message,omitempty"`
	ErrorCode      *string   `json:"error_code,omitempty"`
	ArtifactsDir   string    `json:"artifacts_dir"`
	CreatedAt      time.Time `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
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
