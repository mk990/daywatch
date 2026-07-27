package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mk/daywatch/internal/store"
)

func postForm(values url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/alerts", strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.ParseForm()
	return r
}

func TestRuleFromForm(t *testing.T) {
	rule := ruleFromForm(postForm(url.Values{
		"name":             {"  5xx spike  "},
		"kind":             {"threshold"},
		"status_class":     {"err"},
		"record_type":      {"request"},
		"threshold":        {"9"},
		"window_minutes":   {"10"},
		"cooldown_minutes": {"20"},
		"channel_url":      {" https://ntfy.example.com/daywatch "},
		"channel_format":   {"ntfy"},
		"auth_user":        {" alice "},
		"auth_pass":        {"s3cret"},
	}))

	if rule.Name != "5xx spike" || rule.AuthUser != "alice" {
		t.Errorf("fields not trimmed: name=%q user=%q", rule.Name, rule.AuthUser)
	}
	if rule.ChannelURL != "https://ntfy.example.com/daywatch" {
		t.Errorf("url = %q", rule.ChannelURL)
	}
	if rule.Threshold != 9 || rule.WindowMinutes != 10 || rule.CooldownMinutes != 20 {
		t.Errorf("numbers = %d/%d/%d", rule.Threshold, rule.WindowMinutes, rule.CooldownMinutes)
	}
	// Create and update own these; the form must not set them.
	if rule.ID != 0 || rule.Enabled {
		t.Errorf("form set ID=%d Enabled=%v", rule.ID, rule.Enabled)
	}
}

func TestRuleFromFormNewException(t *testing.T) {
	rule := ruleFromForm(postForm(url.Values{
		"name":         {"new errors"},
		"kind":         {"new-exception"},
		"status_class": {"warn"},
		"record_type":  {"log"},
		"threshold":    {"50"},
		"channel_url":  {"https://example.com/hook"},
	}))
	if rule.Threshold != 1 || rule.RecordType != "exception" || rule.StatusClass != "" {
		t.Fatalf("new-exception rule not normalized: %+v", rule)
	}
}

func TestMergeAuth(t *testing.T) {
	existing := store.AlertRule{AuthUser: "alice", AuthPass: "stored"}

	// The password field is never prefilled, so blank means "keep it".
	got := mergeAuth(store.AlertRule{AuthUser: "alice"}, existing, false)
	if got.AuthPass != "stored" {
		t.Errorf("blank password dropped the stored one: %q", got.AuthPass)
	}

	// Dropping the username while keeping the password switches an ntfy rule
	// from basic auth to bearer-token auth.
	got = mergeAuth(store.AlertRule{}, existing, false)
	if got.AuthUser != "" || got.AuthPass != "stored" {
		t.Errorf("user=%q pass=%q, want the stored token kept", got.AuthUser, got.AuthPass)
	}

	// A submitted password replaces the stored one.
	got = mergeAuth(store.AlertRule{AuthUser: "bob", AuthPass: "new"}, existing, false)
	if got.AuthPass != "new" {
		t.Errorf("password not replaced: %q", got.AuthPass)
	}

	// Only the explicit checkbox removes credentials.
	got = mergeAuth(store.AlertRule{AuthUser: "alice"}, existing, true)
	if got.AuthUser != "" || got.AuthPass != "" {
		t.Errorf("clear left user=%q pass=%q", got.AuthUser, got.AuthPass)
	}
}

func TestAlertFormNeverExposesPassword(t *testing.T) {
	s := &Server{sections: buildSections()}
	rule := store.AlertRule{ID: 7, Name: "r", AuthUser: "alice", AuthPass: "s3cret"}

	f := s.alertForm(nil, rule, true)
	if f.Rule.AuthPass != "" {
		t.Fatalf("form carries the stored password: %q", f.Rule.AuthPass)
	}
	if !f.HasAuth {
		t.Error("HasAuth = false for a rule with credentials")
	}
	if f.Action != "/alerts/7" || !f.Edit {
		t.Errorf("edit form action = %q, edit = %v", f.Action, f.Edit)
	}

	// A rule whose only credential is a bearer token still counts as having one.
	if !s.alertForm(nil, store.AlertRule{AuthPass: "tk_x"}, true).HasAuth {
		t.Error("HasAuth = false for a bearer-token rule")
	}
	if s.alertForm(nil, newRuleDefaults(""), false).HasAuth {
		t.Error("HasAuth = true for a blank new rule")
	}
}

func TestValidateRule(t *testing.T) {
	ok := store.AlertRule{Name: "r", Kind: "threshold", ChannelURL: "https://example.com/hook", ChannelFormat: "json"}
	if msg := validateRule(ok); msg != "" {
		t.Fatalf("valid rule rejected: %s", msg)
	}

	bad := []struct {
		name string
		rule store.AlertRule
		want string
	}{
		{"no name", store.AlertRule{ChannelURL: "https://x/y", ChannelFormat: "json"}, "name"},
		{"bad url", store.AlertRule{Name: "r", ChannelURL: "ftp://x/y", ChannelFormat: "json"}, "URL"},
		{"bad format", store.AlertRule{Name: "r", ChannelURL: "https://x/y", ChannelFormat: "carrier-pigeon"}, "format"},
		{"telegram without chat", store.AlertRule{Name: "r", ChannelURL: "https://x/y", ChannelFormat: "telegram"}, "chat ID"},
		{"ntfy without topic", store.AlertRule{Name: "r", ChannelURL: "https://ntfy.example.com/", ChannelFormat: "ntfy"}, "topic"},
		{"user without password", store.AlertRule{Name: "r", ChannelURL: "https://x/y", ChannelFormat: "json", AuthUser: "alice"}, "password"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			msg := validateRule(c.rule)
			if !strings.Contains(msg, c.want) {
				t.Fatalf("validateRule = %q, want a message mentioning %q", msg, c.want)
			}
		})
	}
}
