package storage_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/storage"
)

func TestInterpretationsStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "interp.db")
	store, err := storage.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.GetInterpretation("job_a"); err != storage.ErrNotFound {
		t.Fatalf("missing interpretation must be ErrNotFound, got %v", err)
	}
	created := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	rec := &storage.Interpretation{JobID: "job_a", Language: "ru", Model: "claude-opus-5-5", CreatedAt: created,
		CostUSD: 0.42, DurationMS: 31000, DigestJSON: `{"d":1}`, ResultJSON: `{"interpretation":{"headline":"h1"}}`}
	if err := store.SaveInterpretation(rec); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetInterpretation("job_a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Language != "ru" || got.Model != "claude-opus-5-5" || got.CostUSD != 0.42 || got.DurationMS != 31000 ||
		!got.CreatedAt.Equal(created) || got.DigestJSON != `{"d":1}` || !strings.Contains(got.ResultJSON, "h1") {
		t.Fatalf("round trip wrong: %+v", got)
	}

	// Regenerate replaces the row.
	rec.Language, rec.ResultJSON = "en", `{"interpretation":{"headline":"h2"}}`
	if err := store.SaveInterpretation(rec); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetInterpretation("job_a")
	if got.Language != "en" || !strings.Contains(got.ResultJSON, "h2") {
		t.Fatalf("upsert must replace the interpretation: %+v", got)
	}

	has, err := store.InterpretedJobs([]string{"job_a", "job_b"})
	if err != nil {
		t.Fatal(err)
	}
	if lang, ok := has["job_a"]; !ok || lang != "en" || len(has) != 1 {
		t.Fatalf("InterpretedJobs wrong: %v", has)
	}
	if empty, err := store.InterpretedJobs(nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty id list: %v %v", empty, err)
	}
	store.Close()

	// A row with NULL columns (written outside this code) is an error, not an empty result.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO interpretations (job_id, language) VALUES ('job_null', 'en')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	store, err = storage.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("re-open must be idempotent: %v", err)
	}
	defer store.Close()
	if _, err := store.GetInterpretation("job_null"); err == nil || !strings.Contains(err.Error(), "has NULL") {
		t.Fatalf("NULL columns must be reported, got %v", err)
	}
}
