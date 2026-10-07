package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ad3002/flylab/internal/config"
)

// rateLimitDetails asserts a 429 RATE_LIMITED envelope with Retry-After and returns details.
func rateLimitDetails(t *testing.T, rec *httptest.ResponseRecorder, scope string) map[string]interface{} {
	t.Helper()
	expectError(t, rec, http.StatusTooManyRequests, "RATE_LIMITED", "")
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("429 must carry Retry-After: %v", rec.Header())
	}
	details, _ := decode(t, rec)["error"].(map[string]interface{})["details"].(map[string]interface{})
	if details == nil || details["scope"] != scope {
		t.Fatalf("expected details.scope=%s, got %s", scope, rec.Body.String())
	}
	if secs, _ := details["retry_after_seconds"].(float64); secs < 1 || fmt.Sprint(int(secs)) != rec.Header().Get("Retry-After") {
		t.Fatalf("retry_after_seconds must match Retry-After (%s): %v", rec.Header().Get("Retry-After"), details)
	}
	return details
}

func TestLoginRateLimitPerIP(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.AuthRateLimitPerIP = 3 })
	for i := 0; i < 3; i++ {
		expectError(t, e.do("POST", "/api/v1/auth/login", fmt.Sprintf(`{"username":"guess%d","password":"wrong-pass-1"}`, i)),
			401, "INVALID_CREDENTIALS", "")
	}
	rec := e.do("POST", "/api/v1/auth/login", `{"username":"guess9","password":"wrong-pass-1"}`)
	details := rateLimitDetails(t, rec, "ip")
	if details["limit"] != float64(3) || details["window_seconds"] != float64(900) {
		t.Fatalf("details must carry limit and window: %v", details)
	}
	if msg := decode(t, rec)["error"].(map[string]interface{})["message"].(string); !strings.Contains(msg, "network address") {
		t.Fatalf("message must say why: %q", msg)
	}
	// Register shares the per-address budget.
	rateLimitDetails(t, e.do("POST", "/api/v1/auth/register", `{"username":"newbie","password":"newbie-pass"}`), "ip")

	// Another TCP peer has its own budget; X-Real-IP from a non-proxy peer is ignored.
	expectError(t, e.do("POST", "/api/v1/auth/login", `{"username":"x1","password":"wrong-pass-1"}`, fromAddr("198.51.100.4:5555")),
		401, "INVALID_CREDENTIALS", "")
	rateLimitDetails(t, e.do("POST", "/api/v1/auth/login", `{"username":"x1","password":"wrong-pass-1"}`,
		header("X-Real-IP", "203.0.113.200")), "ip")

	// Behind the local nginx (loopback peer) X-Real-IP selects the client's bucket.
	for i := 0; i < 3; i++ {
		expectError(t, e.do("POST", "/api/v1/auth/login", `{"username":"y","password":"wrong-pass-1"}`,
			fromAddr("127.0.0.1:40000"), header("X-Real-IP", "203.0.113.10")), 401, "INVALID_CREDENTIALS", "")
	}
	rateLimitDetails(t, e.do("POST", "/api/v1/auth/login", `{"username":"y","password":"wrong-pass-1"}`,
		fromAddr("127.0.0.1:40001"), header("X-Real-IP", "203.0.113.10")), "ip")
	expectError(t, e.do("POST", "/api/v1/auth/login", `{"username":"y","password":"wrong-pass-1"}`,
		fromAddr("127.0.0.1:40002"), header("X-Real-IP", "203.0.113.11")), 401, "INVALID_CREDENTIALS", "")
}

func TestLoginLockoutPerUsername(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.LoginFailuresPerUsername = 2 })
	e.register("victim", "victim-password")
	// Failed guesses from different addresses still count against the username.
	for i, addr := range []string{"198.51.100.1:1", "198.51.100.2:1"} {
		expectError(t, e.do("POST", "/api/v1/auth/login", `{"username":"Victim","password":"guess-number-1"}`, fromAddr(addr)),
			401, "INVALID_CREDENTIALS", "")
		_ = i
	}
	rec := e.do("POST", "/api/v1/auth/login", `{"username":"victim","password":"victim-password"}`, fromAddr("198.51.100.3:1"))
	details := rateLimitDetails(t, rec, "username")
	if details["limit"] != float64(2) {
		t.Fatalf("details.limit must be the failure limit: %v", details)
	}
	if strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("a locked username must not get a session: %s", rec.Body.String())
	}
	// Other usernames are unaffected, and a successful login does not count as a failure.
	e.register("bystander", "bystander-pw")
	for i := 0; i < 3; i++ {
		rec := e.do("POST", "/api/v1/auth/login", `{"username":"bystander","password":"bystander-pw"}`)
		expectStatus(t, rec, http.StatusOK)
		if decode(t, rec)["user"].(map[string]interface{})["username"] != "bystander" {
			t.Fatalf("wrong user: %s", rec.Body.String())
		}
	}
}

