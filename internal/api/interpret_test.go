package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/interpret"
	"github.com/ad3002/flylab/internal/storage"
)

// interpretFixture returns the interpretation test fixtures directory.
func interpretFixture(t *testing.T, parts ...string) string {
	return filepath.Join(append([]string{projectRoot(t), "internal", "interpret", "testdata"}, parts...)...)
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newInterpretEnv: a data dir with the real dataset manifest, the fixture annotations and an
// empty cache dir, and the fake flysim (digest) binary.
func newInterpretEnv(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "dataset")
	if err := os.MkdirAll(filepath.Join(dataDir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(projectRoot(t), "data", "dataset_manifest.json"), filepath.Join(dataDir, "dataset_manifest.json"))
	copyFile(t, interpretFixture(t, "annotations_fixture.tsv"), filepath.Join(dataDir, "annotations_630.tsv"))
	fakeFlysim := interpretFixture(t, "fake_flysim.sh")
	if err := os.Chmod(fakeFlysim, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_FLYSIM_GRAPH", interpretFixture(t, "job_compare", "digest_graph.json"))
	t.Setenv("FAKE_FLYSIM_LOG", filepath.Join(t.TempDir(), "flysim.log"))
	t.Setenv("FAKE_CLAUDE_OUTPUT_FILE", interpretFixture(t, "claude_interpretation.json"))
	return newEnv(t, func(c *config.Config) {
		c.DataDir = dataDir
		c.FlysimBin = fakeFlysim
		if mutate != nil {
			mutate(c)
		}
	})
}

// succeededFixtureJob creates a succeeded job of the user whose artifact dir is a copy of the
// compare fixture (with manifest.json and a placeholder spikes.parquet). skip lists fixture
// files to leave out.
func (e *testEnv) succeededFixtureJob(token, title string, skip ...string) (string, string) {
	e.t.Helper()
	jobID, _ := e.createJob(token, e.validatePlan(), title)
	dir := filepath.Join(e.cfg.ArtifactsDir, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	skipped := map[string]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	src := interpretFixture(e.t, "job_compare")
	entries, err := os.ReadDir(src)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, en := range entries {
		if !skipped[en.Name()] {
			copyFile(e.t, filepath.Join(src, en.Name()), filepath.Join(dir, en.Name()))
		}
	}
	copyFile(e.t, filepath.Join(e.cfg.DataDir, "dataset_manifest.json"), filepath.Join(dir, "manifest.json"))
	if !skipped["spikes.parquet"] {
		if err := os.WriteFile(filepath.Join(dir, "spikes.parquet"), []byte("PAR1 placeholder"), 0o644); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.store.CompleteJob(jobID, domain.StatusSucceeded, nil, nil); err != nil {
		e.t.Fatal(err)
	}
	return jobID, dir
}

func claudeInvocations(t *testing.T, logPath string) int {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(b), "\nSTDIN:") + boolInt(strings.HasPrefix(string(b), "STDIN:"))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type interpretationBody struct {
	Interpretation struct {
		Headline     string `json:"headline"`
		Observations []struct {
			Text     string   `json:"text"`
			Evidence []string `json:"evidence"`
		} `json:"observations"`
		Hypotheses []struct {
			Title      string `json:"title"`
			Confidence string `json:"confidence"`
			Test       struct {
				Plan      map[string]interface{} `json:"plan"`
				PlanID    *string                `json:"plan_id"`
				PlanError *string                `json:"plan_error"`
			} `json:"test"`
		} `json:"hypotheses"`
		SuggestedReading []string `json:"suggested_reading"`
	} `json:"interpretation"`
	Digest struct {
		Totals struct {
			A struct {
				Spikes int `json:"spikes"`
			} `json:"A"`
		} `json:"totals"`
		Coverage struct {
			ActiveAnnotatedShare float64 `json:"active_annotated_share"`
		} `json:"coverage"`
	} `json:"digest"`
	Meta struct {
		Model     string  `json:"model"`
		CostUSD   float64 `json:"cost_usd"`
		Duration  int64   `json:"duration_ms"`
		CreatedAt string  `json:"created_at"`
		Language  string  `json:"language"`
		Cached    bool    `json:"cached"`
	} `json:"meta"`
	Disclaimer       string `json:"disclaimer"`
	EvidenceWarnings []struct {
		Location  string `json:"location"`
		Kind      string `json:"kind"`
		NeuronID  string `json:"neuron_id"`
		Reference string `json:"reference"`
	} `json:"evidence_warnings"`
}

func decodeInterp(t *testing.T, body []byte) interpretationBody {
	t.Helper()
	var out interpretationBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode interpretation: %v: %s", err, body)
	}
	return out
}

func TestInterpretationLifecycle(t *testing.T) {
	logPath := setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, nil)
	tok := e.register("ivan", "ivan-password")
	jobID, _ := e.succeededFixtureJob(tok, "Sugar vs demo silencing")
	otherID, _ := e.succeededFixtureJob(tok, "Other run")
	base := "/api/v1/jobs/" + jobID

	expectError(t, e.do("GET", base+"/interpretation", "", bearer(tok)), 404, "INTERPRETATION_NOT_FOUND", jobID)

	// Deterministic digest, no LLM call.
	rec := e.do("GET", base+"/digest", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	var dg struct {
		JobID  string `json:"job_id"`
		Digest struct {
			Experiment struct {
				Stimulated []struct {
					GroupID string `json:"group_id"`
				} `json:"stimulated"`
			} `json:"experiment"`
			Totals struct {
				A struct{ Spikes int } `json:"A"`
				B struct{ Spikes int } `json:"B"`
			} `json:"totals"`
			Coverage struct {
				ActiveAnnotatedShare float64 `json:"active_annotated_share"`
				Warning              *string `json:"warning"`
			} `json:"coverage"`
			BehaviouralProxies []struct {
				ProxyID string `json:"proxy_id"`
			} `json:"behavioural_proxies"`
		} `json:"digest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dg); err != nil {
		t.Fatal(err)
	}
	if dg.JobID != jobID || dg.Digest.Totals.A.Spikes != 50 || dg.Digest.Totals.B.Spikes != 44 ||
		dg.Digest.Experiment.Stimulated[0].GroupID != "sugar_grn" || dg.Digest.Coverage.ActiveAnnotatedShare != 0.8 ||
		dg.Digest.Coverage.Warning != nil || dg.Digest.BehaviouralProxies[0].ProxyID != "mn9_rostrum_extension" {
		t.Fatalf("digest wrong: %s", rec.Body.String())
	}
	if n := claudeInvocations(t, logPath); n != 0 {
		t.Fatalf("GET /digest must not call claude (%d calls)", n)
	}

	// First interpretation (Russian).
	rec = e.do("POST", base+"/interpretation", `{"language":"ru"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	first := decodeInterp(t, rec.Body.Bytes())
	in := first.Interpretation
	if !strings.HasPrefix(in.Headline, "Гипотеза:") || len(in.Observations) != 1 || len(in.Hypotheses) != 2 {
		t.Fatalf("interpretation content wrong: %s", rec.Body.String())
	}
	h0, h1 := in.Hypotheses[0].Test, in.Hypotheses[1].Test
	if h0.PlanID == nil || !strings.HasPrefix(*h0.PlanID, "plan_") || h0.PlanError != nil {
		t.Fatalf("valid test plan must carry plan_id: %s", rec.Body.String())
	}
	if h1.PlanID != nil || h1.PlanError == nil || !strings.Contains(*h1.PlanError, "duration_ms") || h1.Plan["duration_ms"] != float64(5000) {
		t.Fatalf("invalid test plan must be kept with plan_error: %s", rec.Body.String())
	}
	if first.Meta.Model != "claude-opus-5-5" || first.Meta.CostUSD != 0.0123 || first.Meta.Duration != 1234 ||
		first.Meta.Language != "ru" || first.Meta.Cached || first.Meta.CreatedAt == "" {
		t.Fatalf("meta wrong: %+v", first.Meta)
	}
	if !strings.HasPrefix(first.Disclaimer, "Гипотезы, сгенерированные ИИ") {
		t.Fatalf("disclaimer must be the fixed Russian text: %q", first.Disclaimer)
	}
	if first.Digest.Totals.A.Spikes != 50 {
		t.Fatalf("response must include the digest the AI saw: %s", rec.Body.String())
	}
	kinds := map[string]bool{}
	for _, w := range first.EvidenceWarnings {
		kinds[w.Kind+":"+w.NeuronID+w.Reference] = true
	}
	if !kinds["unknown_neuron_id:720575940999999999"] || !kinds["unknown_reference:Invented et al. 2023. doi:10.1000/fake.1"] || len(first.EvidenceWarnings) != 2 {
		t.Fatalf("evidence warnings wrong: %+v", first.EvidenceWarnings)
	}

	// The claude invocation: planner flags, interpretation model, Russian prompt, digest via stdin.
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(logData)
	for _, want := range []string{
		"ARG:-p\n", "ARG:--model\nARG:claude-opus-5-5\n", "ARG:--tools\nARG:\n", "ARG:--no-session-persistence\n",
		"ARG:--output-format\nARG:json\n", "ARG:--json-schema\n", "HYPOTHESES FOR EXPERT REVIEW", "in Russian.",
		"STDIN:Run title: Sugar vs demo silencing\n", "Original request of the user: stimulate sugar GRNs", "Digest (JSON):\n{",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("claude invocation lacks %q:\n%.3000s", want, log)
		}
	}
	if strings.Contains(log, "ARG:Run title") {
		t.Fatalf("the digest must go through stdin, not argv")
	}

	// The test plan is runnable as a new job, which carries that plan and the hypothesis title.
	rec = e.do("POST", "/api/v1/jobs", `{"plan_id":"`+*h0.PlanID+`","title":"Test: GABA relay"}`, bearer(tok))
	expectStatus(t, rec, http.StatusAccepted)
	newJob, _ := decode(t, rec)["job"].(map[string]interface{})
	newID, _ := newJob["job_id"].(string)
	got := decode(t, e.do("GET", "/api/v1/jobs/"+newID, "", bearer(tok)))
	gotPlan, _ := got["plan"].(map[string]interface{})
	if got["plan_id"] != *h0.PlanID || got["title"] != "Test: GABA relay" || gotPlan["experiment_type"] != "compare_silencing" || gotPlan["duration_ms"] != float64(200) {
		t.Fatalf("job from the hypothesis test must carry its plan and title: %v", got)
	}

	// Cached unless regenerate.
	rec = e.do("POST", base+"/interpretation", `{"language":"en"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	cached := decodeInterp(t, rec.Body.Bytes())
	if !cached.Meta.Cached || cached.Meta.Language != "ru" || cached.Meta.CreatedAt != first.Meta.CreatedAt ||
		cached.Interpretation.Headline != in.Headline || len(cached.EvidenceWarnings) != 2 {
		t.Fatalf("second POST must return the cached interpretation: %s", rec.Body.String())
	}
	if n := claudeInvocations(t, logPath); n != 1 {
		t.Fatalf("cached interpretation must not call claude again (calls=%d)", n)
	}
	rec = e.do("GET", base+"/interpretation", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if got := decodeInterp(t, rec.Body.Bytes()); !got.Meta.Cached || got.Interpretation.Headline != in.Headline || got.Meta.Model != "claude-opus-5-5" {
		t.Fatalf("GET interpretation wrong: %s", rec.Body.String())
	}

	// History marks the interpreted job only.
	rec = e.do("GET", "/api/v1/jobs", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	var list struct {
		Jobs []struct {
			JobID             string `json:"job_id"`
			HasInterpretation *bool  `json:"has_interpretation"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	var langs struct {
		Jobs []struct {
			JobID string  `json:"job_id"`
			Lang  *string `json:"interpretation_language"`
		} `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &langs); err != nil {
		t.Fatal(err)
	}
	for _, j := range langs.Jobs {
		if (j.JobID == jobID && (j.Lang == nil || *j.Lang != "ru")) || (j.JobID == otherID && j.Lang != nil) {
			t.Fatalf("interpretation_language must be the stored language (null without one): %s", rec.Body.String())
		}
	}
	seen := 0
	for _, j := range list.Jobs {
		if j.HasInterpretation == nil {
			t.Fatalf("has_interpretation must always be present: %s", rec.Body.String())
		}
		switch j.JobID {
		case jobID:
			seen++
			if !*j.HasInterpretation {
				t.Fatalf("interpreted job must have has_interpretation true")
			}
		case otherID:
			seen++
			if *j.HasInterpretation {
				t.Fatalf("job without interpretation must have has_interpretation false")
			}
		}
	}
	if seen != 2 {
		t.Fatalf("history lacks the jobs: %s", rec.Body.String())
	}
	one := decode(t, e.do("GET", base, "", bearer(tok)))
	if one["has_interpretation"] != true {
		t.Fatalf("GET job must report has_interpretation: %v", one)
	}

	// Regenerate in English replaces the stored one.
	rec = e.do("POST", base+"/interpretation", `{"language":"en","regenerate":true}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	regen := decodeInterp(t, rec.Body.Bytes())
	if regen.Meta.Cached || regen.Meta.Language != "en" || !strings.HasPrefix(regen.Disclaimer, "AI-generated hypotheses about a computational model.") {
		t.Fatalf("regenerate must produce a fresh English interpretation: %s", rec.Body.String())
	}
	if n := claudeInvocations(t, logPath); n != 2 {
		t.Fatalf("regenerate must call claude once more (calls=%d)", n)
	}
	if got := decodeInterp(t, e.do("GET", base+"/interpretation", "", bearer(tok)).Body.Bytes()); got.Meta.Language != "en" {
		t.Fatalf("the regenerated interpretation must replace the stored one, got language %s", got.Meta.Language)
	}

	// Capabilities report the annotations and the interpretation model.
	caps := decode(t, e.do("GET", "/capabilities", ""))
	if caps["annotations_ready"] != true || caps["annotations_count"] != float64(6) || caps["interpret_model"] != "claude-opus-5-5" || caps["interpret_ready"] != true {
		t.Fatalf("capabilities wrong: %v", caps)
	}
}

func TestInterpretationFailuresAreVisibleAndNotCached(t *testing.T) {
	setClaudeMode(t, "is_error")
	e := newInterpretEnv(t, nil)
	tok := e.register("judy", "judy-password")
	bob := e.register("bobby", "bobby-password")
	jobID, _ := e.succeededFixtureJob(tok, "run")
	base := "/api/v1/jobs/" + jobID

	expectError(t, e.do("POST", base+"/interpretation", `{"language":"en"}`, bearer(tok)), 502, "LLM_ERROR", "529 overloaded")
	expectError(t, e.do("GET", base+"/interpretation", "", bearer(tok)), 404, "INTERPRETATION_NOT_FOUND", "")

	t.Setenv("FAKE_CLAUDE_MODE", "malformed")
	expectError(t, e.do("POST", base+"/interpretation", `{}`, bearer(tok)), 502, "LLM_ERROR", "malformed structured_output")
	expectError(t, e.do("GET", base+"/interpretation", "", bearer(tok)), 404, "INTERPRETATION_NOT_FOUND", "")

	expectError(t, e.do("POST", base+"/interpretation", `{"language":"de"}`, bearer(tok)), 422, "INVALID_LANGUAGE", "de")
	expectError(t, e.do("POST", base+"/interpretation", `{"lang":"en"}`, bearer(tok)), 422, "INVALID_REQUEST_BODY", "lang")

	// Owner only: another user gets 404 JOB_NOT_FOUND everywhere.
	for _, rt := range []struct{ method, path, body string }{
		{"POST", base + "/interpretation", `{"language":"en"}`},
		{"GET", base + "/interpretation", ""},
		{"GET", base + "/digest", ""},
	} {
		expectError(t, e.do(rt.method, rt.path, rt.body, bearer(bob)), 404, "JOB_NOT_FOUND", jobID)
	}

	// Not succeeded: 409.
	queuedID, _ := e.createJob(tok, e.validatePlan(), "queued")
	expectError(t, e.do("POST", "/api/v1/jobs/"+queuedID+"/interpretation", `{"language":"en"}`, bearer(tok)), 409, "JOB_NOT_COMPLETED", "queued")
	expectError(t, e.do("GET", "/api/v1/jobs/"+queuedID+"/digest", "", bearer(tok)), 409, "JOB_NOT_COMPLETED", "queued")

	// A missing claude CLI is an LLM_ERROR for interpretations (no heuristic fallback).
	e2 := newInterpretEnv(t, func(c *config.Config) { c.ClaudeBin = "flylab-no-such-claude" })
	tok2 := e2.register("kate", "kate-password")
	job2, _ := e2.succeededFixtureJob(tok2, "run")
	expectError(t, e2.do("POST", "/api/v1/jobs/"+job2+"/interpretation", `{"language":"en"}`, bearer(tok2)), 502, "LLM_ERROR", "claude CLI not found")
}

func TestInterpretationOldRunGetsDigestLazily(t *testing.T) {
	logPath := setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, nil)
	tok := e.register("leo", "leo-password")
	jobID, dir := e.succeededFixtureJob(tok, "pre-v3 run", "digest_graph.json")
	flysimLog := os.Getenv("FAKE_FLYSIM_LOG")

	rec := e.do("POST", "/api/v1/jobs/"+jobID+"/interpretation", `{"language":"en"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if got := decodeInterp(t, rec.Body.Bytes()); got.Digest.Totals.A.Spikes != 50 || got.Interpretation.Headline == "" {
		t.Fatalf("old run interpretation wrong: %s", rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "digest_graph.json")); err != nil {
		t.Fatalf("digest_graph.json must be generated and cached in the run dir: %v", err)
	}
	logData, err := os.ReadFile(flysimLog)
	if err != nil {
		t.Fatalf("flysim digest was not invoked: %v", err)
	}
	for _, want := range []string{"CALL:digest --resolved-plan " + filepath.Join(dir, "resolved_plan.json"),
		"--spikes " + filepath.Join(dir, "spikes.parquet"), "--output " + filepath.Join(dir, "digest_graph.json"),
		"--cache-dir " + filepath.Join(e.cfg.DataDir, "cache")} {
		if !strings.Contains(string(logData), want) {
			t.Fatalf("flysim invocation lacks %q: %s", want, logData)
		}
	}
	// The cached graph digest is reused.
	rec = e.do("GET", "/api/v1/jobs/"+jobID+"/digest", "", bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if d := decodeDigest(t, rec); d.JobID != jobID || d.Digest.Totals.A.Spikes != 50 || d.Digest.Totals.B.Spikes != 44 || len(d.Digest.Warnings) != 2 {
		t.Fatalf("GET /digest of the old run: %s", rec.Body.String())
	}
	logData, _ = os.ReadFile(flysimLog)
	if n := strings.Count(string(logData), "CALL:digest"); n != 1 {
		t.Fatalf("flysim digest must run once per run, ran %d times", n)
	}
	if claudeInvocations(t, logPath) != 1 {
		t.Fatalf("expected exactly one claude call")
	}
}

func TestInterpretationMissingSpikesIsVisibleDigestError(t *testing.T) {
	logPath := setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, nil)
	tok := e.register("mona", "mona-password")
	jobID, _ := e.succeededFixtureJob(tok, "spikes lost", "digest_graph.json", "spikes.parquet")

	for _, rt := range []struct{ method, path, body string }{
		{"GET", "/api/v1/jobs/" + jobID + "/digest", ""},
		{"POST", "/api/v1/jobs/" + jobID + "/interpretation", `{"language":"en"}`},
	} {
		rec := e.do(rt.method, rt.path, rt.body, bearer(tok))
		expectError(t, rec, 500, "DIGEST_ERROR", "spikes.parquet is missing")
		body := decode(t, rec)
		if msg, _ := body["digest_error"].(string); !strings.Contains(msg, "spikes.parquet is missing for this succeeded run") {
			t.Fatalf("%s %s: digest_error must name the missing file, got %s", rt.method, rt.path, rec.Body.String())
		}
	}
	if claudeInvocations(t, logPath) != 0 {
		t.Fatalf("claude must not be called without a digest")
	}

	// A failing flysim digest is visible too, with its stderr.
	jobID2, _ := e.succeededFixtureJob(tok, "flysim breaks", "digest_graph.json")
	t.Setenv("FAKE_FLYSIM_FAIL", "spikes reference 1 root id(s) that are not in the connectome graph: 42")
	rec := e.do("GET", "/api/v1/jobs/"+jobID2+"/digest", "", bearer(tok))
	expectError(t, rec, 500, "DIGEST_ERROR", "flysim digest failed")
	if msg, _ := decode(t, rec)["digest_error"].(string); !strings.Contains(msg, "not in the connectome graph: 42") {
		t.Fatalf("digest_error must carry flysim's stderr: %s", rec.Body.String())
	}
}

func TestInterpretationRateLimit(t *testing.T) {
	logPath := setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, func(c *config.Config) { c.InterpretRateLimitPerHour = 1 })
	tok := e.register("nina", "nina-password")
	jobID, _ := e.succeededFixtureJob(tok, "run")
	rec := e.do("POST", "/api/v1/jobs/"+jobID+"/interpretation", `{"language":"en"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	first := decodeInterp(t, rec.Body.Bytes())
	// Cached answers are free: the stored one comes back, claude is not called again.
	rec = e.do("POST", "/api/v1/jobs/"+jobID+"/interpretation", `{"language":"en"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if again := decodeInterp(t, rec.Body.Bytes()); first.Meta.Cached || !again.Meta.Cached || again.Meta.CreatedAt != first.Meta.CreatedAt {
		t.Fatalf("second POST must be the cached interpretation: first %+v, again %+v", first.Meta, again.Meta)
	}
	if n := claudeInvocations(t, logPath); n != 1 {
		t.Fatalf("claude calls = %d, want 1", n)
	}
	rec = e.do("POST", "/api/v1/jobs/"+jobID+"/interpretation", `{"language":"en","regenerate":true}`, bearer(tok))
	expectError(t, rec, 429, "RATE_LIMITED", "interpretation limit of 1 per hour")
	details := decode(t, rec)["error"].(map[string]interface{})["details"].(map[string]interface{})
	if details["scope"] != "user" || details["limit_per_hour"] != float64(1) || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("rate-limit details wrong: %v", details)
	}
}

func TestCapabilitiesWithoutAnnotations(t *testing.T) {
	dataDir := t.TempDir()
	copyFile(t, filepath.Join(projectRoot(t), "data", "dataset_manifest.json"), filepath.Join(dataDir, "dataset_manifest.json"))
	e := newEnv(t, func(c *config.Config) { c.DataDir = dataDir })
	caps := decode(t, e.do("GET", "/capabilities", ""))
	if caps["annotations_ready"] != false || caps["annotations_count"] != float64(0) {
		t.Fatalf("missing annotations must be reported: %v", caps)
	}
}

// setClaudeMode selects the fake claude behaviour and returns its invocation log path.
func setClaudeMode(t *testing.T, mode string) string {
	t.Helper()
	t.Setenv("FAKE_CLAUDE_MODE", mode)
	logPath := filepath.Join(t.TempDir(), "claude.log")
	t.Setenv("FAKE_CLAUDE_LOG", logPath)
	return logPath
}

type digestBody struct {
	JobID  string `json:"job_id"`
	Digest struct {
		Totals struct {
			A struct{ Spikes int } `json:"A"`
			B struct{ Spikes int } `json:"B"`
		} `json:"totals"`
		Warnings []string `json:"warnings"`
	} `json:"digest"`
}

func decodeDigest(t *testing.T, rec *httptest.ResponseRecorder) digestBody {
	t.Helper()
	var d digestBody
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode digest: %v: %s", err, rec.Body.String())
	}
	return d
}

func flysimCalls(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("FAKE_FLYSIM_LOG"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return strings.Count(string(b), "CALL:digest")
}

// A cached digest_graph.json that cannot be used is recomputed from spikes.parquet on any
// request (no regenerate needed, so an old run is never stuck); a corrupt one leaves a visible
// warning in the digest, one written by an older flysim does not.
func TestInterpretationRecomputesUnusableGraphDigest(t *testing.T) {
	setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, nil)
	tok := e.register("olga", "olga-password")
	cases := []struct {
		name, content, warning string
	}{
		{"truncated", `{"schema_version": "1.1", "neurons": [`, "could not be used (digest_graph.json cannot be parsed"},
		{"outdated", `{"schema_version": "1.0", "neurons": []}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobID, dir := e.succeededFixtureJob(tok, "old run "+tc.name)
			if err := os.WriteFile(filepath.Join(dir, "digest_graph.json"), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			before := flysimCalls(t)
			rec := e.do("POST", "/api/v1/jobs/"+jobID+"/interpretation", `{"language":"en"}`, bearer(tok))
			expectStatus(t, rec, http.StatusOK)
			var body struct {
				Digest struct {
					SchemaVersion string `json:"schema_version"`
					Totals        struct {
						A struct{ Spikes int } `json:"A"`
					} `json:"totals"`
					Warnings []string `json:"warnings"`
				} `json:"digest"`
				Meta struct {
					Cached bool `json:"cached"`
				} `json:"meta"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Digest.Totals.A.Spikes != 50 || body.Digest.SchemaVersion != "1.1" || body.Meta.Cached {
				t.Fatalf("interpretation after recompute: %s", rec.Body.String())
			}
			if n := flysimCalls(t) - before; n != 1 {
				t.Fatalf("flysim digest must run once to recompute, ran %d times", n)
			}
			joined := strings.Join(body.Digest.Warnings, "|")
			if tc.warning == "" && strings.Contains(joined, "could not be used") {
				t.Fatalf("an outdated cache is an expected upgrade, not a warning: %v", body.Digest.Warnings)
			}
			if tc.warning != "" && !strings.Contains(joined, tc.warning) {
				t.Fatalf("a corrupt cache must leave a visible warning containing %q: %v", tc.warning, body.Digest.Warnings)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "digest_graph.json"))
			if err != nil || !strings.Contains(string(raw), `"readout_inputs"`) {
				t.Fatalf("the recomputed file must replace the bad one: %v %.200s", err, raw)
			}
		})
	}
}

// A stored interpretation row that cannot be parsed is a visible 500 INTERPRETATION_CORRUPT
// with the way out; regenerate:true replaces it.
func TestInterpretationCorruptStoredRow(t *testing.T) {
	logPath := setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, nil)
	tok := e.register("pavel", "pavel-password")
	jobID, _ := e.succeededFixtureJob(tok, "corrupt row")
	if err := e.store.SaveInterpretation(&storage.Interpretation{JobID: jobID, Language: "ru", Model: "m", CreatedAt: time.Now().UTC(),
		DigestJSON: `{}`, ResultJSON: `{"interpretation": tru`}); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/jobs/" + jobID + "/interpretation"
	expectError(t, e.do("GET", base, "", bearer(tok)), 500, "INTERPRETATION_CORRUPT", "is corrupt")
	expectError(t, e.do("POST", base, `{"language":"en"}`, bearer(tok)), 500, "INTERPRETATION_CORRUPT", "regenerate: true")
	if n := claudeInvocations(t, logPath); n != 0 {
		t.Fatalf("a corrupt row without regenerate must not call claude (%d)", n)
	}
	rec := e.do("POST", base, `{"language":"en","regenerate":true}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if got := decodeInterp(t, rec.Body.Bytes()); got.Meta.Cached || got.Meta.Language != "en" || len(got.Interpretation.Hypotheses) != 2 {
		t.Fatalf("regenerate must replace the corrupt row: %s", rec.Body.String())
	}
	if got := decodeInterp(t, e.do("GET", base, "", bearer(tok)).Body.Bytes()); !got.Meta.Cached || got.Meta.Language != "en" {
		t.Fatalf("GET after the replacement: %+v", got.Meta)
	}
}

// Quota is used only when a Claude call starts: digest failures and a busy digest slot are free.
func TestInterpretationQuotaNotUsedWithoutClaudeCall(t *testing.T) {
	logPath := setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, func(c *config.Config) { c.InterpretRateLimitPerHour = 1 })
	tok := e.register("rita", "rita-password")
	broken, _ := e.succeededFixtureJob(tok, "spikes lost", "digest_graph.json", "spikes.parquet")
	for i := 0; i < 2; i++ {
		expectError(t, e.do("POST", "/api/v1/jobs/"+broken+"/interpretation", `{"language":"en"}`, bearer(tok)), 500, "DIGEST_ERROR", "spikes.parquet is missing")
	}

	// A taken digest slot is 503 DIGEST_BUSY (temporary), not a DIGEST_ERROR, and is free too.
	e.interp.SetDigestWait(200 * time.Millisecond)
	t.Setenv("FAKE_FLYSIM_SLEEP", "2")
	slow, _ := e.succeededFixtureJob(tok, "slow digest", "digest_graph.json")
	waiting, _ := e.succeededFixtureJob(tok, "waits for the slot", "digest_graph.json")
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- e.do("GET", "/api/v1/jobs/"+slow+"/digest", "", bearer(tok)) }()
	time.Sleep(500 * time.Millisecond)
	rec := e.do("POST", "/api/v1/jobs/"+waiting+"/interpretation", `{"language":"en"}`, bearer(tok))
	expectError(t, rec, 503, "DIGEST_BUSY", "no digest slot became free")
	if rec.Header().Get("Retry-After") == "" || decode(t, rec)["digest_error"] != nil {
		t.Fatalf("DIGEST_BUSY needs Retry-After and is not a digest_error: %s", rec.Body.String())
	}
	expectStatus(t, <-done, http.StatusOK)
	t.Setenv("FAKE_FLYSIM_SLEEP", "")

	rec = e.do("POST", "/api/v1/jobs/"+waiting+"/interpretation", `{"language":"en"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	if got := decodeInterp(t, rec.Body.Bytes()); got.Meta.Cached || got.Digest.Totals.A.Spikes != 50 {
		t.Fatalf("the valid run must still be interpretable: %s", rec.Body.String())
	}
	if n := claudeInvocations(t, logPath); n != 1 {
		t.Fatalf("claude calls = %d, want 1", n)
	}
	// Now the single unit is used.
	expectError(t, e.do("POST", "/api/v1/jobs/"+waiting+"/interpretation", `{"language":"en","regenerate":true}`, bearer(tok)), 429, "RATE_LIMITED", "limit of 1 per hour")
}

// GET /digest and POST /interpretation on one old run at the same time run flysim once.
func TestInterpretationConcurrentDigestRunsFlysimOnce(t *testing.T) {
	setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, nil)
	tok := e.register("sasha", "sasha-password")
	jobID, _ := e.succeededFixtureJob(tok, "old run", "digest_graph.json")
	t.Setenv("FAKE_FLYSIM_SLEEP", "1")
	var wg sync.WaitGroup
	recs := make([]*httptest.ResponseRecorder, 2)
	wg.Add(2)
	go func() { defer wg.Done(); recs[0] = e.do("GET", "/api/v1/jobs/"+jobID+"/digest", "", bearer(tok)) }()
	go func() {
		defer wg.Done()
		recs[1] = e.do("POST", "/api/v1/jobs/"+jobID+"/interpretation", `{"language":"en"}`, bearer(tok))
	}()
	wg.Wait()
	expectStatus(t, recs[0], http.StatusOK)
	expectStatus(t, recs[1], http.StatusOK)
	if d := decodeDigest(t, recs[0]); d.Digest.Totals.A.Spikes != 50 {
		t.Fatalf("digest: %s", recs[0].Body.String())
	}
	if got := decodeInterp(t, recs[1].Body.Bytes()); got.Digest.Totals.A.Spikes != 50 || got.Interpretation.Headline == "" {
		t.Fatalf("interpretation: %s", recs[1].Body.String())
	}
	if n := flysimCalls(t); n != 1 {
		t.Fatalf("flysim digest ran %d times, want 1", n)
	}
}

// A client that disconnects while Claude works does not lose the paid result: it is stored and
// the next GET finds it.
func TestInterpretationSurvivesClientDisconnect(t *testing.T) {
	logPath := setClaudeMode(t, "from_file")
	t.Setenv("FAKE_CLAUDE_DELAY", "1")
	e := newInterpretEnv(t, nil)
	tok := e.register("tanya", "tanya-password")
	jobID, _ := e.succeededFixtureJob(tok, "reloaded page")
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/v1/jobs/"+jobID+"/interpretation", strings.NewReader(`{"language":"ru"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { e.handler.ServeHTTP(rec, req); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel() // the browser reloads
	<-done
	if n := claudeInvocations(t, logPath); n != 1 {
		t.Fatalf("claude calls = %d", n)
	}
	got := e.do("GET", "/api/v1/jobs/"+jobID+"/interpretation", "", bearer(tok))
	expectStatus(t, got, http.StatusOK)
	if b := decodeInterp(t, got.Body.Bytes()); !b.Meta.Cached || b.Meta.Language != "ru" || !strings.HasPrefix(b.Interpretation.Headline, "Гипотеза:") {
		t.Fatalf("the interpretation must be stored although the client left: %s", got.Body.String())
	}
}

// One POST must finish before nginx gives up (proxy_read_timeout 300 s), or the client gets an
// HTML 504 instead of a JSON answer.
func TestInterpretationWorstCaseFitsProxyTimeout(t *testing.T) {
	cfg := &config.Config{ClaudeInterpretTimeoutSeconds: 180}
	if wc := interpret.WorstCase(cfg); wc >= interpret.ProxyReadTimeout {
		t.Fatalf("worst case %s must stay below nginx proxy_read_timeout %s", wc, interpret.ProxyReadTimeout)
	}
	conf, err := os.ReadFile(filepath.Join(projectRoot(t), "deploy", "nginx-flylab.aglabx.com.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("proxy_read_timeout %ds;", int(interpret.ProxyReadTimeout.Seconds())); !strings.Contains(string(conf), want) {
		t.Fatalf("nginx config lacks %q; keep interpret.ProxyReadTimeout in sync", want)
	}
}

// Stored interpretations get the deterministic test and confidence checks on every read.
func TestInterpretationCarriesTestAndCalibrationWarnings(t *testing.T) {
	setClaudeMode(t, "from_file")
	e := newInterpretEnv(t, nil)
	tok := e.register("uliana", "uliana-password")
	jobID, _ := e.succeededFixtureJob(tok, "checks")
	rec := e.do("POST", "/api/v1/jobs/"+jobID+"/interpretation", `{"language":"ru"}`, bearer(tok))
	expectStatus(t, rec, http.StatusOK)
	var body struct {
		Interpretation struct {
			Hypotheses []struct {
				Confidence         string  `json:"confidence"`
				CalibrationWarning *string `json:"calibration_warning"`
				Test               struct {
					Warnings []string `json:"warnings"`
				} `json:"test"`
			} `json:"hypotheses"`
		} `json:"interpretation"`
	}
	for _, r := range []*httptest.ResponseRecorder{rec, e.do("GET", "/api/v1/jobs/"+jobID+"/interpretation", "", bearer(tok))} {
		if err := json.Unmarshal(r.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		hs := body.Interpretation.Hypotheses
		// fixture H1: low, consistent compare test. H2: medium although "в этом прогоне не
		// проверялось", and a single-run test that expects a delta_hz.
		if len(hs) != 2 || hs[0].Test.Warnings == nil || len(hs[0].Test.Warnings) != 0 || hs[0].CalibrationWarning != nil {
			t.Fatalf("H1 must be clean (warnings [] and calibration_warning null): %s", r.Body.String())
		}
		// the interpretation is Russian, so are the warnings
		if hs[1].CalibrationWarning == nil || !strings.Contains(*hs[1].CalibrationWarning, "не проверялось") ||
			len(hs[1].Test.Warnings) != 1 || !strings.Contains(hs[1].Test.Warnings[0], `опирается на "delta_hz"`) {
			t.Fatalf("H2 must carry a calibration warning and a test warning: %s", r.Body.String())
		}
	}
}
