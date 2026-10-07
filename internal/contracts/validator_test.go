package contracts_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ad3002/flylab/internal/contracts"
)

func TestValidatorAndRegistry(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get wd: %v", err)
	}
	projectRoot := filepath.Dir(filepath.Dir(wd))

	registryDir := filepath.Join(projectRoot, "registry")
	contractsDir := filepath.Join(projectRoot, "contracts")

	reg, err := contracts.LoadRegistry(registryDir)
	if err != nil {
		t.Fatalf("Failed to load registry: %v", err)
	}

	if len(reg.Groups) == 0 {
		t.Fatalf("Expected non-empty groups in registry")
	}

	val, err := contracts.NewValidator(contractsDir, reg)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	// 1. Test valid single fixture
	validSingle, err := os.ReadFile(filepath.Join(contractsDir, "fixtures", "valid_single.json"))
	if err != nil {
		t.Fatalf("Failed to read valid_single.json: %v", err)
	}
	res, err := val.ValidateRawJSON(validSingle)
	if err != nil {
		t.Fatalf("Valid single fixture failed validation: %v", err)
	}
	if res.ResolvedPlan == nil || len(res.ResolvedPlan.Activation) == 0 {
		t.Fatalf("Expected resolved plan with activations")
	}

	// 2. Test valid compare fixture
	validCompare, err := os.ReadFile(filepath.Join(contractsDir, "fixtures", "valid_compare.json"))
	if err != nil {
		t.Fatalf("Failed to read valid_compare.json: %v", err)
	}
	res2, err := val.ValidateRawJSON(validCompare)
	if err != nil {
		t.Fatalf("Valid compare fixture failed validation: %v", err)
	}
	if len(res2.ResolvedPlan.SilencingNeuronIDs) == 0 {
		t.Fatalf("Expected resolved plan with silenced neurons")
	}

	// 3. Test invalid extra field (strict schema rejection)
	invalidExtra, err := os.ReadFile(filepath.Join(contractsDir, "fixtures", "invalid_extra_field.json"))
	if err != nil {
		t.Fatalf("Failed to read invalid_extra_field.json: %v", err)
	}
	_, err = val.ValidateRawJSON(invalidExtra)
	if err == nil {
		t.Fatalf("Expected invalid_extra_field.json to fail validation")
	}

	// 4. Test invalid frequency
	invalidFreq, err := os.ReadFile(filepath.Join(contractsDir, "fixtures", "invalid_frequency.json"))
	if err != nil {
		t.Fatalf("Failed to read invalid_frequency.json: %v", err)
	}
	_, err = val.ValidateRawJSON(invalidFreq)
	if err == nil {
		t.Fatalf("Expected invalid_frequency.json to fail validation")
	}
}

func TestValidatorLimitsComeFromSchema(t *testing.T) {
	wd, _ := os.Getwd()
	projectRoot := filepath.Dir(filepath.Dir(wd))
	reg, err := contracts.LoadRegistry(filepath.Join(projectRoot, "registry"))
	if err != nil {
		t.Fatalf("Failed to load registry: %v", err)
	}
	val, err := contracts.NewValidator(filepath.Join(projectRoot, "contracts"), reg)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}
	l := val.Limits()
	if l.DurationMinMs != 10 || l.DurationMaxMs != 1000 || l.RepeatsMin != 1 || l.RepeatsMax != 3 {
		t.Fatalf("unexpected duration/repeat limits: %+v", l)
	}
	if l.RateMinHz != 0 || l.RateMaxHz != 200 || l.ActivationMax != 2 || l.BaseSeedMax != 2147483645 {
		t.Fatalf("unexpected rate/activation/seed limits: %+v", l)
	}
	if len(l.ExperimentTypes) != 2 || l.ExperimentTypes[0] != "single" || l.ExperimentTypes[1] != "compare_silencing" {
		t.Fatalf("unexpected experiment types: %v", l.ExperimentTypes)
	}
}

// Two activation sets must stay distinct: flysim drives each set at its own rate, so a neuron in
// both sets has no single rate and must be rejected at plan time with a message naming the sets.
func TestValidatorActivationSetsKeepOwnRatesAndMustNotOverlap(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get wd: %v", err)
	}
	projectRoot := filepath.Dir(filepath.Dir(wd))
	reg, err := contracts.LoadRegistry(filepath.Join(projectRoot, "registry"))
	if err != nil {
		t.Fatalf("Failed to load registry: %v", err)
	}
	val, err := contracts.NewValidator(filepath.Join(projectRoot, "contracts"), reg)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}
	plan := func(g1 string, r1 float64, g2 string, r2 float64) []byte {
		return []byte(fmt.Sprintf(`{"schema_version":"1.0","dataset_id":"flywire_630","model_id":"shiu_lif_rust",
			"experiment_type":"single","activation":[{"selector":{"group_id":%q},"rate_hz":%g},{"selector":{"group_id":%q},"rate_hz":%g}],
			"silencing":[],"readout":[{"selector":{"group_id":"mn9"}}],"duration_ms":100,"repeats":1,"base_seed":1,"report_language":"en"}`,
			g1, r1, g2, r2))
	}

	res, err := val.ValidateRawJSON(plan("sugar_grn", 40, "bitter_grn", 160))
	if err != nil {
		t.Fatalf("disjoint sugar+bitter plan must validate: %v", err)
	}
	acts := res.ResolvedPlan.Activation
	if len(acts) != 2 || acts[0].RateHz != 40 || acts[1].RateHz != 160 || len(acts[0].NeuronIDs) != 21 || len(acts[1].NeuronIDs) != 21 {
		t.Fatalf("resolved activations must keep per-set rates and members, got %+v", acts)
	}

	// demo_silencing is one of the sugar GRNs, so it overlaps sugar_grn.
	_, err = val.ValidateRawJSON(plan("sugar_grn", 40, "demo_silencing", 160))
	if err == nil {
		t.Fatalf("overlapping activation sets must be rejected")
	}
	if !strings.Contains(err.Error(), "activation[0]") || !strings.Contains(err.Error(), "activation[1]") {
		t.Fatalf("error must name both activation sets, got %q", err.Error())
	}
}

