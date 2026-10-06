package storage_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/storage"
)

func TestStorageWorkflow(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "flylab_test_db_*")
	if err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "test.db")
	store, err := storage.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("Failed to open store: %v", err)
	}
	defer store.Close()

	// 1. Save and Get Plan
	plan := &domain.ExperimentPlan{
		SchemaVersion:  "1.0",
		DatasetID:      "flywire_630",
		ModelID:        "shiu_lif_rust",
		ExperimentType: "single",
		DurationMs:     100,
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
	resolved := &domain.ResolvedPlan{
		SchemaVersion:  "1.0",
		PlanID:         "test_plan_1",
		PlanHash:       "hash123",
		DatasetID:      "flywire_630",
		ModelID:        "shiu_lif_rust",
		ExperimentType: "single",
		DurationMs:     100,
		Repeats:        1,
		BaseSeed:       42,
		ReportLanguage: "en",
	}

	if err := store.SavePlan(plan, resolved); err != nil {
		t.Fatalf("Failed to save plan: %v", err)
	}

	savedPlan, err := store.GetPlan("test_plan_1")
	if err != nil || savedPlan == nil {
		t.Fatalf("Failed to get plan: %v", err)
	}
	if savedPlan.DatasetID != "flywire_630" {
		t.Fatalf("Expected dataset flywire_630, got %s", savedPlan.DatasetID)
	}

	// 2. Create Job with Idempotency Key
	idempKey := "key-12345"
	job1 := &domain.Job{
		JobID:          "job_1",
		PlanID:         "test_plan_1",
		PlanHash:       "hash123",
		Status:         domain.StatusQueued,
		Stage:          "queued",
		ProgressPct:    0,
		IdempotencyKey: &idempKey,
		ArtifactsDir:   filepath.Join(tmpDir, "artifacts", "job_1"),
		CreatedAt:      time.Now().UTC(),
	}

	createdJob1, isNew, err := store.CreateJob(job1, &idempKey)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}
	if !isNew || createdJob1.JobID != "job_1" {
		t.Fatalf("Expected new job_1, got %v (isNew: %v)", createdJob1, isNew)
	}

	// 3. Repeat Create Job with SAME Idempotency Key and Plan -> Should return existing job
	createdJobDuplicate, isNew2, err := store.CreateJob(job1, &idempKey)
	if err != nil {
		t.Fatalf("Duplicate idempotency create failed: %v", err)
	}
	if isNew2 || createdJobDuplicate.JobID != "job_1" {
		t.Fatalf("Expected existing job_1 returned, got isNew=%v, job=%v", isNew2, createdJobDuplicate)
	}

	// 4. Repeat Create Job with SAME Idempotency Key but DIFFERENT Plan -> Conflict
	jobConflict := &domain.Job{
		JobID:          "job_2",
		PlanID:         "test_plan_2",
		PlanHash:       "hash999",
		Status:         domain.StatusQueued,
		Stage:          "queued",
		ProgressPct:    0,
		IdempotencyKey: &idempKey,
		ArtifactsDir:   filepath.Join(tmpDir, "artifacts", "job_2"),
		CreatedAt:      time.Now().UTC(),
	}
	_, _, err = store.CreateJob(jobConflict, &idempKey)
	if err != storage.ErrConflict {
		t.Fatalf("Expected ErrConflict for mismatched plan with same idempotency key, got: %v", err)
	}

	// 5. Dequeue Next Job
	dequeued, err := store.DequeueNextJob()
	if err != nil {
		t.Fatalf("Failed to dequeue job: %v", err)
	}
	if dequeued == nil || dequeued.JobID != "job_1" {
		t.Fatalf("Expected job_1 dequeued, got %v", dequeued)
	}
	if dequeued.Status != domain.StatusRunning {
		t.Fatalf("Expected status running after dequeue, got %s", dequeued.Status)
	}

	// 6. Update progress and complete
	if err := store.UpdateJobProgress("job_1", "simulating", 50.0); err != nil {
		t.Fatalf("Failed to update progress: %v", err)
	}
	if err := store.CompleteJob("job_1", domain.StatusSucceeded, nil, nil); err != nil {
		t.Fatalf("Failed to complete job: %v", err)
	}

	finishedJob, err := store.GetJob("job_1")
	if err != nil {
		t.Fatalf("Failed to get completed job: %v", err)
	}
	if finishedJob.Status != domain.StatusSucceeded {
		t.Fatalf("Expected job status succeeded, got %s", finishedJob.Status)
	}
}
