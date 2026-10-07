package interpret

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
)

const (
	DigestSchemaVersion = "1.1"
	// GraphSchemaVersion is the digest_graph.json version this code reads (flysim digest).
	GraphSchemaVersion = "1.1"
	// ModelCitation is the model this platform reimplements.
	ModelCitation = "Shiu et al., Nature 2024. doi:10.1038/s41586-024-07763-9"
	// CoverageWarningThreshold: below this share of annotated active neurons the digest warns.
	CoverageWarningThreshold = 0.70

	topClasses      = 15
	topByRate       = 30
	topByDelta      = 20
	maxListedSilenc = 50
	maxListedStim   = 100

	unannotatedClass = "(unannotated)"
	emptyClass       = "(not annotated in this field)"
)

// ModelFacts are the properties of the simulation every interpretation must respect.
var ModelFacts = []string{
	"Leaky integrate-and-fire point neurons (no dendrites, no gap junctions, no compartments); parameters of Shiu et al. 2024.",
	"Synaptic weights = FlyWire synapse count between two neurons x the sign of the presynaptic neuron x 0.275 mV. A neuron is inhibitory when more than half of its presynaptic sites are predicted GABA or glutamate, otherwise excitatory (Shiu et al. 2024); glutamate is assumed inhibitory although it can be excitatory in Drosophila.",
	"No neuromodulation: dopaminergic, serotonergic and octopaminergic neurons are modelled as ordinary fast excitatory synapses, and neuropeptides are absent.",
	"The annotation field top_nt (FlyWire neurotransmitter prediction in Schlegel et al. 2024) is not the source of the model's synapse signs and can disagree with them.",
	"No plasticity or learning: weights are constant during a run.",
	"No body, muscles or sensory feedback: motor and descending neuron spikes are the end of the model, not behaviour.",
	"Input: independent Poisson spike trains at a fixed rate drive only the stimulated neurons; every other neuron starts at rest and receives only synaptic input.",
	"Silencing removes the OUTGOING synapses of the silenced neurons only (they may still spike if driven); condition B = activation + silencing, condition A = activation only, same input spikes (upstream audit). Condition B cannot add or change activation.",
	"Every Poisson input spike pushes a stimulated neuron's membrane potential up by poisson_input_step_mv (model_parameters), far above the distance to threshold, so a stimulated neuron fires at roughly its input rate whatever the network does: its rate reflects the stimulus, not network computation. top_neurons_by_rate therefore lists only non-stimulated neurons; stimulated neurons are in stimulated_neurons.",
	"model_sign is the sign of a neuron's outgoing synapses in the simulation (+1 excitatory, -1 inhibitory, null = no outgoing synapse); it is what the model computes with, unlike top_nt.",
	"Rates are spike counts summed over all repeats divided by (repeats x duration); spike_count_A/B are those summed counts. Small counts (for example 2 spikes) carry large sampling noise. First-spike latencies are from trial 0 only. No neuron can exceed the refractory bound max_rate_hz_bound in model_parameters.",
	"Side labels: the annotation field side is the fly's own side (FlyWire corrected the left/right inversion of the FAFB images). Registry descriptions taken from Shiu et al.'s notebooks predate that correction and use the image convention, which is mirrored: a registry 'right hemisphere' GRN group is annotated side 'left'. The two labels do not conflict; under either convention the sugar, bitter and Ir94e GRN groups are on the side opposite to MN9 720575940660219265.",
	"Connectome: FlyWire materialization 630 (127,400 neurons). Cell-type annotations come from materialization 783 and cover only neurons whose root id did not change between the two versions.",
}

// ---- digest_graph.json (written by `flysim digest`) ----

type graphNeuron struct {
	RootID                    string   `json:"root_id"`
	FirstSpikeMsA             *float64 `json:"first_spike_ms_A"`
	FirstSpikeMsB             *float64 `json:"first_spike_ms_B"`
	HopsFromStimulated        *int     `json:"hops_from_stimulated"`
	DirectInputFromStimulated int64    `json:"direct_input_from_stimulated"`
	HopsFromSilenced          *int     `json:"hops_from_silenced"`
	DirectInputFromSilenced   *int64   `json:"direct_input_from_silenced"`
	ModelSign                 *int     `json:"model_sign"`
}

type graphPartner struct {
	RootID             string `json:"root_id"`
	Synapses           int64  `json:"synapses"`
	ModelSign          *int   `json:"model_sign"`
	HopsFromStimulated *int   `json:"hops_from_stimulated"`
}

type graphReadoutInputs struct {
	PresynapticPartners           int            `json:"presynaptic_partners"`
	ActivePresynapticPartners     int            `json:"active_presynaptic_partners"`
	NetSynapsesFromActivePartners int64          `json:"net_synapses_from_active_partners"`
	TopActivePartners             []graphPartner `json:"top_active_partners"`
}

// LIFParameters are the simulator's parameters as flysim digest reports them (model.rs).
type LIFParameters struct {
	V0mV   float64 `json:"v_0_mv"`
	VRstMV float64 `json:"v_rst_mv"`
	VThMV  float64 `json:"v_th_mv"`
	TMbrMs float64 `json:"t_mbr_ms"`
	TauMs  float64 `json:"tau_ms"`
	TRfcMs float64 `json:"t_rfc_ms"`
	TDlyMs float64 `json:"t_dly_ms"`
	WSynMV float64 `json:"w_syn_mv"`
	FPoi   float64 `json:"f_poi"`
	DtMs   float64 `json:"dt_ms"`
}

