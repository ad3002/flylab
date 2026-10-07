package interpret

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/llm"
)

type Observation struct {
	Text     string   `json:"text"`
	Evidence []string `json:"evidence"`
}

// HypothesisTest is the proposed follow-up. PlanID or PlanError is set by post-processing
// (never by the model): the plan is validated and stored, or the validator error is kept.
type HypothesisTest struct {
	Description    string        `json:"description"`
	ExpectedIfTrue string        `json:"expected_if_true"`
	Plan           *llm.PlanSpec `json:"plan"`
	PlanID         *string       `json:"plan_id"`
	PlanError      *string       `json:"plan_error"`
	// Warnings are deterministic consistency checks of the test (set by post-processing):
	// a plan that cannot produce the numbers expected_if_true names, a description that asks
	// for another experiment type, or a comparison with this run the platform does not make.
	Warnings []string `json:"warnings"`
}

type Hypothesis struct {
	Title            string         `json:"title"`
	Statement        string         `json:"statement"`
	Confidence       string         `json:"confidence"`
	ConfidenceReason string         `json:"confidence_reason"`
	Evidence         []string       `json:"evidence"`
	Caveats          []string       `json:"caveats"`
	Test             HypothesisTest `json:"test"`
	// CalibrationWarning flags a medium/high confidence that rests on no number of this run
	// (set by post-processing, null otherwise).
	CalibrationWarning *string `json:"calibration_warning"`
}

// Interpretation is Claude's structured output after post-processing.
type Interpretation struct {
	Headline         string        `json:"headline"`
	Observations     []Observation `json:"observations"`
	Hypotheses       []Hypothesis  `json:"hypotheses"`
	Limitations      []string      `json:"limitations"`
	SuggestedReading []string      `json:"suggested_reading"`
}

// EvidenceWarning flags a statement that cites something absent from the digest.
type EvidenceWarning struct {
	Location  string `json:"location"`
	Kind      string `json:"kind"` // unknown_neuron_id | unknown_reference
	NeuronID  string `json:"neuron_id,omitempty"`
	Reference string `json:"reference,omitempty"`
	Text      string `json:"text"`
	Message   string `json:"message"`
}

// decodeOutput parses structured_output strictly: unknown fields, a missing or empty required
// field, or a confidence outside low/medium/high is an error (reported as 502 LLM_ERROR).
func decodeOutput(raw []byte) (*Interpretation, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out Interpretation
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	var problems []string
	if strings.TrimSpace(out.Headline) == "" {
		problems = append(problems, "headline is empty")
	}
	if len(out.Observations) == 0 {
		problems = append(problems, "no observations")
	}
	if len(out.Hypotheses) == 0 {
		problems = append(problems, "no hypotheses")
	}
	for i, o := range out.Observations {
		if strings.TrimSpace(o.Text) == "" || len(o.Evidence) == 0 {
			problems = append(problems, fmt.Sprintf("observations[%d] lacks text or evidence", i))
		}
	}
	for i, h := range out.Hypotheses {
		switch h.Confidence {
		case "low", "medium", "high":
		default:
			problems = append(problems, fmt.Sprintf("hypotheses[%d].confidence %q is not low/medium/high", i, h.Confidence))
		}
		if strings.TrimSpace(h.Title) == "" || strings.TrimSpace(h.Statement) == "" || strings.TrimSpace(h.ConfidenceReason) == "" {
			problems = append(problems, fmt.Sprintf("hypotheses[%d] lacks title, statement or confidence_reason", i))
		}
		if len(h.Evidence) == 0 {
			problems = append(problems, fmt.Sprintf("hypotheses[%d] has no evidence", i))
		}
		if h.Test.Plan == nil {
			problems = append(problems, fmt.Sprintf("hypotheses[%d].test has no plan", i))
		}
	}
	problems = append(problems, capViolations(&out)...)
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	if out.Limitations == nil {
		out.Limitations = []string{}
	}
	if out.SuggestedReading == nil {
		out.SuggestedReading = []string{}
	}
	for i := range out.Hypotheses {
		if out.Hypotheses[i].Caveats == nil {
			out.Hypotheses[i].Caveats = []string{}
		}
	}
	return &out, nil
}

