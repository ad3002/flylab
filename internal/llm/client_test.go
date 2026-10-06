package llm_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/llm"
)

func TestLLMParser(t *testing.T) {
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

	cfg := &config.Config{
		OllamaURL:   "http://127.0.0.1:9999", // Unreachable port to test fallback
		OllamaModel: "qwen3:8b",
	}

	client := llm.NewClient(cfg, val, reg)
	ctx := context.Background()

	// 1. Single stimulus prompt
	res1, err := client.ParsePrompt(ctx, "Stimulate sugar_grn at 50 Hz for 100 ms and readout mn9", "flywire_630", "en")
	if err != nil {
		t.Fatalf("Failed to parse prompt 1: %v", err)
	}
	if res1.Status != llm.StatusReady {
		t.Fatalf("Expected status ready, got %s: %s", res1.Status, res1.Message)
	}
	if res1.Plan.ExperimentType != "single" {
		t.Fatalf("Expected experiment_type single, got %s", res1.Plan.ExperimentType)
	}

	// 2. Compare silencing prompt
	res2, err := client.ParsePrompt(ctx, "Compare sugar_grn 50 Hz with and without demo_silencing for 100 ms, readout mn9", "flywire_630", "en")
	if err != nil {
		t.Fatalf("Failed to parse prompt 2: %v", err)
	}
	if res2.Status != llm.StatusReady {
		t.Fatalf("Expected status ready, got %s: %s", res2.Status, res2.Message)
	}
	if res2.Plan.ExperimentType != "compare_silencing" {
		t.Fatalf("Expected experiment_type compare_silencing, got %s", res2.Plan.ExperimentType)
	}
	if len(res2.Plan.Silencing) == 0 {
		t.Fatalf("Expected silencing target populated")
	}

	// 3. Ambiguous prompt -> needs_input
	res3, err := client.ParsePrompt(ctx, "Turn off inhibitory neurons and see what happens", "flywire_630", "en")
	if err != nil {
		t.Fatalf("Failed to parse prompt 3: %v", err)
	}
	if res3.Status != llm.StatusNeedsInput {
		t.Fatalf("Expected status needs_input, got %s", res3.Status)
	}

	// 4. Unsupported behavioral prompt -> unsupported
	res4, err := client.ParsePrompt(ctx, "Show me how the fly will walk after removing these neurons", "flywire_630", "en")
	if err != nil {
		t.Fatalf("Failed to parse prompt 4: %v", err)
	}
	if res4.Status != llm.StatusUnsupported {
		t.Fatalf("Expected status unsupported, got %s", res4.Status)
	}
}
