package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ad3002/flylab/internal/domain"
)

var (
	ErrNotFound       = errors.New("resource not found")
	ErrConflict       = errors.New("resource conflict")
	ErrInvalidState   = errors.New("invalid state transition")
)

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenStore(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}

	// Limit to reasonable connection count for SQLite
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.initSchema(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Store) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS plans (
		plan_id TEXT PRIMARY KEY,
		plan_hash TEXT NOT NULL,
		plan_json TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS resolved_plans (
		plan_id TEXT PRIMARY KEY,
		plan_hash TEXT NOT NULL,
		resolved_json TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS jobs (
		job_id TEXT PRIMARY KEY,
		plan_id TEXT NOT NULL,
		plan_hash TEXT NOT NULL,
		status TEXT NOT NULL,
		stage TEXT NOT NULL,
		progress_pct REAL NOT NULL,
		idempotency_key TEXT,
		error_code TEXT,
		error_message TEXT,
		artifacts_dir TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL,
		started_at TIMESTAMP,
		finished_at TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS idempotency_keys (
		key TEXT PRIMARY KEY,
		job_id TEXT NOT NULL,
		plan_id TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
	CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);
	`
	_, err := s.db.Exec(schema)
	return err
}

func (s *Store) SavePlan(plan *domain.ExperimentPlan, resolved *domain.ResolvedPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	planData, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	resolvedData, err := json.Marshal(resolved)
	if err != nil {
		return err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	_, err = tx.Exec(
		`INSERT OR REPLACE INTO plans (plan_id, plan_hash, plan_json, created_at) VALUES (?, ?, ?, ?)`,
		resolved.PlanID, resolved.PlanHash, string(planData), now,
	)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`INSERT OR REPLACE INTO resolved_plans (plan_id, plan_hash, resolved_json, created_at) VALUES (?, ?, ?, ?)`,
		resolved.PlanID, resolved.PlanHash, string(resolvedData), now,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) GetPlan(planID string) (*domain.ExperimentPlan, error) {
	row := s.db.QueryRow(`SELECT plan_json FROM plans WHERE plan_id = ?`, planID)
	var planJSON string
	if err := row.Scan(&planJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var plan domain.ExperimentPlan
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s *Store) GetResolvedPlan(planID string) (*domain.ResolvedPlan, error) {
	row := s.db.QueryRow(`SELECT resolved_json FROM resolved_plans WHERE plan_id = ?`, planID)
	var resolvedJSON string
	if err := row.Scan(&resolvedJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var resolved domain.ResolvedPlan
	if err := json.Unmarshal([]byte(resolvedJSON), &resolved); err != nil {
		return nil, err
	}
	return &resolved, nil
}

func (s *Store) CreateJob(job *domain.Job, idempotencyKey *string) (*domain.Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if idempotencyKey != nil && *idempotencyKey != "" {
		var existingJobID, existingPlanID string
		err := tx.QueryRow(`SELECT job_id, plan_id FROM idempotency_keys WHERE key = ?`, *idempotencyKey).Scan(&existingJobID, &existingPlanID)
		if err == nil {
			// Found idempotency key!
			if existingPlanID != job.PlanID {
				return nil, false, ErrConflict // same key with different plan
			}
			existingJob, err := s.getJobTx(tx, existingJobID)
			if err != nil {
				return nil, false, err
			}
			return existingJob, false, nil // return existing job (idempotent duplicate)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}

		// Insert new idempotency key
		_, err = tx.Exec(
			`INSERT INTO idempotency_keys (key, job_id, plan_id, created_at) VALUES (?, ?, ?, ?)`,
			*idempotencyKey, job.JobID, job.PlanID, time.Now().UTC(),
		)
		if err != nil {
			return nil, false, err
		}
	}

	_, err = tx.Exec(
		`INSERT INTO jobs (job_id, plan_id, plan_hash, status, stage, progress_pct, idempotency_key, artifacts_dir, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.JobID, job.PlanID, job.PlanHash, string(job.Status), job.Stage, job.ProgressPct, idempotencyKey, job.ArtifactsDir, job.CreatedAt,
	)
	if err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}

	return job, true, nil
}

func (s *Store) getJobTx(tx *sql.Tx, jobID string) (*domain.Job, error) {
	row := tx.QueryRow(
		`SELECT job_id, plan_id, plan_hash, status, stage, progress_pct, idempotency_key, error_code, error_message, artifacts_dir, created_at, started_at, finished_at
		 FROM jobs WHERE job_id = ?`, jobID,
	)
	return scanJob(row)
}

func (s *Store) GetJob(jobID string) (*domain.Job, error) {
	row := s.db.QueryRow(
		`SELECT job_id, plan_id, plan_hash, status, stage, progress_pct, idempotency_key, error_code, error_message, artifacts_dir, created_at, started_at, finished_at
		 FROM jobs WHERE job_id = ?`, jobID,
	)
	return scanJob(row)
}

func (s *Store) ListJobs(limit, offset int) ([]*domain.Job, error) {
	rows, err := s.db.Query(
		`SELECT job_id, plan_id, plan_hash, status, stage, progress_pct, idempotency_key, error_code, error_message, artifacts_dir, created_at, started_at, finished_at
		 FROM jobs ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*domain.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

func (s *Store) DequeueNextJob() (*domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var jobID string
	err = tx.QueryRow(`SELECT job_id FROM jobs WHERE status = 'queued' ORDER BY created_at ASC LIMIT 1`).Scan(&jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // no jobs in queue
		}
		return nil, err
	}

	now := time.Now().UTC()
	_, err = tx.Exec(
		`UPDATE jobs SET status = 'running', stage = 'loading', started_at = ? WHERE job_id = ?`,
		now, jobID,
	)
	if err != nil {
		return nil, err
	}

	job, err := s.getJobTx(tx, jobID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return job, nil
}

func (s *Store) UpdateJobProgress(jobID, stage string, progressPct float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`UPDATE jobs SET stage = ?, progress_pct = ? WHERE job_id = ? AND status = 'running'`,
		stage, progressPct, jobID,
	)
	return err
}

func (s *Store) CompleteJob(jobID string, status domain.JobStatus, errCode, errMsg *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE jobs SET status = ?, error_code = ?, error_message = ?, progress_pct = 100.0, finished_at = ? WHERE job_id = ?`,
		string(status), errCode, errMsg, now, jobID,
	)
	return err
}

func (s *Store) CancelJob(jobID string) (*domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	job, err := s.GetJob(jobID)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	switch job.Status {
	case domain.StatusQueued:
		_, err = s.db.Exec(`UPDATE jobs SET status = 'cancelled', finished_at = ? WHERE job_id = ?`, now, jobID)
		job.Status = domain.StatusCancelled
	case domain.StatusRunning:
		_, err = s.db.Exec(`UPDATE jobs SET status = 'cancelling' WHERE job_id = ?`, jobID)
		job.Status = domain.StatusCancelling
	case domain.StatusCancelling, domain.StatusCancelled:
		// Safe idempotent cancel
		return job, nil
	case domain.StatusSucceeded, domain.StatusFailed:
		return nil, ErrInvalidState // terminal states cannot be cancelled
	}

	return job, err
}

func (s *Store) RecoverOrphanJobs() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	res, err := s.db.Exec(
		`UPDATE jobs SET status = 'failed', error_code = 'WORKER_INTERRUPTED', error_message = 'Worker process terminated or crashed before completion', finished_at = ?
		 WHERE status IN ('running', 'cancelling')`, now,
	)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()
	return int(affected), nil
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanJob(s rowScanner) (*domain.Job, error) {
	var j domain.Job
	var statusStr string
	var startedAt, finishedAt sql.NullTime
	var idempKey, errCode, errMsg sql.NullString

	err := s.Scan(
		&j.JobID, &j.PlanID, &j.PlanHash, &statusStr, &j.Stage, &j.ProgressPct,
		&idempKey, &errCode, &errMsg, &j.ArtifactsDir, &j.CreatedAt, &startedAt, &finishedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	j.Status = domain.JobStatus(statusStr)
	if idempKey.Valid {
		j.IdempotencyKey = &idempKey.String
	}
	if errCode.Valid {
		j.ErrorCode = &errCode.String
	}
	if errMsg.Valid {
		j.ErrorMessage = &errMsg.String
	}
	if startedAt.Valid {
		j.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		j.FinishedAt = &finishedAt.Time
	}

	return &j, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
