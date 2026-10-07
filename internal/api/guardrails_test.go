package api_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/storage"
)

// waitState polls GET .../interpretation until it reports want.
func (e *testEnv) waitState(tok, jobID, want string) interpState {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		rec := e.do("GET", "/api/v1/jobs/"+jobID+"/interpretation", "", bearer(tok))
		if rec.Code == http.StatusOK {
			if st := decodeState(e.t, rec); st.State == want {
				return st
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("state %s not reached: %d %s", want, rec.Code, rec.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func errorDetails(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var env struct {
		Error struct {
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Error.Details == nil {
		t.Fatalf("error envelope without details: %s", body)
	}
	return env.Error.Details
}

// waitClaudeCalls waits until the fake claude has been started n times.
func waitClaudeCalls(t *testing.T, logPath string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for claudeInvocations(t, logPath) < n {
		if time.Now().After(deadline) {
			t.Fatalf("claude was not started %d times", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Contract v4 section 4: one active request per account, repeat POSTs return it, queue
// positions, the queue cap, and interpretations off the planner's slots.
func TestInterpretationQueueLimitsAndPositions(t *testing.T) {
	logPath := setClaudeMode(t, "from_file")
	t.Setenv("FAKE_CLAUDE_DELAY", "2")
	e := newInterpretEnv(t, func(c *config.Config) { c.InterpretQueueMax = 1; c.LLMMaxConcurrency = 1 })
	e.llm.BusyWait = 100 * time.Millisecond
	a, b, c := e.register("anna", "anna-password"), e.register("boris", "boris-password"), e.register("clara", "clara-password")
	jobA, _ := e.succeededFixtureJob(a, "A")
	jobA2, _ := e.succeededFixtureJob(a, "A2")
	jobB, _ := e.succeededFixtureJob(b, "B")
	jobC, _ := e.succeededFixtureJob(c, "C")

	first := e.enqueue(a, jobA, `{"language":"en"}`)
	running := e.waitState(a, jobA, "running")
	waitClaudeCalls(t, logPath, 1) // A's claude process has started (env changes below cannot reach it)
	if running.Request.ID != first.Request.ID || running.Request.Position != 0 || running.Request.StartedAt == nil {
		t.Fatalf("running request: position 0 and started_at: %+v", running.Request)
	}
	// A repeat POST (also with regenerate) returns the same request instead of queuing a second call.
	for _, body := range []string{`{"language":"en"}`, `{"language":"ru","regenerate":true}`} {
		rec := e.do("POST", "/api/v1/jobs/"+jobA+"/interpretation", body, bearer(a))
		expectStatus(t, rec, http.StatusAccepted)
		if st := decodeState(t, rec); st.Request.ID != first.Request.ID || st.State != "running" || st.Request.Language != "en" {
			t.Fatalf("repeat POST must return the active request: %s", rec.Body.String())
		}
	}
	// Another run of the same account: 409 pointing at the active job.
	rec := e.do("POST", "/api/v1/jobs/"+jobA2+"/interpretation", `{"language":"en"}`, bearer(a))
	expectError(t, rec, 409, "INTERPRETATION_IN_PROGRESS", jobA)
	if d := errorDetails(t, rec.Body.Bytes()); d["job_id"] != jobA || d["request_id"] != float64(first.Request.ID) || d["status"] != "running" {
		t.Fatalf("409 details must point at the active job: %v", d)
	}

	// The planner is not blocked by the running interpretation (separate capacity).
	t.Setenv("FAKE_CLAUDE_MODE", "needs_input")
	t.Setenv("FAKE_CLAUDE_DELAY", "")
	rec = e.do("POST", "/api/v1/plans/parse", `{"prompt":"turn off inhibitory neurons"}`, bearer(c))
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["status"] != "needs_input" {
		t.Fatalf("parse during an interpretation must use the planner's own slot: %s", rec.Body.String())
	}
	t.Setenv("FAKE_CLAUDE_MODE", "from_file")
	t.Setenv("FAKE_CLAUDE_DELAY", "2")

	// Another account queues behind it.
	qb := e.enqueue(b, jobB, `{"language":"en"}`)
	if qb.State != "queued" || qb.Request.Position != 1 || qb.Request.StartedAt != nil {
		t.Fatalf("B must be queued at position 1: %+v", qb.Request)
	}
	if st := decodeState(t, e.do("GET", "/api/v1/jobs/"+jobB+"/interpretation", "", bearer(b))); st.State != "queued" || st.Request.Position != 1 {
		t.Fatalf("GET must show B queued at position 1: %+v", st)
	}
	if h := decode(t, e.do("GET", "/api/v1/jobs/"+jobB, "", bearer(b))); h["interpretation_state"] != "queued" {
		t.Fatalf("history must show interpretation_state queued: %v", h)
	}
	caps := decode(t, e.do("GET", "/capabilities", ""))
	if q, _ := caps["interpret_queue"].(map[string]interface{}); q["queued"] != float64(1) || q["running"] != float64(1) {
		t.Fatalf("capabilities.interpret_queue must be {queued:1, running:1}: %v", caps["interpret_queue"])
	}

	// INTERPRET_QUEUE_MAX=1 queued: the next account gets 503 QUEUE_FULL.
	rec = e.do("POST", "/api/v1/jobs/"+jobC+"/interpretation", `{"language":"en"}`, bearer(c))
	expectError(t, rec, 503, "QUEUE_FULL", "queue is full")
	if d := errorDetails(t, rec.Body.Bytes()); d["queue_max"] != float64(1) || d["queued"] != float64(1) || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("QUEUE_FULL details: %v (Retry-After %q)", d, rec.Header().Get("Retry-After"))
	}

	// Both finish in order; then C fits.
	if rec := e.waitInterpretation(a, jobA); decodeState(t, rec).State != "ready" {
		t.Fatalf("A: %s", rec.Body.String())
	}
	if rec := e.waitInterpretation(b, jobB); decodeState(t, rec).State != "ready" {
		t.Fatalf("B: %s", rec.Body.String())
	}
	t.Setenv("FAKE_CLAUDE_DELAY", "")
	e.interpretReady(c, jobC, `{"language":"en"}`)
	if n := claudeInvocations(t, logPath); n != 4 { // A, B, C + the parse
		t.Fatalf("claude calls = %d, want 4", n)
	}
}

// Contract v4 section 3: AI budget on parse, enqueue and interpretation start; /me and
// /capabilities report it.
func TestAIBudgetExhaustion(t *testing.T) {
	logPath := setClaudeMode(t, "from_file") // every fake call costs $0.0123
	e := newInterpretEnv(t, func(c *config.Config) { c.AIUserDailyBudgetUSD = 0.01; c.AIDailyBudgetUSD = 0.05 })
	u := e.register("ulla", "ulla-password")
	job, _ := e.succeededFixtureJob(u, "budget")
	e.interpretReady(u, job, `{"language":"en"}`)

	// The account's budget is used up: interpret and parse are refused with the details.
	for _, rt := range []struct{ path, body string }{
		{"/api/v1/jobs/" + job + "/interpretation", `{"language":"en","regenerate":true}`},
		{"/api/v1/plans/parse", `{"prompt":"sugar GRNs at 50 Hz"}`},
	} {
		rec := e.do("POST", rt.path, rt.body, bearer(u))
		expectError(t, rec, 429, "AI_BUDGET_EXHAUSTED", "your AI budget of $0.01 per 24 hours is used up ($0.01 spent)")
		d := errorDetails(t, rec.Body.Bytes())
		resets, err := time.Parse(time.RFC3339, d["resets_at"].(string))
		if err != nil || d["scope"] != "user" || d["spent_usd"] != 0.0123 || d["budget_usd"] != 0.01 ||
			resets.Before(time.Now().Add(23*time.Hour)) || resets.After(time.Now().Add(24*time.Hour)) {
			t.Fatalf("%s: budget details wrong: %v", rt.path, d)
		}
		if ra, _ := strconv.Atoi(rec.Header().Get("Retry-After")); ra < 23*3600 {
			t.Fatalf("Retry-After must point at the reset (got %q)", rec.Header().Get("Retry-After"))
		}
	}
	me := decode(t, e.do("GET", "/api/v1/me", "", bearer(u)))
	usage, _ := me["ai_usage"].(map[string]interface{})
	if usage["spent_24h_usd"] != 0.0123 || usage["budget_24h_usd"] != 0.01 || usage["resets_at"] == nil ||
		usage["exhausted"] != true || me["ai_usage_error"] != nil {
		t.Fatalf("/me ai_usage wrong: %v", me)
	}
	v := e.register("vera", "vera-password")
	if fresh := decode(t, e.do("GET", "/api/v1/me", "", bearer(v)))["ai_usage"].(map[string]interface{}); fresh["spent_24h_usd"] != float64(0) ||
		fresh["budget_24h_usd"] != 0.01 || fresh["resets_at"] != nil || fresh["exhausted"] != false {
		t.Fatalf("a fresh account has no usage: %v", fresh)
	}

	// The global budget is re-checked when a queued interpretation starts.
	t.Setenv("FAKE_CLAUDE_DELAY", "2")
	w := e.register("wanda", "wanda-password")
	jobV, _ := e.succeededFixtureJob(v, "V")
	jobW, _ := e.succeededFixtureJob(w, "W")
	e.enqueue(v, jobV, `{"language":"en"}`)
	e.waitState(v, jobV, "running")
	waitClaudeCalls(t, logPath, 2)
	e.enqueue(w, jobW, `{"language":"en"}`) // admitted: $0.0123 of $0.05 spent so far
	if err := e.store.RecordLLMUsage(nil, storage.UsageKindPlanner, 1.0, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := e.waitInterpretation(v, jobV); decodeState(t, rec).State != "ready" {
		t.Fatalf("the running interpretation finishes: %s", rec.Body.String())
	}
	rec := e.waitInterpretation(w, jobW)
	st := decodeState(t, rec)
	if st.State != "failed" || *st.Request.ErrorCode != "AI_BUDGET_EXHAUSTED" || !strings.Contains(*st.Request.ErrorMessage, "server-wide AI budget of $0.05") {
		t.Fatalf("a queued interpretation that hits the budget at start must fail with AI_BUDGET_EXHAUSTED: %s", rec.Body.String())
	}
	if caps := decode(t, e.do("GET", "/capabilities", "")); caps["ai_budget_available"] != false || caps["ai_budget_error"] != nil {
		t.Fatalf("capabilities.ai_budget_available must be false: %v", caps)
	}
	x := e.register("xenia", "xenia-password")
	expectError(t, e.do("POST", "/api/v1/plans/parse", `{"prompt":"sugar"}`, bearer(x)), 429, "AI_BUDGET_EXHAUSTED", "server-wide")
}

// Contract v4 section 3: invite-only registration.
func TestRegistrationInviteMode(t *testing.T) {
	e := newEnv(t, func(c *config.Config) {
		c.RegistrationInviteCode = "fly-invite-2026"
		c.RegisterRateLimitPerIPPerHour = 2
	})
	caps := decode(t, e.do("GET", "/capabilities", ""))
	if caps["registration_mode"] != "invite" || caps["registration_open"] != true {
		t.Fatalf("capabilities must report invite mode: %v", caps)
	}
	expectError(t, e.do("POST", "/api/v1/auth/register", `{"username":"ivy","password":"ivy-password"}`), 403, "INVITE_REQUIRED", "invite code")
	expectError(t, e.do("POST", "/api/v1/auth/register", `{"username":"ivy","password":"ivy-password","invite_code":"  "}`), 403, "INVITE_REQUIRED", "")
	for i := 0; i < 2; i++ {
		expectError(t, e.do("POST", "/api/v1/auth/register", `{"username":"ivy","password":"ivy-password","invite_code":"fly-invite-2025"}`),
			403, "INVALID_INVITE", "not valid")
	}
	// Wrong codes count toward the per-address registration limit (guessing is bounded).
	rec := e.do("POST", "/api/v1/auth/register", `{"username":"ivy","password":"ivy-password","invite_code":"fly-invite-2026"}`)
	if d := rateLimitDetails(t, rec, "register_ip"); d["limit"] != float64(2) {
		t.Fatalf("register_ip details: %v", d)
	}
	if _, _, err := e.store.GetUserCredentials("ivy"); err != storage.ErrNotFound {
		t.Fatalf("no account may be created without a valid invite: %v", err)
	}
	rec = e.do("POST", "/api/v1/auth/register", `{"username":"ivy","password":"ivy-password","invite_code":" fly-invite-2026 "}`, fromAddr("198.51.100.20:1"))
	expectStatus(t, rec, http.StatusCreated)
	if decode(t, rec)["user"].(map[string]interface{})["username"] != "ivy" {
		t.Fatalf("a valid invite registers: %s", rec.Body.String())
	}

	if m := decode(t, newEnv(t, nil).do("GET", "/capabilities", ""))["registration_mode"]; m != "open" {
		t.Fatalf("default mode is open, got %v", m)
	}
	closed := newEnv(t, func(c *config.Config) { c.RegistrationOpen = false; c.RegistrationInviteCode = "x-code" })
	if m := decode(t, closed.do("GET", "/capabilities", ""))["registration_mode"]; m != "closed" {
		t.Fatalf("REGISTRATION_OPEN=false is closed even with an invite code, got %v", m)
	}
	expectError(t, closed.do("POST", "/api/v1/auth/register", `{"username":"zed","password":"zed-password","invite_code":"x-code"}`), 403, "REGISTRATION_CLOSED", "")
}

// Contract v4 section 5: explicit neuron ids must exist in the connectome.
func TestValidateRejectsUnknownNeuronIDs(t *testing.T) {
	e := newEnv(t, nil)
	plan := `{"schema_version":"1.0","dataset_id":"flywire_630","model_id":"shiu_lif_rust","experiment_type":"compare_silencing",
		"activation":[{"selector":{"group_id":"sugar_grn"},"rate_hz":50}],
		"silencing":[{"selector":{"neuron_ids":["720575940000000001","720575940616885538"]}}],
		"readout":[{"selector":{"group_id":"mn9"}}],"duration_ms":100,"repeats":1,"base_seed":42,"report_language":"en"}`
	rec := e.do("POST", "/api/v1/plans/validate", plan)
	expectError(t, rec, 422, "VALIDATION_FAILED", "silencing[0].selector.neuron_ids: 720575940000000001")
	d := errorDetails(t, rec.Body.Bytes())
	ids, _ := d["unknown_neuron_ids"].([]interface{})
	fields, _ := d["fields"].([]interface{})
	if len(ids) != 1 || ids[0] != "720575940000000001" || d["unknown_neuron_ids_total"] != float64(1) ||
		len(fields) != 1 || fields[0] != "silencing[0].selector.neuron_ids" {
		t.Fatalf("details.unknown_neuron_ids wrong: %v", d)
	}
	// The same plan with real ids validates.
	rec = e.do("POST", "/api/v1/plans/validate", strings.Replace(plan, "720575940000000001", "720575940620900446", 1))
	expectStatus(t, rec, http.StatusOK)
	if decode(t, rec)["plan_id"] == nil {
		t.Fatalf("a plan with real ids must validate: %s", rec.Body.String())
	}
}

func TestParseUnknownNeuronIDsIsAQuestion(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.register("quinn", "quinn-password")
	t.Setenv("FAKE_CLAUDE_MODE", "unknown_ids")
	rec := e.do("POST", "/api/v1/plans/parse", `{"prompt":"stimulate 720575940000000001 and 720575940999999999"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	var res struct {
		Status           string                 `json:"status"`
		Message          string                 `json:"message"`
		UnresolvedFields []string               `json:"unresolved_fields"`
		Plan             map[string]interface{} `json:"plan"`
		LLMError         *string                `json:"llm_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != "needs_input" || res.Plan != nil || res.LLMError != nil ||
		strings.Join(res.UnresolvedFields, ",") != "activation[0].selector.neuron_ids" ||
		!strings.Contains(res.Message, "720575940000000001, 720575940999999999") {
		t.Fatalf("unknown ids must be a needs_input question: %s", rec.Body.String())
	}
}