// capViolations re-checks the output caps the schema already carries (defence in depth): each
// violation names the field; nothing is truncated.
func capViolations(out *Interpretation) []string {
	var v []string
	text := func(field, s string, max int) {
		if n := utf8.RuneCountInString(s); n > max {
			v = append(v, fmt.Sprintf("%s is %d characters, over the cap of %d", field, n, max))
		}
	}
	list := func(field string, items []string, maxItems, maxChars int) {
		if len(items) > maxItems {
			v = append(v, fmt.Sprintf("%s has %d items, over the cap of %d", field, len(items), maxItems))
		}
		for i, s := range items {
			text(fmt.Sprintf("%s[%d]", field, i), s, maxChars)
		}
	}
	c := Caps
	text("headline", out.Headline, c.Headline)
	if len(out.Observations) > c.Observations {
		v = append(v, fmt.Sprintf("observations has %d items, over the cap of %d", len(out.Observations), c.Observations))
	}
	for i, o := range out.Observations {
		text(fmt.Sprintf("observations[%d].text", i), o.Text, c.ObservationText)
		list(fmt.Sprintf("observations[%d].evidence", i), o.Evidence, c.EvidenceItems, c.EvidenceChars)
	}
	if len(out.Hypotheses) > c.Hypotheses {
		v = append(v, fmt.Sprintf("hypotheses has %d items, over the cap of %d", len(out.Hypotheses), c.Hypotheses))
	}
	for i, h := range out.Hypotheses {
		p := fmt.Sprintf("hypotheses[%d]", i)
		text(p+".title", h.Title, c.Title)
		text(p+".statement", h.Statement, c.Statement)
		text(p+".confidence_reason", h.ConfidenceReason, c.ConfidenceReason)
		list(p+".evidence", h.Evidence, c.EvidenceItems, c.EvidenceChars)
		list(p+".caveats", h.Caveats, c.CaveatItems, c.CaveatChars)
		text(p+".test.description", h.Test.Description, c.TestDescription)
		text(p+".test.expected_if_true", h.Test.ExpectedIfTrue, c.ExpectedIfTrue)
	}
	list("limitations", out.Limitations, c.LimitationItems, c.LimitationChars)
	list("suggested_reading", out.SuggestedReading, c.ReadingItems, c.ReadingChars)
	return v
}

// PlanSaver persists a validated plan (storage.Store.SavePlan).
type PlanSaver interface {
	SavePlan(plan *domain.ExperimentPlan, resolved *domain.ResolvedPlan) error
}

// validateTests runs every hypothesis' test.plan through the plan validator. A valid plan is
// stored and gets plan_id; an invalid one keeps its plan and gets plan_error (never dropped).
// A failure to store a valid plan is returned as an error.
func validateTests(out *Interpretation, v *contracts.Validator, saver PlanSaver, lang string) error {
	for i := range out.Hypotheses {
		t := &out.Hypotheses[i].Test
		t.PlanID, t.PlanError = nil, nil
		plan := llm.ToExperimentPlan(t.Plan, "flywire_630", lang)
		raw, err := json.Marshal(plan)
		if err != nil {
			msg := fmt.Sprintf("plan cannot be encoded: %v", err)
			t.PlanError = &msg
			continue
		}
		res, err := v.ValidateRawJSON(raw)
		if err != nil {
			msg := err.Error()
			t.PlanError = &msg
			continue
		}
		if err := saver.SavePlan(res.Plan, res.ResolvedPlan); err != nil {
			return fmt.Errorf("hypotheses[%d] test plan is valid but could not be saved: %w", i, err)
		}
		id := res.ResolvedPlan.PlanID
		t.PlanID = &id
	}
	return nil
}

var (
	rootIDRe = regexp.MustCompile(`\b[0-9]{15,20}\b`)
	doiRe    = regexp.MustCompile(`(?i)10\.\d{4,9}/[^\s;,)]+`)
)

// digestIDs is every root id that appears anywhere in the digest JSON.
func digestIDs(digestJSON []byte) map[string]bool {
	out := map[string]bool{}
	for _, m := range rootIDRe.FindAll(digestJSON, -1) {
		out[string(m)] = true
	}
	return out
}

