package contracts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/ad3002/flylab/internal/domain"
)

type Registry struct {
	GroupsMap map[string]*domain.NeuronGroup
	Groups    []*domain.NeuronGroup
}

func LoadRegistry(registryDir string) (*Registry, error) {
	path := filepath.Join(registryDir, "groups.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read groups.json: %w", err)
	}

	var root struct {
		Groups []*domain.NeuronGroup `json:"groups"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("failed to unmarshal groups.json: %w", err)
	}

	groupsMap := make(map[string]*domain.NeuronGroup)
	for _, g := range root.Groups {
		groupsMap[g.GroupID] = g
		for _, alias := range g.Aliases {
			groupsMap[alias] = g
		}
	}

	return &Registry{
		GroupsMap: groupsMap,
		Groups:    root.Groups,
	}, nil
}

type Validator struct {
	schema   *jsonschema.Schema
	registry *Registry
	limits   PlanLimits
	// neurons, when set, is the connectome's root id set: explicit neuron_ids outside it are
	// rejected (*UnknownNeuronsError). The server always sets it at startup (cmd/flylab); a
	// validator without it (unit tests of the schema) checks only the shape of ids.
	neurons *NeuronIDSet
}

// SetNeuronIDs enables the neuron id existence check (contract v4 section 5).
func (v *Validator) SetNeuronIDs(set *NeuronIDSet) { v.neurons = set }

// NeuronIDs returns the configured root id set (nil when the check is off).
func (v *Validator) NeuronIDs() *NeuronIDSet { return v.neurons }

// unknownNeurons collects explicit neuron_ids that are not in the connectome, per selector.
func (v *Validator) unknownNeurons(plan *domain.ExperimentPlan) error {
	if v.neurons == nil {
		return nil
	}
	e := &UnknownNeuronsError{}
	seen := map[string]bool{}
	check := func(path string, sel domain.Selector) {
		var bad []string
		local := map[string]bool{}
		for _, raw := range sel.NeuronIDs {
			id := strings.TrimSpace(raw)
			if id == "" || v.neurons.Has(id) || local[id] {
				continue
			}
			local[id] = true
			bad = append(bad, id)
			if !seen[id] {
				seen[id] = true
				e.IDs = append(e.IDs, id)
			}
		}
		if len(bad) > 0 {
			e.Fields = append(e.Fields, UnknownNeuronField{Path: path, IDs: bad})
		}
	}
	for i, a := range plan.Activation {
		check(fmt.Sprintf("activation[%d].selector", i), a.Selector)
	}
	for i, s := range plan.Silencing {
		check(fmt.Sprintf("silencing[%d].selector", i), s.Selector)
	}
	for i, r := range plan.Readout {
		check(fmt.Sprintf("readout[%d].selector", i), r.Selector)
	}
	if len(e.IDs) > 0 {
		return e
	}
	return nil
}

// PlanLimits are the numeric limits of contracts/experiment-plan.schema.json, read from the
// schema file itself so the planner prompt can never drift from what the validator enforces.
type PlanLimits struct {
	ExperimentTypes []string
	RateMinHz       float64
	RateMaxHz       float64
	DurationMinMs   float64
	DurationMaxMs   float64
	RepeatsMin      int
	RepeatsMax      int
	ActivationMax   int
	BaseSeedMax     int64
}

// Limits returns the schema limits.
func (v *Validator) Limits() PlanLimits { return v.limits }

func loadPlanLimits(schemaPath string) (PlanLimits, error) {
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		return PlanLimits{}, fmt.Errorf("read schema: %w", err)
	}
	type numProp struct {
		Minimum  *float64 `json:"minimum"`
		Maximum  *float64 `json:"maximum"`
		MaxItems *int     `json:"maxItems"`
		Enum     []string `json:"enum"`
		Items    *struct {
			Properties map[string]numProp `json:"properties"`
		} `json:"items"`
	}
	var doc struct {
		Properties map[string]numProp `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return PlanLimits{}, fmt.Errorf("parse schema: %w", err)
	}
	var missing []string
	get := func(name string) numProp {
		p, ok := doc.Properties[name]
		if !ok {
			missing = append(missing, name)
		}
		return p
	}
	num := func(v *float64, what string) float64 {
		if v == nil {
			missing = append(missing, what)
			return 0
		}
		return *v
	}

	var l PlanLimits
	et := get("experiment_type")
	if len(et.Enum) == 0 {
		missing = append(missing, "experiment_type.enum")
	}
	l.ExperimentTypes = et.Enum

	act := get("activation")
	if act.MaxItems == nil {
		missing = append(missing, "activation.maxItems")
	} else {
		l.ActivationMax = *act.MaxItems
	}
	if act.Items == nil {
		missing = append(missing, "activation.items")
	} else {
		rate := act.Items.Properties["rate_hz"]
		l.RateMinHz = num(rate.Minimum, "activation.items.rate_hz.minimum")
		l.RateMaxHz = num(rate.Maximum, "activation.items.rate_hz.maximum")
	}

	dur := get("duration_ms")
	l.DurationMinMs = num(dur.Minimum, "duration_ms.minimum")
	l.DurationMaxMs = num(dur.Maximum, "duration_ms.maximum")
	rep := get("repeats")
	l.RepeatsMin = int(num(rep.Minimum, "repeats.minimum"))
	l.RepeatsMax = int(num(rep.Maximum, "repeats.maximum"))
	seed := get("base_seed")
	l.BaseSeedMax = int64(num(seed.Maximum, "base_seed.maximum"))

	if len(missing) > 0 {
		return PlanLimits{}, fmt.Errorf("schema %s lacks limits: %s", schemaPath, strings.Join(missing, ", "))
	}
	return l, nil
}