type graphFile struct {
	SchemaVersion       string                         `json:"schema_version"`
	DatasetID           string                         `json:"dataset_id"`
	PlanHash            *string                        `json:"plan_hash"`
	ExperimentType      string                         `json:"experiment_type"`
	MaxHops             int                            `json:"max_hops"`
	Units               map[string]string              `json:"units"`
	StimulatedCount     int                            `json:"stimulated_count"`
	SilencedCount       int                            `json:"silenced_count"`
	Neurons             []graphNeuron                  `json:"neurons"`
	ReadoutFirstSpikeMs map[string]map[string]*float64 `json:"readout_first_spike_ms"`
	ReadoutInputs       map[string]graphReadoutInputs  `json:"readout_inputs"`
	LIFParameters       *LIFParameters                 `json:"lif_parameters"`
}

// GraphError means the cached digest_graph.json cannot be used for this run. The file is
// derived from spikes.parquet, so the service recomputes it: silently when it is only Outdated
// (written by an older flysim), with a visible digest warning when it was unreadable or did not
// match the run (Reason).
type GraphError struct {
	Reason   string
	Outdated bool
}

func (e *GraphError) Error() string { return e.Reason }

func graphErr(format string, a ...interface{}) error {
	return &GraphError{Reason: fmt.Sprintf(format, a...)}
}

func readGraphFile(path string, rp *domain.ResolvedPlan) (*graphFile, map[string]*graphNeuron, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, graphErr("digest_graph.json cannot be read: %v", err)
	}
	// The version is checked first, so a file of an older flysim is "outdated", not "corrupt".
	var head struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(raw, &head); err == nil && head.SchemaVersion != "" && head.SchemaVersion != GraphSchemaVersion {
		return nil, nil, &GraphError{Outdated: true,
			Reason: fmt.Sprintf("digest_graph.json schema_version %q is not %s", head.SchemaVersion, GraphSchemaVersion)}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var g graphFile
	if err := dec.Decode(&g); err != nil {
		return nil, nil, graphErr("digest_graph.json cannot be parsed: %v", err)
	}
	if g.SchemaVersion != GraphSchemaVersion {
		return nil, nil, graphErr("digest_graph.json schema_version %q is not %s", g.SchemaVersion, GraphSchemaVersion)
	}
	if g.ExperimentType != rp.ExperimentType {
		return nil, nil, graphErr("digest_graph.json is for a %s run but resolved_plan.json is %s", g.ExperimentType, rp.ExperimentType)
	}
	if g.PlanHash != nil && rp.PlanHash != "" && *g.PlanHash != rp.PlanHash {
		return nil, nil, graphErr("digest_graph.json was computed for plan %s, not this run's plan %s", *g.PlanHash, rp.PlanHash)
	}
	if g.LIFParameters == nil || g.LIFParameters.TRfcMs <= 0 || g.LIFParameters.WSynMV <= 0 {
		return nil, nil, graphErr("digest_graph.json lacks valid lif_parameters")
	}
	byID := make(map[string]*graphNeuron, len(g.Neurons))
	for i := range g.Neurons {
		n := &g.Neurons[i]
		if _, dup := byID[n.RootID]; dup {
			return nil, nil, graphErr("digest_graph.json lists neuron %s twice", n.RootID)
		}
		byID[n.RootID] = n
	}
	for _, id := range rp.ReadoutNeuronIDs {
		if _, ok := g.ReadoutInputs[id]; !ok {
			return nil, nil, graphErr("digest_graph.json lacks readout_inputs of readout neuron %s", id)
		}
	}
	return &g, byID, nil
}

// ---- rates.csv ----

const ratesHeader = "condition,trial,root_id,spike_count,rate_hz,is_readout"

type rateEntry struct {
	Spikes int64
	Rate   float64
}

// readRates returns condition -> root id -> (spike_count, rate_hz), validating every row like
// the spikes endpoint does (a malformed row is an error naming the line).
func readRates(path string, compare bool) (map[string]map[string]rateEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("rates.csv is missing for this succeeded run")
		}
		return nil, fmt.Errorf("rates.csv cannot be read: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	if strings.TrimSpace(lines[0]) != ratesHeader {
		return nil, fmt.Errorf("rates.csv line 1 is %q, expected the header %q", strings.TrimSpace(lines[0]), ratesHeader)
	}
	out := map[string]map[string]rateEntry{"A": {}}
	if compare {
		out["B"] = map[string]rateEntry{}
	}
	for i, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n := i + 2
		parts := strings.Split(line, ",")
		if len(parts) != 6 {
			return nil, fmt.Errorf("rates.csv line %d has %d columns, expected 6", n, len(parts))
		}
		cond, id := parts[0], parts[2]
		byID, ok := out[cond]
		if !ok {
			return nil, fmt.Errorf("rates.csv line %d has condition %q, not expected in this run", n, cond)
		}
		if !isRootID(id) {
			return nil, fmt.Errorf("rates.csv line %d: root_id %q is not a FlyWire root id", n, id)
		}
		cnt, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || cnt < 0 {
			return nil, fmt.Errorf("rates.csv line %d: spike_count %q is not a non-negative integer", n, parts[3])
		}
		rate, err := strconv.ParseFloat(parts[4], 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 {
			return nil, fmt.Errorf("rates.csv line %d: rate_hz %q is not a finite non-negative number", n, parts[4])
		}
		if _, dup := byID[id]; dup {
			return nil, fmt.Errorf("rates.csv line %d: neuron %s appears twice in condition %s", n, id, cond)
		}
		byID[id] = rateEntry{Spikes: cnt, Rate: rate}
	}
	return out, nil
}

// ---- digest shape (returned to the client and sent to Claude) ----

