package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLivePageCacheAndInvalidation(t *testing.T) {
	s := &Server{livePages: map[string]cachedPage{}}
	var calls atomic.Int64
	handler := s.cacheLivePages(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("fragment"))
	}))

	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/?range=24h", nil)
		r.Header.Set("X-Live-Reload", "1")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := request().Body.String(); got != "fragment" {
		t.Fatalf("first body = %q", got)
	}
	if got := request().Body.String(); got != "fragment" {
		t.Fatalf("cached body = %q", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("handler calls = %d, want 1", calls.Load())
	}

	s.invalidateLivePages()
	request()
	if calls.Load() != 2 {
		t.Fatalf("handler calls after invalidation = %d, want 2", calls.Load())
	}
}

func TestRenderLiveMainFragment(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(nil, log, AuthConfig{})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/apps", nil)
	r.Header.Set("X-Live-Reload", "1")
	w := httptest.NewRecorder()
	s.render(w, r, "apps.html", map[string]any{"Base": baseData{Range: "24h"}})

	body := w.Body.String()
	if w.Header().Get("X-Daywatch-Fragment") != "main" {
		t.Fatal("response was not marked as a main fragment")
	}
	if strings.Contains(body, "<html") || strings.Contains(body, `<main class="content">`) {
		t.Fatalf("fragment contains the outer document: %q", body)
	}
	if !strings.Contains(body, "<h1") || !strings.Contains(body, "Apps") {
		t.Fatalf("fragment is missing page content: %q", body)
	}
}
