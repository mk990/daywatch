package web

import (
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		auth: AuthConfig{
			Username: "admin",
			Password: "s3cret",
			Secret:   []byte("test-signing-secret"),
		},
		loginLimiter: newLoginLimiter(),
	}
	return s
}

func TestAuthCheck(t *testing.T) {
	a := testServer(t).auth
	if !a.check("admin", "s3cret") {
		t.Fatal("valid credentials rejected")
	}
	for _, c := range [][2]string{{"admin", "wrong"}, {"wrong", "s3cret"}, {"", ""}, {"admin", ""}} {
		if a.check(c[0], c[1]) {
			t.Fatalf("invalid credentials accepted: %v", c)
		}
	}
}

func TestTokenRoundTrip(t *testing.T) {
	s := testServer(t)
	token, err := s.issueToken()
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	if !s.verifyRequest(r) {
		t.Fatal("valid token rejected")
	}

	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(&http.Cookie{Name: sessionCookie, Value: token + "x"})
	if s.verifyRequest(r2) {
		t.Fatal("tampered token accepted")
	}

	// Token signed with a different secret must be rejected.
	other := testServer(t)
	other.auth.Secret = []byte("other-secret")
	foreign, err := other.issueToken()
	if err != nil {
		t.Fatal(err)
	}
	r3 := httptest.NewRequest("GET", "/", nil)
	r3.AddCookie(&http.Cookie{Name: sessionCookie, Value: foreign})
	if s.verifyRequest(r3) {
		t.Fatal("token signed with wrong secret accepted")
	}
}

func TestRequireAuthMiddleware(t *testing.T) {
	s := testServer(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := s.requireAuth(inner)

	// Unauthenticated browser request redirects to login.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("expected redirect to /login, got %d %s", rec.Code, rec.Header().Get("Location"))
	}

	// SSE gets a plain 401.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for /events, got %d", rec.Code)
	}

	// Exempt paths pass through.
	for _, path := range []string{"/login", "/healthz", "/static/style.css"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for exempt %s, got %d", path, rec.Code)
		}
	}

	// Valid session cookie passes through.
	token, _ := s.issueToken()
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid session, got %d", rec.Code)
	}
}

func TestLoginFlow(t *testing.T) {
	s := testServer(t)
	var err error
	s.tmpl, err = templatesForTest()
	if err != nil {
		t.Fatal(err)
	}

	// Wrong password: 401 and no cookie.
	form := url.Values{"username": {"admin"}, "password": {"nope"}}
	r := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleLogin(rec, r)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("bad login: code=%d cookies=%d", rec.Code, len(rec.Result().Cookies()))
	}

	// Correct credentials: redirect with session cookie.
	form = url.Values{"username": {"admin"}, "password": {"s3cret"}}
	r = httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.handleLogin(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login failed: %d", rec.Code)
	}
	var session string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c.Value
			if !c.HttpOnly {
				t.Fatal("session cookie must be HttpOnly")
			}
		}
	}
	if session == "" {
		t.Fatal("no session cookie set")
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	if !s.verifyRequest(r2) {
		t.Fatal("issued session cookie failed verification")
	}
}

func templatesForTest() (tmpl *template.Template, err error) {
	return template.New("").Funcs(template.FuncMap{"icon": icon}).ParseFS(templateFS, "templates/login.html")
}
