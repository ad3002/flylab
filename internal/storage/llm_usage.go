package storage

import (
	"database/sql"
	"fmt"
	"time"
)

// LLM usage kinds (llm_usage.kind).
const (
	UsageKindPlanner     = "planner"
	UsageKindInterpreter = "interpreter"
)

// LLMUsageEntry is one recorded claude -p call (contract v4 section 3).
type LLMUsageEntry struct {
	UserID    *int64
	Kind      string
	CostUSD   float64
	OK        bool
	CreatedAt time.Time
}

// RecordLLMUsage stores one finished claude -p call. The caller must report a failure to the
// user (it is the budget's only source of truth), never just log it.
func (s *Store) RecordLLMUsage(userID *int64, kind string, costUSD float64, ok bool, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	okInt := 0
	if ok {
		okInt = 1
	}
	if _, err := s.db.Exec(
		`INSERT INTO llm_usage (user_id, kind, cost_usd, ok, created_at) VALUES (?, ?, ?, ?, ?)`,
		userID, kind, costUSD, okInt, at.UTC(),
	); err != nil {
		return fmt.Errorf("record AI usage (%s, $%.4f): %w", kind, costUSD, err)
	}
	return nil
}

// LLMUsageSince returns the calls recorded after since, oldest first; userID nil means every
// account (the global budget). The time filter is re-applied on the parsed timestamps, so the
// result never depends on how the driver formats them.
func (s *Store) LLMUsageSince(since time.Time, userID *int64) ([]LLMUsageEntry, error) {
	since = since.UTC()
	// Query a slightly wider window and filter exactly below.
	lower := since.Add(-time.Minute)
	var (
		rows *sql.Rows
		err  error
	)
	if userID == nil {
		rows, err = s.db.Query(
			`SELECT user_id, kind, cost_usd, ok, created_at FROM llm_usage WHERE created_at > ? ORDER BY created_at, id`, lower)
	} else {
		rows, err = s.db.Query(
			`SELECT user_id, kind, cost_usd, ok, created_at FROM llm_usage WHERE user_id = ? AND created_at > ? ORDER BY created_at, id`,
			*userID, lower)
	}
	if err != nil {
		return nil, fmt.Errorf("query AI usage: %w", err)
	}
	defer rows.Close()
	out := []LLMUsageEntry{}
	for rows.Next() {
		var (
			e   LLMUsageEntry
			uid sql.NullInt64
			ok  int
		)
		if err := rows.Scan(&uid, &e.Kind, &e.CostUSD, &ok, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan AI usage: %w", err)
		}
		if !e.CreatedAt.After(since) {
			continue
		}
		if uid.Valid {
			v := uid.Int64
			e.UserID = &v
		}
		e.OK = ok != 0
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate AI usage: %w", err)
	}
	return out, nil
}
