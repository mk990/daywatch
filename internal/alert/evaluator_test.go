package alert

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
