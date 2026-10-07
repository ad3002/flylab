package api

import (
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ad3002/flylab/internal/config"
	"github.com/ad3002/flylab/internal/ratelimit"
)

// ---- client address ----

// clientIP is the address used for rate limiting. X-Real-IP (set by the nginx vhost) is
// trusted only when the TCP peer is loopback, i.e. the request really came through the local
// proxy; otherwise a client could pick its own bucket by sending the header.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" && net.ParseIP(real) != nil {
			return real
		}
	}
	return host
}

// ---- CSRF ----

// csrfCheck guards every state-changing request (anything but GET/HEAD/OPTIONS):
//   - Sec-Fetch-Site, when sent, must be same-origin or none (a sibling *.aglabx.com page is
//     "same-site" and is rejected too: SameSite=Lax would still send it the cookie);
//   - Origin, when sent, must be this host or the configured domain;
//   - the body must be declared application/json. A cross-site <form enctype=text/plain> or a
//     "simple" fetch cannot set that type without a CORS preflight, which this server never
//     authorises with credentials.
//
// ok=false means a response was written.
func (s *Server) csrfCheck(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); site != "" && site != "same-origin" && site != "none" {
		s.writeError(w, r, http.StatusForbidden, "CSRF_REJECTED",
			fmt.Sprintf("cross-site request refused (Sec-Fetch-Site: %s)", site),
			map[string]interface{}{"sec_fetch_site": site})
		return false
	}
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" {
		if !s.originAllowed(origin, r.Host) {
			s.writeError(w, r, http.StatusForbidden, "CSRF_REJECTED",
				fmt.Sprintf("request from origin %q refused; only %s may call this API with a session", origin, s.cfg.Domain),
				map[string]interface{}{"origin": origin})
			return false
		}
	}
	ct := r.Header.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/json" {
		got := ct
		if got == "" {
			got = "(none)"
		}
		s.writeError(w, r, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
			fmt.Sprintf("%s %s requires Content-Type: application/json (got %s)", r.Method, r.URL.Path, got),
			map[string]interface{}{"content_type": ct})
		return false
	}
	return true
}

func (s *Server) originAllowed(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false // includes the opaque "null" origin
	}
	return strings.EqualFold(u.Host, host) || strings.EqualFold(u.Hostname(), s.cfg.Domain)
}

// ---- auth abuse limits ----

const (
	authWindow     = 15 * time.Minute
	registerWindow = time.Hour
	// hashSlotWait is how long a login/register waits for a free password-hash slot before
	// 503 AUTH_BUSY (tests shorten it).
	defaultHashSlotWait = 2 * time.Second
)

// Rate-limit scopes of the auth endpoints (error.details.scope).
const (
	scopeAuthIP     = "ip"
	scopeUsername   = "username"
	scopeRegisterIP = "register_ip"
)

type authGuard struct {
	perIP        *ratelimit.Window // login + register attempts per address
	failsPerUser *ratelimit.Window // failed logins per username
	registerIP   *ratelimit.Window // accounts created per address
	hashSlots    chan struct{}
	hashSlotWait time.Duration
}

func newAuthGuard(cfg *config.Config) *authGuard {
	for name, v := range map[string]int{
		"AUTH_RATE_LIMIT_PER_IP":              cfg.AuthRateLimitPerIP,
		"LOGIN_FAILURES_PER_USERNAME":         cfg.LoginFailuresPerUsername,
		"REGISTER_RATE_LIMIT_PER_IP_PER_HOUR": cfg.RegisterRateLimitPerIPPerHour,
		"PASSWORD_HASH_CONCURRENCY":           cfg.PasswordHashConcurrency,
	} {
		if v < 1 {
			// LoadConfig rejects these; reaching here is a programming error in a caller
			// that built Config by hand, and must not silently disable a limit.
			panic(fmt.Sprintf("api: %s must be >= 1 (got %d)", name, v))
		}
	}
	return &authGuard{
		perIP:        ratelimit.New(cfg.AuthRateLimitPerIP, authWindow),
		failsPerUser: ratelimit.New(cfg.LoginFailuresPerUsername, authWindow),
		registerIP:   ratelimit.New(cfg.RegisterRateLimitPerIPPerHour, registerWindow),
		hashSlots:    make(chan struct{}, cfg.PasswordHashConcurrency),
		hashSlotWait: defaultHashSlotWait,
	}
}

// writeRateLimited writes 429 RATE_LIMITED with Retry-After and the same details shape as
// the parse limiter (limit_per_hour is replaced by limit + window_seconds here).
func (s *Server) writeRateLimited(w http.ResponseWriter, r *http.Request, scope string, lim *ratelimit.Window, retry time.Duration, msg string) {
	secs := ratelimit.RetrySeconds(retry)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	s.writeError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", fmt.Sprintf("%s; retry in %d s", msg, secs),
		map[string]interface{}{
			"scope":               scope,
			"limit":               lim.Limit(),
			"window_seconds":      int(lim.Period().Seconds()),
			"retry_after_seconds": secs,
		})
}

// acquireHashSlot bounds concurrent PBKDF2 work; false means a 503 was written.
func (s *Server) acquireHashSlot(w http.ResponseWriter, r *http.Request) (func(), bool) {
	timer := time.NewTimer(s.auth.hashSlotWait)
	defer timer.Stop()
	select {
	case s.auth.hashSlots <- struct{}{}:
		return func() { <-s.auth.hashSlots }, true
	case <-timer.C:
		w.Header().Set("Retry-After", "2")
		s.writeError(w, r, http.StatusServiceUnavailable, "AUTH_BUSY",
			"too many sign-in requests are being processed; try again in a few seconds",
			map[string]interface{}{"concurrency": cap(s.auth.hashSlots), "retry_after_seconds": 2})
		return nil, false
	case <-r.Context().Done():
		s.writeError(w, r, http.StatusServiceUnavailable, "AUTH_BUSY", "request cancelled while waiting to check the password", nil)
		return nil, false
	}
}
