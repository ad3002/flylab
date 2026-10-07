package worker_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/storage"
	"github.com/ad3002/flylab/internal/worker"

	_ "modernc.org/sqlite"
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
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
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

// A flysim that exits non-zero must fail the job with its stderr in error_message: "exit status 1"
// alone tells the user nothing (the reason used to go only to the server log).
func TestWorkerFailureCarriesFlysimStderr(t *testing.T) {
	wd, _ := os.Getwd()
	projectRoot := filepath.Dir(filepath.Dir(wd))
	tmpDir := t.TempDir()

	fake := filepath.Join(tmpDir, "fake_flysim.sh")
	script := "#!/bin/sh\necho '[flysim] Loading graph' >&2\necho 'Error: \"2 stimulated neuron id(s) are not in the connectome graph: 111, 222\"' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir:        filepath.Join(projectRoot, "data"),
		ArtifactsDir:   filepath.Join(tmpDir, "artifacts"),
		FlysimBin:      fake,
		MaxWallSeconds: 30,
	}
	store, err := storage.OpenStore(filepath.Join(tmpDir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg, err := contracts.LoadRegistry(filepath.Join(projectRoot, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	val, err := contracts.NewValidator(filepath.Join(projectRoot, "contracts"), reg)
	if err != nil {
		t.Fatal(err)
	}
	valRes, err := val.ValidatePlan(&domain.ExperimentPlan{
		SchemaVersion: "1.0", DatasetID: "flywire_630", ModelID: "shiu_lif_rust", ExperimentType: "single",
		DurationMs: 50, Repeats: 1, BaseSeed: 1, ReportLanguage: "en",
		Activation: []domain.ActivationSpec{{Selector: domain.Selector{GroupID: "sugar_grn"}, RateHz: 50}},
		Silencing:  []domain.SilencingSpec{},
		Readout:    []domain.ReadoutSpec{{Selector: domain.Selector{GroupID: "mn9"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePlan(valRes.Plan, valRes.ResolvedPlan); err != nil {
		t.Fatal(err)
	}
	jobID := "worker_test_stderr"
	if _, _, err := store.CreateJob(&domain.Job{
		JobID: jobID, PlanID: valRes.ResolvedPlan.PlanID, PlanHash: valRes.ResolvedPlan.PlanHash,
		Status: domain.StatusQueued, Stage: "queued", ArtifactsDir: filepath.Join(cfg.ArtifactsDir, jobID),
		CreatedAt: time.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}

	w := worker.NewWorker(cfg, store)
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()
	var final *domain.Job
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		j, err := store.GetJob(jobID)
		if err == nil && (j.Status == domain.StatusSucceeded || j.Status == domain.StatusFailed) {
			final = j
			break
		}
	}
	if final == nil {
		t.Fatalf("job did not finish")
	}
	if final.Status != domain.StatusFailed || final.ErrorCode == nil || *final.ErrorCode != "SIMULATION_FAILED" {
		t.Fatalf("expected failed SIMULATION_FAILED, got %s %v", final.Status, final.ErrorCode)
	}
	msg := ""
	if final.ErrorMessage != nil {
		msg = *final.ErrorMessage
	}
	if !strings.Contains(msg, "exit status 1") || !strings.Contains(msg, "not in the connectome graph: 111, 222") {
		t.Fatalf("error_message must carry the exit status and flysim's stderr, got %q", msg)
	}
}

// Start must return the orphan-recovery error: swallowing it left interrupted jobs 'running'
// forever, because the loop only picks up 'queued' jobs.
func TestWorkerStartReturnsRecoveryError(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := storage.OpenStore(filepath.Join(tmpDir, "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	w := worker.NewWorker(&config.Config{ArtifactsDir: tmpDir, MaxWallSeconds: 5}, store)
	err = w.Start()
	if err == nil {
		w.Stop()
		t.Fatalf("Start on an unusable store must fail")
	}
	if !strings.Contains(err.Error(), "recover jobs interrupted by the previous shutdown") || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("error must say recovery failed and why, got %q", err)
	}
}

// When the job's final status cannot be saved, the job would otherwise stay 'running' with no
// visible error. The worker must retry and then report the failure through LastError (which
// the API surfaces as /health 503 and capabilities.worker_error).
func TestWorkerSurfacesUnsavedFinalStatus(t *testing.T) {
	wd, _ := os.Getwd()
	projectRoot := filepath.Dir(filepath.Dir(wd))
	tmpDir := t.TempDir()

	fake := filepath.Join(tmpDir, "fake_flysim.sh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'boom' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir:        filepath.Join(projectRoot, "data"),
		ArtifactsDir:   filepath.Join(tmpDir, "artifacts"),
		FlysimBin:      fake,
		MaxWallSeconds: 30,
	}
	dbPath := filepath.Join(tmpDir, "w.db")
	store, err := storage.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg, err := contracts.LoadRegistry(filepath.Join(projectRoot, "registry"))
	if err != nil {
		t.Fatal(err)
	}
	val, err := contracts.NewValidator(filepath.Join(projectRoot, "contracts"), reg)
	if err != nil {
		t.Fatal(err)
	}
	valRes, err := val.ValidatePlan(&domain.ExperimentPlan{
		SchemaVersion: "1.0", DatasetID: "flywire_630", ModelID: "shiu_lif_rust", ExperimentType: "single",
		DurationMs: 50, Repeats: 1, BaseSeed: 1, ReportLanguage: "en",
		Activation: []domain.ActivationSpec{{Selector: domain.Selector{GroupID: "sugar_grn"}, RateHz: 50}},
		Silencing:  []domain.SilencingSpec{},
		Readout:    []domain.ReadoutSpec{{Selector: domain.Selector{GroupID: "mn9"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePlan(valRes.Plan, valRes.ResolvedPlan); err != nil {
		t.Fatal(err)
	}
	jobID := "worker_test_unsaved"
	if _, _, err := store.CreateJob(&domain.Job{
		JobID: jobID, PlanID: valRes.ResolvedPlan.PlanID, PlanHash: valRes.ResolvedPlan.PlanHash,
		Status: domain.StatusQueued, Stage: "queued", ArtifactsDir: filepath.Join(cfg.ArtifactsDir, jobID),
		CreatedAt: time.Now().UTC(),
	}, nil); err != nil {
		t.Fatal(err)
	}

	// Simulate a write failure that hits only the final status update (like SQLITE_BUSY from
	// a concurrent CLI write or a full disk): a trigger aborts any terminal-status UPDATE.
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER block_final BEFORE UPDATE OF status ON jobs
		WHEN NEW.status IN ('succeeded','failed','cancelled')
		BEGIN SELECT RAISE(ABORT, 'simulated disk I/O error'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	w := worker.NewWorker(cfg, store)
	w.PersistRetryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	if w.LastError() != nil {
		t.Fatalf("a fresh worker must be healthy, got %v", w.LastError())
	}
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()

	var lastErr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if lastErr = w.LastError(); lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatalf("an unsaved final status must be reported by LastError")
	}
	for _, want := range []string{jobID, "finished as failed", "could not be saved", "simulated disk I/O error"} {
		if !strings.Contains(lastErr.Error(), want) {
			t.Fatalf("LastError %q does not mention %q", lastErr, want)
		}
	}
	j, err := store.GetJob(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != domain.StatusRunning {
		t.Fatalf("the trigger must have kept the job running (test precondition), got %s", j.Status)
	}
	// The degraded state is sticky: a later healthy dequeue does not hide the stuck job.
	time.Sleep(700 * time.Millisecond)
	if w.LastError() == nil {
		t.Fatalf("the persist failure must stay visible until restart")
	}
}
