package alert

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mk/daywatch/internal/store"
)

func testEvaluator(baseURL string) *Evaluator {
	return &Evaluator{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		baseURL: baseURL,
		client:  &http.Client{Timeout: 2 * time.Second},
	}
}

func TestMessage(t *testing.T) {
	e := testEvaluator("http://panel.example")
	r := store.AlertRule{Name: "5xx spike", RecordType: "request", StatusClass: "err", Threshold: 5, WindowMinutes: 10}

	msg := e.message(r, 12, false)
	for _, want := range []string{"5xx spike", "12", "error", "request", "10m", "threshold 5", "http://panel.example"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "[TEST]") {
		t.Fatal("non-test message marked as test")
	}
	if !strings.HasPrefix(e.message(r, 0, true), "[TEST]") {
		t.Fatal("test message not marked")
	}
}

func TestDeliverFormats(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content type = %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	e := testEvaluator("")
	cases := []struct {
		format string
		key    string
	}{
		{"slack", "text"},
		{"discord", "content"},
		{"json", "message"},
	}
	for _, c := range cases {
		got = nil
		r := store.AlertRule{Name: "r", ChannelURL: srv.URL, ChannelFormat: c.format}
		if err := e.deliver(context.Background(), r, "hello"); err != nil {
			t.Fatalf("%s: %v", c.format, err)
		}
		if _, ok := got[c.key]; !ok {
			t.Fatalf("%s payload missing %q key: %v", c.format, c.key, got)
		}
	}

	// Telegram includes the chat id.
	got = nil
	r := store.AlertRule{Name: "r", ChannelURL: srv.URL, ChannelFormat: "telegram", TelegramChatID: "-10042"}
	if err := e.deliver(context.Background(), r, "hi"); err != nil {
		t.Fatal(err)
	}
	if got["chat_id"] != "-10042" || got["text"] != "hi" {
		t.Fatalf("telegram payload wrong: %v", got)
	}
}

func TestDeliverNtfy(t *testing.T) {
	var (
		body    string
		headers http.Header
		user    string
		pass    string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, headers = string(b), r.Header.Clone()
		user, pass, _ = r.BasicAuth()
	}))
	defer srv.Close()

	e := testEvaluator("http://panel.example")
	r := store.AlertRule{
		Name: "5xx spike", App: "shop", StatusClass: "err",
		ChannelURL: srv.URL + "/daywatch", ChannelFormat: "ntfy",
		AuthUser: "alice", AuthPass: "s3cret",
	}
	if err := e.deliver(context.Background(), r, "hello"); err != nil {
		t.Fatal(err)
	}
	if body != "hello" {
		t.Fatalf("body = %q, want the raw message", body)
	}
	if ct := headers.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}
	if got := headers.Get("X-Title"); got != "Daywatch: 5xx spike [shop]" {
		t.Errorf("X-Title = %q", got)
	}
	if got := headers.Get("X-Priority"); got != "high" {
		t.Errorf("X-Priority = %q, want high for an error rule", got)
	}
	if got := headers.Get("X-Click"); got != "http://panel.example" {
		t.Errorf("X-Click = %q", got)
	}
	if user != "alice" || pass != "s3cret" {
		t.Errorf("basic auth = %q/%q", user, pass)
	}

	// A password without a username is a bearer token (ntfy access token).
	r.AuthUser, r.AuthPass = "", "tk_abc123"
	if err := e.deliver(context.Background(), r, "hello"); err != nil {
		t.Fatal(err)
	}
	if got := headers.Get("Authorization"); got != "Bearer tk_abc123" {
		t.Errorf("Authorization = %q", got)
	}

	// Unicode titles are RFC 2047 encoded so ntfy renders them correctly.
	r.Name = "خطای سرور"
	if err := e.deliver(context.Background(), r, "hello"); err != nil {
		t.Fatal(err)
	}
	got := headers.Get("X-Title")
	if !strings.HasPrefix(got, "=?UTF-8?") {
		t.Errorf("X-Title = %q, want RFC 2047 encoding", got)
	}
	if dec, err := new(mime.WordDecoder).DecodeHeader(got); err != nil || !strings.Contains(dec, "خطای سرور") {
		t.Errorf("X-Title %q does not decode back to the rule name (%v)", got, err)
	}
}

func TestDeliverNoAuthHeaderWhenUnset(t *testing.T) {
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
	}))
	defer srv.Close()

	e := testEvaluator("")
	r := store.AlertRule{Name: "r", ChannelURL: srv.URL, ChannelFormat: "json"}
	if err := e.deliver(context.Background(), r, "x"); err != nil {
		t.Fatal(err)
	}
	if got := headers.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want none", got)
	}
}

func TestDeliverFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := testEvaluator("")
	r := store.AlertRule{Name: "r", ChannelURL: srv.URL, ChannelFormat: "json"}
	if err := e.deliver(context.Background(), r, "x"); err == nil {
		t.Fatal("expected error for 500 webhook response")
	}
}
