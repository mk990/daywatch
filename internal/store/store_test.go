package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummarize(t *testing.T) {
	cases := []struct {
		name string
		rec  map[string]any
		want string
	}{
		{"request gets its method", map[string]any{"url": "/orders/12", "method": "GET"}, "GET /orders/12"},
		{"url without a method", map[string]any{"url": "/orders/12"}, "/orders/12"},
		{"query", map[string]any{"sql": "select * from users"}, "select * from users"},
		{"exception prefers the message", map[string]any{"class": "RuntimeException", "message": "boom"}, "boom"},
		{"falls back to the class", map[string]any{"class": "RuntimeException"}, "RuntimeException"},
		{"cache key", map[string]any{"key": "user:1"}, "user:1"},
		{"nothing summarizable", map[string]any{"queries": 3}, ""},
		{"empty values are skipped", map[string]any{"url": "", "message": "fallback"}, "fallback"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := summarize(c.rec); got != c.want {
				t.Fatalf("summarize = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSummarizeTruncatesWithoutSplittingRunes(t *testing.T) {
	long := strings.Repeat("خطای سرور ", 200) // multi-byte, well past the cap
	got := summarize(map[string]any{"message": long})

	if n := utf8.RuneCountInString(got); n != summaryMax {
		t.Fatalf("summary is %d runes, want %d", n, summaryMax)
	}
	if !utf8.ValidString(got) {
		t.Fatal("summary is not valid UTF-8: truncation split a rune")
	}
}

func TestTruncRunes(t *testing.T) {
	if got := truncRunes("abc", 10); got != "abc" {
		t.Fatalf("short string changed: %q", got)
	}
	if got := truncRunes("سلام دنیا", 4); got != "سلام" {
		t.Fatalf("truncRunes = %q, want %q", got, "سلام")
	}
}
