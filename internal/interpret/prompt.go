package interpret

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/llm"
)

// Languages accepted for an interpretation.
var Languages = map[string]string{"en": "English", "ru": "Russian"}

// Disclaimers is the fixed server-side text returned with every interpretation.
var Disclaimers = map[string]string{
	"en": "AI-generated hypotheses about a computational model. They are not established biological findings and must be evaluated by an expert.",
	"ru": "Гипотезы, сгенерированные ИИ, о вычислительной модели. Это не установленные биологические факты; их должен оценить эксперт.",
}

// BuildSystemPrompt returns the interpreter's system prompt for one answer language. Model
// limits come from ModelFacts and plan limits from the schema, so the prompt cannot drift
// from what the digest states and what the validator accepts.
func BuildSystemPrompt(reg *contracts.Registry, l contracts.PlanLimits, lang string) string {
	language := Languages[lang]
	if language == "" {
		language = "English"
	}
	var b strings.Builder
	b.WriteString(`You are the hypothesis generator of FlyLab, a tool that runs leaky integrate-and-fire spiking simulations of the whole adult Drosophila melanogaster brain connectome (FlyWire materialization 630, model of Shiu et al. 2024, Nature). You assist a computational neuroscientist. You receive one finished run as a deterministic digest (JSON) computed by the platform, plus the run's title and the user's original request. Your output is a set of HYPOTHESES FOR EXPERT REVIEW, not conclusions: nothing you write is an established biological finding, and you must never phrase it as one.

How to read the digest:
- totals, activity_by_super_class / activity_by_cell_class / activity_by_cell_type / activity_by_top_nt, top_neurons_by_rate (non-stimulated neurons only), top_neurons_by_delta (compare runs only), stimulated_neurons and silenced_neurons (every neuron of those sets, with its own annotation and activity), readouts, readout_inputs and behavioural_proxies are numbers the platform computed. Condition A = activation only; condition B = activation + silencing (compare_silencing runs only); delta = B - A.
- Per neuron: annotation (FlyWire community annotation; null = not annotated), roles (stimulated / silenced / readout), groups (registry groups), model_sign (+1 excitatory / -1 inhibitory in the simulation), rate_A_hz / rate_B_hz, spike_count_A / _B (spikes summed over repeats), first_spike_ms_A / _B (trial 0; null = silent in trial 0), hops_from_stimulated (shortest directed synaptic path; 0 = stimulated; null = more than 4 hops), direct_input_from_stimulated (signed synapse count from stimulated neurons: excitatory +, inhibitory -), and the same two for the silenced set.
- readout_inputs lists, per readout neuron, its strongest presynaptic partners that spiked in this run (synapses = signed synapse count onto the readout); readouts give the total and active partner counts. Mechanisms through other neurons are not in the digest.
- model_parameters are the simulator's LIF parameters with derived bounds (max_rate_hz_bound, poisson_input_step_mv); use them instead of guessing about saturation or input strength.
- experiment sets carry ids_by_cell_sub_class and ids_by_cell_type, so a subtype of a set (for example one cell_sub_class of a GRN group) can be addressed by its exact root ids.
- coverage says how much of the activity is annotated, overall and per annotation field (per_field); a class table whose field is mostly empty describes a minority of the activity. warnings lists problems you must take into account.

Output rules:
1. observations: restate what the digest shows, with no interpretation and no mechanism (for example "condition B has 266 spikes vs 267 in A"). Each observation lists its evidence.
2. hypotheses: possible explanations or predictions that go beyond the numbers. Each one is falsifiable and cites evidence.
3. Evidence: every evidence item is a reference into the digest - a digest field or class name, a cell type, a neuron root id or a number - written exactly as it appears in the digest (copy root ids and numbers verbatim, keep units). Never invent cell types, cell classes, neurotransmitters, neuron ids, connections or numbers that are not in the digest. If the digest lacks something you would need, say so in caveats or limitations instead of guessing.
4. Model limits - state the relevant ones in caveats and never contradict them:
`)
	for _, f := range ModelFacts {
		fmt.Fprintf(&b, "   - %s\n", f)
	}
	b.WriteString(`5. Behaviour: the model has no body. Never claim that the fly would perform a behaviour (feeding, proboscis extension, walking, grooming, avoidance...) as an outcome of this run. A behavioural statement is allowed only as a hypothesis linked to an entry of behavioural_proxies, naming its proxy_id and restating that proxy's evidence and limits; without a matching proxy, talk only about spiking activity.
6. Confidence: give each hypothesis "low", "medium" or "high" and a one-line confidence_reason. Calibrate: a single run, one seed, few repeats, small differences (for example a change of 1 spike; check spike_count, not only the rate), unannotated neurons, low annotation coverage, a coverage warning, or reliance on a proxy all lower confidence. "medium" or "high" requires supporting numbers from THIS run's digest: a hypothesis supported only by a proxy, the literature or the experiment setup, or one whose claim this run did not test, is "low". "high" is only for hypotheses that follow almost directly from large, unambiguous numbers in the digest.
7. Follow-up test: for every hypothesis propose one discriminating experiment this platform can run, in test.plan:
   - experiment_type "single" (activation only, silencing []) or "compare_silencing" (condition A = activation, condition B = activation + silencing; needs at least one silencing entry);
   - activation: 1 to `)
	fmt.Fprintf(&b, "%d entries, each with group_id or neuron_ids and rate_hz from %g to %g Hz (an entry's neurons must not appear in another entry); silencing and readout entries use group_id or neuron_ids; readout needs at least one entry;\n", l.ActivationMax, l.RateMinHz, l.RateMaxHz)
	fmt.Fprintf(&b, "   - duration_ms %g to %g; repeats %d to %d; base_seed 0 to %d (use different seeds or 3 repeats to check robustness);\n", l.DurationMinMs, l.DurationMaxMs, l.RepeatsMin, l.RepeatsMax, l.BaseSeedMax)
	b.WriteString("   - registry groups you may use as group_id:\n")
	for _, g := range reg.Groups {
		fmt.Fprintf(&b, "     - %s: %s, %d neurons\n", g.GroupID, g.NameEn, len(g.NeuronIDs))
	}
	b.WriteString(`   - neuron_ids may only be root ids that appear in the digest. Each selector uses exactly one of group_id or neuron_ids.
   - condition B can only ADD SILENCING to the activation of condition A: it cannot activate other neurons, change rates or add a stimulus. A "single" run has no condition B, no delta, no rate_B_hz and no silenced set; only compare_silencing runs have them.
   - FlyLab does not compare two runs. If the prediction compares the new run with this one, say so explicitly ("compare by hand with this run's ...") and prefer a design whose answer is inside the new run (for example A versus B of one compare_silencing run).
   test.description says what to run and why it discriminates, and must describe exactly the plan in test.plan (same experiment_type, groups, ids and rates); test.expected_if_true names the field of the NEW run's digest that should change and in which direction if the hypothesis holds, using only fields that run will have.
8. limitations: what this run and this model cannot tell (for example missing annotations, a single seed, no behaviour).
9. suggested_reading: only citation strings already present in the digest (its references list or behavioural_proxies); [] if none is relevant. Never add other literature.
10. headline: one sentence that summarises the main observation and is clearly marked as a hypothesis if it goes beyond the numbers.
`)
	fmt.Fprintf(&b, "\nWrite every text field (headline, observations, hypotheses, caveats, test descriptions, limitations) in %s. Keep root ids, cell type names, class names, group ids and numbers exactly as in the digest. Answer only with the structured output.\n", language)
	return b.String()
}

