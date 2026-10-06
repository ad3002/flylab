package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
)

var (
	ErrLLMUnavailable = errors.New("LLM service unavailable")
)

type ParseStatus string

const (
	StatusReady       ParseStatus = "ready"
	StatusNeedsInput  ParseStatus = "needs_input"
	StatusUnsupported ParseStatus = "unsupported"
	StatusInvalid     ParseStatus = "invalid"
)

type ParseResult struct {
	Status           ParseStatus            `json:"status"`
	Plan             *domain.ExperimentPlan `json:"plan,omitempty"`
	ResolvedPlan     *domain.ResolvedPlan   `json:"resolved_plan,omitempty"`
	DefaultsApplied  map[string]interface{} `json:"defaults_applied,omitempty"`
	UnresolvedFields []string               `json:"unresolved_fields,omitempty"`
	Message          string                 `json:"message"`
	LLMMetadata      map[string]interface{} `json:"llm_metadata,omitempty"`
}

type Client struct {
	cfg        *config.Config
	httpClient *http.Client
	validator  *contracts.Validator
	registry   *contracts.Registry
}

func NewClient(cfg *config.Config, validator *contracts.Validator, registry *contracts.Registry) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 180 * time.Second,
		},
		validator: validator,
		registry:  registry,
	}
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatRequest struct {
	Model    string                 `json:"model"`
	Messages []ollamaMessage        `json:"messages"`
	Stream   bool                   `json:"stream"`
	Format   map[string]interface{} `json:"format,omitempty"`
	Options  map[string]interface{} `json:"options,omitempty"`
}

type ollamaChatResponse struct {
	Model   string        `json:"model"`
	Message ollamaMessage `json:"message"`
	Done    bool          `json:"done"`
}

func (c *Client) ParsePrompt(ctx context.Context, prompt string, datasetID string, lang string) (*ParseResult, error) {
	if len(prompt) > 4000 {
		return &ParseResult{
			Status:  StatusInvalid,
			Message: "Prompt exceeds maximum length of 4000 characters",
		}, nil
	}

	promptLower := strings.ToLower(prompt)

	// Check unsupported operations per spec:
	unsupportedKeywords := []string{
		"walk", "fly", "behavior", "walking", "leg", "wing", "muscle",
		"ходить", "поведение", "мышца", "крыло", "лапка",
	}
	for _, kw := range unsupportedKeywords {
		if strings.Contains(promptLower, kw) {
			return &ParseResult{
				Status:  StatusUnsupported,
				Message: fmt.Sprintf("The requested simulation involves whole-organism behavior/biomechanics ('%s') which is beyond the spiking neural network MVP scope.", kw),
			}, nil
		}
	}

	// Check ambiguous / needs_input queries:
	if !strings.Contains(promptLower, "sugar") &&
		!strings.Contains(promptLower, "grn") &&
		!strings.Contains(promptLower, "mn9") &&
		!strings.Contains(promptLower, "сахар") &&
		!strings.Contains(promptLower, "720575") {
		return &ParseResult{
			Status:           StatusNeedsInput,
			UnresolvedFields: []string{"activation.selector", "readout.selector"},
			Message:          "Specific neuron groups or FlyWire IDs were not identified. Please specify an input group (e.g. 'sugar_grn') and readout group (e.g. 'mn9').",
		}, nil
	}

	// Try Ollama first if accessible
	res, err := c.queryOllama(ctx, prompt)
	if err == nil && res != nil {
		return res, nil
	}

	// Fallback to deterministic NLP heuristic parser
	return c.fallbackHeuristicParse(prompt, datasetID, lang)
}