// AnnotationSummary counts a neuron set by annotation field (keys sorted by encoding/json).
type AnnotationSummary struct {
	Annotated      int            `json:"annotated"`
	Unannotated    int            `json:"unannotated"`
	BySuperClass   map[string]int `json:"by_super_class"`
	ByCellClass    map[string]int `json:"by_cell_class"`
	ByCellSubClass map[string]int `json:"by_cell_sub_class"`
	ByCellType     map[string]int `json:"by_cell_type"`
	ByTopNT        map[string]int `json:"by_top_nt"`
	BySide         map[string]int `json:"by_side"`
	// IDsByCellSubClass / IDsByCellType list the set's root ids per annotation value, so a
	// subtype of the set can be addressed by id (unannotated ids are under "(unannotated)").
	IDsByCellSubClass map[string][]string `json:"ids_by_cell_sub_class"`
	IDsByCellType     map[string][]string `json:"ids_by_cell_type"`
}

type NeuronSet struct {
	GroupID         string            `json:"group_id,omitempty"`
	GroupName       string            `json:"group_name,omitempty"`
	GroupSource     string            `json:"group_source,omitempty"`
	GroupDesc       string            `json:"group_description,omitempty"`
	RateHz          *float64          `json:"rate_hz,omitempty"`
	NeuronCount     int               `json:"neuron_count"`
	NeuronIDs       []string          `json:"neuron_ids"`
	NeuronIDsListed int               `json:"neuron_ids_listed"`
	Annotation      AnnotationSummary `json:"annotation_summary"`
}

type Experiment struct {
	Type       string      `json:"type"`
	DatasetID  string      `json:"dataset_id"`
	ModelID    string      `json:"model_id"`
	DurationMs float64     `json:"duration_ms"`
	Repeats    int         `json:"repeats"`
	BaseSeed   uint64      `json:"base_seed"`
	Stimulated []NeuronSet `json:"stimulated"`
	Silenced   *NeuronSet  `json:"silenced"`
	Readout    NeuronSet   `json:"readout"`
}

type ConditionTotals struct {
	Spikes        int64 `json:"spikes"`
	ActiveNeurons int   `json:"active_neurons"`
}

type Totals struct {
	A                     ConditionTotals  `json:"A"`
	B                     *ConditionTotals `json:"B"`
	DeltaSpikes           *int64           `json:"delta_spikes"`
	RelativeChangeSpikes  *float64         `json:"relative_change_spikes"`
	DeltaActiveNeurons    *int             `json:"delta_active_neurons"`
	RelativeChangeNeurons *float64         `json:"relative_change_active_neurons"`
}

type ClassStats struct {
	ActiveNeurons int     `json:"active_neurons"`
	TotalSpikes   int64   `json:"total_spikes"`
	MeanRateHz    float64 `json:"mean_rate_hz"`
}

type ClassActivity struct {
	Class              string      `json:"class"`
	A                  ClassStats  `json:"A"`
	B                  *ClassStats `json:"B"`
	DeltaSpikes        *int64      `json:"delta_spikes"`
	DeltaActiveNeurons *int        `json:"delta_active_neurons"`
	DeltaMeanRateHz    *float64    `json:"delta_mean_rate_hz"`
}

type ActivityTable struct {
	Field        string          `json:"field"`
	ClassesTotal int             `json:"classes_total"`
	Note         string          `json:"note"`
	Rows         []ClassActivity `json:"rows"`
}

type NeuronFacts struct {
	RootID                    string      `json:"root_id"`
	Roles                     []string    `json:"roles"`
	Groups                    []string    `json:"groups"`
	Annotation                *Annotation `json:"annotation"`
	ModelSign                 *int        `json:"model_sign"`
	RateAHz                   float64     `json:"rate_A_hz"`
	RateBHz                   *float64    `json:"rate_B_hz"`
	DeltaHz                   *float64    `json:"delta_hz"`
	SpikeCountA               int64       `json:"spike_count_A"`
	SpikeCountB               *int64      `json:"spike_count_B"`
	FirstSpikeMsA             *float64    `json:"first_spike_ms_A"`
	FirstSpikeMsB             *float64    `json:"first_spike_ms_B"`
	HopsFromStimulated        *int        `json:"hops_from_stimulated"`
	DirectInputFromStimulated *int64      `json:"direct_input_from_stimulated"`
	HopsFromSilenced          *int        `json:"hops_from_silenced"`
	DirectInputFromSilenced   *int64      `json:"direct_input_from_silenced"`
}

type ReadoutFacts struct {
	NeuronFacts
	ProxyIDs                      []string `json:"proxy_ids"`
	PresynapticPartners           int      `json:"presynaptic_partners"`
	ActivePresynapticPartners     int      `json:"active_presynaptic_partners"`
	NetSynapsesFromActivePartners int64    `json:"net_synapses_from_active_partners"`
}

// ReadoutInput is one of the strongest active presynaptic partners of a readout neuron.
type ReadoutInput struct {
	ReadoutID          string      `json:"readout_id"`
	RootID             string      `json:"root_id"`
	Synapses           int64       `json:"synapses"`
	ModelSign          *int        `json:"model_sign"`
	HopsFromStimulated *int        `json:"hops_from_stimulated"`
	Roles              []string    `json:"roles"`
	Annotation         *Annotation `json:"annotation"`
	RateAHz            float64     `json:"rate_A_hz"`
	RateBHz            *float64    `json:"rate_B_hz"`
	DeltaHz            *float64    `json:"delta_hz"`
	SpikeCountA        int64       `json:"spike_count_A"`
	SpikeCountB        *int64      `json:"spike_count_B"`
}

