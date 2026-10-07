package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/api"
	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/contracts"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/interpret"
	"github.com/ad3002/flylab/internal/llm"
	"github.com/ad3002/flylab/internal/storage"
)

type testEnv struct {
	t       *testing.T
	handler http.Handler
	store   *storage.Store
	cfg     *config.Config
	server  *api.Server
	interp  *interpret.Service
	reg     *contracts.Registry
	val     *contracts.Validator
	llm     *llm.Client
	// bodies collects every response body so tests can assert that secrets never leak.
	bodies   []string
	bodiesMu sync.Mutex
}

func projectRoot(t *testing.T) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func newEnv(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	root := projectRoot(t)
	tmpDir := t.TempDir()

	fake := filepath.Join(root, "internal", "llm", "testdata", "fake_claude.sh")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatalf("chmod fake claude: %v", err)
	}

	cfg := &config.Config{
		Host:                  "127.0.0.1",
		Port:                  8080,
		Domain:                "flylab.aglabx.com",
		DBPath:                filepath.Join(tmpDir, "test.db"),
		DataDir:               filepath.Join(root, "data"),
		ArtifactsDir:          filepath.Join(tmpDir, "artifacts"),
		RegistryDir:           filepath.Join(root, "registry"),
		ContractsDir:          filepath.Join(root, "contracts"),
		WebDir:                filepath.Join(root, "web"),
		FlysimBin:             filepath.Join(root, "bin", "flysim"),
		MaxWallSeconds:        3600,
		MaxRSSBytes:           24 * 1024 * 1024 * 1024,
		MaxArtifactBytes:      2 * 1024 * 1024 * 1024,
		ClaudeBin:             fake,
		ClaudeModel:           "claude-sonnet-5-5",
		ClaudeTimeoutSeconds:  10,
		LLMMaxConcurrency:     2,
		ParseRateLimitPerHour: 1000,
		RegistrationOpen:      true,

		ParseRateLimitPerIPPerHour:    1000,
		ParseGlobalLimitPerHour:       1000,
		AuthRateLimitPerIP:            1000,
		LoginFailuresPerUsername:      1000,
		RegisterRateLimitPerIPPerHour: 1000,
		PasswordHashConcurrency:       4,

		ClaudeInterpretModel:           "claude-opus-5-5",
		ClaudeInterpretTimeoutSeconds:  10,
		InterpretRateLimitPerHour:      1000,
		InterpretRateLimitPerIPPerHour: 1000,
		InterpretGlobalLimitPerHour:    1000,
		InterpretConcurrency:           1,
		InterpretQueueMax:              20,

		AIDailyBudgetUSD:     1000,
		AIUserDailyBudgetUSD: 1000,
	}
	if mutate != nil {
		mutate(cfg)
	}

	reg, err := contracts.LoadRegistry(cfg.RegistryDir)
	if err != nil {
		t.Fatalf("Failed to load registry: %v", err)
	}
	val, err := contracts.NewValidator(cfg.ContractsDir, reg)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}
	// Like the server: explicit neuron ids must be neurons of the real v630 connectome.
	val.SetNeuronIDs(realNeuronIDs(t))
	store, err := storage.OpenStore(cfg.DBPath)
	if err != nil {
		t.Fatalf("Failed to open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	budget, err := llm.NewBudget(store, cfg.AIDailyBudgetUSD, cfg.AIUserDailyBudgetUSD)
	if err != nil {
		t.Fatalf("Failed to create the AI budget: %v", err)
	}
	llmClient, err := llm.NewClient(cfg, val, reg, budget)
	if err != nil {
		t.Fatalf("Failed to create llm client: %v", err)
	}
	server := api.NewServer(cfg, store, val, reg, llmClient)
	e := &testEnv{t: t, store: store, cfg: cfg, server: server, reg: reg, val: val, llm: llmClient}
	e.interp = e.startInterpreter()
	e.handler = server.Router()
	return e
}

// startInterpreter builds and starts an interpretation service (queue worker) on the env's
// store and connects it to the server; it is stopped when the test ends.
func (e *testEnv) startInterpreter() *interpret.Service {
	e.t.Helper()
	interp, err := interpret.NewService(e.cfg, e.store, e.reg, e.val, e.llm)
	if err != nil {
		e.t.Fatalf("Failed to create interpretation service: %v", err)
	}
	interp.PollInterval = 20 * time.Millisecond
	interp.PersistRetryDelays = []time.Duration{10 * time.Millisecond}
	if err := interp.Start(); err != nil {
		e.t.Fatalf("Failed to start the interpretation worker: %v", err)
	}
	e.t.Cleanup(interp.Stop)
	e.server.SetInterpreter(interp)
	return interp
}

var (
	neuronIDsOnce sync.Once
	neuronIDs     *contracts.NeuronIDSet
	neuronIDsErr  error
)

func realNeuronIDs(t *testing.T) *contracts.NeuronIDSet {
	t.Helper()
	neuronIDsOnce.Do(func() { neuronIDs, neuronIDsErr = contracts.LoadNeuronIDs(filepath.Join(projectRoot(t), "data")) })
	if neuronIDsErr != nil {
		t.Fatalf("load the v630 neuron ids: %v", neuronIDsErr)
	}
	return neuronIDs
}

type reqOpt func(*http.Request)

func bearer(token string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

func header(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func withCookie(c *http.Cookie) reqOpt {
	return func(r *http.Request) { r.AddCookie(c) }
}

func fromAddr(remote string) reqOpt {
	return func(r *http.Request) { r.RemoteAddr = remote }
}

func (e *testEnv) do(method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	// Like the web UI, every POST declares JSON (the CSRF guard requires it); tests of the
	// guard override the header.
	if body != "" || method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	e.bodiesMu.Lock()
	e.bodies = append(e.bodies, rec.Body.String())
	e.bodiesMu.Unlock()
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not a JSON object (%d): %q", rec.Code, rec.Body.String())
	}
	return out
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("expected HTTP %d, got %d: %s", want, rec.Code, rec.Body.String())
	}
}

// expectError checks status and the error envelope code (and optionally a message part).
func expectError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, msgPart string) {
	t.Helper()
	expectStatus(t, rec, status)
	body := decode(t, rec)
	errObj, ok := body["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected an error envelope, got %s", rec.Body.String())
	}
	if errObj["code"] != code {
		t.Fatalf("expected error code %s, got %v (%s)", code, errObj["code"], rec.Body.String())
	}
	if msgPart != "" && !strings.Contains(fmt.Sprint(errObj["message"]), msgPart) {
		t.Fatalf("error message %q does not contain %q", errObj["message"], msgPart)
	}
	if errObj["request_id"] == "" || errObj["request_id"] == nil {
		t.Fatalf("error envelope lacks request_id: %s", rec.Body.String())
	}
}