func TestRegisterRateLimitPerIP(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.RegisterRateLimitPerIPPerHour = 1 })
	e.register("first", "first-password")
	// A malformed request does not use the budget, but a second account does.
	expectError(t, e.do("POST", "/api/v1/auth/register", `{"username":"a b","password":"long-enough"}`), 422, "INVALID_USERNAME", "")
	rec := e.do("POST", "/api/v1/auth/register", `{"username":"second","password":"second-password"}`)
	details := rateLimitDetails(t, rec, "register_ip")
	if details["limit"] != float64(1) || details["window_seconds"] != float64(3600) {
		t.Fatalf("register details wrong: %v", details)
	}
	if _, _, err := e.store.GetUserCredentials("second"); err == nil {
		t.Fatalf("a rate-limited registration must not create the account")
	}
	rec = e.do("POST", "/api/v1/auth/register", `{"username":"second","password":"second-password"}`, fromAddr("198.51.100.9:1"))
	expectStatus(t, rec, http.StatusCreated)
	if decode(t, rec)["user"].(map[string]interface{})["username"] != "second" {
		t.Fatalf("other address must be able to register: %s", rec.Body.String())
	}
}

func TestParseRateLimitPerIPAcrossAccounts(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.ParseRateLimitPerIPPerHour = 1 })
	t.Setenv("FAKE_CLAUDE_MODE", "needs_input")
	a := e.register("acct_a", "acct-a-password")
	b := e.register("acct_b", "acct-b-password")
	rec := e.do("POST", "/api/v1/plans/parse", `{"prompt":"x"}`, bearer(a))
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["status"] != "needs_input" {
		t.Fatalf("first parse must reach the planner: %s", rec.Body.String())
	}
	rec = e.do("POST", "/api/v1/plans/parse", `{"prompt":"x"}`, bearer(b))
	details := rateLimitDetails(t, rec, "ip")
	if details["limit_per_hour"] != float64(1) {
		t.Fatalf("details must carry limit_per_hour: %v", details)
	}
}