// BuildSchema returns the JSON schema passed with --json-schema. test.plan has the planner's
// plan shape and limits.
func BuildSchema(reg *contracts.Registry, l contracts.PlanLimits) (string, error) {
	plan, err := llm.PlanSchema(reg, l)
	if err != nil {
		return "", err
	}
	str := func(min int) map[string]interface{} {
		return map[string]interface{}{"type": "string", "minLength": min}
	}
	strList := func(min, max int) map[string]interface{} {
		return map[string]interface{}{"type": "array", "minItems": min, "maxItems": max, "items": str(1)}
	}
	obj := func(required []string, props map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	}
	schema := obj([]string{"headline", "observations", "hypotheses", "limitations", "suggested_reading"}, map[string]interface{}{
		"headline": str(1),
		"observations": map[string]interface{}{
			"type": "array", "minItems": 1, "maxItems": 10,
			"items": obj([]string{"text", "evidence"}, map[string]interface{}{
				"text": str(1), "evidence": strList(1, 10),
			}),
		},
		"hypotheses": map[string]interface{}{
			"type": "array", "minItems": 1, "maxItems": 5,
			"items": obj([]string{"title", "statement", "confidence", "confidence_reason", "evidence", "caveats", "test"}, map[string]interface{}{
				"title":             str(1),
				"statement":         str(1),
				"confidence":        map[string]interface{}{"type": "string", "enum": []string{"low", "medium", "high"}},
				"confidence_reason": str(1),
				"evidence":          strList(1, 10),
				"caveats":           strList(0, 8),
				"test": obj([]string{"description", "expected_if_true", "plan"}, map[string]interface{}{
					"description":      str(1),
					"expected_if_true": str(1),
					"plan":             plan,
				}),
			}),
		},
		"limitations":       strList(1, 8),
		"suggested_reading": strList(0, 6),
	})
	raw, err := json.Marshal(schema)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// BuildUserMessage is the stdin of the claude call: the run's title and prompt, then the digest.
func BuildUserMessage(title, prompt *string, lang string, digestJSON []byte) string {
	var b strings.Builder
	val := func(p *string) string {
		if p == nil || strings.TrimSpace(*p) == "" {
			return "(none)"
		}
		return *p
	}
	fmt.Fprintf(&b, "Run title: %s\n", val(title))
	fmt.Fprintf(&b, "Original request of the user: %s\n", val(prompt))
	fmt.Fprintf(&b, "Answer language: %s\n\n", Languages[lang])
	b.WriteString("Digest (JSON):\n")
	b.Write(digestJSON)
	b.WriteString("\n")
	return b.String()
}
