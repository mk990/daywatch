package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mk/daywatch/internal/config"
	"github.com/mk/daywatch/internal/store"
)

// handleApps renders the app management page: registered apps with their
// ingest tokens, per-app record stats, and a create form.
func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	base, _ := s.base(r, "apps")
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	s.render(w, "apps.html", map[string]any{
		"Base":    base,
		"Apps":    apps,
		"Error":   r.URL.Query().Get("error"),
		"Created": r.URL.Query().Get("created"),
	})
}

// handleAppCreate registers a new app with a freshly generated token.
func (s *Server) handleAppCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	fail := func(msg string) {
		http.Redirect(w, r, "/apps?error="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if !store.ValidAppName(name) {
		fail("app name must be 1-40 letters, digits, - or _")
		return
	}
	token, err := store.GenerateAppToken()
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	if err := s.store.CreateApp(r.Context(), name, token, config.TokenHash(token)); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			fail("an app named " + name + " already exists")
			return
		}
		httpError(w, s.log, err)
		return
	}
	s.log.Info("app created", "app", name)
	s.hub.Notify()
	http.Redirect(w, r, "/apps?created="+url.QueryEscape(name), http.StatusSeeOther)
}

func (s *Server) appFromPath(w http.ResponseWriter, r *http.Request) *store.App {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	app, err := s.store.GetApp(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return nil
	}
	return app
}

// handleAppRegenerate rotates an app's ingest token. The old token stops
// working immediately; senders must update their NIGHTWATCH_TOKEN.
func (s *Server) handleAppRegenerate(w http.ResponseWriter, r *http.Request) {
	app := s.appFromPath(w, r)
	if app == nil {
		return
	}
	token, err := store.GenerateAppToken()
	if err != nil {
		httpError(w, s.log, err)
		return
	}
	if err := s.store.UpdateAppToken(r.Context(), app.ID, token, config.TokenHash(token)); err != nil {
		httpError(w, s.log, err)
		return
	}
	s.log.Info("app token rotated", "app", app.Name)
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}

// handleAppDelete unregisters an app; its stored records are kept.
func (s *Server) handleAppDelete(w http.ResponseWriter, r *http.Request) {
	app := s.appFromPath(w, r)
	if app == nil {
		return
	}
	if err := s.store.DeleteApp(r.Context(), app.ID); err != nil {
		httpError(w, s.log, err)
		return
	}
	s.log.Info("app deleted", "app", app.Name)
	s.hub.Notify()
	http.Redirect(w, r, "/apps", http.StatusSeeOther)
}
