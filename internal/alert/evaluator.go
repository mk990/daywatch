// Package alert evaluates alert rules against ingested records and
// delivers webhook notifications.
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/mk/daywatch/internal/store"
)

// Notifier is implemented by anything that should be poked after an alert
// fires (the web panel's live-reload hub).
type Notifier interface{ Notify() }

type Evaluator struct {
	store    *store.Store
	log      *slog.Logger
	baseURL  string // optional panel URL included in notifications
	notifier Notifier
	client   *http.Client
	interval time.Duration
}

func New(st *store.Store, log *slog.Logger, baseURL string, notifier Notifier) *Evaluator {
	return &Evaluator{
		store:    st,
		log:      log,
		baseURL:  baseURL,
		notifier: notifier,
		client:   &http.Client{Timeout: 10 * time.Second},
		interval: 30 * time.Second,
	}
}

// Run evaluates all enabled rules on a fixed interval until ctx is done.
func (e *Evaluator) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			e.evaluateAll(ctx)
		}
	}
}

func (e *Evaluator) evaluateAll(ctx context.Context) {
	rules, err := e.store.ListAlertRules(ctx)
	if err != nil {
		e.log.Error("alert: listing rules failed", "error", err)
		return
	}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if err := e.evaluate(ctx, r); err != nil {
			e.log.Error("alert: rule evaluation failed", "rule", r.Name, "error", err)
		}
	}
}

func (e *Evaluator) evaluate(ctx context.Context, r store.AlertRule) error {
	last, err := e.store.LastFired(ctx, r.ID)
	if err != nil {
		return err
	}
	if !last.IsZero() && time.Since(last) < time.Duration(r.CooldownMinutes)*time.Minute {
		return nil // cooling down
	}

	since := time.Now().Add(-time.Duration(r.WindowMinutes) * time.Minute)
	matched, err := e.store.CountMatching(ctx, r, since)
	if err != nil {
		return err
	}
	if matched < int64(r.Threshold) {
		return nil
	}

	e.Fire(ctx, r, matched, false)
	return nil
}

// Fire delivers the notification and records the event. When test is true
// the message is marked as a test and the event is still recorded, which
// also starts the cooldown — callers use it to verify webhook wiring.
func (e *Evaluator) Fire(ctx context.Context, r store.AlertRule, matched int64, test bool) {
	msg := e.message(r, matched, test)
	deliverErr := e.deliver(ctx, r, msg)

	event := store.AlertEvent{
		RuleID:    r.ID,
		FiredAt:   time.Now(),
		Matched:   matched,
		Message:   msg,
		Delivered: deliverErr == nil,
	}
	if deliverErr != nil {
		event.Error = deliverErr.Error()
		e.log.Warn("alert: delivery failed", "rule", r.Name, "error", deliverErr)
	} else {
		e.log.Info("alert fired", "rule", r.Name, "matched", matched)
	}
	if err := e.store.InsertAlertEvent(ctx, event); err != nil {
		e.log.Error("alert: recording event failed", "rule", r.Name, "error", err)
	}
	if e.notifier != nil {
		e.notifier.Notify()
	}
}

func (e *Evaluator) message(r store.AlertRule, matched int64, test bool) string {
	var msg string
	if r.Kind == "new-exception" {
		msg = fmt.Sprintf("🚨 Daywatch alert: %s — %d new exception type(s) first seen in the last %dm",
			r.Name, matched, r.WindowMinutes)
	} else {
		what := r.RecordType
		if what == "" {
			what = "event"
		}
		class := ""
		switch r.StatusClass {
		case "err":
			class = "error "
		case "warn":
			class = "warning "
		}
		msg = fmt.Sprintf("🚨 Daywatch alert: %s — %d %s%s record(s) in the last %dm (threshold %d)",
			r.Name, matched, class, what, r.WindowMinutes, r.Threshold)
	}
	if r.App != "" {
		msg += " [app: " + r.App + "]"
	}
	if test {
		msg = "[TEST] " + msg
	}
	if e.baseURL != "" {
		msg += " " + e.baseURL
	}
	return msg
}

func (e *Evaluator) deliver(ctx context.Context, r store.AlertRule, msg string) error {
	var body []byte
	var err error
	switch r.ChannelFormat {
	case "slack":
		body, err = json.Marshal(map[string]string{"text": msg})
	case "discord":
		body, err = json.Marshal(map[string]string{"content": msg})
	case "telegram":
		body, err = json.Marshal(map[string]string{"chat_id": r.TelegramChatID, "text": msg})
	default: // generic JSON webhook
		body, err = json.Marshal(map[string]any{
			"source":  "daywatch",
			"rule":    r.Name,
			"message": msg,
		})
	}
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.ChannelURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}
