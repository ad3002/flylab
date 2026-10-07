package storage_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/storage"
)

// v3Schema is the production schema before v4 (jobs with the v2 columns, accounts,
// interpretations), used to prove the v4 migration is additive and idempotent.
const v3Schema = `
CREATE TABLE plans (plan_id TEXT PRIMARY KEY, plan_hash TEXT NOT NULL, plan_json TEXT NOT NULL, created_at TIMESTAMP NOT NULL);
CREATE TABLE resolved_plans (plan_id TEXT PRIMARY KEY, plan_hash TEXT NOT NULL, resolved_json TEXT NOT NULL, created_at TIMESTAMP NOT NULL);
CREATE TABLE jobs (job_id TEXT PRIMARY KEY, plan_id TEXT NOT NULL, plan_hash TEXT NOT NULL, status TEXT NOT NULL, stage TEXT NOT NULL,
  progress_pct REAL NOT NULL, idempotency_key TEXT, error_code TEXT, error_message TEXT, artifacts_dir TEXT NOT NULL,
  created_at TIMESTAMP NOT NULL, started_at TIMESTAMP, finished_at TIMESTAMP, user_id INTEGER, prompt TEXT, title TEXT);
CREATE TABLE idempotency_keys (key TEXT PRIMARY KEY, job_id TEXT NOT NULL, plan_id TEXT NOT NULL, created_at TIMESTAMP NOT NULL);
CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, display_name TEXT NOT NULL, password_hash TEXT NOT NULL, created_at TIMESTAMP NOT NULL);
CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL, created_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL);
CREATE TABLE user_idempotency_keys (user_id INTEGER NOT NULL, key TEXT NOT NULL, job_id TEXT NOT NULL, plan_id TEXT NOT NULL, created_at TIMESTAMP NOT NULL, PRIMARY KEY (user_id, key));
CREATE TABLE interpretations (job_id TEXT PRIMARY KEY, language TEXT, model TEXT, created_at TIMESTAMP, cost_usd REAL, duration_ms INTEGER, digest_json TEXT, result_json TEXT);
INSERT INTO users (id, username, display_name, password_hash, created_at) VALUES (1, 'olduser', 'Old', 'x', '2026-10-06 10:00:00+00:00');
INSERT INTO interpretations VALUES ('job_old', 'ru', 'claude-opus-5-5', '2026-10-06 10:00:00+00:00', 0.5, 120000, '{}', '{"interpretation":{"headline":"old"}}');
`

func TestV4MigrationIsAdditiveAndIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v3.db")
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(v3Schema); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	for i := 0; i < 2; i++ { // the second open must be a no-op
		store, err := storage.OpenStore(dbPath)
		if err != nil {
			t.Fatalf("open %d of a v3 database must succeed: %v", i, err)
		}
		got, err := store.GetInterpretation("job_old")
		if err != nil || got.Language != "ru" || !strings.Contains(got.ResultJSON, `"old"`) || got.CostUSD != 0.5 {
			t.Fatalf("the v3 interpretation must survive the migration: %+v %v", got, err)
		}
		if err := store.RecordLLMUsage(nil, storage.UsageKindPlanner, 0.01, true, time.Now()); err != nil {
			t.Fatalf("llm_usage must exist after open %d: %v", i, err)
		}
		if _, err := store.InsertInterpretationRequest("job_old", 1, "en", true, time.Now()); err != nil {
			t.Fatalf("interpretation_requests must exist after open %d: %v", i, err)
		}
		store.Close()
	}
	raw, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var names []string
	rows, err := raw.Query(`SELECT name FROM sqlite_master WHERE name IN ('llm_usage', 'interpretation_requests',
		'idx_llm_usage_created', 'idx_llm_usage_user_created', 'idx_interp_req_status') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if strings.Join(names, ",") != "idx_interp_req_status,idx_llm_usage_created,idx_llm_usage_user_created,interpretation_requests,llm_usage" {
		t.Fatalf("v4 tables/indexes missing: %v", names)
	}
	var n int
	raw.QueryRow(`SELECT COUNT(*) FROM llm_usage`).Scan(&n)
	if n != 2 {
		t.Fatalf("rows written between opens must be kept (llm_usage has %d rows)", n)
	}
}

func TestInterpretationQueuePrimitives(t *testing.T) {
	store, err := storage.OpenStore(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	if r, err := store.ClaimNextInterpretationRequest(t0); err != nil || r != nil {
		t.Fatalf("empty queue: %v %v", r, err)
	}
	a, _ := store.InsertInterpretationRequest("job_a", 1, "en", false, t0)
	b, _ := store.InsertInterpretationRequest("job_b", 2, "ru", true, t0.Add(time.Second))
	c, _ := store.InsertInterpretationRequest("job_c", 3, "en", false, t0.Add(2*time.Second))
	for i, r := range []*storage.InterpretationRequest{a, b, c} {
		if pos, err := store.QueuePosition(r); err != nil || pos != i+1 {
			t.Fatalf("position of request %d = %d (%v), want %d", r.ID, pos, err, i+1)
		}
	}
	got, err := store.ClaimNextInterpretationRequest(t0.Add(3 * time.Second))
	if err != nil || got.ID != a.ID || got.Status != storage.RequestRunning || got.StartedAt == nil {
		t.Fatalf("claim must take the oldest queued request: %+v %v", got, err)
	}
	if pos, _ := store.QueuePosition(got); pos != 0 {
		t.Fatalf("a running request has position 0, got %d", pos)
	}
	b2, _ := store.GetInterpretationRequest(b.ID)
	if pos, _ := store.QueuePosition(b2); pos != 1 || !b2.Regenerate || b2.Language != "ru" {
		t.Fatalf("b must move to position 1: %d %+v", pos, b2)
	}
	if q, r, err := store.InterpretationQueueCounts(); err != nil || q != 2 || r != 1 {
		t.Fatalf("counts: queued=%d running=%d %v", q, r, err)
	}
	if act, err := store.ActiveInterpretationRequestForUser(1); err != nil || act == nil || act.ID != a.ID {
		t.Fatalf("user 1 has the running request: %+v %v", act, err)
	}
	if act, err := store.ActiveInterpretationRequestForUser(9); err != nil || act != nil {
		t.Fatalf("user 9 has none: %+v %v", act, err)
	}

	// Success stores the interpretation and the status in one step.
	rec := &storage.Interpretation{JobID: "job_a", Language: "en", Model: "m", CreatedAt: t0.Add(4 * time.Second),
		CostUSD: 0.4, DurationMS: 1000, DigestJSON: `{}`, ResultJSON: `{"interpretation":{"headline":"a"}}`}
	if err := store.CompleteInterpretationRequest(a.ID, rec, t0.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	done, _ := store.GetInterpretationRequest(a.ID)
	if done.Status != storage.RequestSucceeded || done.FinishedAt == nil {
		t.Fatalf("request a must be succeeded: %+v", done)
	}
	if got, err := store.GetInterpretation("job_a"); err != nil || got.CostUSD != 0.4 {
		t.Fatalf("interpretation of job_a must be stored: %+v %v", got, err)
	}

	// A later failure of the same job makes its state failed; an earlier one does not.
	r2, _ := store.InsertInterpretationRequest("job_a", 1, "en", true, t0.Add(5*time.Second))
	if err := store.FailInterpretationRequest(r2.ID, "LLM_ERROR", "claude interpretation failed: 529", t0.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	infos, err := store.InterpretationInfos([]string{"job_a", "job_b", "job_none"})
	if err != nil {
		t.Fatal(err)
	}
	ia := infos["job_a"]
	if ia == nil || !ia.HasResult || ia.Latest.ID != r2.ID || *ia.Latest.ErrorCode != "LLM_ERROR" ||
		storage.InterpretationState(ia.Latest, ia.HasResult, ia.ResultAt) != storage.StateFailed {
		t.Fatalf("job_a: failed after its result must be state failed: %+v", ia)
	}
	if st := storage.InterpretationState(infos["job_b"].Latest, false, nil); st != storage.StateQueued {
		t.Fatalf("job_b is queued, got %q", st)
	}
	if infos["job_none"] != nil {
		t.Fatalf("a job with nothing must be absent: %+v", infos["job_none"])
	}
	before := t0.Add(3 * time.Second)
	older := &storage.InterpretationRequest{Status: storage.RequestFailed, FinishedAt: &before}
	resultAt := t0.Add(4 * time.Second)
	if st := storage.InterpretationState(older, true, &resultAt); st != storage.StateReady {
		t.Fatalf("a failure older than the result leaves the result ready, got %q", st)
	}
	if st := storage.InterpretationState(nil, false, nil); st != "" {
		t.Fatalf("nothing at all is the empty state, got %q", st)
	}

	// Startup recovery: a request left running is failed WORKER_INTERRUPTED; queued ones stay.
	if _, err := store.ClaimNextInterpretationRequest(t0.Add(7 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := store.FailInterruptedInterpretationRequests(t0.Add(8 * time.Second))
	if err != nil || n != 1 {
		t.Fatalf("recovery: %d %v", n, err)
	}
	b3, _ := store.GetInterpretationRequest(b.ID)
	c3, _ := store.GetInterpretationRequest(c.ID)
	if b3.Status != storage.RequestFailed || *b3.ErrorCode != "WORKER_INTERRUPTED" || !strings.Contains(*b3.ErrorMessage, "restarted") ||
		c3.Status != storage.RequestQueued {
		t.Fatalf("recovery wrong: b=%+v c=%+v", b3, c3)
	}
}

func TestLLMUsageFilters(t *testing.T) {
	store, err := storage.OpenStore(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	u1, u2 := int64(1), int64(2)
	for _, e := range []struct {
		uid  *int64
		cost float64
		at   time.Time
	}{{&u1, 0.1, now.Add(-25 * time.Hour)}, {&u1, 0.2, now.Add(-time.Hour)}, {&u2, 0.4, now.Add(-time.Minute)}} {
		if err := store.RecordLLMUsage(e.uid, storage.UsageKindInterpreter, e.cost, true, e.at); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.LLMUsageSince(now.Add(-24*time.Hour), nil)
	if err != nil || len(all) != 2 || all[0].CostUSD != 0.2 || all[1].CostUSD != 0.4 {
		t.Fatalf("global window wrong: %+v %v", all, err)
	}
	mine, err := store.LLMUsageSince(now.Add(-24*time.Hour), &u1)
	if err != nil || len(mine) != 1 || mine[0].CostUSD != 0.2 || *mine[0].UserID != 1 || mine[0].Kind != storage.UsageKindInterpreter {
		t.Fatalf("user window wrong: %+v %v", mine, err)
	}
}