// ModelParameters are the simulator's LIF parameters plus bounds derived from them.
type ModelParameters struct {
	LIFParameters
	ThresholdAboveRestMV float64 `json:"threshold_above_rest_mv"`
	PoissonInputStepMV   float64 `json:"poisson_input_step_mv"`
	MaxRateHzBound       float64 `json:"max_rate_hz_bound"`
	Note                 string  `json:"note"`
}

type ProxyNeuron struct {
	RootID            string   `json:"root_id"`
	MeasuredAsReadout bool     `json:"measured_as_readout"`
	RateAHz           float64  `json:"rate_A_hz"`
	RateBHz           *float64 `json:"rate_B_hz"`
	DeltaHz           *float64 `json:"delta_hz"`
}

type ProxyFacts struct {
	Proxy
	NeuronsInRun []ProxyNeuron `json:"neurons_in_run"`
}

type ConditionCoverage struct {
	ActiveNeurons         int      `json:"active_neurons"`
	ActiveAnnotated       int      `json:"active_annotated"`
	ActiveAnnotatedShare  *float64 `json:"active_annotated_share"`
	Spikes                int64    `json:"spikes"`
	SpikesFromAnnotated   int64    `json:"spikes_from_annotated"`
	SpikeShareAnnotated   *float64 `json:"spike_share_annotated"`
	UnannotatedActiveList int      `json:"unannotated_active_neurons"`
}

// FieldCoverage is how many active neurons have a non-empty value in one annotation field.
type FieldCoverage struct {
	ActiveWithValue int      `json:"active_with_value"`
	Share           *float64 `json:"share"`
}

type Coverage struct {
	AnnotationsReady      bool                         `json:"annotations_ready"`
	AnnotationsSource     string                       `json:"annotations_source"`
	ActiveNeurons         int                          `json:"active_neurons"`
	ActiveAnnotated       int                          `json:"active_annotated"`
	ActiveAnnotatedShare  *float64                     `json:"active_annotated_share"`
	SpikeShareAnnotated   *float64                     `json:"spike_share_annotated"`
	PerCondition          map[string]ConditionCoverage `json:"per_condition"`
	UnannotatedTopNeurons []string                     `json:"unannotated_top_neurons"`
	Warning               *string                      `json:"warning"`
	// PerField: share of all active neurons with a non-empty value in each annotation field
	// (a neuron can be annotated and still have an empty cell_class).
	PerField      map[string]FieldCoverage `json:"per_field"`
	FieldWarnings []string                 `json:"field_warnings"`
}

// Digest is the deterministic, factual description of one run.
type Digest struct {
	SchemaVersion        string            `json:"schema_version"`
	JobID                string            `json:"job_id"`
	Title                *string           `json:"title"`
	Experiment           Experiment        `json:"experiment"`
	Totals               Totals            `json:"totals"`
	ActivityBySuperClass ActivityTable     `json:"activity_by_super_class"`
	ActivityByCellClass  ActivityTable     `json:"activity_by_cell_class"`
	ActivityByTopNT      ActivityTable     `json:"activity_by_top_nt"`
	ActivityByCellType   ActivityTable     `json:"activity_by_cell_type"`
	TopNeuronsByRate     []NeuronFacts     `json:"top_neurons_by_rate"`
	TopNeuronsByDelta    []NeuronFacts     `json:"top_neurons_by_delta"`
	StimulatedNeurons    []NeuronFacts     `json:"stimulated_neurons"`
	SilencedNeurons      []NeuronFacts     `json:"silenced_neurons"`
	Readouts             []ReadoutFacts    `json:"readouts"`
	ReadoutInputs        []ReadoutInput    `json:"readout_inputs"`
	BehaviouralProxies   []ProxyFacts      `json:"behavioural_proxies"`
	Coverage             Coverage          `json:"coverage"`
	GraphUnits           map[string]string `json:"graph_units"`
	ModelParameters      ModelParameters   `json:"model_parameters"`
	ModelFacts           []string          `json:"model_facts"`
	References           []string          `json:"references"`
	Warnings             []string          `json:"warnings"`
}

// DigestInput is what BuildDigest needs besides the artifact files.
type DigestInput struct {
	JobID        string
	Title        *string
	ArtifactsDir string
	Registry     *contracts.Registry
	Annotations  *Annotations
	Proxies      []Proxy
	// ExtraWarnings are added to Warnings (e.g. a missing manifest.json in the run dir).
	ExtraWarnings []string
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func fptr(v float64) *float64 { return &v }
func iptr(v int) *int         { return &v }
func i64ptr(v int64) *int64   { return &v }

func classOf(a *Annotation, field string) string {
	if a == nil {
		return unannotatedClass
	}
	var v string
	switch field {
	case "super_class":
		v = a.SuperClass
	case "cell_class":
		v = a.CellClass
	case "cell_sub_class":
		v = a.CellSubClass
	case "cell_type":
		v = a.CellType
	case "top_nt":
		v = a.TopNT
	case "side":
		v = a.Side
	}
	if v == "" {
		return emptyClass
	}
	return v
}

func summarize(ids []string, ann *Annotations) AnnotationSummary {
	s := AnnotationSummary{
		BySuperClass: map[string]int{}, ByCellClass: map[string]int{}, ByCellSubClass: map[string]int{},
		ByCellType: map[string]int{}, ByTopNT: map[string]int{}, BySide: map[string]int{},
		IDsByCellSubClass: map[string][]string{}, IDsByCellType: map[string][]string{},
	}
	for _, id := range ids {
		a := ann.Lookup(id)
		s.IDsByCellSubClass[classOf(a, "cell_sub_class")] = append(s.IDsByCellSubClass[classOf(a, "cell_sub_class")], id)
		s.IDsByCellType[classOf(a, "cell_type")] = append(s.IDsByCellType[classOf(a, "cell_type")], id)
		if a == nil {
			s.Unannotated++
			continue
		}
		s.Annotated++
		s.BySuperClass[classOf(a, "super_class")]++
		s.ByCellClass[classOf(a, "cell_class")]++
		s.ByCellSubClass[classOf(a, "cell_sub_class")]++
		s.ByCellType[classOf(a, "cell_type")]++
		s.ByTopNT[classOf(a, "top_nt")]++
		s.BySide[classOf(a, "side")]++
	}
	return s
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]bool, len(a))
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