func TestParseGlobalBudget(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.ParseGlobalLimitPerHour = 1 })
	t.Setenv("FAKE_CLAUDE_MODE", "needs_input")
	a := e.register("glob_a", "glob-a-password")
	b := e.register("glob_b", "glob-b-password")
	expectStatus(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"x"}`, bearer(a)), http.StatusOK)
	rec := e.do("POST", "/api/v1/plans/parse", `{"prompt":"x"}`, bearer(b), fromAddr("198.51.100.77:1"))
	details := rateLimitDetails(t, rec, "global")
	if details["limit_per_hour"] != float64(1) {
		t.Fatalf("global details wrong: %v", details)
	}
	if msg := decode(t, rec)["error"].(map[string]interface{})["message"].(string); !strings.Contains(msg, "server-wide") {
		t.Fatalf("message must say the budget is server-wide: %q", msg)
	}
}

// A cross-site <form enctype="text/plain"> can produce a JSON-looking body without a CORS
// preflight; it must not be able to log the victim into the attacker's account.
func TestCSRFTextPlainLoginRejected(t *testing.T) {
	e := newEnv(t, nil)
	e.register("attacker", "attackerpw1")
	body := `{"username":"attacker","password":"attackerpw1","x":"="}`
	rec := e.do("POST", "/api/v1/auth/login", body, header("Content-Type", "text/plain"))
	expectError(t, rec, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "application/json")
	for _, c := range rec.Result().Cookies() {
		if c.Name == "flylab_session" {
			t.Fatalf("a rejected request must not set a session cookie: %+v", c)
		}
	}
	// Bodiless POSTs (logout) need the JSON type too, so a form cannot sign victims out.
	expectError(t, e.do("POST", "/api/v1/auth/logout", "", header("Content-Type", "application/x-www-form-urlencoded")),
		http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "")
	// application/json with parameters is fine.
	rec = e.do("POST", "/api/v1/auth/login", body, header("Content-Type", "application/json; charset=utf-8"))
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["user"].(map[string]interface{})["username"] != "attacker" {
		t.Fatalf("json login must still work: %s", rec.Body.String())
	}
}

func TestCSRFForeignOriginAndSecFetchSiteRejected(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("orig", "orig-password")
	planID := e.validatePlan()
	jobBody := fmt.Sprintf(`{"plan_id":%q}`, planID)

	for name, opt := range map[string]reqOpt{
		"sibling origin":  header("Origin", "https://evil.aglabx.com"),
		"foreign origin":  header("Origin", "https://attacker.example"),
		"null origin":     header("Origin", "null"),
		"cross-site":      header("Sec-Fetch-Site", "cross-site"),
		"same-site (sub)": header("Sec-Fetch-Site", "same-site"),
	} {
		rec := e.do("POST", "/api/v1/jobs", jobBody, bearer(tok), opt)
		expectError(t, rec, http.StatusForbidden, "CSRF_REJECTED", "")
		if !strings.Contains(rec.Body.String(), "refused") {
			t.Fatalf("%s: message must explain the refusal: %s", name, rec.Body.String())
		}
	}
	if list := decode(t, e.do("GET", "/api/v1/jobs", "", bearer(tok))); list["total"] != float64(0) {
		t.Fatalf("rejected requests must not create jobs: %v", list)
	}

	// Same-origin requests pass: the configured domain, this host, Sec-Fetch-Site same-origin.
	for _, opts := range [][]reqOpt{
		{header("Origin", "https://flylab.aglabx.com")},
		{header("Origin", "http://example.com")}, // httptest Host
		{header("Sec-Fetch-Site", "same-origin")},
	} {
		rec := e.do("POST", "/api/v1/jobs", jobBody, append([]reqOpt{bearer(tok)}, opts...)...)
		expectStatus(t, rec, http.StatusAccepted)
		if decode(t, rec)["job"].(map[string]interface{})["status"] != "queued" {
			t.Fatalf("same-origin job must be queued: %s", rec.Body.String())
		}
	}
	// GET is not affected.
	rec := e.do("GET", "/api/v1/jobs", "", bearer(tok), header("Origin", "https://attacker.example"))
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["total"] != float64(3) {
		t.Fatalf("expected the 3 same-origin jobs: %s", rec.Body.String())
	}
}

type fakeWorker struct{ err error }

func (f fakeWorker) LastError() error { return f.err }

func TestWorkerErrorIsVisibleInHealthAndCapabilities(t *testing.T) {
	e := newEnv(t, nil)
	e.server.SetWorker(fakeWorker{})
	rec := e.do("GET", "/health", "")
	expectStatus(t, rec, http.StatusOK)
	if h := decode(t, rec); h["status"] != "ok" || h["worker_error"] != nil {
		t.Fatalf("healthy worker: %s", rec.Body.String())
	}

	e.server.SetWorker(fakeWorker{err: fmt.Errorf("job job_1 finished as succeeded but its status could not be saved: disk full")})
	rec = e.do("GET", "/health", "")
	expectStatus(t, rec, http.StatusServiceUnavailable)
	h := decode(t, rec)
	if h["status"] != "degraded" || !strings.Contains(fmt.Sprint(h["worker_error"]), "could not be saved: disk full") {
		t.Fatalf("degraded worker must fail health with the reason: %s", rec.Body.String())
	}
	caps := decode(t, e.do("GET", "/capabilities", ""))
	if !strings.Contains(fmt.Sprint(caps["worker_error"]), "job job_1") || caps["worker_ready"] != false {
		t.Fatalf("capabilities must carry worker_error and worker_ready=false: %v", caps)
	}
}

func TestSpikesRowValidation(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("rows", "rows-password")
	planID := e.validatePlan()
	jobID, _ := e.createJob(tok, planID, "rates")
	e.markSucceeded(jobID, goodSummary)
	path := filepath.Join(e.cfg.ArtifactsDir, jobID, "rates.csv")
	header := "condition,trial,root_id,spike_count,rate_hz,is_readout\n"
	good := "A,0,720575940645521262,3,30.0000,true\n"

	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A corrupt row on a later page and in CSV mode must still be reported.
	write(header + good + "A,0,720575940000000001,abc,NaNx,true\n")
	for _, q := range []string{"", "?format=json&limit=1&offset=0", "?condition=A&limit=1"} {
		expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/spikes"+q, "", bearer(tok)), 500, "RATES_CORRUPT", "line 3 spike_count \"abc\"")
	}
	for content, want := range map[string]string{
		header + good + "A,0,1,2,3.0,true,extra\n": "line 3 has 7 columns",
		header + "A,0,1,2,NaN,true\n":              "rate_hz \"NaN\"",
		header + "A,0,1,2,3.0,yes\n":               "is_readout \"yes\"",
		header + "A,-1,1,2,3.0,true\n":             "trial \"-1\"",
		"cond,trial\n" + good:                      "line 1",
	} {
		write(content)
		expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/spikes", "", bearer(tok)), 500, "RATES_CORRUPT", want)
	}

	// Valid file: CSV mode streams the rows unchanged.
	write(header + good + "B,0,720575940645521262,2,20.0000,false\n")
	rec := e.do("GET", "/api/v1/jobs/"+jobID+"/spikes?condition=B", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if rec.Body.String() != header+"B,0,720575940645521262,2,20.0000,false\n" {
		t.Fatalf("csv output wrong: %q", rec.Body.String())
	}

	// Missing file -> 404; unreadable (a directory in its place) -> 500 RATES_UNREADABLE.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/spikes", "", bearer(tok)), 404, "RATES_NOT_FOUND", jobID)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	expectError(t, e.do("GET", "/api/v1/jobs/"+jobID+"/spikes", "", bearer(tok)), 500, "RATES_UNREADABLE", "cannot be read")
}
