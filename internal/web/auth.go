package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const sessionCookie = "daywatch_session"
const sessionTTL = 7 * 24 * time.Hour

// Login rate limiting: after loginMaxFailures failed attempts from one IP
// within loginFailWindow, further attempts get 429 until the window passes.
const (
	loginMaxFailures = 5
	loginFailWindow  = 15 * time.Minute
)

type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string][]time.Time{}}
}

func (l *loginLimiter) prune(ip string, now time.Time) []time.Time {
	kept := l.failures[ip][:0]
	for _, t := range l.failures[ip] {
		if now.Sub(t) < loginFailWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, ip)
		return nil
	}
	l.failures[ip] = kept
	return kept
}

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip, time.Now())) >= loginMaxFailures
}

func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.prune(ip, now)
	l.failures[ip] = append(l.failures[ip], now)
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// secureRequest reports whether the client connection is HTTPS, directly
// or via a trusted reverse proxy setting X-Forwarded-Proto.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// AuthConfig protects the panel. Empty Username disables authentication.
type AuthConfig struct {
	Username string
	Password string
	Secret   []byte
}

func (a AuthConfig) Enabled() bool { return a.Username != "" }

func (a AuthConfig) check(username, password string) bool {
	// Hash before comparing so timing doesn't leak credential lengths.
	wantUser := sha256.Sum256([]byte(a.Username))
	wantPass := sha256.Sum256([]byte(a.Password))
	gotUser := sha256.Sum256([]byte(username))
	gotPass := sha256.Sum256([]byte(password))
	userOK := subtle.ConstantTimeCompare(wantUser[:], gotUser[:])
	passOK := subtle.ConstantTimeCompare(wantPass[:], gotPass[:])
	return userOK&passOK == 1
}

func (s *Server) issueToken() (string, error) {
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   s.auth.Username,
		Issuer:    "daywatch",
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(sessionTTL)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.auth.Secret)
}

func (s *Server) verifyRequest(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	token, err := jwt.ParseWithClaims(cookie.Value, &jwt.RegisteredClaims{},
		func(t *jwt.Token) (any, error) { return s.auth.Secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer("daywatch"),
		jwt.WithExpirationRequired(),
	)
	return err == nil && token.Valid
}

// requireAuth wraps the panel; login, static assets, and the ingest-side
// health check stay reachable.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Enabled() ||
			r.URL.Path == "/login" ||
			r.URL.Path == "/healthz" ||
			strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		if s.verifyRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		// SSE and fetch clients get a plain 401; browsers get the login page.
		if r.URL.Path == "/events" || r.Header.Get("X-Live-Reload") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.auth.Enabled() || s.verifyRequest(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	var loginError string
	if r.Method == http.MethodPost {
		ip := clientIP(r)
		if s.loginLimiter.blocked(ip) {
			s.log.Warn("login rate limited", "remote", r.RemoteAddr)
			w.WriteHeader(http.StatusTooManyRequests)
			s.tmpl.ExecuteTemplate(w, "login.html", map[string]any{
				"Error": "Too many failed attempts. Try again in a few minutes.",
			})
			return
		}
		if err := r.ParseForm(); err == nil &&
			s.auth.check(r.PostFormValue("username"), r.PostFormValue("password")) {
			token, err := s.issueToken()
			if err != nil {
				httpError(w, s.log, err)
				return
			}
			s.loginLimiter.reset(ip)
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookie,
				Value:    token,
				Path:     "/",
				MaxAge:   int(sessionTTL.Seconds()),
				HttpOnly: true,
				Secure:   secureRequest(r),
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		s.loginLimiter.recordFailure(ip)
		s.log.Warn("failed login attempt", "remote", r.RemoteAddr)
		time.Sleep(500 * time.Millisecond) // dampen brute force
		loginError = "Invalid username or password."
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if loginError != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	if err := s.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": loginError}); err != nil {
		s.log.Error("template render failed", "template", "login.html", "error", err)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