// neuronSet describes ids, naming the registry group when the ids are exactly one group.
func neuronSet(ids []string, reg *contracts.Registry, ann *Annotations, listLimit int) NeuronSet {
	sorted := sortedCopy(ids)
	ns := NeuronSet{NeuronCount: len(sorted), Annotation: summarize(sorted, ann)}
	for _, g := range reg.Groups {
		if sameSet(g.NeuronIDs, sorted) {
			ns.GroupID, ns.GroupName, ns.GroupSource, ns.GroupDesc = g.GroupID, g.NameEn, g.SourceReference, g.Description
			break
		}
	}
	if listLimit > 0 && len(sorted) > listLimit {
		ns.NeuronIDs = sorted[:listLimit]
	} else {
		ns.NeuronIDs = sorted
	}
	ns.NeuronIDsListed = len(ns.NeuronIDs)
	return ns
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// BuildDigest reads resolved_plan.json, rates.csv and digest_graph.json from the run's
// artifact directory and returns the digest. Any missing or malformed input is an error.
func BuildDigest(in DigestInput) (*Digest, error) {
	rpRaw, err := os.ReadFile(filepath.Join(in.ArtifactsDir, "resolved_plan.json"))
	if err != nil {
		return nil, fmt.Errorf("resolved_plan.json cannot be read: %v", err)
	}
	var rp domain.ResolvedPlan
	if err := json.Unmarshal(rpRaw, &rp); err != nil {
		return nil, fmt.Errorf("resolved_plan.json cannot be parsed: %v", err)
	}
	compare := rp.ExperimentType == "compare_silencing"
	if !compare && rp.ExperimentType != "single" {
		return nil, fmt.Errorf("resolved_plan.json has unknown experiment_type %q", rp.ExperimentType)
	}
	rates, err := readRates(filepath.Join(in.ArtifactsDir, "rates.csv"), compare)
	if err != nil {
		return nil, err
	}
	graph, graphByID, err := readGraphFile(filepath.Join(in.ArtifactsDir, "digest_graph.json"), &rp)
	if err != nil {
		return nil, err
	}

	ann := in.Annotations
	stimulated := map[string]bool{}
	for _, a := range rp.Activation {
		for _, id := range a.NeuronIDs {
			stimulated[id] = true
		}
	}
	silenced := map[string]bool{}
	for _, id := range rp.SilencingNeuronIDs {
		silenced[id] = true
	}
	readout := map[string]bool{}
	for _, id := range rp.ReadoutNeuronIDs {
		readout[id] = true
	}
	groupsOf := map[string][]string{}
	for _, g := range in.Registry.Groups {
		for _, id := range g.NeuronIDs {
			groupsOf[id] = append(groupsOf[id], g.GroupID)
		}
	}

	// Every neuron with spikes in rates.csv must be in the graph digest, or the digest is stale.
	active := map[string]bool{}
	for cond, byID := range rates {
		for id, e := range byID {
			if e.Spikes > 0 {
				active[id] = true
				if _, ok := graphByID[id]; !ok {
					return nil, graphErr("digest_graph.json lacks neuron %s that has spikes in condition %s of rates.csv", id, cond)
				}
			}
		}
	}

	facts := func(id string) NeuronFacts {
		f := NeuronFacts{RootID: id, Groups: groupsOf[id], Annotation: ann.Lookup(id), Roles: []string{}}
		if f.Groups == nil {
			f.Groups = []string{}
		}
		if stimulated[id] {
			f.Roles = append(f.Roles, "stimulated")
		}
		if silenced[id] {
			f.Roles = append(f.Roles, "silenced")
		}
		if readout[id] {
			f.Roles = append(f.Roles, "readout")
		}
		f.RateAHz = rates["A"][id].Rate
		f.SpikeCountA = rates["A"][id].Spikes
		if compare {
			b := rates["B"][id].Rate
			f.RateBHz = fptr(b)
			f.DeltaHz = fptr(round(b-f.RateAHz, 4))
			f.SpikeCountB = i64ptr(rates["B"][id].Spikes)
		}
		if g, ok := graphByID[id]; ok {
			f.ModelSign = g.ModelSign
			f.FirstSpikeMsA, f.FirstSpikeMsB = g.FirstSpikeMsA, g.FirstSpikeMsB
			f.HopsFromStimulated = g.HopsFromStimulated
			f.DirectInputFromStimulated = i64ptr(g.DirectInputFromStimulated)
			f.HopsFromSilenced, f.DirectInputFromSilenced = g.HopsFromSilenced, g.DirectInputFromSilenced
		}
		return f
	}

	d := &Digest{
		SchemaVersion:   DigestSchemaVersion,
		JobID:           in.JobID,
		Title:           in.Title,
		GraphUnits:      graph.Units,
		ModelParameters: modelParameters(*graph.LIFParameters),
		ModelFacts:      ModelFacts,
		Warnings:        append([]string{}, in.ExtraWarnings...),
	}

	// Experiment.
	exp := Experiment{
		Type: rp.ExperimentType, DatasetID: rp.DatasetID, ModelID: rp.ModelID,
		DurationMs: rp.DurationMs, Repeats: rp.Repeats, BaseSeed: rp.BaseSeed,
		Stimulated: []NeuronSet{},
	}
	for _, a := range rp.Activation {
		ns := neuronSet(a.NeuronIDs, in.Registry, ann, 0)
		ns.RateHz = fptr(a.RateHz)
		exp.Stimulated = append(exp.Stimulated, ns)
	}
	if compare {
		ns := neuronSet(rp.SilencingNeuronIDs, in.Registry, ann, maxListedSilenc)
		exp.Silenced = &ns
	}
	exp.Readout = neuronSet(rp.ReadoutNeuronIDs, in.Registry, ann, 0)
	d.Experiment = exp

	// Totals.
	totals := func(cond string) ConditionTotals {
		var t ConditionTotals
		for _, e := range rates[cond] {
			t.Spikes += e.Spikes
			if e.Spikes > 0 {
				t.ActiveNeurons++
			}
		}
		return t
	}
	d.Totals.A = totals("A")
	if compare {
		b := totals("B")
		d.Totals.B = &b
		d.Totals.DeltaSpikes = i64ptr(b.Spikes - d.Totals.A.Spikes)
		d.Totals.DeltaActiveNeurons = iptr(b.ActiveNeurons - d.Totals.A.ActiveNeurons)
		if d.Totals.A.Spikes > 0 {
			d.Totals.RelativeChangeSpikes = fptr(round(float64(b.Spikes-d.Totals.A.Spikes)/float64(d.Totals.A.Spikes), 4))
		}
		if d.Totals.A.ActiveNeurons > 0 {
			d.Totals.RelativeChangeNeurons = fptr(round(float64(b.ActiveNeurons-d.Totals.A.ActiveNeurons)/float64(d.Totals.A.ActiveNeurons), 4))
		}
	}

	// Activity by class.
	conds := []string{"A"}
	if compare {
		conds = append(conds, "B")
	}
	classTable := func(field string) ActivityTable {
		type acc struct {
			n     int
			spk   int64
			rates float64
		}
		per := map[string]map[string]*acc{}
		// neurons active in either condition, per class: the denominator of delta_mean_rate_hz
		union := map[string]map[string]bool{}
		for _, cond := range conds {
			for id, e := range rates[cond] {
				if e.Spikes == 0 {
					continue
				}
				cls := classOf(ann.Lookup(id), field)
				if per[cls] == nil {
					per[cls] = map[string]*acc{}
				}
				if per[cls][cond] == nil {
					per[cls][cond] = &acc{}
				}
				if union[cls] == nil {
					union[cls] = map[string]bool{}
				}
				union[cls][id] = true
				a := per[cls][cond]
				a.n++
				a.spk += e.Spikes
				a.rates += e.Rate
			}
		}
		stats := func(a *acc) ClassStats {
			if a == nil {
				return ClassStats{}
			}
			return ClassStats{ActiveNeurons: a.n, TotalSpikes: a.spk, MeanRateHz: round(a.rates/float64(a.n), 3)}
		}
		rows := make([]ClassActivity, 0, len(per))
		for cls, byCond := range per {
			row := ClassActivity{Class: cls, A: stats(byCond["A"])}
			if compare {
				b := stats(byCond["B"])
				row.B = &b
				row.DeltaSpikes = i64ptr(b.TotalSpikes - row.A.TotalSpikes)
				row.DeltaActiveNeurons = iptr(b.ActiveNeurons - row.A.ActiveNeurons)
				// Over the union of neurons active in A or B, a neuron silent in one condition
				// counting 0 Hz there, so its sign always agrees with delta_spikes.
				var sumA, sumB float64
				if x := byCond["A"]; x != nil {
					sumA = x.rates
				}
				if x := byCond["B"]; x != nil {
					sumB = x.rates
				}
				row.DeltaMeanRateHz = fptr(round((sumB-sumA)/float64(len(union[cls])), 3))
			}
			rows = append(rows, row)
		}
		maxSpk := func(r ClassActivity) int64 {
			if r.B != nil && r.B.TotalSpikes > r.A.TotalSpikes {
				return r.B.TotalSpikes
			}
			return r.A.TotalSpikes
		}
		sort.Slice(rows, func(i, j int) bool {
			if mi, mj := maxSpk(rows[i]), maxSpk(rows[j]); mi != mj {
				return mi > mj
			}
			return rows[i].Class < rows[j].Class
		})
		t := ActivityTable{Field: field, ClassesTotal: len(rows),
			Note: fmt.Sprintf("Active neurons only, top %d classes by total spikes (max over conditions); mean_rate_hz is over the neurons of the class active in that condition; delta_mean_rate_hz is over the neurons of the class active in A or B, a neuron silent in one condition counting 0 Hz there; %q = no annotation for the neuron, %q = annotated but this field is empty.", topClasses, unannotatedClass, emptyClass)}
		if len(rows) > topClasses {
			rows = rows[:topClasses]
		}
		t.Rows = rows
		return t
	}
	d.ActivityBySuperClass = classTable("super_class")
	d.ActivityByCellClass = classTable("cell_class")
	d.ActivityByTopNT = classTable("top_nt")
	d.ActivityByCellType = classTable("cell_type")

	// Top neurons by rate in condition A, network neurons only: a stimulated neuron's rate is
	// set by its Poisson input (stimulated_neurons lists those).
	var byRate []string
	for id, e := range rates["A"] {
		if e.Spikes > 0 && !stimulated[id] {
			byRate = append(byRate, id)
		}
	}
	sort.Slice(byRate, func(i, j int) bool {
		ri, rj := rates["A"][byRate[i]].Rate, rates["A"][byRate[j]].Rate
		if ri != rj {
			return ri > rj
		}
		return byRate[i] < byRate[j]
	})
	if len(byRate) > topByRate {
		byRate = byRate[:topByRate]
	}
	d.TopNeuronsByRate = []NeuronFacts{}
	for _, id := range byRate {
		d.TopNeuronsByRate = append(d.TopNeuronsByRate, facts(id))
	}

	// Every stimulated (and silenced) neuron with its own annotation and activity, sorted by
	// rate in A, then id.
	listSet := func(set map[string]bool, limit int) []NeuronFacts {
		ids := sortedKeys(set)
		sort.SliceStable(ids, func(i, j int) bool { return rates["A"][ids[i]].Rate > rates["A"][ids[j]].Rate })
		if len(ids) > limit {
			ids = ids[:limit]
		}
		out := make([]NeuronFacts, 0, len(ids))
		for _, id := range ids {
			out = append(out, facts(id))
		}
		return out
	}
	d.StimulatedNeurons = listSet(stimulated, maxListedStim)
	if len(stimulated) > maxListedStim {
		d.Warnings = append(d.Warnings, fmt.Sprintf("stimulated_neurons lists the %d most active of %d stimulated neurons.", maxListedStim, len(stimulated)))
	}
	if compare {
		d.SilencedNeurons = listSet(silenced, maxListedSilenc)
		if len(silenced) > maxListedSilenc {
			d.Warnings = append(d.Warnings, fmt.Sprintf("silenced_neurons lists the %d most active of %d silenced neurons.", maxListedSilenc, len(silenced)))
		}
	}

	// Top neurons by |delta| (compare runs).
	unannotatedTop := map[string]bool{}
	for _, f := range d.TopNeuronsByRate {
		if f.Annotation == nil {
			unannotatedTop[f.RootID] = true
		}
	}
	if compare {
		type dl struct {
			id string
			d  float64
		}
		var deltas []dl
		for id := range active {
			delta := rates["B"][id].Rate - rates["A"][id].Rate
			if round(delta, 4) != 0 {
				deltas = append(deltas, dl{id, delta})
			}
		}
		sort.Slice(deltas, func(i, j int) bool {
			ai, aj := math.Abs(deltas[i].d), math.Abs(deltas[j].d)
			if ai != aj {
				return ai > aj
			}
			return deltas[i].id < deltas[j].id
		})
		if len(deltas) > topByDelta {
			deltas = deltas[:topByDelta]
		}
		d.TopNeuronsByDelta = []NeuronFacts{}
		for _, x := range deltas {
			f := facts(x.id)
			d.TopNeuronsByDelta = append(d.TopNeuronsByDelta, f)
			if f.Annotation == nil {
				unannotatedTop[f.RootID] = true
			}
		}
	}

	// Readouts and behavioural proxies.
	proxiesOf := map[string][]string{}
	for _, p := range in.Proxies {
		for _, id := range p.NeuronIDs {
			proxiesOf[id] = append(proxiesOf[id], p.ProxyID)
		}
	}
	d.Readouts = []ReadoutFacts{}
	d.ReadoutInputs = []ReadoutInput{}
	for _, id := range sortedCopy(rp.ReadoutNeuronIDs) {
		r := ReadoutFacts{NeuronFacts: facts(id), ProxyIDs: proxiesOf[id]}
		if r.ProxyIDs == nil {
			r.ProxyIDs = []string{}
		}
		ri := graph.ReadoutInputs[id]
		r.PresynapticPartners, r.ActivePresynapticPartners = ri.PresynapticPartners, ri.ActivePresynapticPartners
		r.NetSynapsesFromActivePartners = ri.NetSynapsesFromActivePartners
		for _, p := range ri.TopActivePartners {
			pf := facts(p.RootID)
			d.ReadoutInputs = append(d.ReadoutInputs, ReadoutInput{
				ReadoutID: id, RootID: p.RootID, Synapses: p.Synapses, ModelSign: p.ModelSign,
				HopsFromStimulated: p.HopsFromStimulated, Roles: pf.Roles, Annotation: pf.Annotation,
				RateAHz: pf.RateAHz, RateBHz: pf.RateBHz, DeltaHz: pf.DeltaHz,
				SpikeCountA: pf.SpikeCountA, SpikeCountB: pf.SpikeCountB,
			})
		}
		if g := graph.ReadoutFirstSpikeMs; g != nil {
			if v, ok := g["A"][id]; ok {
				r.FirstSpikeMsA = v
			}
			if v, ok := g["B"][id]; ok && compare {
				r.FirstSpikeMsB = v
			}
		}
		d.Readouts = append(d.Readouts, r)
	}
	d.BehaviouralProxies = []ProxyFacts{}
	refs := []string{ModelCitation}
	for _, p := range in.Proxies {
		var inRun []ProxyNeuron
		for _, id := range sortedCopy(p.NeuronIDs) {
			if !readout[id] && !active[id] {
				continue
			}
			pn := ProxyNeuron{RootID: id, MeasuredAsReadout: readout[id], RateAHz: rates["A"][id].Rate}
			if compare {
				b := rates["B"][id].Rate
				pn.RateBHz = fptr(b)
				pn.DeltaHz = fptr(round(b-pn.RateAHz, 4))
			}
			inRun = append(inRun, pn)
		}
		if len(inRun) == 0 {
			continue
		}
		d.BehaviouralProxies = append(d.BehaviouralProxies, ProxyFacts{Proxy: p, NeuronsInRun: inRun})
		refs = append(refs, p.Reference)
		refs = append(refs, p.SecondaryReferences...)
	}

	// Coverage.
	cov := Coverage{
		AnnotationsReady:  ann != nil && ann.Ready,
		AnnotationsSource: AnnotationsCitation,
		PerCondition:      map[string]ConditionCoverage{},
	}
	var totalSpikes, annotatedSpikes int64
	for _, cond := range conds {
		var c ConditionCoverage
		for id, e := range rates[cond] {
			if e.Spikes == 0 {
				continue
			}
			c.ActiveNeurons++
			c.Spikes += e.Spikes
			if ann.Lookup(id) != nil {
				c.ActiveAnnotated++
				c.SpikesFromAnnotated += e.Spikes
			} else {
				c.UnannotatedActiveList++
			}
		}
		if c.ActiveNeurons > 0 {
			c.ActiveAnnotatedShare = fptr(round(float64(c.ActiveAnnotated)/float64(c.ActiveNeurons), 4))
			c.SpikeShareAnnotated = fptr(round(float64(c.SpikesFromAnnotated)/float64(c.Spikes), 4))
		}
		totalSpikes += c.Spikes
		annotatedSpikes += c.SpikesFromAnnotated
		cov.PerCondition[cond] = c
	}
	cov.ActiveNeurons = len(active)
	for id := range active {
		if ann.Lookup(id) != nil {
			cov.ActiveAnnotated++
		}
	}
	if cov.ActiveNeurons > 0 {
		cov.ActiveAnnotatedShare = fptr(round(float64(cov.ActiveAnnotated)/float64(cov.ActiveNeurons), 4))
		cov.SpikeShareAnnotated = fptr(round(float64(annotatedSpikes)/float64(totalSpikes), 4))
	}
	cov.UnannotatedTopNeurons = sortedKeys(unannotatedTop)
	cov.PerField = map[string]FieldCoverage{}
	cov.FieldWarnings = []string{}
	for _, field := range coverageFields {
		n := 0
		for id := range active {
			if c := classOf(ann.Lookup(id), field); c != unannotatedClass && c != emptyClass {
				n++
			}
		}
		fc := FieldCoverage{ActiveWithValue: n}
		if cov.ActiveNeurons > 0 && cov.AnnotationsReady {
			share := round(float64(n)/float64(cov.ActiveNeurons), 4)
			fc.Share = &share
			if share < CoverageWarningThreshold {
				cov.FieldWarnings = append(cov.FieldWarnings, fmt.Sprintf(
					"Field %s is filled for only %d of %d active neurons (%.1f%%, below %.0f%%): activity grouped by %s describes a minority of the activity; most of it is in the %q row.",
					field, n, cov.ActiveNeurons, 100*share, 100*CoverageWarningThreshold, field, emptyClass))
			}
		}
		cov.PerField[field] = fc
	}
	switch {
	case !cov.AnnotationsReady:
		w := "Neuron annotations are not installed on this server (data/annotations_630.tsv is missing): annotation coverage is 0 and no neuron has a cell type, class or neurotransmitter. Interpret only ids, rates, latencies and graph distances."
		cov.Warning = &w
		zero := 0.0
		cov.ActiveAnnotatedShare, cov.SpikeShareAnnotated = &zero, &zero
	case cov.ActiveNeurons == 0:
		w := "No neuron spiked in this run, so there is no activity to annotate."
		cov.Warning = &w
	case *cov.ActiveAnnotatedShare < CoverageWarningThreshold:
		w := fmt.Sprintf("Low annotation coverage: only %d of %d active neurons (%.1f%%) have a FlyWire annotation (below %.0f%%); class-level summaries describe a minority of the activity and conclusions about cell types are weak.",
			cov.ActiveAnnotated, cov.ActiveNeurons, 100**cov.ActiveAnnotatedShare, 100*CoverageWarningThreshold)
		cov.Warning = &w
	}
	if cov.Warning != nil {
		d.Warnings = append(d.Warnings, *cov.Warning)
	}
	d.Warnings = append(d.Warnings, cov.FieldWarnings...)
	d.Coverage = cov
	if cov.AnnotationsReady {
		refs = append(refs, AnnotationsCitation)
	}
	d.References = dedupe(refs)
	return d, nil
}

// coverageFields are the annotation fields the digest groups activity by or relies on.
var coverageFields = []string{"super_class", "cell_class", "cell_sub_class", "cell_type", "top_nt"}

func modelParameters(p LIFParameters) ModelParameters {
	return ModelParameters{
		LIFParameters:        p,
		ThresholdAboveRestMV: round(p.VThMV-p.V0mV, 3),
		PoissonInputStepMV:   round(p.WSynMV*p.FPoi, 3),
		MaxRateHzBound:       round(1000/p.TRfcMs, 1),
		Note: "LIF parameters of the simulator (Shiu et al. 2024): v_0/v_rst rest and reset, v_th threshold (mV), t_mbr membrane and tau synaptic time constants, t_rfc refractory period, t_dly synaptic delay (ms), w_syn mV per synapse, f_poi scaling of a Poisson input spike, dt time step. " +
			"max_rate_hz_bound = 1000 / t_rfc_ms is an upper bound on any neuron's rate; poisson_input_step_mv = w_syn_mv x f_poi is the jump one input spike gives a stimulated neuron.",
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