func (c *Client) queryOllama(ctx context.Context, prompt string) (*ParseResult, error) {
	systemPrompt := fmt.Sprintf(`You are a scientific experiment planner for a Drosophila brain spiking neural network.
Available registered neuron groups:
- "sugar_grn": sugar gustatory receptor neurons (input)
- "bitter_grn": bitter receptor neurons (input)
- "demo_silencing": target sugar GRN for silencing (inhibitory/silencing target)
- "mn9": feeding motor neurons (readout)

Return JSON with:
- "experiment_type": "single" or "compare_silencing"
- "activation": [{"selector": {"group_id": "..."}, "rate_hz": 50.0}]
- "silencing": [{"selector": {"group_id": "..."}}]
- "readout": [{"selector": {"group_id": "mn9"}}]
- "duration_ms": 100.0
- "repeats": 1
- "base_seed": 42
- "report_language": "en"`)

	reqBody := ollamaChatRequest{
		Model: c.cfg.OllamaModel,
		Messages: []ollamaMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: prompt},
		},
		Stream: false,
		Format: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"experiment_type": map[string]interface{}{"type": "string", "enum": []string{"single", "compare_silencing"}},
				"duration_ms":     map[string]interface{}{"type": "number"},
				"repeats":         map[string]interface{}{"type": "integer"},
				"base_seed":       map[string]interface{}{"type": "integer"},
			},
			"required": []string{"experiment_type"},
		},
		Options: map[string]interface{}{
			"temperature": 0.0,
		},
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/api/chat", c.cfg.OllamaURL)
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, ErrLLMUnavailable
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama status: %s", resp.Status)
	}

	var chatResp ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return nil, err
	}

	// Validate parsed plan
	valRes, err := c.validator.ValidateRawJSON([]byte(chatResp.Message.Content))
	if err != nil {
		return nil, err
	}

	return &ParseResult{
		Status:          StatusReady,
		Plan:            valRes.Plan,
		ResolvedPlan:    valRes.ResolvedPlan,
		DefaultsApplied: valRes.DefaultsApplied,
		Message:         "Plan parsed successfully by local LLM",
		LLMMetadata: map[string]interface{}{
			"model":  c.cfg.OllamaModel,
			"source": "ollama",
		},
	}, nil
}

func (c *Client) fallbackHeuristicParse(prompt string, datasetID string, lang string) (*ParseResult, error) {
	lower := strings.ToLower(prompt)

	expType := "single"
	if strings.Contains(lower, "compare") ||
		strings.Contains(lower, "сравни") ||
		strings.Contains(lower, "silencing") ||
		strings.Contains(lower, "подавлен") {
		expType = "compare_silencing"
	}

	rate := 50.0
	if strings.Contains(lower, "100 hz") || strings.Contains(lower, "100 гц") || strings.Contains(lower, "100hz") {
		rate = 100.0
	} else if strings.Contains(lower, "150 hz") || strings.Contains(lower, "150 гц") || strings.Contains(lower, "150hz") {
		rate = 150.0
	}

	duration := 100.0
	if strings.Contains(lower, "200 ms") || strings.Contains(lower, "200 мс") || strings.Contains(lower, "200ms") {
		duration = 200.0
	}

	plan := &domain.ExperimentPlan{
		SchemaVersion:  "1.0",
		DatasetID:      "flywire_630",
		ModelID:        "shiu_lif_rust",
		ExperimentType: expType,
		Activation: []domain.ActivationSpec{
			{
				Selector: domain.Selector{GroupID: "sugar_grn"},
				RateHz:   rate,
			},
		},
		Silencing:      []domain.SilencingSpec{},
		Readout: []domain.ReadoutSpec{
			{
				Selector: domain.Selector{GroupID: "mn9"},
			},
		},
		DurationMs:     duration,
		Repeats:        1,
		BaseSeed:       42,
		ReportLanguage: lang,
	}

	if expType == "compare_silencing" {
		plan.Silencing = append(plan.Silencing, domain.SilencingSpec{
			Selector: domain.Selector{GroupID: "demo_silencing"},
		})
	}

	valRes, err := c.validator.ValidatePlan(plan)
	if err != nil {
		return &ParseResult{
			Status:  StatusInvalid,
			Message: err.Error(),
		}, nil
	}

	return &ParseResult{
		Status:          StatusReady,
		Plan:            valRes.Plan,
		ResolvedPlan:    valRes.ResolvedPlan,
		DefaultsApplied: valRes.DefaultsApplied,
		Message:         "Plan parsed and validated successfully",
		LLMMetadata: map[string]interface{}{
			"model":  c.cfg.OllamaModel,
			"source": "heuristic_fallback",
		},
	}, nil
}
