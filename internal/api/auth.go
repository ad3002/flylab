package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ad3002/flylab/internal/auth"
	"github.com/ad3002/flylab/internal/domain"
	"github.com/ad3002/flylab/internal/storage"
)

var errUnauthenticated = errors.New("unauthenticated")

type authedHandler func(w http.ResponseWriter, r *http.Request, user *domain.User)

// sessionToken returns the bearer token if an Authorization header is present, otherwise the
// flylab_session cookie value ("" when neither is sent).
func sessionToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return ""
	}
	if c, err := r.Cookie(auth.CookieName); err == nil {
		return c.Value
	}
	return ""
}

func (s *Server) currentUser(r *http.Request) (*domain.User, error) {
	token := sessionToken(r)
	if token == "" {
		return nil, errUnauthenticated
	}
	user, err := s.store.GetSessionUser(auth.HashToken(token), time.Now().UTC())
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, errUnauthenticated
		}
		return nil, err
	}
	return user, nil
}

func (s *Server) requireAuth(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := s.currentUser(r)
		if err != nil {
			if errors.Is(err, errUnauthenticated) {
				s.writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "Sign in to use this endpoint", nil)
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "SESSION_LOOKUP_FAILED", err.Error(), nil)
			return
		}
		h(w, r, user)
	}
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})
}

// issueSession creates a session row and sets the cookie; it returns the plaintext token.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, user *domain.User) (string, error) {
	token, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	if err := s.store.CreateSession(user.ID, auth.HashToken(token), time.Now().UTC().Add(auth.SessionLifetime)); err != nil {
		return "", err
	}
	s.setSessionCookie(w, r, token, int(auth.SessionLifetime.Seconds()))
	return token, nil
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.RegistrationOpen {
		s.writeError(w, r, http.StatusForbidden, "REGISTRATION_CLOSED",
			"Registration is closed; ask an administrator for an account", nil)
		return
	}
	ip := clientIP(r)
	if ok, retry := s.auth.perIP.Allow(ip, time.Now()); !ok {
		s.writeRateLimited(w, r, scopeAuthIP, s.auth.perIP, retry, "too many sign-in and registration attempts from this network address")
		return
	}
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REQUEST_BODY", err.Error(), nil)
		return
	}
	username, err := auth.NormalizeUsername(body.Username)
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_USERNAME", err.Error(), nil)
		return
	}
	if err := auth.ValidatePassword(body.Password); err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "WEAK_PASSWORD", err.Error(), nil)
		return
	}
	displayName, err := auth.NormalizeDisplayName(body.DisplayName, username)
	if err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_DISPLAY_NAME", err.Error(), nil)
		return
	}
	// Counted only once the request is well-formed, right before the expensive hash, so a
	// typo does not use up a visitor's account budget.
	if ok, retry := s.auth.registerIP.Allow(ip, time.Now()); !ok {
		s.writeRateLimited(w, r, scopeRegisterIP, s.auth.registerIP, retry, "too many accounts created from this network address")
		return
	}
	release, ok := s.acquireHashSlot(w, r)
	if !ok {
		return
	}
	hash, err := auth.HashPassword(body.Password)
	release()
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "PASSWORD_HASH_FAILED", err.Error(), nil)
		return
	}
	user, err := s.store.CreateUser(username, displayName, hash)
	if err != nil {
		if errors.Is(err, storage.ErrUsernameTaken) {
			s.writeError(w, r, http.StatusConflict, "USERNAME_TAKEN", fmt.Sprintf("Username %q is already taken", username), nil)
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "USER_CREATION_FAILED", err.Error(), nil)
		return
	}
	token, err := s.issueSession(w, r, user)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "SESSION_CREATION_FAILED",
			fmt.Sprintf("account created but sign-in failed: %v", err), nil)
		return
	}
	s.writeJSON(w, http.StatusCreated, map[string]interface{}{"user": user, "token": token})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	if ok, retry := s.auth.perIP.Allow(clientIP(r), now); !ok {
		s.writeRateLimited(w, r, scopeAuthIP, s.auth.perIP, retry, "too many sign-in and registration attempts from this network address")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeError(w, r, http.StatusUnprocessableEntity, "INVALID_REQUEST_BODY", err.Error(), nil)
		return
	}
	invalid := func() {
		s.writeError(w, r, http.StatusUnauthorized, "INVALID_CREDENTIALS", "Wrong username or password", nil)
	}
	// Failed attempts are counted per (normalised) username, so guessing one account's
	// password from many addresses is still bounded; checked before any hashing.
	failKey := strings.ToLower(strings.TrimSpace(body.Username))
	if ok, retry := s.auth.failsPerUser.Check(failKey, now); !ok {
		s.writeRateLimited(w, r, scopeUsername, s.auth.failsPerUser, retry,
			"too many failed sign-in attempts for this username")
		return
	}
	fail := func() {
		s.auth.failsPerUser.Record(failKey, time.Now())
		invalid()
	}
	release, ok := s.acquireHashSlot(w, r)
	if !ok {
		return
	}
	defer release()

	username, err := auth.NormalizeUsername(body.Username)
	if err != nil {
		auth.BurnPasswordCheck(body.Password)
		fail()
		return
	}
	user, hash, err := s.store.GetUserCredentials(username)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			auth.BurnPasswordCheck(body.Password)
			fail()
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "LOGIN_FAILED", err.Error(), nil)
		return
	}
	ok, err = auth.VerifyPassword(body.Password, hash)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "PASSWORD_HASH_CORRUPT",
			fmt.Sprintf("stored credentials of %q are unreadable: %v", username, err), nil)
		return
	}
	if !ok {
		fail()
		return
	}
	token, err := s.issueSession(w, r, user)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "SESSION_CREATION_FAILED", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"user": user, "token": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := sessionToken(r); token != "" {
		if err := s.store.DeleteSession(auth.HashToken(token)); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "LOGOUT_FAILED", err.Error(), nil)
			return
		}
	}
	s.setSessionCookie(w, r, "", -1)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, user *domain.User) {
	stats, err := s.store.UserStats(user.ID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "STATS_ERROR", err.Error(), nil)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"user": user, "stats": stats})
}