// register creates an account and returns its bearer token.
func (e *testEnv) register(username, password string) string {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/auth/register",
		fmt.Sprintf(`{"username":%q,"password":%q,"display_name":"User %s"}`, username, password, username))
	expectStatus(e.t, rec, http.StatusCreated)
	body := decode(e.t, rec)
	token, _ := body["token"].(string)
	if token == "" {
		e.t.Fatalf("register returned no token: %s", rec.Body.String())
	}
	return token
}

const singlePlanJSON = `{
	"schema_version": "1.0", "dataset_id": "flywire_630", "model_id": "shiu_lif_rust",
	"experiment_type": "single", "duration_ms": 100,
	"activation": [{"selector": {"group_id": "sugar_grn"}, "rate_hz": 50.0}],
	"silencing": [], "readout": [{"selector": {"group_id": "mn9"}}],
	"repeats": 1, "base_seed": 42, "report_language": "en"
}`

func (e *testEnv) validatePlan() string {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/plans/validate", singlePlanJSON)
	expectStatus(e.t, rec, http.StatusOK)
	planID, _ := decode(e.t, rec)["plan_id"].(string)
	if planID == "" {
		e.t.Fatalf("validate returned no plan_id: %s", rec.Body.String())
	}
	return planID
}

func (e *testEnv) createJob(token, planID, title string, opts ...reqOpt) (string, *httptest.ResponseRecorder) {
	e.t.Helper()
	body := fmt.Sprintf(`{"plan_id":%q,"title":%q,"prompt":"stimulate sugar GRNs"}`, planID, title)
	rec := e.do("POST", "/api/v1/jobs", body, append([]reqOpt{bearer(token)}, opts...)...)
	if rec.Code != http.StatusAccepted && rec.Code != http.StatusOK {
		e.t.Fatalf("create job failed (%d): %s", rec.Code, rec.Body.String())
	}
	job, _ := decode(e.t, rec)["job"].(map[string]interface{})
	id, _ := job["job_id"].(string)
	if id == "" {
		e.t.Fatalf("no job_id in %s", rec.Body.String())
	}
	return id, rec
}

