package storage_test

import (
	"database/sql"
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

	// 2. Create Job with Idempotency Key (keys are scoped to the owner)
	owner, err := store.CreateUser("owner1", "Owner One", "pbkdf2_sha256$1$c2FsdA$aGFzaA")
	if err != nil {
		t.Fatalf("Failed to create owner: %v", err)
	}
	ownerID := owner.ID
	idempKey := "key-12345"
	job1 := &domain.Job{
		UserID:         &ownerID,
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
		UserID:         &ownerID,
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

func openTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Failed to open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newJob(id, planID string, userID *int64, created time.Time) *domain.Job {
	return &domain.Job{
		JobID: id, PlanID: planID, PlanHash: "h_" + planID, Status: domain.StatusQueued,
		Stage: "queued", ArtifactsDir: "/nonexistent/" + id, CreatedAt: created, UserID: userID,
	}
}

func TestMigrationAddsJobColumnsToLegacyDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	// Build a v1 database by hand: jobs without user_id / prompt / title, with one row.
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	_, err = legacy.Exec(`CREATE TABLE jobs (
		job_id TEXT PRIMARY KEY, plan_id TEXT NOT NULL, plan_hash TEXT NOT NULL, status TEXT NOT NULL,
		stage TEXT NOT NULL, progress_pct REAL NOT NULL, idempotency_key TEXT, error_code TEXT,
		error_message TEXT, artifacts_dir TEXT NOT NULL, created_at TIMESTAMP NOT NULL,
		started_at TIMESTAMP, finished_at TIMESTAMP);
		INSERT INTO jobs (job_id, plan_id, plan_hash, status, stage, progress_pct, artifacts_dir, created_at)
		VALUES ('legacy_job', 'plan_x', 'hash_x', 'succeeded', 'done', 100, '/a/legacy_job', '2026-01-02 03:04:05+00:00');`)
	if err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	legacy.Close()

	store, err := storage.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("OpenStore must migrate a legacy DB, got: %v", err)
	}
	defer store.Close()

	j, err := store.GetJob("legacy_job")
	if err != nil {
		t.Fatalf("legacy job unreadable after migration: %v", err)
	}
	if j.UserID != nil || j.Prompt != nil || j.Title != nil {
		t.Fatalf("legacy job must have NULL owner/prompt/title, got %v %v %v", j.UserID, j.Prompt, j.Title)
	}
	if j.Status != domain.StatusSucceeded || j.PlanID != "plan_x" {
		t.Fatalf("legacy job fields changed by migration: %+v", j)
	}
	// Legacy jobs have no owner: nobody can see them through the owner-scoped accessors.
	if _, err := store.GetJobForUser("legacy_job", 1); err != storage.ErrNotFound {
		t.Fatalf("ownerless job must be ErrNotFound for any user, got %v", err)
	}
	store.Close()

	// Re-opening is idempotent (columns already present).
	store2, err := storage.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("second OpenStore failed: %v", err)
	}
	store2.Close()
}

func TestPerUserIdempotencyAndListing(t *testing.T) {
	store := openTestStore(t)
	a, err := store.CreateUser("alice", "Alice", "x")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	b, err := store.CreateUser("bob", "Bob", "x")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if _, err := store.CreateUser("alice", "Again", "x"); err != storage.ErrUsernameTaken {
		t.Fatalf("duplicate username must be ErrUsernameTaken, got %v", err)
	}

	key := "same-key"
	base := time.Now().UTC()
	ja, isNew, err := store.CreateJob(newJob("job_a", "plan_1", &a.ID, base), &key)
	if err != nil || !isNew {
		t.Fatalf("alice job: isNew=%v err=%v", isNew, err)
	}
	jb, isNew, err := store.CreateJob(newJob("job_b", "plan_2", &b.ID, base.Add(time.Second)), &key)
	if err != nil {
		t.Fatalf("bob reusing alice's key must not conflict, got %v", err)
	}
	if !isNew || jb.JobID != "job_b" {
		t.Fatalf("bob must get his own new job, got %s isNew=%v", jb.JobID, isNew)
	}
	if ja.JobID != "job_a" {
		t.Fatalf("unexpected alice job id %s", ja.JobID)
	}

	title := "second"
	j2 := newJob("job_a2", "plan_3", &a.ID, base.Add(2*time.Second))
	j2.Title = &title
	if _, _, err := store.CreateJob(j2, nil); err != nil {
		t.Fatalf("alice second job: %v", err)
	}

	jobs, total, err := store.ListJobsForUser(a.ID, "", 24, 0)
	if err != nil {
		t.Fatalf("list alice jobs: %v", err)
	}
	if total != 2 || len(jobs) != 2 || jobs[0].JobID != "job_a2" || jobs[1].JobID != "job_a" {
		t.Fatalf("alice must see exactly her 2 jobs newest first, got total=%d %v", total, jobIDs(jobs))
	}
	if jobs[0].Title == nil || *jobs[0].Title != "second" {
		t.Fatalf("title not persisted: %v", jobs[0].Title)
	}
	if _, err := store.GetJobForUser("job_b", a.ID); err != storage.ErrNotFound {
		t.Fatalf("alice must not see bob's job, got %v", err)
	}
	if got, err := store.GetJobForUser("job_b", b.ID); err != nil || got.JobID != "job_b" {
		t.Fatalf("bob must see his job, got %v %v", got, err)
	}

	queued, total, err := store.ListJobsForUser(a.ID, "succeeded", 24, 0)
	if err != nil || total != 0 || len(queued) != 0 {
		t.Fatalf("status filter: expected 0 succeeded jobs, got %d (%v)", total, err)
	}

	st, err := store.UserStats(a.ID)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.TotalJobs != 2 || st.Running != 2 || st.Succeeded != 0 || st.LastJobAt == nil {
		t.Fatalf("unexpected stats %+v", st)
	}
	empty, err := store.UserStats(9999)
	if err != nil || empty.TotalJobs != 0 || empty.LastJobAt != nil {
		t.Fatalf("stats for user without jobs: %+v err=%v", empty, err)
	}
}

func TestSessions(t *testing.T) {
	store := openTestStore(t)
	u, err := store.CreateUser("carol", "Carol", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now().UTC()
	if err := store.CreateSession(u.ID, "live", now.Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.CreateSession(u.ID, "expired", now.Add(-time.Minute)); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	got, err := store.GetSessionUser("live", now)
	if err != nil || got.Username != "carol" {
		t.Fatalf("live session must resolve to carol, got %v %v", got, err)
	}
	if _, err := store.GetSessionUser("expired", now); err != storage.ErrNotFound {
		t.Fatalf("expired session must be ErrNotFound, got %v", err)
	}
	_, revoked, err := store.UpdatePassword("carol", "newhash")
	if err != nil || revoked != 2 {
		t.Fatalf("password change must revoke both sessions, revoked=%d err=%v", revoked, err)
	}
	if _, err := store.GetSessionUser("live", now); err != storage.ErrNotFound {
		t.Fatalf("session must be gone after password change, got %v", err)
	}
	_, hash, err := store.GetUserCredentials("carol")
	if err != nil || hash != "newhash" {
		t.Fatalf("password hash not updated: %q %v", hash, err)
	}
	if _, _, err := store.UpdatePassword("nobody", "x"); err != storage.ErrNotFound {
		t.Fatalf("unknown user must be ErrNotFound, got %v", err)
	}
}

func jobIDs(jobs []*domain.Job) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.JobID)
	}
	return out
}
