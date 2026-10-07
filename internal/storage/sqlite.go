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
	ErrNotFound      = errors.New("resource not found")
	ErrConflict      = errors.New("resource conflict")
	ErrInvalidState  = errors.New("invalid state transition")
	ErrUsernameTaken = errors.New("username already taken")
)

// jobColumns is the column list every job SELECT uses (must match scanJob).
const jobColumns = `job_id, plan_id, plan_hash, status, stage, progress_pct, idempotency_key, error_code, error_message, artifacts_dir, created_at, started_at, finished_at, user_id, prompt, title`

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

	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY,
		username TEXT UNIQUE NOT NULL,
		display_name TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL
	);

	CREATE TABLE IF NOT EXISTS sessions (
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL,
		created_at TIMESTAMP NOT NULL,
		expires_at TIMESTAMP NOT NULL
	);

	-- Idempotency keys scoped per user: the same key from two users never collides.
	CREATE TABLE IF NOT EXISTS user_idempotency_keys (
		user_id INTEGER NOT NULL,
		key TEXT NOT NULL,
		job_id TEXT NOT NULL,
		plan_id TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL,
		PRIMARY KEY (user_id, key)
	);

	-- v3: the latest AI interpretation of a job (one row per job, replaced on regenerate).
	CREATE TABLE IF NOT EXISTS interpretations (
		job_id TEXT PRIMARY KEY,
		language TEXT,
		model TEXT,
		created_at TIMESTAMP,
		cost_usd REAL,
		duration_ms INTEGER,
		digest_json TEXT,
		result_json TEXT
	);

	-- v4: every finished claude -p call (planner or interpreter, success or failure) with the
	-- CLI's total_cost_usd, for the rolling 24 h AI budgets.
	CREATE TABLE IF NOT EXISTS llm_usage (
		id INTEGER PRIMARY KEY,
		user_id INTEGER,
		kind TEXT NOT NULL,
		cost_usd REAL NOT NULL,
		ok INTEGER NOT NULL,
		created_at TIMESTAMP NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_llm_usage_created ON llm_usage(created_at);
	CREATE INDEX IF NOT EXISTS idx_llm_usage_user_created ON llm_usage(user_id, created_at);

	-- v4: the persisted interpretation queue (results stay in interpretations).
	CREATE TABLE IF NOT EXISTS interpretation_requests (
		id INTEGER PRIMARY KEY,
		job_id TEXT NOT NULL,
		user_id INTEGER NOT NULL,
		language TEXT NOT NULL,
		regenerate INTEGER NOT NULL,
		status TEXT NOT NULL,
		error_code TEXT,
		error_message TEXT,
		queued_at TIMESTAMP NOT NULL,
		started_at TIMESTAMP,
		finished_at TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_interp_req_status ON interpretation_requests(status, id);
	CREATE INDEX IF NOT EXISTS idx_interp_req_job ON interpretation_requests(job_id, id);
	CREATE INDEX IF NOT EXISTS idx_interp_req_user_status ON interpretation_requests(user_id, status);

	CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
	CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);
	CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if err := s.migrateJobsColumns(); err != nil {
		return fmt.Errorf("migrate jobs table: %w", err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_jobs_user_created ON jobs(user_id, created_at)`); err != nil {
		return fmt.Errorf("create jobs(user_id) index: %w", err)
	}
	return nil
}

// migrateJobsColumns adds user_id, prompt and title to an existing jobs table, only when
// PRAGMA table_info shows they are missing. Any failure is returned (startup error).
func (s *Store) migrateJobsColumns() error {
	rows, err := s.db.Query(`PRAGMA table_info(jobs)`)
	if err != nil {
		return fmt.Errorf("PRAGMA table_info(jobs): %w", err)
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return fmt.Errorf("scan table_info(jobs): %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate table_info(jobs): %w", err)
	}
	rows.Close()

	for _, col := range []struct{ name, ddl string }{
		{"user_id", `ALTER TABLE jobs ADD COLUMN user_id INTEGER`},
		{"prompt", `ALTER TABLE jobs ADD COLUMN prompt TEXT`},
		{"title", `ALTER TABLE jobs ADD COLUMN title TEXT`},
	} {
		if have[col.name] {
			continue
		}
		if _, err := s.db.Exec(col.ddl); err != nil {
			return fmt.Errorf("%s: %w", col.ddl, err)
		}
	}
	return nil
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

// CreateJob inserts a job. When idempotencyKey is set the key is scoped to job.UserID:
// the same key and plan from the same user returns the existing job (isNew=false), the same
// key with a different plan is ErrConflict, and the same key from another user is unrelated.
func (s *Store) CreateJob(job *domain.Job, idempotencyKey *string) (*domain.Job, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if idempotencyKey != nil && *idempotencyKey != "" {
		if job.UserID == nil {
			return nil, false, errors.New("idempotency key requires a job owner (user_id)")
		}
		var existingJobID, existingPlanID string
		err := tx.QueryRow(
			`SELECT job_id, plan_id FROM user_idempotency_keys WHERE user_id = ? AND key = ?`,
			*job.UserID, *idempotencyKey,
		).Scan(&existingJobID, &existingPlanID)
		if err == nil {
			if existingPlanID != job.PlanID {
				return nil, false, ErrConflict // same key with different plan
			}
			existingJob, err := s.getJobTx(tx, existingJobID)
			if err != nil {
				return nil, false, err
			}
			return existingJob, false, nil // idempotent duplicate
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}

		_, err = tx.Exec(
			`INSERT INTO user_idempotency_keys (user_id, key, job_id, plan_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			*job.UserID, *idempotencyKey, job.JobID, job.PlanID, time.Now().UTC(),
		)
		if err != nil {
			return nil, false, err
		}
	}

	_, err = tx.Exec(
		`INSERT INTO jobs (job_id, plan_id, plan_hash, status, stage, progress_pct, idempotency_key, artifacts_dir, created_at, user_id, prompt, title)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.JobID, job.PlanID, job.PlanHash, string(job.Status), job.Stage, job.ProgressPct, idempotencyKey,
		job.ArtifactsDir, job.CreatedAt, job.UserID, job.Prompt, job.Title,
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
		`SELECT `+jobColumns+` FROM jobs WHERE job_id = ?`, jobID,
	)
	return scanJob(row)
}

func (s *Store) GetJob(jobID string) (*domain.Job, error) {
	row := s.db.QueryRow(
		`SELECT `+jobColumns+` FROM jobs WHERE job_id = ?`, jobID,
	)
	return scanJob(row)
}

// GetJobForUser returns the job only when it belongs to userID. A job owned by someone else
// or by nobody is ErrNotFound (existence is not leaked).
func (s *Store) GetJobForUser(jobID string, userID int64) (*domain.Job, error) {
	row := s.db.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE job_id = ? AND user_id = ?`, jobID, userID)
	return scanJob(row)
}

// ListJobsForUser returns the caller's jobs newest first plus the total matching count.
// status == "" means all statuses.
func (s *Store) ListJobsForUser(userID int64, status string, limit, offset int) ([]*domain.Job, int, error) {
	where := `WHERE user_id = ?`
	args := []interface{}{userID}
	if status != "" {
		where += ` AND status = ?`
		args = append(args, status)
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count jobs: %w", err)
	}

	rows, err := s.db.Query(
		`SELECT `+jobColumns+` FROM jobs `+where+` ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	jobs := make([]*domain.Job, 0)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, 0, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate jobs: %w", err)
	}
	return jobs, total, nil
}

// UserStats aggregates the user's jobs for GET /api/v1/me.
func (s *Store) UserStats(userID int64) (*domain.UserStats, error) {
	var st domain.UserStats
	var succeeded, failed, running sql.NullInt64
	err := s.db.QueryRow(
		`SELECT COUNT(*),
		        SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status IN ('queued', 'running', 'cancelling') THEN 1 ELSE 0 END)
		   FROM jobs WHERE user_id = ?`, userID,
	).Scan(&st.TotalJobs, &succeeded, &failed, &running)
	if err != nil {
		return nil, fmt.Errorf("aggregate user jobs: %w", err)
	}
	st.Succeeded = int(succeeded.Int64)
	st.Failed = int(failed.Int64)
	st.Running = int(running.Int64)

	var last sql.NullTime
	err = s.db.QueryRow(
		`SELECT created_at FROM jobs WHERE user_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, userID,
	).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("last job time: %w", err)
	}
	if last.Valid {
		t := last.Time
		st.LastJobAt = &t
	}
	return &st, nil
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
	res, err := s.db.Exec(
		`UPDATE jobs SET status = ?, error_code = ?, error_message = ?, progress_pct = 100.0, finished_at = ? WHERE job_id = ?`,
		string(status), errCode, errMsg, now, jobID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete job %s: rows affected unknown: %w", jobID, err)
	}
	if n == 0 {
		return fmt.Errorf("complete job %s: %w", jobID, ErrNotFound)
	}
	return nil
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
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recover orphan jobs: rows affected unknown: %w", err)
	}
	return int(affected), nil
}

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanJob(s rowScanner) (*domain.Job, error) {
	var j domain.Job
	var statusStr string
	var startedAt, finishedAt sql.NullTime
	var idempKey, errCode, errMsg, prompt, title sql.NullString
	var userID sql.NullInt64

	err := s.Scan(
		&j.JobID, &j.PlanID, &j.PlanHash, &statusStr, &j.Stage, &j.ProgressPct,
		&idempKey, &errCode, &errMsg, &j.ArtifactsDir, &j.CreatedAt, &startedAt, &finishedAt,
		&userID, &prompt, &title,
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
	if userID.Valid {
		uid := userID.Int64
		j.UserID = &uid
	}
	if prompt.Valid {
		j.Prompt = &prompt.String
	}
	if title.Valid {
		j.Title = &title.String
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
