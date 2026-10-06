package contracts_test

import (
	"os"
	"path/filepath"
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
