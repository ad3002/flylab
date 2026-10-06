package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ad3002/flylab/internal/api"
	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/storage"
)

func setupTestServer(t *testing.T) (*api.Server, *storage.Store, func()) {
	tmpDir, err := os.MkdirTemp("", "flylab_api_test_*")
	if err != nil {
		t.Fatalf("Failed to create tmp dir: %v", err)
	}

	wd, _ := os.Getwd()
	projectRoot := filepath.Dir(filepath.Dir(wd))

	cfg := &config.Config{
		Host:             "127.0.0.1",
		Port:             8080,
		Domain:           "flylab.aglabx.com",
		DBPath:           filepath.Join(tmpDir, "test.db"),
		DataDir:          filepath.Join(projectRoot, "data"),
		ArtifactsDir:     filepath.Join(tmpDir, "artifacts"),
		RegistryDir:      filepath.Join(projectRoot, "registry"),
		ContractsDir:     filepath.Join(projectRoot, "contracts"),
		WebDir:           filepath.Join(projectRoot, "web"),
		FlysimBin:        filepath.Join(projectRoot, "bin", "flysim"),
		MaxWallSeconds:   3600,
		MaxRSSBytes:      24 * 1024 * 1024 * 1024,
		MaxArtifactBytes: 2 * 1024 * 1024 * 1024,
	}

	reg, err := contracts.LoadRegistry(cfg.RegistryDir)
	if err != nil {
		t.Fatalf("Failed to load registry: %v", err)
	}

	val, err := contracts.NewValidator(cfg.ContractsDir, reg)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	store, err := storage.OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatalf("Failed to open store: %v", err)
	}

	llmClient := llm.NewClient(cfg, val, reg)
	server := api.NewServer(cfg, store, val, reg, llmClient)

	cleanup := func() {
		store.Close()
		os.RemoveAll(tmpDir)
	}

	return server, store, cleanup
}

func TestAPIBaseEndpoints(t *testing.T) {
	srv, _, cleanup := setupTestServer(t)
	defer cleanup()

	handler := srv.Router()

	// 1. GET /health
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /health, got %d", rec.Code)
	}

	// 2. GET /capabilities
	req = httptest.NewRequest("GET", "/capabilities", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /capabilities, got %d", rec.Code)
	}

	// 3. GET /datasets
	req = httptest.NewRequest("GET", "/datasets", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /datasets, got %d", rec.Code)
	}

	// 4. GET /groups
	req = httptest.NewRequest("GET", "/groups", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /groups, got %d", rec.Code)
	}

	// 5. GET /neurons?q=mn9
	req = httptest.NewRequest("GET", "/neurons?q=mn9", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /neurons, got %d", rec.Code)
	}
}

func TestAPIPlanAndJobWorkflow(t *testing.T) {
	srv, _, cleanup := setupTestServer(t)
	defer cleanup()

	handler := srv.Router()

	// 1. POST /api/v1/plans/validate
	planJSON := `{
		"schema_version": "1.0",
		"dataset_id": "flywire_630",
		"model_id": "shiu_lif_rust",
		"experiment_type": "single",
		"duration_ms": 100,
		"activation": [
			{
				"selector": {"group_id": "sugar_grn"},
				"rate_hz": 50.0
			}
		],
		"silencing": [],
		"readout": [
			{
				"selector": {"group_id": "mn9"}
			}
		],
		"repeats": 1,
		"base_seed": 42,
		"report_language": "en"
	}`

	req := httptest.NewRequest("POST", "/api/v1/plans/validate", bytes.NewBufferString(planJSON))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /api/v1/plans/validate, got %d: %s", rec.Code, rec.Body.String())
	}

	var valResp struct {
		PlanID       string `json:"plan_id"`
		ResolvedPlan struct {
			PlanID   string `json:"plan_id"`
			PlanHash string `json:"plan_hash"`
		} `json:"resolved_plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &valResp); err != nil {
		t.Fatalf("Failed to parse validate response: %v", err)
	}
	if valResp.PlanID == "" {
		t.Fatalf("Expected non-empty plan_id")
	}

	// 2. POST /api/v1/jobs
	jobReqBody := fmt.Sprintf(`{"plan_id": "%s"}`, valResp.PlanID)
	req = httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewBufferString(jobReqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "test-key-1")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("Expected 202 Accepted from /api/v1/jobs, got %d: %s", rec.Code, rec.Body.String())
	}

	var jobResp struct {
		Job struct {
			JobID  string `json:"job_id"`
			Status string `json:"status"`
		} `json:"job"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jobResp); err != nil {
		t.Fatalf("Failed to parse job response: %v", err)
	}
	if jobResp.Job.JobID == "" {
		t.Fatalf("Expected non-empty job_id")
	}

	// 3. GET /api/v1/jobs/{job_id}
	req = httptest.NewRequest("GET", "/api/v1/jobs/"+jobResp.Job.JobID, nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /api/v1/jobs/{id}, got %d: %s", rec.Code, rec.Body.String())
	}
}
