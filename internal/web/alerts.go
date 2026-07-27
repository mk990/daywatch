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

var alertFormats = []string{"json", "slack", "discord", "telegram", "ntfy"}

// alertForm backs the shared create/edit rule form. Rule carries the values
// to prefill; its AuthPass is always cleared before rendering so a stored
// secret never reaches the page — HasAuth says whether one exists.
type alertForm struct {
	Rule    store.AlertRule
	Action  string
	Submit  string
	Edit    bool
	HasAuth bool
	Types   []Section
	Apps    []string
	Formats []string
}

func (s *Server) alertForm(apps []string, rule store.AlertRule, edit bool) alertForm {
	f := alertForm{
		Rule:    rule,
		Action:  "/alerts",
		Submit:  "Create rule",
		Edit:    edit,
		HasAuth: rule.AuthUser != "" || rule.AuthPass != "",
		Types:   s.sections,
		Apps:    apps,
		Formats: alertFormats,
	}
	if edit {
		f.Action = "/alerts/" + strconv.FormatInt(rule.ID, 10)
		f.Submit = "Save changes"
	}
	f.Rule.AuthPass = ""
	return f
}

// newRuleDefaults prefills the create form.
func newRuleDefaults(app string) store.AlertRule {
	return store.AlertRule{
		Kind:            "threshold",
		App:             app,
		StatusClass:     "err",
		Threshold:       5,
		WindowMinutes:   5,
		CooldownMinutes: 15,
		ChannelFormat:   alertFormats[0],
	}
}

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
	apps, err := s.store.AppNames(r.Context())
	if err != nil {
		httpError(w, s.log, err)
		return
	}

	s.render(w, "alerts.html", map[string]any{
		"Base":   base,
		"Rules":  rules,
		"Events": events,
		"Form":   s.alertForm(apps, newRuleDefaults(base.App), false),
		"Error":  r.URL.Query().Get("error"),
	})
}

func (s *Server) handleAlertEdit(w http.ResponseWriter, r *http.Request) {
	rule := s.alertRuleFromPath(w, r)
	if rule == nil {
		return
	}
	apps, err := s.store.AppNames(r.Context())
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	base, _ := s.base(r, "alerts")
	s.render(w, "alert_edit.html", map[string]any{
		"Base":  base,
		"Form":  s.alertForm(apps, *rule, true),
		"Error": r.URL.Query().Get("error"),
	})
}

// ruleFromForm reads the shared rule form. It does not set ID or Enabled,
// which the create and update paths own.
func ruleFromForm(r *http.Request) store.AlertRule {
	rule := store.AlertRule{
		Name:            strings.TrimSpace(r.PostFormValue("name")),
		Kind:            r.PostFormValue("kind"),
		App:             r.PostFormValue("app"),
		RecordType:      r.PostFormValue("record_type"),
		StatusClass:     r.PostFormValue("status_class"),
		Threshold:       formInt(r, "threshold", 1, 1, 1_000_000),
		WindowMinutes:   formInt(r, "window_minutes", 5, 1, 1440),
		CooldownMinutes: formInt(r, "cooldown_minutes", 15, 1, 10080),
		ChannelURL:      strings.TrimSpace(r.PostFormValue("channel_url")),
		ChannelFormat:   r.PostFormValue("channel_format"),
		TelegramChatID:  strings.TrimSpace(r.PostFormValue("telegram_chat_id")),
		AuthUser:        strings.TrimSpace(r.PostFormValue("auth_user")),
		AuthPass:        r.PostFormValue("auth_pass"),
	}
	// New-exception rules fire on any new group; the threshold/type/class
	// fields don't apply.
	if rule.Kind == "new-exception" {
		rule.Threshold = 1
		rule.RecordType = "exception"
		rule.StatusClass = ""
	}
	return rule
}

// mergeAuth carries stored credentials across an edit. The password field is
// never prefilled — it cannot be, without exposing the secret — so an empty
// one means "keep what is stored"; clearing them takes an explicit checkbox.
func mergeAuth(rule, existing store.AlertRule, clear bool) store.AlertRule {
	if clear {
		rule.AuthUser, rule.AuthPass = "", ""
		return rule
	}
	if rule.AuthPass == "" {
		rule.AuthPass = existing.AuthPass
	}
	return rule
}

// checkRule validates a rule, resolving the app filter against registered
// apps. It returns a user-facing message, or "" when the rule is good.
func (s *Server) checkRule(ctx context.Context, rule store.AlertRule) (string, error) {
	if rule.App != "" {
		apps, err := s.store.AppNames(ctx)
		if err != nil {
			return "", err
		}
		if !slices.Contains(apps, rule.App) {
			return "unknown app", nil
		}
	}
	return validateRule(rule), nil
}

func (s *Server) handleAlertCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/alerts?error="+url.QueryEscape("invalid form"), http.StatusSeeOther)
		return
	}
	rule := ruleFromForm(r)
	rule.Enabled = true

	msg, err := s.checkRule(r.Context(), rule)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	if msg != "" {
		http.Redirect(w, r, "/alerts?error="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	if err := s.store.CreateAlertRule(r.Context(), rule); err != nil {
		httpError(w, s.log, err)
		return
	}
	http.Redirect(w, r, "/alerts", http.StatusSeeOther)
}

func (s *Server) handleAlertUpdate(w http.ResponseWriter, r *http.Request) {
	existing := s.alertRuleFromPath(w, r)
	if existing == nil {
		return
	}
	editURL := "/alerts/" + strconv.FormatInt(existing.ID, 10) + "/edit"
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, editURL+"?error="+url.QueryEscape("invalid form"), http.StatusSeeOther)
		return
	}

	rule := ruleFromForm(r)
	rule.ID = existing.ID
	rule.Enabled = existing.Enabled // paused/active is the toggle button's job
	rule = mergeAuth(rule, *existing, r.PostFormValue("clear_auth") == "1")

	msg, err := s.checkRule(r.Context(), rule)
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	if msg != "" {
		http.Redirect(w, r, editURL+"?error="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	if err := s.store.UpdateAlertRule(r.Context(), rule); err != nil {
		httpError(w, s.log, err)
		return
	}
	http.Redirect(w, r, "/alerts", http.StatusSeeOther)
}

func validateRule(r store.AlertRule) string {
	if r.Name == "" {
		return "name is required"
	}
	switch r.Kind {
	case "", "threshold", "new-exception":
	default:
		return "invalid rule kind"
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
	if r.ChannelFormat == "ntfy" && strings.Trim(u.Path, "/") == "" {
		return "ntfy URL must include the topic, e.g. https://ntfy.example.com/daywatch"
	}
	if r.AuthUser != "" && r.AuthPass == "" {
		return "a username needs a password"
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