func TestAPIBaseEndpoints(t *testing.T) {
	e := newEnv(t, nil)

	rec := e.do("GET", "/health", "")
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["status"] != "ok" {
		t.Fatalf("health status not ok: %s", rec.Body.String())
	}

	rec = e.do("GET", "/capabilities", "")
	expectStatus(t, rec, http.StatusOK)
	caps := decode(t, rec)
	if caps["llm_provider"] != "claude-cli" || caps["llm_model"] != "claude-sonnet-5-5" || caps["llm_ready"] != true {
		t.Fatalf("unexpected llm capabilities: %s", rec.Body.String())
	}
	if caps["registration_open"] != true {
		t.Fatalf("registration_open must be reported: %s", rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "ollama") {
		t.Fatalf("capabilities still mention ollama: %s", rec.Body.String())
	}

	rec = e.do("GET", "/groups", "")
	expectStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"group_id":"sugar_grn"`) {
		t.Fatalf("groups lack sugar_grn: %s", rec.Body.String())
	}

	rec = e.do("GET", "/neurons?q=720575940645521262", "")
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["total"] != float64(1) || !strings.Contains(rec.Body.String(), `"group_id":"mn9"`) {
		t.Fatalf("neuron search for an MN9 root id failed: %s", rec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "dataset_manifest.json")); err == nil {
		rec = e.do("GET", "/datasets", "")
		expectStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), "flywire_630") {
			t.Fatalf("datasets lack flywire_630: %s", rec.Body.String())
		}
	}
}

func TestCapabilitiesReportMissingClaude(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.ClaudeBin = "flylab-no-such-claude"; c.RegistrationOpen = false })
	caps := decode(t, e.do("GET", "/capabilities", ""))
	if caps["llm_ready"] != false || caps["registration_open"] != false {
		t.Fatalf("expected llm_ready=false registration_open=false, got %v", caps)
	}
}

func TestPagesAndFavicon(t *testing.T) {
	web := t.TempDir()
	for path, content := range map[string]string{
		"templates/index.html":   "<html>LANDING</html>",
		"templates/app.html":     "<html>APPLICATION</html>",
		"static/img/favicon.svg": `<svg xmlns="http://www.w3.org/2000/svg"></svg>`,
		"static/css/style.css":   "body{}",
	} {
		p := filepath.Join(web, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := newEnv(t, func(c *config.Config) { c.WebDir = web })

	for path, want := range map[string]string{
		"/":                     "LANDING",
		"/app":                  "APPLICATION",
		"/app/":                 "APPLICATION",
		"/app/history/x":        "APPLICATION",
		"/static/css/style.css": "body{}",
	} {
		rec := e.do("GET", path, "")
		expectStatus(t, rec, http.StatusOK)
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("GET %s: expected %q, got %q", path, want, rec.Body.String())
		}
	}
	rec := e.do("GET", "/favicon.ico", "")
	expectStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("favicon content type %q, want image/svg+xml", ct)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("favicon body is not the svg: %q", rec.Body.String())
	}
}

func TestAuthFlow(t *testing.T) {
	e := newEnv(t, nil)

	rec := e.do("POST", "/api/v1/auth/register", `{"username":"Alice","password":"alice-password","display_name":"Alice A."}`)
	expectStatus(t, rec, http.StatusCreated)
	body := decode(t, rec)
	user := body["user"].(map[string]interface{})
	if user["username"] != "alice" || user["display_name"] != "Alice A." || user["id"] == nil || user["created_at"] == nil {
		t.Fatalf("unexpected user object: %s", rec.Body.String())
	}
	token := body["token"].(string)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "flylab_session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != token || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode ||
		cookie.Path != "/" || cookie.MaxAge != 2592000 || cookie.Secure {
		t.Fatalf("session cookie attributes wrong: %+v", cookie)
	}

	// me with cookie and with bearer
	for name, opt := range map[string]reqOpt{"cookie": withCookie(cookie), "bearer": bearer(token)} {
		rec = e.do("GET", "/api/v1/me", "", opt)
		expectStatus(t, rec, http.StatusOK)
		me := decode(t, rec)
		if me["user"].(map[string]interface{})["username"] != "alice" {
			t.Fatalf("me via %s returned wrong user: %s", name, rec.Body.String())
		}
		stats := me["stats"].(map[string]interface{})
		if stats["total_jobs"] != float64(0) || stats["last_job_at"] != nil {
			t.Fatalf("fresh user stats wrong: %v", stats)
		}
	}

	// logout clears the cookie and deletes the session
	rec = e.do("POST", "/api/v1/auth/logout", "", withCookie(cookie))
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["ok"] != true {
		t.Fatalf("logout body: %s", rec.Body.String())
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "flylab_session" && c.MaxAge < 0 && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout did not clear the cookie: %v", rec.Result().Cookies())
	}
	expectError(t, e.do("GET", "/api/v1/me", "", withCookie(cookie)), 401, "UNAUTHENTICATED", "")
	expectError(t, e.do("GET", "/api/v1/me", "", bearer(token)), 401, "UNAUTHENTICATED", "")
	expectError(t, e.do("GET", "/api/v1/me", ""), 401, "UNAUTHENTICATED", "")

	// login again
	rec = e.do("POST", "/api/v1/auth/login", `{"username":"ALICE","password":"alice-password"}`)
	expectStatus(t, rec, http.StatusOK)
	token2 := decode(t, rec)["token"].(string)
	if token2 == token {
		t.Fatalf("login must issue a fresh token")
	}
	rec = e.do("GET", "/api/v1/me", "", bearer(token2))
	expectStatus(t, rec, http.StatusOK)
	if me := decode(t, rec); me["user"].(map[string]interface{})["username"] != "alice" ||
		me["user"].(map[string]interface{})["id"] != user["id"] {
		t.Fatalf("me with the fresh login token must be alice (id %v): %s", user["id"], rec.Body.String())
	}

	// errors
	expectError(t, e.do("POST", "/api/v1/auth/register", `{"username":"alice","password":"another-pass"}`), 409, "USERNAME_TAKEN", "alice")
	expectError(t, e.do("POST", "/api/v1/auth/login", `{"username":"alice","password":"wrong-password"}`), 401, "INVALID_CREDENTIALS", "")
	expectError(t, e.do("POST", "/api/v1/auth/login", `{"username":"nobody","password":"whatever-1"}`), 401, "INVALID_CREDENTIALS", "")
	expectError(t, e.do("POST", "/api/v1/auth/register", `{"username":"a b","password":"long-enough"}`), 422, "INVALID_USERNAME", "")
	expectError(t, e.do("POST", "/api/v1/auth/register", `{"username":"bob","password":"short"}`), 422, "WEAK_PASSWORD", "")
	expectError(t, e.do("GET", "/api/v1/me", "", bearer("not-a-real-token")), 401, "UNAUTHENTICATED", "")

	// https behind the proxy -> Secure cookie
	rec = e.do("POST", "/api/v1/auth/login", `{"username":"alice","password":"alice-password"}`, header("X-Forwarded-Proto", "https"))
	expectStatus(t, rec, http.StatusOK)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "flylab_session" && !c.Secure {
			t.Fatalf("cookie must be Secure behind https")
		}
	}

	// the password hash never appears in any response
	for _, b := range e.bodies {
		if strings.Contains(b, "pbkdf2") || strings.Contains(b, "password_hash") || strings.Contains(b, "alice-password") {
			t.Fatalf("response leaks password material: %s", b)
		}
	}
}

func TestRegistrationClosed(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.RegistrationOpen = false })
	expectError(t, e.do("POST", "/api/v1/auth/register", `{"username":"carol","password":"carol-password"}`), 403, "REGISTRATION_CLOSED", "")
	if _, _, err := e.store.GetUserCredentials("carol"); err != storage.ErrNotFound {
		t.Fatalf("no user must be created while registration is closed, got %v", err)
	}
}

func TestProtectedEndpointsRequireAuth(t *testing.T) {
	e := newEnv(t, nil)
	for _, rt := range []struct{ method, path, body string }{
		{"POST", "/api/v1/plans/parse", `{"prompt":"x"}`},
		{"POST", "/api/v1/jobs", `{"plan_id":"p"}`},
		{"GET", "/api/v1/jobs", ""},
		{"GET", "/api/v1/jobs/job_x", ""},
		{"POST", "/api/v1/jobs/job_x/cancel", ""},
		{"GET", "/api/v1/jobs/job_x/results", ""},
		{"GET", "/api/v1/jobs/job_x/spikes", ""},
		{"GET", "/api/v1/jobs/job_x/export", ""},
		{"GET", "/api/v1/jobs/job_x/artifacts/report.md", ""},
		{"GET", "/api/v1/jobs/job_x/digest", ""},
		{"GET", "/api/v1/jobs/job_x/interpretation", ""},
		{"POST", "/api/v1/jobs/job_x/interpretation", `{"language":"en"}`},
		{"POST", "/plans/parse", `{"prompt":"x"}`},
		{"POST", "/jobs", `{"plan_id":"p"}`},
		{"GET", "/jobs", ""},
		{"GET", "/jobs/job_x", ""},
	} {
		expectError(t, e.do(rt.method, rt.path, rt.body), 401, "UNAUTHENTICATED", "")
	}
	// validate stays public, on both prefixes
	e.validatePlan()
	rec := e.do("POST", "/plans/validate", singlePlanJSON)
	expectStatus(t, rec, http.StatusOK)
	legacy := decode(t, rec)
	planID, _ := legacy["plan_id"].(string)
	resolved, _ := legacy["resolved_plan"].(map[string]interface{})
	if !strings.HasPrefix(planID, "plan_") || resolved == nil || resolved["plan_id"] != planID || legacy["plan_hash"] == "" {
		t.Fatalf("legacy /plans/validate must return plan_id and resolved_plan: %s", rec.Body.String())
	}
}

func TestOwnershipAndPerUserIdempotency(t *testing.T) {
	e := newEnv(t, nil)
	alice := e.register("alice", "alice-password")
	bob := e.register("bob", "bob-password")
	planID := e.validatePlan()

	jobA, rec := e.createJob(alice, planID, "alice run", header("Idempotency-Key", "shared-key"))
	expectStatus(t, rec, http.StatusAccepted)
	jobB, rec := e.createJob(bob, planID, "bob run", header("Idempotency-Key", "shared-key"))
	expectStatus(t, rec, http.StatusAccepted)
	if jobA == jobB {
		t.Fatalf("the same Idempotency-Key from two users must create two jobs, both got %s", jobA)
	}
	again, rec := e.createJob(alice, planID, "alice run", header("Idempotency-Key", "shared-key"))
	expectStatus(t, rec, http.StatusOK)
	if again != jobA {
		t.Fatalf("idempotent replay must return %s, got %s", jobA, again)
	}

	// Bob cannot see anything of Alice's job (404, existence not leaked).
	for _, p := range []string{
		"/api/v1/jobs/" + jobA, "/api/v1/jobs/" + jobA + "/results", "/api/v1/jobs/" + jobA + "/spikes",
		"/api/v1/jobs/" + jobA + "/export", "/api/v1/jobs/" + jobA + "/artifacts/report.md", "/jobs/" + jobA,
	} {
		expectError(t, e.do("GET", p, "", bearer(bob)), 404, "JOB_NOT_FOUND", jobA)
	}
	expectError(t, e.do("POST", "/api/v1/jobs/"+jobA+"/cancel", "", bearer(bob)), 404, "JOB_NOT_FOUND", "")
	if j, _ := e.store.GetJob(jobA); j.Status != domain.StatusQueued {
		t.Fatalf("bob's cancel must not touch alice's job, status=%s", j.Status)
	}

	// Lists contain only own jobs.
	for token, want := range map[string]string{alice: jobA, bob: jobB} {
		rec := e.do("GET", "/api/v1/jobs", "", bearer(token))
		expectStatus(t, rec, http.StatusOK)
		body := decode(t, rec)
		jobs := body["jobs"].([]interface{})
		if body["total"] != float64(1) || len(jobs) != 1 || jobs[0].(map[string]interface{})["job_id"] != want {
			t.Fatalf("expected only %s, got %s", want, rec.Body.String())
		}
	}

	// An ownerless (legacy) job is invisible to everyone.
	legacy := &domain.Job{JobID: "job_legacy", PlanID: planID, PlanHash: "h", Status: domain.StatusQueued,
		Stage: "queued", ArtifactsDir: t.TempDir()}
	if _, _, err := e.store.CreateJob(legacy, nil); err != nil {
		t.Fatalf("create legacy job: %v", err)
	}
	expectError(t, e.do("GET", "/api/v1/jobs/job_legacy", "", bearer(alice)), 404, "JOB_NOT_FOUND", "")

	// Alice can cancel her own queued job.
	rec = e.do("POST", "/api/v1/jobs/"+jobA+"/cancel", "", bearer(alice))
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["status"] != "cancelled" {
		t.Fatalf("own cancel failed: %s", rec.Body.String())
	}
}

func TestJobCreateValidation(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("dave", "dave-password")
	planID := e.validatePlan()
	expectError(t, e.do("POST", "/api/v1/jobs", fmt.Sprintf(`{"plan_id":%q,"title":%q}`, planID, strings.Repeat("t", 121)), bearer(tok)),
		422, "TITLE_TOO_LONG", "120")
	expectError(t, e.do("POST", "/api/v1/jobs", fmt.Sprintf(`{"plan_id":%q,"prompt":%q}`, planID, strings.Repeat("p", 4001)), bearer(tok)),
		422, "PROMPT_TOO_LONG", "4000")
	expectError(t, e.do("POST", "/api/v1/jobs", `{"plan_id":"plan_nope"}`, bearer(tok)), 404, "PLAN_NOT_FOUND", "plan_nope")
	expectError(t, e.do("GET", "/api/v1/jobs?limit=0", "", bearer(tok)), 422, "INVALID_LIMIT", "")
	expectError(t, e.do("GET", "/api/v1/jobs?limit=abc", "", bearer(tok)), 422, "INVALID_LIMIT", "")
	expectError(t, e.do("GET", "/api/v1/jobs?offset=-1", "", bearer(tok)), 422, "INVALID_OFFSET", "")
	expectError(t, e.do("GET", "/api/v1/jobs?status=done", "", bearer(tok)), 422, "INVALID_STATUS", "")
}

// markSucceeded simulates a finished worker run with the given summary.json content.
func (e *testEnv) markSucceeded(jobID, summary string) {
	e.t.Helper()
	dir := filepath.Join(e.cfg.ArtifactsDir, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), []byte(summary), 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := e.store.CompleteJob(jobID, domain.StatusSucceeded, nil, nil); err != nil {
		e.t.Fatal(err)
	}
}

const goodSummary = `{"experiment_type": "compare_silencing", "active_neurons_count_A": 86, "active_neurons_count_B": 85, "total_spikes_A": 267,
 "total_spikes_B": 266, "readout_summary": [{"root_id": "720575940645521262", "rate_A_hz": 0.0, "rate_B_hz": 0.0, "delta_hz": 0.0}]}`

func TestHistoryReturnsPlanAndSummary(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("erin", "erin-password")
	planID := e.validatePlan()
	jobID, _ := e.createJob(tok, planID, "Sugar 50 Hz")
	queuedID, _ := e.createJob(tok, planID, "Still queued")
	e.markSucceeded(jobID, goodSummary)

	rec := e.do("GET", "/api/v1/jobs", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	var list struct {
		Jobs []struct {
			JobID        string                 `json:"job_id"`
			Status       string                 `json:"status"`
			Title        *string                `json:"title"`
			Prompt       *string                `json:"prompt"`
			Plan         *domain.ExperimentPlan `json:"plan"`
			Summary      *domain.JobSummary     `json:"summary"`
			SummaryError *string                `json:"summary_error"`
		} `json:"jobs"`
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if list.Total != 2 || list.Limit != 24 || list.Offset != 0 || len(list.Jobs) != 2 {
		t.Fatalf("unexpected paging: %s", rec.Body.String())
	}
	// newest first: the queued job was created last
	if list.Jobs[0].JobID != queuedID || list.Jobs[1].JobID != jobID {
		t.Fatalf("history not newest first: %s", rec.Body.String())
	}
	done := list.Jobs[1]
	if done.Status != "succeeded" || done.Title == nil || *done.Title != "Sugar 50 Hz" || done.Prompt == nil {
		t.Fatalf("title/prompt/status wrong: %s", rec.Body.String())
	}
	if done.Plan == nil || done.Plan.Activation[0].Selector.GroupID != "sugar_grn" || done.Plan.DurationMs != 100 {
		t.Fatalf("plan missing from history: %s", rec.Body.String())
	}
	if done.Summary == nil || done.Summary.TotalSpikesA != 267 || done.Summary.TotalSpikesB == nil || *done.Summary.TotalSpikesB != 266 ||
		done.Summary.ActiveNeuronsCountB != 85 || !strings.Contains(string(done.Summary.ReadoutSummary), "720575940645521262") {
		t.Fatalf("summary wrong: %s", rec.Body.String())
	}
	if done.SummaryError != nil {
		t.Fatalf("healthy summary must have summary_error null, got %q", *done.SummaryError)
	}
	if list.Jobs[0].Summary != nil || list.Jobs[0].SummaryError != nil {
		t.Fatalf("queued job must have summary null and summary_error null: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"summary_error":null`) {
		t.Fatalf("summary_error must be present as null: %s", rec.Body.String())
	}

	// status filter and single-job endpoint use the same shape
	rec = e.do("GET", "/api/v1/jobs?status=succeeded&limit=5", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if b := decode(t, rec); b["total"] != float64(1) || b["limit"] != float64(5) {
		t.Fatalf("status filter wrong: %s", rec.Body.String())
	}
	rec = e.do("GET", "/api/v1/jobs/"+jobID, "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	one := decode(t, rec)
	if one["summary"].(map[string]interface{})["total_spikes_A"] != float64(267) || one["plan"] == nil {
		t.Fatalf("single job lacks summary/plan: %s", rec.Body.String())
	}

	// me stats reflect the jobs
	me := decode(t, e.do("GET", "/api/v1/me", "", bearer(tok)))
	stats := me["stats"].(map[string]interface{})
	if stats["total_jobs"] != float64(2) || stats["succeeded"] != float64(1) || stats["running"] != float64(1) || stats["last_job_at"] == nil {
		t.Fatalf("me stats wrong: %v", stats)
	}
}

func TestHistoryCorruptSummaryIsVisible(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("frank", "frank-password")
	planID := e.validatePlan()
	jobID, _ := e.createJob(tok, planID, "corrupt")
	e.markSucceeded(jobID, `{"total_spikes_A": 267, "total_spikes_B": `) // truncated write

	for _, path := range []string{"/api/v1/jobs", "/api/v1/jobs/" + jobID} {
		rec := e.do("GET", path, "", bearer(tok))
		expectStatus(t, rec, http.StatusOK)
		var job map[string]interface{}
		body := decode(t, rec)
		if jobs, ok := body["jobs"].([]interface{}); ok {
			job = jobs[0].(map[string]interface{})
		} else {
			job = body
		}
		msg, _ := job["summary_error"].(string)
		if !strings.Contains(msg, "summary.json cannot be parsed") || !strings.Contains(msg, "unexpected end of JSON input") {
			t.Fatalf("%s: summary_error must carry the parse message, got %q (%s)", path, msg, rec.Body.String())
		}
		if job["summary"] != nil {
			t.Fatalf("%s: corrupt summary must not produce a summary object", path)
		}
		if job["plan"] == nil {
			t.Fatalf("%s: plan must still be returned", path)
		}
	}

	// results endpoint reports corruption instead of returning summary:null
	expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/results", "", bearer(tok)), 500, "RESULTS_CORRUPT", "cannot be parsed")

	// schema mismatch is also visible
	if err := os.WriteFile(filepath.Join(e.cfg.ArtifactsDir, jobID, "summary.json"), []byte(`{"total_spikes_A": 1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	job := decode(t, e.do("GET", "/api/v1/jobs/"+jobID, "", bearer(tok)))
	if msg, _ := job["summary_error"].(string); !strings.Contains(msg, "lacks fields: total_spikes_B") {
		t.Fatalf("missing fields must be reported, got %q", msg)
	}

	// a succeeded job without summary.json is reported too
	if err := os.Remove(filepath.Join(e.cfg.ArtifactsDir, jobID, "summary.json")); err != nil {
		t.Fatal(err)
	}
	job = decode(t, e.do("GET", "/api/v1/jobs/"+jobID, "", bearer(tok)))
	if msg, _ := job["summary_error"].(string); !strings.Contains(msg, "missing") {
		t.Fatalf("missing summary.json must be reported, got %q", msg)
	}
}

func TestJobResultsAndSpikesForOwner(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("gina", "gina-password")
	planID := e.validatePlan()
	jobID, _ := e.createJob(tok, planID, "rates")
	e.markSucceeded(jobID, goodSummary)
	rates := "condition,trial,root_id,spike_count,rate_hz,is_readout\nA,0,720575940645521262,3,30.0,true\nB,0,720575940645521262,2,20.0,true\n"
	if err := os.WriteFile(filepath.Join(e.cfg.ArtifactsDir, jobID, "rates.csv"), []byte(rates), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := e.do("GET", "/api/v1/jobs/"+jobID+"/results", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["summary"].(map[string]interface{})["total_spikes_A"] != float64(267) {
		t.Fatalf("results wrong: %s", rec.Body.String())
	}

	rec = e.do("GET", "/api/v1/jobs/"+jobID+"/spikes?format=json&condition=B", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	sp := decode(t, rec)
	rows := sp["spikes"].([]interface{})
	if sp["total"] != float64(1) || rows[0].(map[string]interface{})["spike_count"] != float64(2) {
		t.Fatalf("spikes wrong: %s", rec.Body.String())
	}

	// Pagination: a valid limit pages, a malformed limit/offset is a visible 422, never a default.
	rec = e.do("GET", "/api/v1/jobs/"+jobID+"/spikes?format=json&limit=1&offset=1", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	sp = decode(t, rec)
	rows = sp["spikes"].([]interface{})
	if sp["total"] != float64(2) || len(rows) != 1 || rows[0].(map[string]interface{})["condition"] != "B" {
		t.Fatalf("spikes page wrong: %s", rec.Body.String())
	}
	expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/spikes?format=json&limit=abc", "", bearer(tok)), 422, "INVALID_LIMIT", "abc")
	expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/spikes?format=json&limit=0", "", bearer(tok)), 422, "INVALID_LIMIT", "10000")
	expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/spikes?format=json&offset=-1", "", bearer(tok)), 422, "INVALID_OFFSET", "-1")

	if err := os.WriteFile(filepath.Join(e.cfg.ArtifactsDir, jobID, "rates.csv"), []byte(rates+"broken,row\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/spikes?format=json", "", bearer(tok)), 500, "RATES_CORRUPT", "line 4")

	rec = e.do("GET", "/api/v1/jobs/"+jobID+"/export", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "application/zip" || !bytes.HasPrefix(rec.Body.Bytes(), []byte("PK")) {
		t.Fatalf("export is not a zip: %q", rec.Header().Get("Content-Type"))
	}
}

func TestPlanParseThroughClaude(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("hank", "hank-password")

	t.Setenv("FAKE_CLAUDE_MODE", "ready_single")
	rec := e.do("POST", "/api/v1/plans/parse", `{"prompt":"FlyWire: stimulate sugar GRNs at 80 Hz for 200 ms"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	var res struct {
		Status       string                 `json:"status"`
		Plan         *domain.ExperimentPlan `json:"plan"`
		ResolvedPlan *domain.ResolvedPlan   `json:"resolved_plan"`
		LLMMetadata  map[string]interface{} `json:"llm_metadata"`
		LLMError     *string                `json:"llm_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "ready" || res.LLMError != nil || res.LLMMetadata["source"] != "claude" ||
		res.LLMMetadata["model"] != "claude-sonnet-5-5" || res.LLMMetadata["cost_usd"] != 0.0123 || res.LLMMetadata["duration_ms"] != float64(1234) {
		t.Fatalf("unexpected parse response: %s", rec.Body.String())
	}
	if res.Plan.Activation[0].RateHz != 80 || res.ResolvedPlan == nil {
		t.Fatalf("plan not validated/resolved: %s", rec.Body.String())
	}
	// the parsed plan was stored and can be run
	jobID, _ := e.createJob(tok, res.ResolvedPlan.PlanID, "from parse")
	if jobID == "" {
		t.Fatalf("job from parsed plan not created")
	}

	t.Setenv("FAKE_CLAUDE_MODE", "is_error")
	expectError(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"stimulate sugar"}`, bearer(tok)), 502, "LLM_ERROR", "API Error: 529 overloaded")

	t.Setenv("FAKE_CLAUDE_MODE", "malformed")
	expectError(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"stimulate sugar"}`, bearer(tok)), 502, "LLM_ERROR", "malformed structured_output")

	t.Setenv("FAKE_CLAUDE_MODE", "invalid_plan")
	expectError(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"stimulate sugar for 5 s"}`, bearer(tok)), 502, "LLM_ERROR", "duration_ms")

	t.Setenv("FAKE_CLAUDE_MODE", "nonzero")
	expectError(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"stimulate sugar"}`, bearer(tok)), 502, "LLM_ERROR", "authentication failed")

	expectError(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"   "}`, bearer(tok)), 422, "EMPTY_PROMPT", "")
	expectError(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"x","report_language":"de"}`, bearer(tok)), 422, "INVALID_REPORT_LANGUAGE", "")
}

func TestPlanParseMissingClaudeFallsBackVisibly(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.ClaudeBin = "flylab-no-such-claude" })
	tok := e.register("ivan", "ivan-password")
	rec := e.do("POST", "/api/v1/plans/parse", `{"prompt":"Using FlyWire, stimulate sugar GRN at 100 Hz"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	body := decode(t, rec)
	msg, _ := body["llm_error"].(string)
	if !strings.Contains(msg, "claude CLI not found") {
		t.Fatalf("fallback must carry llm_error, got %s", rec.Body.String())
	}
	if body["llm_metadata"].(map[string]interface{})["source"] != "heuristic_fallback" || body["status"] != "ready" {
		t.Fatalf("expected a ready heuristic plan: %s", rec.Body.String())
	}
	if body["status"] == "unsupported" {
		t.Fatalf("FlyWire prompt rejected as unsupported")
	}
}

func TestPlanParseRateLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.ParseRateLimitPerHour = 2 })
	t.Setenv("FAKE_CLAUDE_MODE", "needs_input")
	a := e.register("judy", "judy-password")
	b := e.register("kate", "kate-password")
	for i := 0; i < 2; i++ {
		rec := e.do("POST", "/api/v1/plans/parse", `{"prompt":"turn off inhibitory neurons"}`, bearer(a))
		expectStatus(t, rec, http.StatusOK)
		if decode(t, rec)["status"] != "needs_input" {
			t.Fatalf("needs_input expected: %s", rec.Body.String())
		}
	}
	rec := e.do("POST", "/api/v1/plans/parse", `{"prompt":"again"}`, bearer(a))
	expectError(t, rec, 429, "RATE_LIMITED", "2 requests per hour")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 must carry Retry-After")
	}
	rec = e.do("POST", "/api/v1/plans/parse", `{"prompt":"other user"}`, bearer(b))
	expectStatus(t, rec, http.StatusOK)
	other := decode(t, rec)
	if other["status"] != "needs_input" || other["llm_metadata"].(map[string]interface{})["source"] != "claude" || other["llm_error"] != nil {
		t.Fatalf("another user must get a normal Claude result: %s", rec.Body.String())
	}
	details := decode(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"again"}`, bearer(a)))["error"].(map[string]interface{})["details"].(map[string]interface{})
	if details["scope"] != "user" || details["limit_per_hour"] != float64(2) || details["retry_after_seconds"].(float64) < 1 {
		t.Fatalf("429 details must name scope=user and the limit: %v", details)
	}
}

// flysim writes "total_spikes_B": null for single-condition runs; that is a valid summary.
const singleSummary = `{"experiment_type": "single", "active_neurons_count_A": 40, "active_neurons_count_B": 0,
 "total_spikes_A": 120, "total_spikes_B": null, "readout_summary": [{"root_id": "720575940645521262", "rate_A_hz": 10.0}]}`

func TestHistorySingleRunSummaryHasNullConditionB(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("lena", "lena-password")
	planID := e.validatePlan()
	jobID, _ := e.createJob(tok, planID, "single run")
	e.markSucceeded(jobID, singleSummary)

	rec := e.do("GET", "/api/v1/jobs/"+jobID, "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	job := decode(t, rec)
	if job["summary_error"] != nil {
		t.Fatalf("a single run with total_spikes_B=null must not be an error: %s", rec.Body.String())
	}
	summary, _ := job["summary"].(map[string]interface{})
	if summary == nil || summary["total_spikes_A"] != float64(120) || summary["total_spikes_B"] != nil {
		t.Fatalf("single-run summary wrong: %s", rec.Body.String())
	}
	if _, present := summary["total_spikes_B"]; !present {
		t.Fatalf("total_spikes_B must be present as null: %s", rec.Body.String())
	}

	// The same null in a compare_silencing run is corruption and must be visible.
	bad := strings.Replace(singleSummary, `"single"`, `"compare_silencing"`, 1)
	if err := os.WriteFile(filepath.Join(e.cfg.ArtifactsDir, jobID, "summary.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	job = decode(t, e.do("GET", "/api/v1/jobs/"+jobID, "", bearer(tok)))
	if msg, _ := job["summary_error"].(string); !strings.Contains(msg, "total_spikes_B is null for a compare_silencing run") {
		t.Fatalf("expected a compare_silencing null-B error, got %q", msg)
	}
}
