package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ad3002/flylab/internal/contracts"
)

// buildSystemPrompt describes the registry and the plan limits to the planner. It is generated
// from the registry and the schema limits, so it cannot drift from what the validator accepts.
func buildSystemPrompt(reg *contracts.Registry, l contracts.PlanLimits) string {
	var b strings.Builder
	b.WriteString(`You are the experiment planner of FlyLab, a web tool that runs leaky integrate-and-fire (LIF) spiking simulations of the whole adult Drosophila melanogaster brain connectome (FlyWire v630: 127,400 neurons, 14.7 million synaptic connections; model of Shiu et al. 2024, Nature). You translate one natural-language request (English or Russian) into a structured experiment plan. You do not run anything yourself.

What one simulation can do:
- Drive one or two sets of neurons with Poisson spike trains at a fixed rate ("activation").
- Optionally silence a set of neurons (their outgoing synapses are set to zero) and compare the network with and without that silencing: experiment_type "compare_silencing" runs condition A = activation only and condition B = activation + silencing.
- Record spike counts and firing rates of readout neurons (and of the whole brain).

Registered neuron groups (use these group_id values exactly):
`)
	for _, g := range reg.Groups {
		fmt.Fprintf(&b, "- %s: %s, %d neurons. %s", g.GroupID, g.NameEn, len(g.NeuronIDs), strings.TrimSpace(g.Description))
		if len(g.Aliases) > 0 {
			fmt.Fprintf(&b, " Also called: %s.", strings.Join(g.Aliases, ", "))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, `
The user may instead give explicit FlyWire root IDs (15-20 digit numbers); put those in neuron_ids instead of group_id. Never invent root IDs, never use group_id values that are not listed above. Each selector uses exactly one of group_id or neuron_ids.

Plan limits (enforced by a validator; a plan outside them is rejected):
- experiment_type: one of %s. "single" = activation only, silencing must be []. "compare_silencing" needs at least one silencing entry.
- activation: 1 to %d entries; rate_hz from %g to %g Hz and greater than 0. Each entry is driven at its own rate, so two entries must not share any neuron.
- duration_ms: %g to %g ms.
- repeats: %d to %d.
- base_seed: integer from 0 to %d.
- readout: at least one entry.

Defaults for values the user does not state (say in message which defaults you used): rate_hz 50, duration_ms 100, repeats 1, base_seed 42, readout mn9 (the feeding motor neuron pair). The stimulated neurons and, for compare_silencing, the silenced neurons are never defaulted: they must come from the request.

Choose status:
- "ready": the request maps unambiguously onto a plan within the limits. Fill plan completely. message: one or two sentences summarising the planned experiment.
- "needs_input": it is unclear which neurons to stimulate (or which to silence for a comparison), the request names neurons or cell types that are not registered and gives no root IDs, or it asks for values outside the limits. Put the missing or invalid plan paths in unresolved_fields (for example "activation.selector", "silencing.selector", "readout.selector", "duration_ms", "activation.rate_hz") and ask one concrete question in message. Do not guess. Omit plan or give your best partial draft.
- "unsupported": the request is about whole-animal behaviour or biomechanics (walking, flight, grooming, courtship, escape, legs, wings, muscles, body movement), learning or plasticity over time, neuromodulators or drugs, other species, or anything that is not a spiking simulation of this connectome. In message explain briefly what can be simulated instead (for example the firing of motor neurons such as mn9 as a proxy).
Mentioning the fly, Drosophila, FlyWire, the connectome or the brain is NOT by itself a reason for "unsupported".

Write message in the language of the request (Russian if the request is in Russian). unresolved_fields is [] when nothing is missing. Answer only with the structured output.
`, quoteList(l.ExperimentTypes), l.ActivationMax, l.RateMinHz, l.RateMaxHz,
		l.DurationMinMs, l.DurationMaxMs, l.RepeatsMin, l.RepeatsMax, l.BaseSeedMax)
	return b.String()
}

func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = `"` + s + `"`
	}
	return strings.Join(q, ", ")
}

// PlanSchema returns the JSON schema object of a planner plan (the "plan" property of the
// planner output). The interpretation schema reuses it for each hypothesis' test plan, so both
// LLM outputs share one plan shape and one set of limits.
func PlanSchema(reg *contracts.Registry, l contracts.PlanLimits) (map[string]interface{}, error) {
	groupIDs := make([]string, 0, len(reg.Groups))
	for _, g := range reg.Groups {
		groupIDs = append(groupIDs, g.GroupID)
	}
	if len(groupIDs) == 0 {
		return nil, fmt.Errorf("registry has no groups")
	}
	neuronIDs := map[string]interface{}{
		"type":     "array",
		"minItems": 1,
		"maxItems": 500,
		"items":    map[string]interface{}{"type": "string", "pattern": "^[0-9]{15,20}$"},
	}
	groupID := map[string]interface{}{"type": "string", "enum": groupIDs}
	selector := map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]interface{}{
			"group_id":   groupID,
			"neuron_ids": neuronIDs,
		},
	}
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"experiment_type", "activation", "silencing", "readout", "duration_ms", "repeats", "base_seed"},
		"properties": map[string]interface{}{
			"experiment_type": map[string]interface{}{"type": "string", "enum": l.ExperimentTypes},
			"activation": map[string]interface{}{
				"type":     "array",
				"minItems": 1,
				"maxItems": l.ActivationMax,
				"items": map[string]interface{}{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"rate_hz"},
					"properties": map[string]interface{}{
						"group_id":   groupID,
						"neuron_ids": neuronIDs,
						"rate_hz":    map[string]interface{}{"type": "number", "minimum": l.RateMinHz, "maximum": l.RateMaxHz},
					},
				},
			},
			"silencing":   map[string]interface{}{"type": "array", "maxItems": 20, "items": selector},
			"readout":     map[string]interface{}{"type": "array", "minItems": 1, "maxItems": 20, "items": selector},
			"duration_ms": map[string]interface{}{"type": "number", "minimum": l.DurationMinMs, "maximum": l.DurationMaxMs},
			"repeats":     map[string]interface{}{"type": "integer", "minimum": l.RepeatsMin, "maximum": l.RepeatsMax},
			"base_seed":   map[string]interface{}{"type": "integer", "minimum": 0, "maximum": l.BaseSeedMax},
		},
	}, nil
}

// buildPlannerSchema returns the JSON schema passed with --json-schema.
func buildPlannerSchema(reg *contracts.Registry, l contracts.PlanLimits) (string, error) {
	plan, err := PlanSchema(reg, l)
	if err != nil {
		return "", err
	}
	schema := map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"status", "message", "unresolved_fields"},
		"properties": map[string]interface{}{
			"status":            map[string]interface{}{"type": "string", "enum": []string{"ready", "needs_input", "unsupported"}},
			"message":           map[string]interface{}{"type": "string", "minLength": 1},
			"unresolved_fields": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
			"plan":              plan,
		},
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
