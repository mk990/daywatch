package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/mk/daywatch/internal/store"
)

// AlertTester fires a rule immediately (used by the "send test" button).
type AlertTester interface {
	Fire(ctx context.Context, r store.AlertRule, matched int64, test bool)
}

// SetAlertTester wires the evaluator in after construction (avoids an
// import cycle between web and alert).
func (s *Server) SetAlertTester(t AlertTester) { s.alertTester = t }

var alertFormats = []string{"json", "slack", "discord", "telegram"}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	base, _ := s.base(r, "alerts")

	rules, err := s.store.ListAlertRules(r.Context())
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	events, err := s.store.ListAlertEvents(r.Context(), 50)
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	s.render(w, "alerts.html", map[string]any{
		"Base":    base,
		"Rules":   rules,
		"Events":  events,
		"Types":   s.sections,
		"Apps":    s.apps,
		"Formats": alertFormats,
		"Error":   r.URL.Query().Get("error"),
	})
}

func (s *Server) handleAlertCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/alerts?error="+url.QueryEscape("invalid form"), http.StatusSeeOther)
		return
	}

	rule := store.AlertRule{
		Name:            strings.TrimSpace(r.PostFormValue("name")),
		Enabled:         true,
		App:             r.PostFormValue("app"),
		RecordType:      r.PostFormValue("record_type"),
		StatusClass:     r.PostFormValue("status_class"),
		Threshold:       formInt(r, "threshold", 1, 1, 1_000_000),
		WindowMinutes:   formInt(r, "window_minutes", 5, 1, 1440),
		CooldownMinutes: formInt(r, "cooldown_minutes", 15, 1, 10080),
		ChannelURL:      strings.TrimSpace(r.PostFormValue("channel_url")),
		ChannelFormat:   r.PostFormValue("channel_format"),
		TelegramChatID:  strings.TrimSpace(r.PostFormValue("telegram_chat_id")),
	}

	if rule.App != "" && !slices.Contains(s.apps, rule.App) {
		http.Redirect(w, r, "/alerts?error="+url.QueryEscape("unknown app"), http.StatusSeeOther)
		return
	}
	if msg := validateRule(rule); msg != "" {
		http.Redirect(w, r, "/alerts?error="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	if err := s.store.CreateAlertRule(r.Context(), rule); err != nil {
		httpError(w, s.log, err)
		return
	}
	http.Redirect(w, r, "/alerts", http.StatusSeeOther)
}

func validateRule(r store.AlertRule) string {
	if r.Name == "" {
		return "name is required"
	}
	u, err := url.Parse(r.ChannelURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "webhook URL must be a valid http(s) URL"
	}
	valid := false
	for _, f := range alertFormats {
		if r.ChannelFormat == f {
			valid = true
		}
	}
	if !valid {
		return "invalid channel format"
	}
	if r.ChannelFormat == "telegram" && r.TelegramChatID == "" {
		return "telegram format requires a chat ID"
	}
	switch r.StatusClass {
	case "", "err", "warn":
	default:
		return "invalid status class"
	}
	return ""
}

func (s *Server) alertRuleFromPath(w http.ResponseWriter, r *http.Request) *store.AlertRule {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	rule, err := s.store.GetAlertRule(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	return rule
}

func (s *Server) handleAlertToggle(w http.ResponseWriter, r *http.Request) {
	if rule := s.alertRuleFromPath(w, r); rule != nil {
		if err := s.store.ToggleAlertRule(r.Context(), rule.ID); err != nil {
			httpError(w, s.log, err)
			return
		}
		http.Redirect(w, r, "/alerts", http.StatusSeeOther)
	}
}

func (s *Server) handleAlertDelete(w http.ResponseWriter, r *http.Request) {
	if rule := s.alertRuleFromPath(w, r); rule != nil {
		if err := s.store.DeleteAlertRule(r.Context(), rule.ID); err != nil {
			httpError(w, s.log, err)
			return
		}
		http.Redirect(w, r, "/alerts", http.StatusSeeOther)
	}
}

func (s *Server) handleAlertTest(w http.ResponseWriter, r *http.Request) {
	rule := s.alertRuleFromPath(w, r)
	if rule == nil {
		return
	}
	if s.alertTester == nil {
		httpError(w, s.log, nil)
		return
	}
	s.alertTester.Fire(r.Context(), *rule, 0, true)
	http.Redirect(w, r, "/alerts", http.StatusSeeOther)
}

func formInt(r *http.Request, key string, def, min, max int) int {
	n, err := strconv.Atoi(r.PostFormValue(key))
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