// evidenceWarnings flags neuron ids that are not in the digest (in observations' and
// hypotheses' evidence, hypothesis statements and test plans' neuron_ids) and suggested
// reading that is not one of the digest's references.
func evidenceWarnings(out *Interpretation, digestJSON []byte, references []string) []EvidenceWarning {
	known := digestIDs(digestJSON)
	warnings := []EvidenceWarning{}
	check := func(location, text string) {
		seen := map[string]bool{}
		for _, id := range rootIDRe.FindAllString(text, -1) {
			if known[id] || seen[id] {
				continue
			}
			seen[id] = true
			warnings = append(warnings, EvidenceWarning{
				Location: location, Kind: "unknown_neuron_id", NeuronID: id, Text: text,
				Message: fmt.Sprintf("neuron %s is not in the digest the AI was given", id),
			})
		}
	}
	for i, o := range out.Observations {
		for j, e := range o.Evidence {
			check(fmt.Sprintf("observations[%d].evidence[%d]", i, j), e)
		}
	}
	for i, h := range out.Hypotheses {
		check(fmt.Sprintf("hypotheses[%d].statement", i), h.Statement)
		for j, e := range h.Evidence {
			check(fmt.Sprintf("hypotheses[%d].evidence[%d]", i, j), e)
		}
		if p := h.Test.Plan; p != nil {
			var sels [][]string
			for _, a := range p.Activation {
				sels = append(sels, a.NeuronIDs)
			}
			for _, s := range p.Silencing {
				sels = append(sels, s.NeuronIDs)
			}
			for _, r := range p.Readout {
				sels = append(sels, r.NeuronIDs)
			}
			for _, ids := range sels {
				for _, id := range ids {
					check(fmt.Sprintf("hypotheses[%d].test.plan", i), id)
				}
			}
		}
	}

	knownDOIs := map[string]bool{}
	for _, r := range references {
		for _, d := range doiRe.FindAllString(r, -1) {
			knownDOIs[strings.ToLower(strings.TrimRight(d, "."))] = true
		}
	}
	for i, s := range out.SuggestedReading {
		ok := false
		dois := doiRe.FindAllString(s, -1)
		if len(dois) > 0 {
			ok = true
			for _, d := range dois {
				if !knownDOIs[strings.ToLower(strings.TrimRight(d, "."))] {
					ok = false
				}
			}
		} else {
			ls := strings.ToLower(strings.TrimSpace(s))
			for _, r := range references {
				lr := strings.ToLower(r)
				if ls != "" && (strings.Contains(lr, ls) || strings.Contains(ls, lr)) {
					ok = true
				}
			}
		}
		if !ok {
			warnings = append(warnings, EvidenceWarning{
				Location: fmt.Sprintf("suggested_reading[%d]", i), Kind: "unknown_reference", Reference: s, Text: s,
				Message: "this citation is not among the references in the digest",
			})
		}
	}
	return warnings
}

var (
	// Fields and wording that exist only in compare_silencing runs.
	bOnlyRe   = regexp.MustCompile(`(?i)\b(delta_hz|rate_B_hz|spike_count_B|first_spike_ms_B|delta_spikes|delta_active_neurons|delta_mean_rate_hz|relative_change_spikes|relative_change_active_neurons|hops_from_silenced|direct_input_from_silenced|totals\.B|top_neurons_by_delta)\b|condition B|услови[еяию]\s+B\b`)
	compareRe = regexp.MustCompile(`(?i)compare_silencing`)
	decimalRe = regexp.MustCompile(`-?\d+\.\d{2,}`)
	// A confidence_reason that admits the claim was not tested in this run.
	untestedRe = regexp.MustCompile(`(?i)\bnot (been |yet )?(tested|checked|measured|shown|examined)\b|\buntested\b|\bnot (in|from) this run\b|не провер|не измер|не показ|не тестир|в этом прогоне не`)
	// Evidence that refers to the literature, a proxy or the setup rather than to this run's numbers.
	nonRunEvidenceRe = regexp.MustCompile(`(?i)behavioural_proxies|\bproxy\b|прокси|model_facts|model_parameters|\bexperiment\.|\bet al\b|references|registry`)
)