func NewValidator(contractsDir string, registry *Registry) (*Validator, error) {
	schemaPath := filepath.Join(contractsDir, "experiment-plan.schema.json")
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		return nil, fmt.Errorf("failed to compile experiment-plan schema: %w", err)
	}

	limits, err := loadPlanLimits(schemaPath)
	if err != nil {
		return nil, err
	}

	return &Validator{
		schema:   schema,
		registry: registry,
		limits:   limits,
	}, nil
}

type ValidationResult struct {
	Plan            *domain.ExperimentPlan
	ResolvedPlan    *domain.ResolvedPlan
	DefaultsApplied map[string]interface{}
}

func (v *Validator) ValidateRawJSON(raw []byte) (*ValidationResult, error) {
	// 1. Strict schema validation
	var vObj interface{}
	if err := json.Unmarshal(raw, &vObj); err != nil {
		return nil, fmt.Errorf("malformed JSON: %w", err)
	}

	if err := v.schema.Validate(vObj); err != nil {
		return nil, fmt.Errorf("schema validation error: %w", err)
	}

	// 2. Strict decoding with DisallowUnknownFields
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var plan domain.ExperimentPlan
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("strict decode error: %w", err)
	}

	return v.ValidatePlan(&plan)
}

func (v *Validator) ValidatePlan(plan *domain.ExperimentPlan) (*ValidationResult, error) {
	defaultsApplied := make(map[string]interface{})

	// Apply technical defaults if missing
	if plan.SchemaVersion == "" {
		plan.SchemaVersion = "1.0"
		defaultsApplied["schema_version"] = "1.0"
	}
	if plan.DatasetID == "" {
		plan.DatasetID = "flywire_630"
		defaultsApplied["dataset_id"] = "flywire_630"
	}
	if plan.ModelID == "" {
		plan.ModelID = "shiu_lif_rust"
		defaultsApplied["model_id"] = "shiu_lif_rust"
	}
	if plan.DurationMs <= 0.0 {
		plan.DurationMs = 100.0
		defaultsApplied["duration_ms"] = 100.0
	}
	if plan.Repeats <= 0 {
		plan.Repeats = 1
		defaultsApplied["repeats"] = 1
	}
	if plan.BaseSeed == 0 {
		plan.BaseSeed = 42
		defaultsApplied["base_seed"] = 42
	}
	if plan.ReportLanguage == "" {
		plan.ReportLanguage = "en"
		defaultsApplied["report_language"] = "en"
	}

	// Range checks
	if plan.DurationMs < 10.0 || plan.DurationMs > 1000.0 {
		return nil, fmt.Errorf("duration_ms must be between 10.0 and 1000.0 (got %.1f)", plan.DurationMs)
	}
	if plan.Repeats < 1 || plan.Repeats > 3 {
		return nil, fmt.Errorf("repeats must be between 1 and 3 (got %d)", plan.Repeats)
	}
	if len(plan.Activation) == 0 || len(plan.Activation) > 2 {
		return nil, fmt.Errorf("activation must have 1 or 2 sets (got %d)", len(plan.Activation))
	}
	if len(plan.Readout) == 0 || len(plan.Readout) > 20 {
		return nil, fmt.Errorf("readout must have between 1 and 20 selectors (got %d)", len(plan.Readout))
	}
	if len(plan.Silencing) > 20 {
		return nil, fmt.Errorf("silencing cannot exceed 20 selectors (got %d)", len(plan.Silencing))
	}
	if plan.ExperimentType == "compare_silencing" && len(plan.Silencing) == 0 {
		return nil, fmt.Errorf("compare_silencing requires at least 1 silencing selector")
	}
	if err := v.unknownNeurons(plan); err != nil {
		return nil, err
	}

	// Resolve Selectors
	var resolvedActivations []domain.ResolvedActivation
	var allActivationIDs []string
	// flysim drives each activation set at its own rate, so a neuron may belong to one set only.
	activationOwner := make(map[string]int)

	for idx, act := range plan.Activation {
		if act.RateHz <= 0.0 {
			act.RateHz = 50.0
			defaultsApplied[fmt.Sprintf("activation[%d].rate_hz", idx)] = 50.0
		}
		if act.RateHz < 0.0 || act.RateHz > 200.0 {
			return nil, fmt.Errorf("rate_hz must be between 0.0 and 200.0 (got %.1f)", act.RateHz)
		}
		ids, err := v.resolveSelector(act.Selector)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve activation selector: %w", err)
		}
		if len(ids) == 0 || len(ids) > 500 {
			return nil, fmt.Errorf("activation set must contain 1 to 500 unique neurons (got %d)", len(ids))
		}
		for _, id := range ids {
			if prev, ok := activationOwner[id]; ok {
				return nil, fmt.Errorf("neuron %s is in both activation[%d] and activation[%d]; each neuron can be driven at only one rate", id, prev, idx)
			}
			activationOwner[id] = idx
		}
		resolvedActivations = append(resolvedActivations, domain.ResolvedActivation{
			NeuronIDs: ids,
			RateHz:    act.RateHz,
		})
		allActivationIDs = append(allActivationIDs, ids...)
	}

	// Resolve Silencing
	var allSilencingIDs []string
	for _, slnc := range plan.Silencing {
		ids, err := v.resolveSelector(slnc.Selector)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve silencing selector: %w", err)
		}
		allSilencingIDs = append(allSilencingIDs, ids...)
	}
	allSilencingIDs = deduplicateAndSort(allSilencingIDs)
	if len(allSilencingIDs) > 500 {
		return nil, fmt.Errorf("total silenced neurons cannot exceed 500 (got %d)", len(allSilencingIDs))
	}

	// Reject overlap between activation and silencing per spec
	actSet := make(map[string]bool)
	for _, id := range allActivationIDs {
		actSet[id] = true
	}

	// Resolve Readout
	var allReadoutIDs []string
	for _, ro := range plan.Readout {
		ids, err := v.resolveSelector(ro.Selector)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve readout selector: %w", err)
		}
		allReadoutIDs = append(allReadoutIDs, ids...)
	}
	allReadoutIDs = deduplicateAndSort(allReadoutIDs)
	if len(allReadoutIDs) == 0 || len(allReadoutIDs) > 500 {
		return nil, fmt.Errorf("readout must contain between 1 and 500 unique neurons (got %d)", len(allReadoutIDs))
	}

	// Generate ResolvedPlan
	planID := fmt.Sprintf("plan_%s", uuid.New().String()[:8])
	rawResolved, _ := json.Marshal(struct {
		DatasetID string
		Duration  float64
		Repeats   int
		Seed      uint64
		Act       []domain.ResolvedActivation
		Slnc      []string
		Readout   []string
	}{
		DatasetID: plan.DatasetID,
		Duration:  plan.DurationMs,
		Repeats:   plan.Repeats,
		Seed:      plan.BaseSeed,
		Act:       resolvedActivations,
		Slnc:      allSilencingIDs,
		Readout:   allReadoutIDs,
	})
	hasher := sha256.New()
	hasher.Write(rawResolved)
	planHash := hex.EncodeToString(hasher.Sum(nil))

	resolvedPlan := &domain.ResolvedPlan{
		SchemaVersion:      "1.0",
		PlanID:             planID,
		PlanHash:           planHash,
		DatasetID:          plan.DatasetID,
		ModelID:            plan.ModelID,
		ExperimentType:     plan.ExperimentType,
		Activation:         resolvedActivations,
		SilencingNeuronIDs: allSilencingIDs,
		ReadoutNeuronIDs:   allReadoutIDs,
		DurationMs:         plan.DurationMs,
		Repeats:            plan.Repeats,
		BaseSeed:           plan.BaseSeed,
		ReportLanguage:     plan.ReportLanguage,
		DtMs:               0.1,
		CreatedAt:          time.Now().UTC(),
	}

	return &ValidationResult{
		Plan:            plan,
		ResolvedPlan:    resolvedPlan,
		DefaultsApplied: defaultsApplied,
	}, nil
}

func (v *Validator) resolveSelector(sel domain.Selector) ([]string, error) {
	if sel.GroupID != "" && len(sel.NeuronIDs) > 0 {
		return nil, fmt.Errorf("selector cannot specify both group_id and neuron_ids")
	}
	if sel.GroupID == "" && len(sel.NeuronIDs) == 0 {
		return nil, fmt.Errorf("selector must specify either group_id or neuron_ids")
	}

	if sel.GroupID != "" {
		group, exists := v.registry.GroupsMap[sel.GroupID]
		if !exists {
			return nil, fmt.Errorf("unknown group_id: '%s'", sel.GroupID)
		}
		return deduplicateAndSort(group.NeuronIDs), nil
	}

	var cleaned []string
	for _, id := range sel.NeuronIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	return deduplicateAndSort(cleaned), nil
}

func deduplicateAndSort(ids []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
