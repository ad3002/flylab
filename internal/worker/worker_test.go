package worker_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/storage"
	"github.com/ad3002/flylab/internal/worker"
)

func TestWorkerExecution(t *testing.T) {
	wd, _ := os.Getwd()
	projectRoot := filepath.Dir(filepath.Dir(wd))

	tmpDir, err := os.MkdirTemp("", "flylab_worker_test_*")
	if err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "worker_test.db")
	artifactsDir := filepath.Join(tmpDir, "artifacts")
	_ = os.MkdirAll(artifactsDir, 0755)

	cfg := &config.Config{
		DataDir:        filepath.Join(projectRoot, "data"),
		ArtifactsDir:   artifactsDir,
		FlysimBin:      filepath.Join(projectRoot, "bin", "flysim"),
		MaxWallSeconds: 30,
	}

	store, err := storage.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("Failed to open store: %v", err)
	}
	defer store.Close()

	reg, err := contracts.LoadRegistry(filepath.Join(projectRoot, "registry"))
	if err != nil {
		t.Fatalf("Failed to load registry: %v", err)
	}

	val, err := contracts.NewValidator(filepath.Join(projectRoot, "contracts"), reg)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	// Validate a single test plan
	plan := &domain.ExperimentPlan{
		SchemaVersion:  "1.0",
		DatasetID:      "flywire_630",
		ModelID:        "shiu_lif_rust",
		ExperimentType: "single",
		DurationMs:     50.0, // Short 50ms test
		Repeats:        1,
		BaseSeed:       42,
		ReportLanguage: "en",
		Activation: []domain.ActivationSpec{
			{Selector: domain.Selector{GroupID: "sugar_grn"}, RateHz: 50.0},
		},
		Silencing: []domain.SilencingSpec{},
		Readout: []domain.ReadoutSpec{
			{Selector: domain.Selector{GroupID: "mn9"}},
		},
	}

	valRes, err := val.ValidatePlan(plan)
	if err != nil {
		t.Fatalf("Failed to validate plan: %v", err)
	}

	if err := store.SavePlan(valRes.Plan, valRes.ResolvedPlan); err != nil {
		t.Fatalf("Failed to save plan: %v", err)
	}

	jobID := "worker_test_job_1"
	job := &domain.Job{
		JobID:        jobID,
		PlanID:       valRes.ResolvedPlan.PlanID,
		PlanHash:     valRes.ResolvedPlan.PlanHash,
		Status:       domain.StatusQueued,
		Stage:        "queued",
		ProgressPct:  0,
		ArtifactsDir: filepath.Join(artifactsDir, jobID),
		CreatedAt:    time.Now().UTC(),
	}

	if _, _, err := store.CreateJob(job, nil); err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	w := worker.NewWorker(cfg, store)
	w.Start()
	defer w.Stop()

	// Wait up to 10 seconds for worker to process job
	deadline := time.Now().Add(10 * time.Second)
	var finalJob *domain.Job
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		j, err := store.GetJob(jobID)
		if err == nil && (j.Status == domain.StatusSucceeded || j.Status == domain.StatusFailed) {
			finalJob = j
			break
		}
	}

	if finalJob == nil {
		t.Fatalf("Job did not finish before deadline")
	}

	if finalJob.Status != domain.StatusSucceeded {
		t.Fatalf("Expected job status succeeded, got %s (error: %v)", finalJob.Status, finalJob.ErrorMessage)
	}

	// Verify artifacts were created
	requiredArtifacts := []string{"spikes.parquet", "rates.csv", "summary.json", "report.md", "plan.json", "resolved_plan.json"}
	for _, art := range requiredArtifacts {
		path := filepath.Join(finalJob.ArtifactsDir, art)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Fatalf("Expected artifact %s to exist at %s", art, path)
		}
	}
}