// qualityTexts are the warning texts per answer language (the panel shows them next to
// Claude's text, which is in that language).
var qualityTexts = map[string]struct {
	bOnly, compareDesc, crossRun, calibration, untested, nonRun string
}{
	"en": {
		bOnly:       "expected_if_true relies on %q, but the plan is a single run: it has no condition B, no delta and no silenced set, so the new run's digest will not contain that number and this test cannot confirm or refute the hypothesis as written.",
		compareDesc: "The description asks for a compare_silencing run, but the plan is a single run (no silencing, no condition B).",
		crossRun:    "expected_if_true compares the new run with numbers of this run (%s). FlyLab does not compare two runs: compare the new run's digest with this one by hand, and keep in mind that seeds and repeats may differ.",
		calibration: "Confidence %q may be too high: %s. Medium or high confidence needs evidence from this run's digest; treat this hypothesis as low confidence.",
		untested:    "its confidence_reason says the claim was not tested in this run (%q)",
		nonRun:      "every evidence item refers to a proxy, the literature or the experiment setup, not to a number of this run",
	},
	"ru": {
		bOnly:       "expected_if_true опирается на %q, но план — одиночный запуск (single): в нём нет условия B, дельты и заглушённых нейронов, поэтому в сводке нового запуска этого числа не будет, и в таком виде проверка не подтвердит и не опровергнет гипотезу.",
		compareDesc: "Описание просит запуск compare_silencing, но план — одиночный запуск (без заглушения и без условия B).",
		crossRun:    "expected_if_true сравнивает новый запуск с числами этого запуска (%s). FlyLab не сравнивает два запуска: сравните сводку нового запуска с этой вручную и учтите, что зерно и число повторов могут отличаться.",
		calibration: "Уверенность %q, возможно, завышена: %s. Для средней или высокой уверенности нужны числа из сводки этого запуска; считайте эту гипотезу гипотезой низкой уверенности.",
		untested:    "в confidence_reason сказано, что в этом запуске утверждение не проверялось (%q)",
		nonRun:      "все доказательства ссылаются на прокси, литературу или настройки эксперимента, а не на числа этого запуска",
	},
}

// qualityWarnings sets test.warnings and calibration_warning deterministically from the
// interpretation and the digest JSON the model saw, in the interpretation's language. It
// replaces whatever was there.
func qualityWarnings(out *Interpretation, digestJSON []byte, lang string) {
	tx, ok := qualityTexts[lang]
	if !ok {
		tx = qualityTexts["en"]
	}
	digestDecimals := map[string]bool{}
	for _, m := range decimalRe.FindAll(digestJSON, -1) {
		digestDecimals[strings.TrimPrefix(string(m), "-")] = true
	}
	for i := range out.Hypotheses {
		h := &out.Hypotheses[i]
		t := &h.Test
		t.Warnings = []string{}
		h.CalibrationWarning = nil
		if t.Plan != nil {
			single := t.Plan.ExperimentType == "single"
			if single {
				if m := bOnlyRe.FindString(t.ExpectedIfTrue); m != "" {
					t.Warnings = append(t.Warnings, fmt.Sprintf(tx.bOnly, m))
				}
				if compareRe.MatchString(t.Description) {
					t.Warnings = append(t.Warnings, tx.compareDesc)
				}
			}
			var cross []string
			seen := map[string]bool{}
			for _, m := range decimalRe.FindAllString(t.ExpectedIfTrue, -1) {
				v := strings.TrimPrefix(m, "-")
				if digestDecimals[v] && !seen[v] {
					seen[v] = true
					cross = append(cross, m)
				}
			}
			if len(cross) > 0 {
				t.Warnings = append(t.Warnings, fmt.Sprintf(tx.crossRun, strings.Join(cross, ", ")))
			}
		}
		if h.Confidence == "medium" || h.Confidence == "high" {
			var reason string
			if m := untestedRe.FindString(h.ConfidenceReason); m != "" {
				reason = fmt.Sprintf(tx.untested, m)
			} else if len(h.Evidence) > 0 {
				allNonRun := true
				for _, e := range h.Evidence {
					if !nonRunEvidenceRe.MatchString(e) {
						allNonRun = false
						break
					}
				}
				if allNonRun {
					reason = tx.nonRun
				}
			}
			if reason != "" {
				msg := fmt.Sprintf(tx.calibration, h.Confidence, reason)
				h.CalibrationWarning = &msg
			}
		}
	}
}
