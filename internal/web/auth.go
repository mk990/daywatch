package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const sessionCookie = "daywatch_session"
const sessionTTL = 7 * 24 * time.Hour

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
		if err := r.ParseForm(); err == nil &&
			s.auth.check(r.PostFormValue("username"), r.PostFormValue("password")) {
			token, err := s.issueToken()
			if err != nil {
				httpError(w, s.log, err)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     sessionCookie,
				Value:    token,
				Path:     "/",
				MaxAge:   int(sessionTTL.Seconds()),
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
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
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
