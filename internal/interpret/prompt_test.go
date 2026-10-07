package interpret

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/llm"
)

func loadValidator(t *testing.T, reg *contracts.Registry) *contracts.Validator {
	t.Helper()
	v, err := contracts.NewValidator(filepath.Join(projectRoot(t), "contracts"), reg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSystemPromptRequirements(t *testing.T) {
	reg := loadRegistry(t)
	l := loadValidator(t, reg).Limits()
	for lang, language := range map[string]string{"en": "English", "ru": "Russian"} {
		p := BuildSystemPrompt(reg, l, lang)
		for _, want := range []string{
			"hypothesis generator", "computational neuroscientist", "HYPOTHESES FOR EXPERT REVIEW, not conclusions",
			"observations: restate what the digest shows, with no interpretation",
			"hypotheses: possible explanations", "exactly as it appears in the digest",
			"Never invent cell types, cell classes, neurotransmitters, neuron ids, connections or numbers that are not in the digest",
			"Never claim that the fly would perform a behaviour", "behavioural_proxies", "proxy_id",
			`"low", "medium" or "high"`, "confidence_reason", "unannotated neurons, low annotation coverage",
			"discriminating experiment this platform can run",
			fmt.Sprintf("rate_hz from %g to %g Hz", l.RateMinHz, l.RateMaxHz),
			fmt.Sprintf("duration_ms %g to %g; repeats %d to %d", l.DurationMinMs, l.DurationMaxMs, l.RepeatsMin, l.RepeatsMax),
			"neuron_ids may only be root ids that appear in the digest",
			"suggested_reading: only citation strings already present in the digest",
			"Write every text field (headline, observations, hypotheses, caveats, test descriptions, limitations) in " + language,
			// digest fields an expert's hypotheses need (review findings)
			"top_neurons_by_rate (non-stimulated neurons only)", "stimulated_neurons and silenced_neurons", "model_sign",
			"spike_count_A", "readout_inputs", "model_parameters", "ids_by_cell_sub_class", "per_field",
			// tests that can falsify something
			"condition B can only ADD SILENCING", `A "single" run has no condition B, no delta, no rate_B_hz`,
			"FlyLab does not compare two runs", "must describe exactly the plan in test.plan",
			// calibration
			`"medium" or "high" requires supporting numbers from THIS run's digest`,
		} {
			if !strings.Contains(p, want) {
				t.Fatalf("%s prompt lacks %q:\n%s", lang, want, p)
			}
		}
		for _, f := range ModelFacts {
			if !strings.Contains(p, f) {
				t.Fatalf("%s prompt lacks model fact %q", lang, f)
			}
		}
		for _, g := range reg.Groups {
			if !strings.Contains(p, "- "+g.GroupID+": ") {
				t.Fatalf("%s prompt does not list registry group %s", lang, g.GroupID)
			}
		}
		if !strings.Contains(p, "OUTGOING synapses of the silenced neurons only") {
			t.Fatalf("prompt must state the silencing semantics")
		}
	}
}

func TestSchemaReusesPlannerPlanShape(t *testing.T) {
	reg := loadRegistry(t)
	l := loadValidator(t, reg).Limits()
	raw, err := BuildSchema(reg, l)
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	props := s["properties"].(map[string]interface{})
	hyp := props["hypotheses"].(map[string]interface{})["items"].(map[string]interface{})["properties"].(map[string]interface{})
	conf := hyp["confidence"].(map[string]interface{})["enum"].([]interface{})
	if fmt.Sprint(conf) != "[low medium high]" {
		t.Fatalf("confidence enum: %v", conf)
	}
	plan := hyp["test"].(map[string]interface{})["properties"].(map[string]interface{})["plan"]
	want, err := llm.PlanSchema(reg, l)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(want)
	var wantObj interface{}
	_ = json.Unmarshal(wantJSON, &wantObj)
	if !reflect.DeepEqual(plan, wantObj) {
		t.Fatalf("test.plan schema must equal the planner plan schema")
	}
	req := fmt.Sprint(s["required"])
	if req != "[headline observations hypotheses limitations suggested_reading]" {
		t.Fatalf("required: %s", req)
	}
}

const sampleOutput = `{
 "headline": "Silencing 720575940616885538 lowers MN9 firing",
 "observations": [{"text": "B has 44 spikes vs 50 in A", "evidence": ["totals.A.spikes = 50", "totals.B.spikes = 44"]}],
 "hypotheses": [
  {"title": "GABA relay", "statement": "Neuron 720575940000000001 relays sugar input", "confidence": "low",
   "confidence_reason": "one seed", "evidence": ["720575940000000001 delta_hz -40", "720575940999999999 rate"],
   "caveats": [],
   "test": {"description": "silence it", "expected_if_true": "MN9 rate drops",
     "plan": {"experiment_type": "compare_silencing", "activation": [{"group_id": "sugar_grn", "rate_hz": 100}],
       "silencing": [{"neuron_ids": ["720575940000000001"]}], "readout": [{"group_id": "mn9"}], "duration_ms": 200, "repeats": 2, "base_seed": 7}}},
  {"title": "Long run", "statement": "x", "confidence": "medium", "confidence_reason": "r", "evidence": ["totals"], "caveats": ["c"],
   "test": {"description": "d", "expected_if_true": "e",
     "plan": {"experiment_type": "single", "activation": [{"group_id": "sugar_grn", "rate_hz": 100}], "silencing": [],
       "readout": [{"neuron_ids": ["720575940888888888"]}], "duration_ms": 5000, "repeats": 1, "base_seed": 1}}}
 ],
 "limitations": ["single seed"],
 "suggested_reading": ["Shiu et al., Nature 2024. doi:10.1038/s41586-024-07763-9", "Invented et al. 2023. doi:10.1000/fake.1"]
}`

type countingSaver struct{ n int }

func (c *countingSaver) SavePlan(_ *domain.ExperimentPlan, _ *domain.ResolvedPlan) error {
	c.n++
	return nil
}

func TestDecodeValidateAndWarn(t *testing.T) {
	reg := loadRegistry(t)
	val := loadValidator(t, reg)
	out, err := decodeOutput([]byte(sampleOutput))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	saver := &countingSaver{}
	if err := validateTests(out, val, saver, "en"); err != nil {
		t.Fatal(err)
	}
	h0, h1 := out.Hypotheses[0].Test, out.Hypotheses[1].Test
	if h0.PlanID == nil || !strings.HasPrefix(*h0.PlanID, "plan_") || h0.PlanError != nil || saver.n != 1 {
		t.Fatalf("valid test plan must get a plan_id and be saved: %+v (saved %d)", h0, saver.n)
	}
	if h1.PlanID != nil || h1.PlanError == nil || !strings.Contains(*h1.PlanError, "duration_ms") || h1.Plan == nil || h1.Plan.DurationMs != 5000 {
		t.Fatalf("invalid test plan must keep the plan and carry plan_error: %+v", h1)
	}

	digestJSON := []byte(`{"top":[{"root_id":"720575940000000001"},{"root_id":"720575940616885538"}]}`)
	ws := evidenceWarnings(out, digestJSON, []string{ModelCitation})
	var got []string
	for _, w := range ws {
		got = append(got, w.Kind+"@"+w.Location+"@"+w.NeuronID+w.Reference)
	}
	want := []string{
		"unknown_neuron_id@hypotheses[0].evidence[1]@720575940999999999",
		"unknown_neuron_id@hypotheses[1].test.plan@720575940888888888",
		"unknown_reference@suggested_reading[1]@Invented et al. 2023. doi:10.1000/fake.1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("evidence warnings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDecodeOutputRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"not an object":  `"text"`,
		"unknown field":  strings.Replace(sampleOutput, `"headline"`, `"verdict": "x", "headline"`, 1),
		"bad confidence": strings.Replace(sampleOutput, `"confidence": "low"`, `"confidence": "certain"`, 1),
		"no hypotheses":  `{"headline":"h","observations":[{"text":"t","evidence":["e"]}],"hypotheses":[],"limitations":[],"suggested_reading":[]}`,
		"no plan":        strings.Replace(sampleOutput, `"plan": {"experiment_type": "single"`, `"planx": {"experiment_type": "single"`, 1),
	}
	for name, body := range cases {
		if _, err := decodeOutput([]byte(body)); err == nil {
			t.Fatalf("%s: expected a decode error", name)
		}
	}
}

func TestQualityWarnings(t *testing.T) {
	compare := func(desc, expected string) HypothesisTest {
		return HypothesisTest{Description: desc, ExpectedIfTrue: expected, Plan: &llm.PlanSpec{ExperimentType: "compare_silencing"}}
	}
	single := func(desc, expected string) HypothesisTest {
		return HypothesisTest{Description: desc, ExpectedIfTrue: expected, Plan: &llm.PlanSpec{ExperimentType: "single"}}
	}
	digest := []byte(`{"readouts":[{"root_id":"720575940660219265","rate_A_hz":8.3333,"rate_B_hz":3.3333,"delta_hz":-3.75}]}`)
	cases := []struct {
		name      string
		h         Hypothesis
		wantTest  []string // substrings, one per expected test warning, in order
		wantCalib string   // "" = no calibration warning
	}{
		{"job_e84e0822 H4: single plan for a compare description, delta of a run that has none",
			Hypothesis{Confidence: "low", ConfidenceReason: "one seed", Evidence: []string{"720575940660219265 delta_hz -3.75"},
				Test: single("Запустить compare_silencing, где в условии B дополнительно активируются bitter_grn",
					"delta_hz у MN9 будет заметно сильнее по модулю, чем -10 и -3.75 Hz")},
			[]string{`relies on "delta_hz"`, "asks for a compare_silencing run", "numbers of this run (-3.75)"}, ""},
		{"job_cb57003e: comparison with this run's rates",
			Hypothesis{Confidence: "low", ConfidenceReason: "r", Evidence: []string{"e"},
				Test: single("sugar_grn alone", "rate_A_hz MN9 выше, чем 8.3333 и 3.3333 Hz")},
			[]string{"numbers of this run (8.3333, 3.3333)"}, ""},
		{"condition B named for a single run (ru)",
			Hypothesis{Confidence: "low", ConfidenceReason: "r", Evidence: []string{"e"},
				Test: single("d", "в условии B частота MN9 упадёт")},
			[]string{`relies on "условии B"`}, ""},
		{"consistent compare test",
			Hypothesis{Confidence: "medium", ConfidenceReason: "MN9 drops from 30 to 10 Hz in this run",
				Evidence: []string{"720575940660219265 rate_A_hz 30, rate_B_hz 10"},
				Test:     compare("silence 720575940000000001 in B", "rate_B_hz of 720575940660219265 below its rate_A_hz; delta_hz < 0")},
			nil, ""},
		{"job_cb57003e H3: medium on a claim this run did not test",
			Hypothesis{Confidence: "medium", ConfidenceReason: "согласуется с описанием прокси, но в этом прогоне не проверялось",
				Evidence: []string{"bitter_grn 100 Hz"}, Test: single("bitter_grn alone", "rate_A_hz MN9 = 0")},
			nil, "in this run"},
		{"high on proxy-only evidence",
			Hypothesis{Confidence: "high", ConfidenceReason: "the paper says so",
				Evidence: []string{"behavioural_proxies[0].evidence: bitter suppresses MN9", "Shiu et al. 2024"}, Test: single("d", "e")},
			nil, "every evidence item refers to a proxy"},
		{"low confidence is never flagged",
			Hypothesis{Confidence: "low", ConfidenceReason: "not tested in this run", Evidence: []string{"behavioural_proxies"}, Test: single("d", "e")},
			nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := &Interpretation{Hypotheses: []Hypothesis{tc.h}}
			// stale values from the model or an earlier pass are replaced
			out.Hypotheses[0].Test.Warnings = []string{"stale"}
			qualityWarnings(out, digest, "en")
			h := out.Hypotheses[0]
			if len(h.Test.Warnings) != len(tc.wantTest) {
				t.Fatalf("test warnings %q, want %d matching %q", h.Test.Warnings, len(tc.wantTest), tc.wantTest)
			}
			for i, w := range tc.wantTest {
				if !strings.Contains(h.Test.Warnings[i], w) {
					t.Fatalf("test warning %d = %q, want it to contain %q", i, h.Test.Warnings[i], w)
				}
			}
			switch {
			case tc.wantCalib == "" && h.CalibrationWarning != nil:
				t.Fatalf("unexpected calibration warning %q", *h.CalibrationWarning)
			case tc.wantCalib != "" && (h.CalibrationWarning == nil || !strings.Contains(*h.CalibrationWarning, tc.wantCalib)):
				t.Fatalf("calibration warning %v, want it to contain %q", h.CalibrationWarning, tc.wantCalib)
			}
		})
	}
	// JSON always carries both fields (warnings [] and calibration_warning null when clean).
	out := &Interpretation{Hypotheses: []Hypothesis{cases[3].h}}
	qualityWarnings(out, digest, "en")
	b, _ := json.Marshal(out.Hypotheses[0])
	if !strings.Contains(string(b), `"warnings":[]`) || !strings.Contains(string(b), `"calibration_warning":null`) {
		t.Fatalf("clean hypothesis JSON: %s", b)
	}
	// Russian interpretations get Russian warnings (same checks).
	ru := &Interpretation{Hypotheses: []Hypothesis{cases[0].h, cases[4].h}}
	qualityWarnings(ru, digest, "ru")
	if w := ru.Hypotheses[0].Test.Warnings; len(w) != 3 || !strings.Contains(w[0], "план — одиночный запуск") || !strings.Contains(w[2], "(-3.75)") {
		t.Fatalf("ru test warnings: %q", w)
	}
	if c := ru.Hypotheses[1].CalibrationWarning; c == nil || !strings.Contains(*c, "возможно, завышена") {
		t.Fatalf("ru calibration warning: %v", c)
	}
	for lang, tx := range qualityTexts {
		for _, f := range []string{tx.bOnly, tx.crossRun, tx.untested} {
			if strings.Count(f, "%") != 1 {
				t.Fatalf("%s: %q must take exactly one argument", lang, f)
			}
		}
		if strings.Count(tx.calibration, "%") != 2 {
			t.Fatalf("%s calibration text must take two arguments", lang)
		}
	}
}