// Contract v4 section 5: explicit neuron_ids must be neurons of the v630 connectome.
func TestValidatorRejectsUnknownNeuronIDs(t *testing.T) {
	wd, _ := os.Getwd()
	projectRoot := filepath.Dir(filepath.Dir(wd))
	reg, err := contracts.LoadRegistry(filepath.Join(projectRoot, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	val, err := contracts.NewValidator(filepath.Join(projectRoot, "contracts"), reg)
	if err != nil {
		t.Fatal(err)
	}
	set, err := contracts.LoadNeuronIDs(filepath.Join(projectRoot, "data"))
	if err != nil {
		t.Fatalf("the real completeness table must load: %v", err)
	}
	if set.Len() != 127400 || !set.Has("720575940660219265") || set.Has("720575940000000001") {
		t.Fatalf("neuron set wrong: %d neurons", set.Len())
	}
	val.SetNeuronIDs(set)

	plan := func(act, sil, ro string) []byte {
		return []byte(fmt.Sprintf(`{"schema_version":"1.0","dataset_id":"flywire_630","model_id":"shiu_lif_rust",
			"experiment_type":"compare_silencing","activation":[{"selector":%s,"rate_hz":50}],
			"silencing":[{"selector":%s}],"readout":[{"selector":%s}],"duration_ms":100,"repeats":1,"base_seed":42,"report_language":"en"}`, act, sil, ro))
	}
	// Real ids pass.
	if _, err := val.ValidateRawJSON(plan(`{"group_id":"sugar_grn"}`, `{"neuron_ids":["720575940616885538"]}`, `{"neuron_ids":["720575940660219265"]}`)); err != nil {
		t.Fatalf("real ids must validate: %v", err)
	}
	// Unknown ids in silencing and readout are reported per selector, deduplicated.
	_, err = val.ValidateRawJSON(plan(`{"group_id":"sugar_grn"}`,
		`{"neuron_ids":["720575940000000001","720575940616885538","720575940000000001"]}`,
		`{"neuron_ids":["720575940660219265","720575940999999999","720575940000000001"]}`))
	var unk *contracts.UnknownNeuronsError
	if !errors.As(err, &unk) {
		t.Fatalf("expected *UnknownNeuronsError, got %T %v", err, err)
	}
	if strings.Join(unk.IDs, ",") != "720575940000000001,720575940999999999" ||
		strings.Join(unk.Paths(), ",") != "silencing[0].selector.neuron_ids,readout[0].selector.neuron_ids" {
		t.Fatalf("unknown ids/paths wrong: %v %v", unk.IDs, unk.Paths())
	}
	if !strings.Contains(err.Error(), "2 neuron id(s) are not neurons of the flywire_630 connectome") ||
		!strings.Contains(err.Error(), "readout[0].selector.neuron_ids: 720575940999999999, 720575940000000001") {
		t.Fatalf("message must name the selectors and ids: %v", err)
	}

	// details list at most 20 ids.
	var many []string
	for i := 0; i < 25; i++ {
		many = append(many, fmt.Sprintf(`"7205759400000%05d"`, i))
	}
	_, err = val.ValidateRawJSON(plan(`{"neuron_ids":[`+strings.Join(many, ",")+`]}`, `{"group_id":"demo_silencing"}`, `{"group_id":"mn9"}`))
	if !errors.As(err, &unk) || len(unk.IDs) != 25 || len(unk.ReportedIDs()) != 20 || unk.ReportedIDs()[0] != "720575940000000000" {
		t.Fatalf("expected 25 unknown ids with 20 reported, got %v", err)
	}
}

func TestLoadNeuronIDsRejectsBrokenTables(t *testing.T) {
	write := func(t *testing.T, manifest, csv string) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "dataset_manifest.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if csv != "" {
			if err := os.WriteFile(filepath.Join(dir, "comp.csv"), []byte(csv), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	m := `{"files":{"completeness":{"filename":"comp.csv"}}}`
	cases := []struct{ name, manifest, csv, want string }{
		{"missing file", m, "", "comp.csv cannot be read"},
		{"no entry", `{"files":{}}`, "x", "no files.completeness entry"},
		{"malformed id", m, ",Completed\n720575940660219265,True\nabc,True\n", `line 3: "abc" is not a FlyWire root id`},
		{"duplicate", m, ",Completed\n720575940660219265,True\n720575940660219265,True\n", "line 3: root id 720575940660219265 appears twice"},
		{"header only", m, ",Completed\n", "has a header but no neurons"},
		{"row count", `{"files":{"completeness":{"filename":"comp.csv","rows":3}}}`, ",Completed\n720575940660219265,True\n", "has 1 neurons, the manifest says 3"},
		{"sha mismatch", `{"files":{"completeness":{"filename":"comp.csv","sha256":"00"}}}`, ",Completed\n720575940660219265,True\n", "the manifest expects 00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := contracts.LoadNeuronIDs(write(t, tc.manifest, tc.csv))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected an error containing %q, got %v", tc.want, err)
			}
		})
	}
	set, err := contracts.LoadNeuronIDs(write(t, m, ",Completed\n720575940660219265,True\n\n720575940645521262,False\n"))
	if err != nil || set.Len() != 2 || !set.Has("720575940645521262") {
		t.Fatalf("a well-formed table must load: %v", err)
	}
}
