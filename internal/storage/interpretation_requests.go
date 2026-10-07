package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Interpretation request statuses (contract v4 section 4).
const (
	RequestQueued    = "queued"
	RequestRunning   = "running"
	RequestSucceeded = "succeeded"
	RequestFailed    = "failed"
)

// InterpretationRequest is one row of the persisted interpretation queue.
type InterpretationRequest struct {
	ID           int64
	JobID        string
	UserID       int64
	Language     string
	Regenerate   bool
	Status       string
	ErrorCode    *string
	ErrorMessage *string
	QueuedAt     time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// Active reports whether the request is queued or running.
func (r *InterpretationRequest) Active() bool {
	return r.Status == RequestQueued || r.Status == RequestRunning
}

const requestColumns = `id, job_id, user_id, language, regenerate, status, error_code, error_message, queued_at, started_at, finished_at`

func scanRequest(sc rowScanner) (*InterpretationRequest, error) {
	var (
		r                 InterpretationRequest
		regen             int
		code, msg         sql.NullString
		started, finished sql.NullTime
	)
	if err := sc.Scan(&r.ID, &r.JobID, &r.UserID, &r.Language, &regen, &r.Status, &code, &msg, &r.QueuedAt, &started, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Regenerate = regen != 0
	if code.Valid {
		r.ErrorCode = &code.String
	}
	if msg.Valid {
		r.ErrorMessage = &msg.String
	}
	if started.Valid {
		t := started.Time
		r.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time
		r.FinishedAt = &t
	}
	switch r.Status {
	case RequestQueued, RequestRunning, RequestSucceeded, RequestFailed:
	default:
		return nil, fmt.Errorf("interpretation request %d has unknown status %q", r.ID, r.Status)
	}
	return &r, nil
}

// InsertInterpretationRequest appends a queued request and returns it with its id.
func (s *Store) InsertInterpretationRequest(jobID string, userID int64, language string, regenerate bool, now time.Time) (*InterpretationRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	regen := 0
	if regenerate {
		regen = 1
	}
	now = now.UTC()
	res, err := s.db.Exec(
		`INSERT INTO interpretation_requests (job_id, user_id, language, regenerate, status, queued_at) VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, userID, language, regen, RequestQueued, now)
	if err != nil {
		return nil, fmt.Errorf("enqueue interpretation of %s: %w", jobID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("enqueue interpretation of %s: id unknown: %w", jobID, err)
	}
	return &InterpretationRequest{ID: id, JobID: jobID, UserID: userID, Language: language, Regenerate: regenerate,
		Status: RequestQueued, QueuedAt: now}, nil
}

// GetInterpretationRequest returns one request or ErrNotFound.
func (s *Store) GetInterpretationRequest(id int64) (*InterpretationRequest, error) {
	return scanRequest(s.db.QueryRow(`SELECT `+requestColumns+` FROM interpretation_requests WHERE id = ?`, id))
}

// ActiveInterpretationRequestForUser returns the user's queued or running request, or nil.
func (s *Store) ActiveInterpretationRequestForUser(userID int64) (*InterpretationRequest, error) {
	r, err := scanRequest(s.db.QueryRow(
		`SELECT `+requestColumns+` FROM interpretation_requests WHERE user_id = ? AND status IN ('queued', 'running') ORDER BY id LIMIT 1`, userID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up the active interpretation of user %d: %w", userID, err)
	}
	return r, nil
}

// LatestInterpretationRequest returns the job's newest request, or nil when it has none.
func (s *Store) LatestInterpretationRequest(jobID string) (*InterpretationRequest, error) {
	r, err := scanRequest(s.db.QueryRow(
		`SELECT `+requestColumns+` FROM interpretation_requests WHERE job_id = ? ORDER BY id DESC LIMIT 1`, jobID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up the interpretation requests of %s: %w", jobID, err)
	}
	return r, nil
}

// InterpretationQueueCounts returns how many requests are queued and running.
func (s *Store) InterpretationQueueCounts() (queued, running int, err error) {
	var q, r sql.NullInt64
	err = s.db.QueryRow(
		`SELECT SUM(CASE WHEN status = 'queued' THEN 1 ELSE 0 END), SUM(CASE WHEN status = 'running' THEN 1 ELSE 0 END)
		   FROM interpretation_requests WHERE status IN ('queued', 'running')`).Scan(&q, &r)
	if err != nil {
		return 0, 0, fmt.Errorf("count the interpretation queue: %w", err)
	}
	return int(q.Int64), int(r.Int64), nil
}

// RequestWithPosition re-reads a request and its queue position in one statement, so the
// status and the position always come from the same snapshot (a worker may claim the request
// at any moment).
func (s *Store) RequestWithPosition(id int64) (*InterpretationRequest, int, error) {
	var pos int
	row := s.db.QueryRow(`SELECT `+requestColumns+`,
		(SELECT COUNT(*) FROM interpretation_requests q WHERE q.status = 'queued' AND q.id <= r.id AND r.status = 'queued')
		FROM interpretation_requests r WHERE r.id = ?`, id)
	r, err := scanRequest(scannerWithExtra{row, &pos})
	if err != nil {
		return nil, 0, fmt.Errorf("read interpretation request %d: %w", id, err)
	}
	return r, pos, nil
}

// scannerWithExtra appends one destination to a row scan.
type scannerWithExtra struct {
	row   rowScanner
	extra interface{}
}

func (s scannerWithExtra) Scan(dest ...interface{}) error {
	return s.row.Scan(append(dest, s.extra)...)
}

// QueuePosition is the 1-based position of a queued request among queued requests (oldest
// first), or 0 when it is not queued.
func (s *Store) QueuePosition(r *InterpretationRequest) (int, error) {
	if r.Status != RequestQueued {
		return 0, nil
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM interpretation_requests WHERE status = 'queued' AND id <= ?`, r.ID).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue position of request %d: %w", r.ID, err)
	}
	return n, nil
}

// ClaimNextInterpretationRequest marks the oldest queued request running and returns it (nil
// when the queue is empty).
func (s *Store) ClaimNextInterpretationRequest(now time.Time) (*InterpretationRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	r, err := scanRequest(tx.QueryRow(`SELECT ` + requestColumns + ` FROM interpretation_requests WHERE status = 'queued' ORDER BY id LIMIT 1`))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	if _, err := tx.Exec(`UPDATE interpretation_requests SET status = 'running', started_at = ? WHERE id = ?`, now, r.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	r.Status, r.StartedAt = RequestRunning, &now
	return r, nil
}

// FailInterpretationRequest marks a request failed with a code and a user-facing message.
func (s *Store) FailInterpretationRequest(id int64, code, message string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE interpretation_requests SET status = 'failed', error_code = ?, error_message = ?, finished_at = ? WHERE id = ?`,
		code, message, now.UTC(), id)
	if err != nil {
		return fmt.Errorf("mark interpretation request %d failed: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("mark interpretation request %d failed: no row updated (%v)", id, err)
	}
	return nil
}

// CompleteInterpretationRequest stores the interpretation (replacing the job's previous one)
// and marks the request succeeded, in one transaction.
func (s *Store) CompleteInterpretationRequest(id int64, in *Interpretation, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(upsertInterpretationSQL,
		in.JobID, in.Language, in.Model, in.CreatedAt, in.CostUSD, in.DurationMS, in.DigestJSON, in.ResultJSON); err != nil {
		return fmt.Errorf("save interpretation of %s: %w", in.JobID, err)
	}
	res, err := tx.Exec(`UPDATE interpretation_requests SET status = 'succeeded', error_code = NULL, error_message = NULL, finished_at = ? WHERE id = ?`,
		now.UTC(), id)
	if err != nil {
		return fmt.Errorf("mark interpretation request %d succeeded: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return fmt.Errorf("mark interpretation request %d succeeded: no row updated (%v)", id, err)
	}
	return tx.Commit()
}

// FailInterruptedInterpretationRequests marks requests left running by a previous process as
// failed WORKER_INTERRUPTED (the user can retry; nothing is re-run automatically).
func (s *Store) FailInterruptedInterpretationRequests(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE interpretation_requests SET status = 'failed', error_code = 'WORKER_INTERRUPTED',
		        error_message = 'The server restarted while this interpretation was running; request it again.', finished_at = ?
		  WHERE status = 'running'`, now.UTC())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("interrupted interpretations: rows affected unknown: %w", err)
	}
	return int(n), nil
}

