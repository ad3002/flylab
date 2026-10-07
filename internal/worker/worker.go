package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/storage"
)

// How much of a failing flysim's stderr is copied into the job's error_message.
const (
	stderrTailLines = 20
	stderrTailBytes = 1500
)

type Worker struct {
	cfg       *config.Config
	store     *storage.Store
	stopCh    chan struct{}
	activeCmd *exec.Cmd
	activeJob *domain.Job
	mu        sync.Mutex

	// persistErr is sticky: a job whose final status could not be saved stays 'running' in
	// the DB until a restart's RecoverOrphanJobs marks it failed, so the degraded state is
	// reported (via LastError -> /health 503, capabilities.worker_error, UI banner) until then.
	// dequeueErr is cleared by the next successful dequeue.
	statusMu   sync.Mutex
	persistErr error
	dequeueErr error

	// PersistRetryDelays are the waits between attempts to save a job's final status.
	PersistRetryDelays []time.Duration
}

func NewWorker(cfg *config.Config, store *storage.Store) *Worker {
	return &Worker{
		cfg:                cfg,
		store:              store,
		stopCh:             make(chan struct{}),
		PersistRetryDelays: []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second},
	}
}

// Start recovers jobs orphaned by a previous process and starts the queue loop. A failed
// recovery is returned (and is fatal at startup): otherwise the orphaned jobs would stay
// 'running' forever, since the loop only picks up 'queued' jobs.
func (w *Worker) Start() error {
	recovered, err := w.store.RecoverOrphanJobs()
	if err != nil {
		return fmt.Errorf("recover jobs interrupted by the previous shutdown: %w", err)
	}
	if recovered > 0 {
		log.Printf("[worker] Recovered %d interrupted jobs from previous shutdown.", recovered)
	}

	go w.loop()
	return nil
}

// LastError is nil while the worker is healthy, else the failure the operator and users must
// see (an unsaved final job status, or the queue being unreadable).
func (w *Worker) LastError() error {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	return errors.Join(w.persistErr, w.dequeueErr)
}

// completeJob saves a job's final status, retrying briefly. When every attempt fails the
// worker enters the degraded state reported by LastError.
func (w *Worker) completeJob(jobID string, status domain.JobStatus, code, msg *string) error {
	var err error
	for attempt := 0; ; attempt++ {
		if err = w.store.CompleteJob(jobID, status, code, msg); err == nil {
			return nil
		}
		if attempt >= len(w.PersistRetryDelays) {
			break
		}
		log.Printf("[worker] saving final status %s of job %s failed (attempt %d): %v", status, jobID, attempt+1, err)
		time.Sleep(w.PersistRetryDelays[attempt])
	}
	perr := fmt.Errorf("job %s finished as %s but its status could not be saved (it will show as running until the service restarts): %w",
		jobID, status, err)
	log.Printf("[worker] %v", perr)
	w.statusMu.Lock()
	w.persistErr = errors.Join(w.persistErr, perr)
	w.statusMu.Unlock()
	return perr
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
	w.statusMu.Lock()
	if err != nil {
		w.dequeueErr = fmt.Errorf("the job queue cannot be read, new runs will not start: %w", err)
	} else {
		w.dequeueErr = nil
	}
	w.statusMu.Unlock()
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

	// Persist plan.json, resolved_plan.json and manifest.json into the artifact folder. A
	// failed write fails the job: an export without them would not be reproducible.
	resolvedPlanPath := filepath.Join(jobArtifactsDir, "resolved_plan.json")
	manifestSrc := filepath.Join(w.cfg.DataDir, "dataset_manifest.json")
	manifestData, err := os.ReadFile(manifestSrc)
	if err != nil {
		w.failJob(job.JobID, "DATASET_MANIFEST_ERROR", fmt.Sprintf("Failed to read dataset manifest: %v", err))
		return
	}
	for _, f := range []struct {
		path string
		v    interface{}
		raw  []byte
	}{
		{filepath.Join(jobArtifactsDir, "plan.json"), plan, nil},
		{resolvedPlanPath, resolved, nil},
		{filepath.Join(jobArtifactsDir, "manifest.json"), nil, manifestData},
	} {
		data := f.raw
		if data == nil {
			data, err = json.MarshalIndent(f.v, "", "  ")
			if err != nil {
				w.failJob(job.JobID, "ARTIFACT_WRITE_ERROR", fmt.Sprintf("Failed to encode %s: %v", filepath.Base(f.path), err))
				return
			}
		}
		if err := os.WriteFile(f.path, data, 0644); err != nil {
			w.failJob(job.JobID, "ARTIFACT_WRITE_ERROR", fmt.Sprintf("Failed to write %s: %v", filepath.Base(f.path), err))
			return
		}
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

	// Both pipes must be drained before cmd.Wait (os/exec requirement). The last stderr lines
	// are kept so a failing flysim's reason reaches error_message, not only the server log.
	var pipes sync.WaitGroup
	var stderrTail []string
	pipes.Add(2)
	go func() {
		defer pipes.Done()
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			log.Printf("[flysim stderr] %s", line)
			stderrTail = append(stderrTail, line)
			if len(stderrTail) > stderrTailLines {
				stderrTail = stderrTail[1:]
			}
		}
		if err := scanner.Err(); err != nil {
			stderrTail = append(stderrTail, "(reading flysim stderr failed: "+err.Error()+")")
		}
	}()

	// Monitor stdout JSON lines for progress
	go func() {
		defer pipes.Done()
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

	pipes.Wait()
	err = cmd.Wait()

	// Check final state in store in case it was cancelled
	currentJob, _ := w.store.GetJob(job.JobID)
	if currentJob != nil && (currentJob.Status == domain.StatusCancelling || currentJob.Status == domain.StatusCancelled) {
		if err := w.completeJob(job.JobID, domain.StatusCancelled, nil, nil); err != nil {
			return
		}
		log.Printf("[worker] Job %s successfully cancelled.", job.JobID)
		return
	}

	if ctx.Err() == context.DeadlineExceeded {
		w.failJob(job.JobID, "TIMEOUT_LIMIT_EXCEEDED", fmt.Sprintf("Execution exceeded max wall time limit of %d seconds", w.cfg.MaxWallSeconds))
		return
	}

	if err != nil {
		msg := err.Error()
		if tail := strings.TrimSpace(strings.Join(stderrTail, "\n")); tail != "" {
			if len(tail) > stderrTailBytes {
				tail = "…" + tail[len(tail)-stderrTailBytes:]
			}
			msg += ": " + tail
		}
		w.failJob(job.JobID, "SIMULATION_FAILED", msg)
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
	if err := w.completeJob(job.JobID, domain.StatusSucceeded, nil, nil); err != nil {
		return
	}
	log.Printf("[worker] Job %s completed successfully.", job.JobID)
}

// failJob records a job failure; a failure to save it is reported through LastError.
func (w *Worker) failJob(jobID, code, msg string) error {
	log.Printf("[worker] Job %s failed [%s]: %s", jobID, code, msg)
	return w.completeJob(jobID, domain.StatusFailed, &code, &msg)
}
