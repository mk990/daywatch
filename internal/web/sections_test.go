package web

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTrunc(t *testing.T) {
	if got := trunc("short", 10); got != "short" {
		t.Errorf("trunc padded or cut a short string: %q", got)
	}
	if got := trunc("abcdefgh", 3); got != "abc…" {
		t.Errorf("trunc = %q, want %q", got, "abc…")
	}

	// Byte slicing would cut into the middle of a Persian character here and
	// emit invalid UTF-8, which then renders as a replacement glyph.
	got := trunc("خطای سرور رخ داد", 4)
	if got != "خطای…" {
		t.Errorf("trunc = %q, want %q", got, "خطای…")
	}
	if !utf8.ValidString(got) {
		t.Error("truncation produced invalid UTF-8")
	}
}

func TestTruncLongMultibyte(t *testing.T) {
	got := trunc(strings.Repeat("سلام ", 100), 70)
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "…")); n != 70 {
		t.Fatalf("kept %d runes, want 70", n)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation produced invalid UTF-8")
	}
}
