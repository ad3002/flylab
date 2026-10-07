package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/config"
)

// When every password-hash slot is taken, login must answer 503 AUTH_BUSY instead of
// queueing unbounded PBKDF2 work that starves the planner and worker.
func TestLoginAuthBusyWhenHashSlotsFull(t *testing.T) {
	cfg := &config.Config{
		Domain: "flylab.aglabx.com", AuthRateLimitPerIP: 100, LoginFailuresPerUsername: 100,
		RegisterRateLimitPerIPPerHour: 100, PasswordHashConcurrency: 1,
	}
	s := &Server{cfg: cfg, auth: newAuthGuard(cfg)}
	s.auth.hashSlotWait = 20 * time.Millisecond
	s.auth.hashSlots <- struct{}{} // another request is hashing

	for _, h := range []func(http.ResponseWriter, *http.Request){s.handleLogin, s.handleRegister} {
		cfg.RegistrationOpen = true
		req := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(`{"username":"someone","password":"some-password"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Error struct {
				Code    string                 `json:"code"`
				Message string                 `json:"message"`
				Details map[string]interface{} `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != "AUTH_BUSY" || body.Error.Details["concurrency"] != float64(1) || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("expected AUTH_BUSY with details and Retry-After: %s", rec.Body.String())
		}
	}
}