// JobInterpretationInfo is what the history list needs to compute interpretation_state.
type JobInterpretationInfo struct {
	// Latest is the job's newest request (nil when it has none).
	Latest *InterpretationRequest
	// HasResult: an interpretations row exists; ResultAt is its created_at (nil if NULL).
	HasResult bool
	ResultAt  *time.Time
	Language  string
}

// InterpretationInfos returns, for each of jobIDs that has a request or a result, its latest
// request and whether (and when) a result is stored.
func (s *Store) InterpretationInfos(jobIDs []string) (map[string]*JobInterpretationInfo, error) {
	out := map[string]*JobInterpretationInfo{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	args := make([]interface{}, len(jobIDs))
	for i, id := range jobIDs {
		args[i] = id
	}
	in := `(?` + strings.Repeat(",?", len(jobIDs)-1) + `)`
	get := func(id string) *JobInterpretationInfo {
		if out[id] == nil {
			out[id] = &JobInterpretationInfo{}
		}
		return out[id]
	}

	rows, err := s.db.Query(`SELECT `+requestColumns+` FROM interpretation_requests
		WHERE id IN (SELECT MAX(id) FROM interpretation_requests WHERE job_id IN `+in+` GROUP BY job_id)`, args...)
	if err != nil {
		return nil, fmt.Errorf("query interpretation requests: %w", err)
	}
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan interpretation requests: %w", err)
		}
		get(r.JobID).Latest = r
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate interpretation requests: %w", err)
	}
	rows.Close()

	rows, err = s.db.Query(`SELECT job_id, COALESCE(language, ''), created_at FROM interpretations WHERE job_id IN `+in, args...)
	if err != nil {
		return nil, fmt.Errorf("query interpretations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, lang string
			created  sql.NullTime
		)
		if err := rows.Scan(&id, &lang, &created); err != nil {
			return nil, fmt.Errorf("scan interpretations: %w", err)
		}
		info := get(id)
		info.HasResult, info.Language = true, lang
		if created.Valid {
			t := created.Time
			info.ResultAt = &t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate interpretations: %w", err)
	}
	return out, nil
}

// Interpretation states (GET .../interpretation state, HistoryJob.interpretation_state).
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StateFailed  = "failed"
	StateReady   = "ready"
)

// InterpretationState is "queued"/"running" while the latest request is active, "failed" when
// the latest request failed after the latest result (or there is no result), "ready" when a
// result exists, and "" when there is neither (null in JSON).
func InterpretationState(latest *InterpretationRequest, hasResult bool, resultAt *time.Time) string {
	if latest != nil {
		switch latest.Status {
		case RequestQueued:
			return StateQueued
		case RequestRunning:
			return StateRunning
		case RequestFailed:
			if !hasResult || resultAt == nil || latest.FinishedAt == nil || latest.FinishedAt.After(*resultAt) {
				return StateFailed
			}
		}
	}
	if hasResult {
		return StateReady
	}
	return ""
}
