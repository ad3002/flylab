package interpret

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ad3002/flylab/internal/contracts"
)

func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func loadRegistry(t *testing.T) *contracts.Registry {
	t.Helper()
	reg, err := contracts.LoadRegistry(filepath.Join(projectRoot(t), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func fixtureAnnotations(t *testing.T) *Annotations {
	t.Helper()
	a, err := LoadAnnotations(filepath.Join("testdata", "annotations_fixture.tsv"))
	if err != nil {
		t.Fatalf("fixture annotations: %v", err)
	}
	if !a.Ready || a.Count() != 6 {
		t.Fatalf("fixture annotations: ready=%v count=%d", a.Ready, a.Count())
	}
	return a
}

// copyFixture copies testdata/<name> into a temp dir so a test can corrupt it.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join("testdata", name, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func lowCoverageAnnotations(t *testing.T) *Annotations {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ann.tsv")
	body := strings.Join(AnnotationColumns, "\t") + "\n" +
		"720575940616885538\tafferent\tsensory\tgustatory\tsugar\tLB3c\t\tacetylcholine\t0.5\tleft\tMxLbN\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAnnotations(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func findRow(rows []ClassActivity, class string) *ClassActivity {
	for i := range rows {
		if rows[i].Class == class {
			return &rows[i]
		}
	}
	return nil
}

func TestBuildDigestFixtures(t *testing.T) {
	reg := loadRegistry(t)
	proxies, err := LoadProxies(filepath.Join(projectRoot(t), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	missing, err := LoadAnnotations(filepath.Join(t.TempDir(), "absent.tsv"))
	if err != nil || missing.Ready {
		t.Fatalf("absent annotations must be Ready=false without error, got %v %v", missing.Ready, err)
	}

	cases := []struct {
		name  string
		job   string
		ann   *Annotations
		check func(t *testing.T, d *Digest)
	}{
		{"compare with annotations", "job_compare", fixtureAnnotations(t), func(t *testing.T, d *Digest) {
			if d.Experiment.Type != "compare_silencing" || d.Experiment.DurationMs != 100 || d.Experiment.Repeats != 1 || d.Experiment.BaseSeed != 42 {
				t.Fatalf("experiment: %+v", d.Experiment)
			}
			st := d.Experiment.Stimulated[0]
			if st.GroupID != "sugar_grn" || *st.RateHz != 100 || st.NeuronCount != 21 || st.Annotation.Annotated != 4 || st.Annotation.Unannotated != 17 {
				t.Fatalf("stimulated set: %+v", st)
			}
			if st.Annotation.ByCellType["LB3c"] != 2 || st.Annotation.ByCellType["LB3d"] != 1 || st.Annotation.ByCellType["LB4b"] != 1 ||
				st.Annotation.BySuperClass["sensory"] != 4 || st.Annotation.ByCellClass["gustatory"] != 4 {
				t.Fatalf("sugar_grn must annotate as sensory/gustatory LB3*/LB4b: %+v", st.Annotation)
			}
			if d.Experiment.Silenced == nil || d.Experiment.Silenced.GroupID != "demo_silencing" || d.Experiment.Silenced.NeuronIDs[0] != "720575940616885538" {
				t.Fatalf("silenced set: %+v", d.Experiment.Silenced)
			}
			if d.Experiment.Readout.GroupID != "mn9" || d.Experiment.Readout.Annotation.ByCellType["CB0701"] != 1 ||
				d.Experiment.Readout.Annotation.BySuperClass["motor"] != 1 || d.Experiment.Readout.Annotation.Unannotated != 1 {
				t.Fatalf("mn9 must annotate as motor CB0701 (one id unannotated in v783): %+v", d.Experiment.Readout)
			}
			tt := d.Totals
			if tt.A.Spikes != 50 || tt.A.ActiveNeurons != 5 || tt.B.Spikes != 44 || tt.B.ActiveNeurons != 4 ||
				*tt.DeltaSpikes != -6 || *tt.RelativeChangeSpikes != -0.12 || *tt.DeltaActiveNeurons != -1 || *tt.RelativeChangeNeurons != -0.2 {
				t.Fatalf("totals: %+v", tt)
			}
			rows := d.ActivityBySuperClass.Rows
			if rows[0].Class != "sensory" || rows[0].A.ActiveNeurons != 2 || rows[0].A.TotalSpikes != 38 || rows[0].A.MeanRateHz != 190 {
				t.Fatalf("super_class rows not ordered by spikes: %+v", rows)
			}
			if r := findRow(rows, "central"); r == nil || r.A.TotalSpikes != 4 || r.B.TotalSpikes != 0 || *r.DeltaSpikes != -4 || *r.DeltaActiveNeurons != -1 {
				t.Fatalf("central class delta wrong: %+v", r)
			}
			if r := findRow(rows, unannotatedClass); r == nil || r.A.TotalSpikes != 5 {
				t.Fatalf("unannotated activity must be its own visible class: %+v", rows)
			}
			if r := findRow(d.ActivityByCellClass.Rows, emptyClass); r == nil || r.A.TotalSpikes != 4 {
				t.Fatalf("annotated-but-empty cell_class must be visible: %+v", d.ActivityByCellClass.Rows)
			}
			if r := findRow(d.ActivityByTopNT.Rows, "gaba"); r == nil || r.A.ActiveNeurons != 1 {
				t.Fatalf("top_nt table: %+v", d.ActivityByTopNT.Rows)
			}
			// Network neurons only: the three stimulated GRNs (200, 180, 50 Hz) are not in it.
			top := d.TopNeuronsByRate
			if len(top) != 2 || top[0].RootID != "720575940000000001" || top[0].RateAHz != 40 || top[1].RootID != "720575940660219265" {
				t.Fatalf("top by rate must rank non-stimulated neurons only: %+v", top)
			}
			if top[0].Annotation == nil || top[0].Annotation.CellType != "FIXTURE_GABA_1" || len(top[0].Roles) != 0 ||
				*top[0].ModelSign != -1 || top[0].SpikeCountA != 4 || *top[0].SpikeCountB != 0 || *top[0].DeltaHz != -40 {
				t.Fatalf("top neuron facts: %+v", top[0])
			}
			// Every stimulated neuron has its own row (21), most active first, silent ones by id.
			st0 := d.StimulatedNeurons
			if len(st0) != 21 || st0[0].RootID != "720575940616885538" || st0[1].RootID != "720575940612670570" || st0[2].RootID != "720575940620900446" ||
				st0[3].RootID != "720575940611875570" || st0[3].RateAHz != 0 || st0[3].Annotation == nil || st0[3].Annotation.CellSubClass != "high_salt/heavy_metal" {
				t.Fatalf("stimulated_neurons: %+v", st0[:4])
			}
			if st0[0].Annotation.CellType != "LB3c" || strings.Join(st0[0].Roles, ",") != "stimulated,silenced" || *st0[0].HopsFromStimulated != 0 ||
				*st0[0].FirstSpikeMsA != 0.2 || st0[0].SpikeCountA != 20 || *st0[0].ModelSign != 1 {
				t.Fatalf("stimulated neuron facts: %+v", st0[0])
			}
			if len(d.SilencedNeurons) != 1 || d.SilencedNeurons[0].RootID != "720575940616885538" {
				t.Fatalf("silenced_neurons: %+v", d.SilencedNeurons)
			}
			if ids := st.Annotation.IDsByCellSubClass; strings.Join(ids["high_salt/heavy_metal"], ",") != "720575940611875570" ||
				strings.Join(ids["sugar"], ",") != "720575940612670570,720575940616885538" || len(ids[unannotatedClass]) != 17 {
				t.Fatalf("stimulated ids by cell_sub_class: %v", ids)
			}
			if ids := st.Annotation.IDsByCellType; strings.Join(ids["LB4b"], ",") != "720575940624963786" {
				t.Fatalf("stimulated ids by cell_type: %v", ids)
			}
			if len(d.TopNeuronsByDelta) != 2 || d.TopNeuronsByDelta[0].RootID != "720575940000000001" || *d.TopNeuronsByDelta[0].DeltaHz != -40 ||
				*d.TopNeuronsByDelta[0].HopsFromSilenced != 1 || *d.TopNeuronsByDelta[0].DirectInputFromSilenced != 5 ||
				d.TopNeuronsByDelta[1].RootID != "720575940660219265" || *d.TopNeuronsByDelta[1].DeltaHz != -20 {
				t.Fatalf("top by delta: %+v", d.TopNeuronsByDelta)
			}
			if len(d.Readouts) != 2 || d.Readouts[1].RootID != "720575940660219265" || *d.Readouts[1].FirstSpikeMsB != 14.2 ||
				d.Readouts[1].ProxyIDs[0] != "mn9_rostrum_extension" || d.Readouts[0].Annotation != nil || *d.Readouts[0].HopsFromStimulated != 3 {
				t.Fatalf("readouts: %+v", d.Readouts)
			}
			r1 := d.Readouts[1]
			if r1.SpikeCountA != 3 || *r1.SpikeCountB != 1 || r1.PresynapticPartners != 12 || r1.ActivePresynapticPartners != 2 ||
				r1.NetSynapsesFromActivePartners != 3 || d.Readouts[0].PresynapticPartners != 5 || d.Readouts[0].ActivePresynapticPartners != 0 {
				t.Fatalf("readout counts and inputs: %+v / %+v", r1, d.Readouts[0])
			}
			ri := d.ReadoutInputs
			if len(ri) != 2 || ri[0].ReadoutID != "720575940660219265" || ri[0].RootID != "720575940616885538" || ri[0].Synapses != 7 ||
				strings.Join(ri[0].Roles, ",") != "stimulated,silenced" || ri[0].RateAHz != 200 ||
				ri[1].RootID != "720575940000000001" || ri[1].Synapses != -4 || *ri[1].ModelSign != -1 || *ri[1].HopsFromStimulated != 2 ||
				ri[1].Annotation == nil || ri[1].Annotation.TopNT != "gaba" || *ri[1].DeltaHz != -40 || *ri[1].SpikeCountB != 0 {
				t.Fatalf("readout_inputs: %+v", ri)
			}
			mp := d.ModelParameters
			if mp.TRfcMs != 2.2 || mp.MaxRateHzBound != 454.5 || mp.PoissonInputStepMV != 68.75 || mp.ThresholdAboveRestMV != 7 {
				t.Fatalf("model parameters: %+v", mp)
			}
			if r := findRow(d.ActivityByCellType.Rows, "LB3c"); r == nil || r.A.ActiveNeurons != 2 || r.A.TotalSpikes != 38 {
				t.Fatalf("activity by cell_type: %+v", d.ActivityByCellType.Rows)
			}
			if len(d.BehaviouralProxies) != 1 || len(d.BehaviouralProxies[0].NeuronsInRun) != 2 || !d.BehaviouralProxies[0].NeuronsInRun[0].MeasuredAsReadout {
				t.Fatalf("proxies: %+v", d.BehaviouralProxies)
			}
			c := d.Coverage
			if !c.AnnotationsReady || c.ActiveNeurons != 5 || c.ActiveAnnotated != 4 || *c.ActiveAnnotatedShare != 0.8 || c.Warning != nil ||
				*c.PerCondition["B"].ActiveAnnotatedShare != 0.75 || *c.SpikeShareAnnotated != round(84.0/94.0, 4) {
				t.Fatalf("coverage: %+v", c)
			}
			if len(c.UnannotatedTopNeurons) != 0 {
				t.Fatalf("unannotated top neurons: %v", c.UnannotatedTopNeurons)
			}
			// Per field: 5 active neurons; cell_class and cell_sub_class are empty for the GABA
			// neuron and missing for the unannotated one (3/5 < 70%), the others are 4/5.
			pf := c.PerField
			if pf["super_class"].ActiveWithValue != 4 || *pf["super_class"].Share != 0.8 || *pf["cell_class"].Share != 0.6 ||
				*pf["cell_sub_class"].Share != 0.6 || *pf["cell_type"].Share != 0.8 || *pf["top_nt"].Share != 0.8 {
				t.Fatalf("per-field coverage: %+v", pf)
			}
			if len(c.FieldWarnings) != 2 || !strings.Contains(c.FieldWarnings[0], "Field cell_class is filled for only 3 of 5 active neurons (60.0%") ||
				!strings.Contains(c.FieldWarnings[1], "Field cell_sub_class") {
				t.Fatalf("field warnings: %v", c.FieldWarnings)
			}
			refs := strings.Join(d.References, "|")
			for _, want := range []string{"10.1038/s41586-024-07763-9", "10.1016/j.neuron.2008.12.033", "10.1038/s41586-024-07686-5"} {
				if !strings.Contains(refs, want) {
					t.Fatalf("references lack %s: %v", want, d.References)
				}
			}
			if strings.Count(refs, "10.1038/s41586-024-07763-9") != 1 {
				t.Fatalf("references must be deduplicated: %v", d.References)
			}
			if strings.Join(d.Warnings, "|") != strings.Join(c.FieldWarnings, "|") {
				t.Fatalf("only the field warnings expected: %v", d.Warnings)
			}
		}},
		{"single without annotations", "job_single", missing, func(t *testing.T, d *Digest) {
			if d.Totals.B != nil || d.Totals.DeltaSpikes != nil || d.Experiment.Silenced != nil || d.TopNeuronsByDelta != nil {
				t.Fatalf("single runs have no condition B: %+v", d.Totals)
			}
			if d.Totals.A.Spikes != 19 || d.Totals.A.ActiveNeurons != 3 {
				t.Fatalf("totals: %+v", d.Totals)
			}
			if d.TopNeuronsByRate[0].RateBHz != nil || d.TopNeuronsByRate[0].HopsFromSilenced != nil || d.TopNeuronsByRate[0].Annotation != nil {
				t.Fatalf("single-run neuron must have null B fields and no annotation: %+v", d.TopNeuronsByRate[0])
			}
			c := d.Coverage
			if c.AnnotationsReady || *c.ActiveAnnotatedShare != 0 || c.Warning == nil || !strings.Contains(*c.Warning, "annotations_630.tsv is missing") {
				t.Fatalf("missing annotations must give coverage 0 and a visible warning: %+v", c)
			}
			if len(d.Warnings) != 1 || d.Warnings[0] != *c.Warning || c.PerField["cell_type"].Share != nil {
				t.Fatalf("coverage warning must be in warnings (no per-field shares without annotations): %v %+v", d.Warnings, c.PerField)
			}
			if d.TopNeuronsByRate[0].RootID != "720575940660219265" || d.TopNeuronsByRate[0].SpikeCountB != nil || len(d.StimulatedNeurons) != 21 || d.SilencedNeurons != nil {
				t.Fatalf("single run: top = MN9 (only non-stimulated active neuron), no silenced list: %+v", d.TopNeuronsByRate)
			}
			if r := findRow(d.ActivityBySuperClass.Rows, unannotatedClass); r == nil || r.A.TotalSpikes != 19 || r.B != nil {
				t.Fatalf("all activity is unannotated: %+v", d.ActivityBySuperClass.Rows)
			}
			b, _ := json.Marshal(d)
			if !strings.Contains(string(b), `"top_neurons_by_delta":null`) || !strings.Contains(string(b), `"B":null`) {
				t.Fatalf("single digest JSON must carry explicit nulls: %s", b)
			}
		}},
		{"compare with low coverage", "job_compare", lowCoverageAnnotations(t), func(t *testing.T, d *Digest) {
			c := d.Coverage
			if *c.ActiveAnnotatedShare != 0.2 || c.Warning == nil || !strings.Contains(*c.Warning, "only 1 of 5 active neurons (20.0%)") {
				t.Fatalf("coverage below 70%% must warn: %+v", c)
			}
			if len(d.Warnings) != 6 || d.Warnings[0] != *c.Warning || len(c.FieldWarnings) != 5 {
				t.Fatalf("coverage warning plus one per field expected: %v", d.Warnings)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := DigestInput{JobID: "job_fixture", ArtifactsDir: filepath.Join("testdata", tc.job),
				Registry: reg, Annotations: tc.ann, Proxies: proxies}
			d, err := BuildDigest(in)
			if err != nil {
				t.Fatalf("BuildDigest: %v", err)
			}
			tc.check(t, d)
			// Deterministic: the same inputs give byte-identical JSON.
			d2, err := BuildDigest(in)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(d)
			b, _ := json.Marshal(d2)
			if string(a) != string(b) {
				t.Fatalf("digest is not deterministic")
			}
		})
	}
}

func TestBuildDigestErrors(t *testing.T) {
	reg := loadRegistry(t)
	cases := []struct {
		name   string
		job    string
		mutate func(dir string) error
		want   string
	}{
		{"missing rates", "job_compare", func(d string) error { return os.Remove(filepath.Join(d, "rates.csv")) }, "rates.csv is missing"},
		{"corrupt rates row", "job_compare", func(d string) error {
			return appendFile(filepath.Join(d, "rates.csv"), "A,0,720575940000000002,x,1.0,false\n")
		}, "rates.csv line 13: spike_count \"x\""},
		{"condition B in single run", "job_single", func(d string) error {
			return appendFile(filepath.Join(d, "rates.csv"), "B,0,720575940000000002,1,10.0,false\n")
		}, `condition "B", not expected`},
		{"graph lacks a spiking neuron", "job_compare", func(d string) error {
			return appendFile(filepath.Join(d, "rates.csv"), "A,0,720575940000000002,1,10.0,false\n")
		}, "digest_graph.json lacks neuron 720575940000000002"},
		{"graph for another plan", "job_compare", func(d string) error {
			return replaceIn(filepath.Join(d, "digest_graph.json"), "hash_fixture_compare_silencing", "hash_other")
		}, "computed for plan hash_other"},
		{"truncated graph", "job_compare", func(d string) error {
			return os.WriteFile(filepath.Join(d, "digest_graph.json"), []byte(`{"schema_version": "1.0", "neurons": [`), 0o644)
		}, "digest_graph.json cannot be parsed"},
		{"missing graph", "job_compare", func(d string) error { return os.Remove(filepath.Join(d, "digest_graph.json")) }, "digest_graph.json cannot be read"},
		{"graph without readout inputs", "job_compare", func(d string) error {
			return replaceIn(filepath.Join(d, "digest_graph.json"), `"720575940645521262": {
      "presynaptic_partners"`, `"720575940000000009": {
      "presynaptic_partners"`)
		}, "lacks readout_inputs of readout neuron 720575940645521262"},
		{"outdated graph", "job_compare", func(d string) error {
			return replaceIn(filepath.Join(d, "digest_graph.json"), `"schema_version": "1.1"`, `"schema_version": "1.0"`)
		}, `schema_version "1.0" is not 1.1`},
		{"corrupt resolved plan", "job_single", func(d string) error {
			return os.WriteFile(filepath.Join(d, "resolved_plan.json"), []byte("{"), 0o644)
		}, "resolved_plan.json cannot be parsed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyFixture(t, tc.job)
			if err := tc.mutate(dir); err != nil {
				t.Fatal(err)
			}
			_, err := BuildDigest(DigestInput{JobID: "j", ArtifactsDir: dir, Registry: reg, Annotations: fixtureAnnotations(t)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
			// Problems of the cached graph file are a *GraphError (the service recomputes the
			// file); only a file of an older flysim is Outdated (recomputed without a warning).
			var ge *GraphError
			isGraph := strings.Contains(tc.want, "digest_graph.json") || strings.Contains(tc.want, "plan hash_other") ||
				strings.Contains(tc.want, "readout_inputs") || strings.Contains(tc.want, "schema_version")
			if errors.As(err, &ge) != isGraph {
				t.Fatalf("GraphError=%v for %v", !isGraph, err)
			}
			if isGraph && ge.Outdated != (tc.name == "outdated graph") {
				t.Fatalf("Outdated=%v for %v", ge.Outdated, err)
			}
		})
	}
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func replaceIn(path, old, new string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.ReplaceAll(string(b), old, new)), 0o644)
}

func TestLoadAnnotations(t *testing.T) {
	header := strings.Join(AnnotationColumns, "\t")
	good := "720575940616885538\tafferent\tsensory\tgustatory\tsugar\tLB3c\t\tacetylcholine\t0.5174\tleft\tMxLbN"
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad header", "root_id\tflow\n" + good + "\n", "line 1 is"},
		{"short row", header + "\n720575940616885538\tafferent\n", "line 2 has 2 columns, expected 11"},
		{"bad root id", header + "\nabc" + good[18:] + "\n", `line 2: root_id "abc"`},
		{"duplicate", header + "\n" + good + "\n" + good + "\n", "line 3: root_id 720575940616885538 appears twice"},
		{"bad confidence", header + "\n" + strings.Replace(good, "0.5174", "high", 1) + "\n", `top_nt_conf "high"`},
		{"empty file", "", "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "a.tsv")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadAnnotations(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
	p := filepath.Join(t.TempDir(), "ok.tsv")
	if err := os.WriteFile(p, []byte(header+"\n"+good+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := LoadAnnotations(p)
	if err != nil {
		t.Fatal(err)
	}
	got := a.Lookup("720575940616885538")
	if got == nil || got.CellType != "LB3c" || got.TopNT != "acetylcholine" || *got.TopNTConf != 0.517 || got.HemibrainType != "" {
		t.Fatalf("parsed annotation wrong: %+v", got)
	}
	if a.Lookup("720575940000000009") != nil {
		t.Fatalf("unknown id must have no annotation")
	}
}

// TestRegistryGroupsAnnotateWithRealData checks the derived table of setup_data.sh, when it
// is installed: sugar GRNs are gustatory sensory LB3*/LB4b neurons, MN9 is motor CB0701.
func TestRegistryGroupsAnnotateWithRealData(t *testing.T) {
	path := filepath.Join(projectRoot(t), "data", "annotations_630.tsv")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("data/annotations_630.tsv not installed (run scripts/setup_data.sh): %v", err)
	}
	a, err := LoadAnnotations(path)
	if err != nil {
		t.Fatalf("installed annotations are malformed: %v", err)
	}
	if a.Count() < 100000 || a.Count() > 127400 {
		t.Fatalf("expected ~106k annotated v630 neurons, got %d", a.Count())
	}
	reg := loadRegistry(t)
	annotated := 0
	for _, id := range reg.GroupsMap["sugar_grn"].NeuronIDs {
		x := a.Lookup(id)
		if x == nil {
			continue
		}
		annotated++
		if x.SuperClass != "sensory" || x.CellClass != "gustatory" ||
			!(strings.HasPrefix(x.CellType, "LB3") || x.CellType == "LB4b") {
			t.Fatalf("sugar_grn neuron %s annotated as %+v", id, x)
		}
	}
	if annotated < 20 {
		t.Fatalf("expected at least 20 of 21 sugar_grn neurons annotated, got %d", annotated)
	}
	mn9 := a.Lookup("720575940660219265")
	if mn9 == nil || mn9.SuperClass != "motor" || mn9.CellType != "CB0701" {
		t.Fatalf("MN9 720575940660219265 must be motor CB0701, got %+v", mn9)
	}
	if a.Lookup("720575940645521262") != nil {
		t.Fatalf("MN9 720575940645521262 was edited after v630 and must stay unannotated")
	}
}

func TestLoadProxiesFromRegistry(t *testing.T) {
	ps, err := LoadProxies(filepath.Join(projectRoot(t), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].ProxyID != "mn9_rostrum_extension" || len(ps[0].NeuronIDs) != 2 ||
		!strings.Contains(ps[0].Reference, "10.1038/s41586-024-07763-9") || ps[0].BehaviourRu == "" || ps[0].Limits == "" {
		t.Fatalf("unexpected proxies: %+v", ps)
	}
	reg := loadRegistry(t)
	if !sameSet(ps[0].NeuronIDs, reg.GroupsMap["mn9"].NeuronIDs) {
		t.Fatalf("mn9 proxy ids must equal the mn9 registry group")
	}

	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field": `{"schema_version":"1.0","proxies":[{"proxy_id":"x","extra":1}]}`,
		"empty field":   `{"schema_version":"1.0","proxies":[{"proxy_id":"x","neuron_ids":["720575940660219265"],"behaviour_en":"a","behaviour_ru":"b","evidence":"","limits":"l","reference":"r"}]}`,
		"bad id":        `{"schema_version":"1.0","proxies":[{"proxy_id":"x","neuron_ids":["mn9"],"behaviour_en":"a","behaviour_ru":"b","evidence":"e","limits":"l","reference":"r"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, "readout_proxies.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProxies(dir); err == nil {
			t.Fatalf("%s: malformed proxies must be an error", name)
		}
	}
	if _, err := LoadProxies(t.TempDir()); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("missing proxies file must be an error, got %v", err)
	}
}

// proxyArithmetic recomputes every "X of [the] N ... (P%)" in a text and returns the pairs
// whose percentage does not match X/N to one decimal.
func proxyArithmetic(text string) (checked int, bad []string) {
	re := regexp.MustCompile(`(\d+) of (?:the )?(\d+)\b[^()%]*?\((\d+(?:\.\d+)?)%`)
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		x, _ := strconv.Atoi(m[1])
		n, _ := strconv.Atoi(m[2])
		p, _ := strconv.ParseFloat(m[3], 64)
		checked++
		if n == 0 || math.Abs(round(100*float64(x)/float64(n), 1)-p) > 0.05 {
			bad = append(bad, m[0])
		}
	}
	return checked, bad
}

// The proxy evidence is the only literature fact the model gets; its numbers must agree with
// each other and with Shiu et al. 2024 (11 predicted to activate MN9, 10 of them did; 4 of the
// 95 predicted not to did: 101 of 106 correct, reported as >90% accuracy; Fig. 2a-c).
func TestProxyEvidenceArithmetic(t *testing.T) {
	if n, bad := proxyArithmetic("10 of the 11 cell types did, and 4 of the 95 predicted not to did (84% overall accuracy)"); n != 1 || len(bad) != 1 {
		t.Fatalf("the checker must catch the old inconsistent sentence: checked %d, bad %v", n, bad)
	}
	ps, err := LoadProxies(filepath.Join(projectRoot(t), "registry"))
	if err != nil {
		t.Fatal(err)
	}
	ev := ps[0].Evidence
	n, bad := proxyArithmetic(ev)
	if n == 0 || len(bad) > 0 {
		t.Fatalf("proxy evidence arithmetic: checked %d pairs, inconsistent %v", n, bad)
	}
	for _, want := range []string{"10 of those 11", "4 did", "101 of the 106 predictions correct (95.3%)", "greater than 90% accuracy",
		"Fig. 2a-c", "Fig. 3d,e", "In the model (not measured in flies)", "Fig. 1c, Extended Data Fig. 1d"} {
		if !strings.Contains(ev, want) {
			t.Fatalf("proxy evidence lacks %q: %s", want, ev)
		}
	}
	if strings.Contains(ev, "84%") {
		t.Fatalf("the unsupported 84%% figure must be gone: %s", ev)
	}
}

// Registry descriptions and the annotation side must not read as a conflict: the registry uses
// the (mirrored) FAFB image convention of Shiu et al.'s notebooks and says so.
func TestRegistrySideConventionIsExplained(t *testing.T) {
	reg := loadRegistry(t)
	for _, id := range []string{"sugar_grn", "bitter_grn", "ir94e"} {
		d := reg.GroupsMap[id].Description
		if !strings.Contains(d, "FAFB image convention") || !strings.Contains(d, "annotation side: left") {
			t.Fatalf("%s description must state the side convention: %q", id, d)
		}
	}
	if d := reg.GroupsMap["mn9"].Description; !strings.Contains(d, "720575940660219265 is on the fly's right (FlyWire annotation side: right)") {
		t.Fatalf("mn9 description must state the side convention: %q", d)
	}
	found := false
	for _, f := range ModelFacts {
		if strings.Contains(f, "do not conflict") && strings.Contains(f, "side") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ModelFacts must explain the side convention")
	}
}

// delta_mean_rate_hz is over the neurons active in A or B: when a class recruits a neuron in B
// its delta has the sign of delta_spikes (the per-condition means would say the opposite).
func TestDeltaMeanRateUsesUnionOfActiveNeurons(t *testing.T) {
	dir := copyFixture(t, "job_compare")
	// the unannotated readout 720575940645521262 fires 1 spike (10 Hz) in B only; the other
	// unannotated active neuron 720575940620900446 fires 5 spikes (50 Hz) in both conditions.
	if err := replaceIn(filepath.Join(dir, "rates.csv"), "B,0,720575940645521262,0,0.0000,true", "B,0,720575940645521262,1,10.0000,true"); err != nil {
		t.Fatal(err)
	}
	d, err := BuildDigest(DigestInput{JobID: "j", ArtifactsDir: dir, Registry: loadRegistry(t), Annotations: fixtureAnnotations(t)})
	if err != nil {
		t.Fatal(err)
	}
	r := findRow(d.ActivityBySuperClass.Rows, unannotatedClass)
	if r == nil || r.A.ActiveNeurons != 1 || r.B.ActiveNeurons != 2 || *r.DeltaSpikes != 1 || r.A.MeanRateHz != 50 || r.B.MeanRateHz != 30 {
		t.Fatalf("unannotated row: %+v", r)
	}
	// (50 + 10 - 50) / 2 neurons = +5 Hz, same sign as delta_spikes (+1); the old per-condition
	// difference was 30 - 50 = -20.
	if *r.DeltaMeanRateHz != 5 {
		t.Fatalf("delta_mean_rate_hz = %v, want +5", *r.DeltaMeanRateHz)
	}
	if c := findRow(d.ActivityBySuperClass.Rows, "central"); c == nil || *c.DeltaMeanRateHz != -40 {
		t.Fatalf("a neuron silent in B counts 0 Hz there: %+v", c)
	}
}
