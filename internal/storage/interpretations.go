package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Interpretation is a stored AI interpretation (contract v3 section 4). DigestJSON is the
// digest the model saw; ResultJSON is the post-processed interpretation plus evidence warnings.
type Interpretation struct {
	JobID      string
	Language   string
	Model      string
	CreatedAt  time.Time
	CostUSD    float64
	DurationMS int64
	DigestJSON string
	ResultJSON string
}

// SaveInterpretation stores the interpretation of a job, replacing the previous one.
func (s *Store) SaveInterpretation(in *Interpretation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO interpretations (job_id, language, model, created_at, cost_usd, duration_ms, digest_json, result_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(job_id) DO UPDATE SET language = excluded.language, model = excluded.model,
		   created_at = excluded.created_at, cost_usd = excluded.cost_usd, duration_ms = excluded.duration_ms,
		   digest_json = excluded.digest_json, result_json = excluded.result_json`,
		in.JobID, in.Language, in.Model, in.CreatedAt, in.CostUSD, in.DurationMS, in.DigestJSON, in.ResultJSON,
	)
	if err != nil {
		return fmt.Errorf("save interpretation of %s: %w", in.JobID, err)
	}
	return nil
}

// GetInterpretation returns the stored interpretation of a job or ErrNotFound. A row with a
// NULL column (written outside this code) is an error, not an empty interpretation.
func (s *Store) GetInterpretation(jobID string) (*Interpretation, error) {
	var (
		out                         Interpretation
		lang, model, digest, result sql.NullString
		created                     sql.NullTime
		cost                        sql.NullFloat64
		dur                         sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT job_id, language, model, created_at, cost_usd, duration_ms, digest_json, result_json
		   FROM interpretations WHERE job_id = ?`, jobID,
	).Scan(&out.JobID, &lang, &model, &created, &cost, &dur, &digest, &result)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("load interpretation of %s: %w", jobID, err)
	}
	var nulls []string
	for name, ok := range map[string]bool{"language": lang.Valid, "model": model.Valid, "created_at": created.Valid,
		"cost_usd": cost.Valid, "duration_ms": dur.Valid, "digest_json": digest.Valid, "result_json": result.Valid} {
		if !ok {
			nulls = append(nulls, name)
		}
	}
	if len(nulls) > 0 {
		return nil, fmt.Errorf("stored interpretation of %s has NULL %s", jobID, strings.Join(nulls, ", "))
	}
	out.Language, out.Model, out.CreatedAt = lang.String, model.String, created.Time
	out.CostUSD, out.DurationMS, out.DigestJSON, out.ResultJSON = cost.Float64, dur.Int64, digest.String, result.String
	return &out, nil
}

// InterpretedJobs returns which of jobIDs have a stored interpretation, mapped to its
// language ("" when the row's language is NULL).
func (s *Store) InterpretedJobs(jobIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(jobIDs))
	if len(jobIDs) == 0 {
		return out, nil
	}
	args := make([]interface{}, len(jobIDs))
	for i, id := range jobIDs {
		args[i] = id
	}
	rows, err := s.db.Query(
		`SELECT job_id, COALESCE(language, '') FROM interpretations WHERE job_id IN (?`+strings.Repeat(",?", len(jobIDs)-1)+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("query interpretations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, lang string
		if err := rows.Scan(&id, &lang); err != nil {
			return nil, fmt.Errorf("scan interpretations: %w", err)
		}
		out[id] = lang
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate interpretations: %w", err)
	}
	return out, nil
}
