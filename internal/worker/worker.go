package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/storage"
)

type Worker struct {
	cfg        *config.Config
	store      *storage.Store
	stopCh     chan struct{}
	activeCmd  *exec.Cmd
	activeJob  *domain.Job
	mu         sync.Mutex
}

func NewWorker(cfg *config.Config, store *storage.Store) *Worker {
	return &Worker{
		cfg:    cfg,
		store:  store,
		stopCh: make(chan struct{}),
	}
}

func (w *Worker) Start() {
	// First perform startup recovery of orphan jobs
	if recovered, err := w.store.RecoverOrphanJobs(); err == nil && recovered > 0 {
		log.Printf("[worker] Recovered %d interrupted jobs from previous shutdown.", recovered)
	}

	go w.loop()
}

func (w *Worker) Stop() {
	close(w.stopCh)
	w.mu.Lock()
	if w.activeCmd != nil && w.activeCmd.Process != nil {
		_ = w.activeCmd.Process.Kill()
	}
	w.mu.Unlock()
}

func (w *Worker) loop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.processNextJob()
		}
	}
}

func (w *Worker) processNextJob() {
	job, err := w.store.DequeueNextJob()
	if err != nil {
		log.Printf("[worker] Error dequeuing job: %v", err)
		return
	}
	if job == nil {
		return // Queue is empty
	}

	log.Printf("[worker] Picked up job %s (plan %s)", job.JobID, job.PlanID)
	w.executeJob(job)
}

func (w *Worker) executeJob(job *domain.Job) {
	plan, err := w.store.GetPlan(job.PlanID)
	if err != nil {
		w.failJob(job.JobID, "PLAN_NOT_FOUND", fmt.Sprintf("Failed to load plan: %v", err))
		return
	}

	resolved, err := w.store.GetResolvedPlan(job.PlanID)
	if err != nil {
		w.failJob(job.JobID, "RESOLVED_PLAN_NOT_FOUND", fmt.Sprintf("Failed to load resolved plan: %v", err))
		return
	}

	jobArtifactsDir := filepath.Join(w.cfg.ArtifactsDir, job.JobID)
	if err := os.MkdirAll(jobArtifactsDir, 0755); err != nil {
		w.failJob(job.JobID, "ARTIFACTS_DIR_ERROR", fmt.Sprintf("Failed to create artifacts dir: %v", err))
		return
	}

	// Persist plan.json and resolved_plan.json into artifact folder
	planBytes, _ := json.MarshalIndent(plan, "", "  ")
	_ = os.WriteFile(filepath.Join(jobArtifactsDir, "plan.json"), planBytes, 0644)

	resolvedBytes, _ := json.MarshalIndent(resolved, "", "  ")
	resolvedPlanPath := filepath.Join(jobArtifactsDir, "resolved_plan.json")
	_ = os.WriteFile(resolvedPlanPath, resolvedBytes, 0644)

	// Copy dataset manifest as manifest.json
	manifestSrc := filepath.Join(w.cfg.DataDir, "dataset_manifest.json")
	if manifestData, err := os.ReadFile(manifestSrc); err == nil {
		_ = os.WriteFile(filepath.Join(jobArtifactsDir, "manifest.json"), manifestData, 0644)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(w.cfg.MaxWallSeconds)*time.Second)
	defer cancel()

	cacheDir := filepath.Join(w.cfg.DataDir, "cache")
	cmd := exec.CommandContext(ctx, w.cfg.FlysimBin,
		"run",
		"--resolved-plan", resolvedPlanPath,
		"--output", jobArtifactsDir,
		"--cache-dir", cacheDir,
		"--manifest", manifestSrc,
	)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		w.failJob(job.JobID, "EXEC_PIPE_ERROR", err.Error())
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		w.failJob(job.JobID, "EXEC_PIPE_ERROR", err.Error())
		return
	}

	w.mu.Lock()
	w.activeCmd = cmd
	w.activeJob = job
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.activeCmd = nil
		w.activeJob = nil
		w.mu.Unlock()
	}()

	if err := cmd.Start(); err != nil {
		w.failJob(job.JobID, "EXEC_START_ERROR", err.Error())
		return
	}

	// Monitor stderr asynchronously
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			log.Printf("[flysim stderr] %s", scanner.Text())
		}
	}()

	// Monitor stdout JSON lines for progress
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Bytes()
			var event struct {
				Type        string                 `json:"type"`
				Stage       string                 `json:"stage"`
				ProgressPct float64                `json:"progress_pct"`
				Details     map[string]interface{} `json:"details"`
			}
			if err := json.Unmarshal(line, &event); err == nil && event.Type == "progress" {
				_ = w.store.UpdateJobProgress(job.JobID, event.Stage, event.ProgressPct)
			}
		}
	}()

	// Periodic cancellation & budget check
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				currentJob, err := w.store.GetJob(job.JobID)
				if err == nil && currentJob.Status == domain.StatusCancelling {
					log.Printf("[worker] Job %s requested cancellation, terminating process.", job.JobID)
					if cmd.Process != nil {
						_ = cmd.Process.Kill()
					}
					return
				}
			}
		}
	}()

	err = cmd.Wait()

	// Check final state in store in case it was cancelled
	currentJob, _ := w.store.GetJob(job.JobID)
	if currentJob != nil && (currentJob.Status == domain.StatusCancelling || currentJob.Status == domain.StatusCancelled) {
		_ = w.store.CompleteJob(job.JobID, domain.StatusCancelled, nil, nil)
		log.Printf("[worker] Job %s successfully cancelled.", job.JobID)
		return
	}

	if ctx.Err() == context.DeadlineExceeded {
		w.failJob(job.JobID, "TIMEOUT_LIMIT_EXCEEDED", fmt.Sprintf("Execution exceeded max wall time limit of %d seconds", w.cfg.MaxWallSeconds))
		return
	}

	if err != nil {
		w.failJob(job.JobID, "SIMULATION_FAILED", err.Error())
		return
	}

	// Verify expected outputs exist
	requiredFiles := []string{"spikes.parquet", "rates.csv", "summary.json", "report.md"}
	for _, req := range requiredFiles {
		if _, err := os.Stat(filepath.Join(jobArtifactsDir, req)); os.IsNotExist(err) {
			w.failJob(job.JobID, "OUTPUT_ARTIFACT_MISSING", fmt.Sprintf("Required output artifact missing: %s", req))
			return
		}
	}

	// Mark success
	_ = w.store.CompleteJob(job.JobID, domain.StatusSucceeded, nil, nil)
	log.Printf("[worker] Job %s completed successfully.", job.JobID)
}

func (w *Worker) failJob(jobID, code, msg string) {
	log.Printf("[worker] Job %s failed [%s]: %s", jobID, code, msg)
	_ = w.store.CompleteJob(jobID, domain.StatusFailed, &code, &msg)
}
