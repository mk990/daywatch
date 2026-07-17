package web

import (
	"fmt"
	"html/template"

	"github.com/mk/daywatch/internal/store"
)

func esc(s string) template.HTML { return template.HTML(template.HTMLEscapeString(s)) }

func field(r store.Record, key string) template.HTML {
	return esc(anyToString(r.Data[key]))
}

func anyToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "yes"
		}
		return "no"
	default:
		return fmt.Sprintf("%v", t)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func durationCell(r store.Record) template.HTML {
	f := float64(r.Duration)
	switch {
	case f >= 1_000_000:
		return esc(fmt.Sprintf("%.2fs", f/1_000_000))
	case f >= 1_000:
		return esc(fmt.Sprintf("%.1fms", f/1_000))
	default:
		return esc(fmt.Sprintf("%.0fµs", f))
	}
}

func statusBadge(r store.Record) template.HTML {
	cls := "badge"
	switch {
	case r.Status == "":
		return ""
	case r.Status >= "200" && r.Status < "300", r.Status == "0", r.Status == "sent",
		r.Status == "processed", r.Status == "handled", r.Status == "hit", r.Status == "success":
		cls += " ok"
	case r.Status >= "500" && r.Status <= "599", r.Status == "failed", r.Status == "unhandled",
		r.Status == "error", r.Status == "critical", r.Status == "emergency", r.Status == "alert":
		cls += " err"
	case r.Status >= "400" && r.Status < "500", r.Status == "warning", r.Status == "miss",
		r.Status == "released":
		cls += " warn"
	}
	return template.HTML(fmt.Sprintf(`<span class="%s">%s</span>`, cls, template.HTMLEscapeString(r.Status)))
}

func buildSections() []Section {
	return []Section{
		{
			Slug: "requests", Type: "request", Title: "Requests", Icon: "🌐", StatusLabel: "Status",
			GroupLabelExpr: "concat(data->>'method', ' ', coalesce(nullif(data->>'route_path',''), data->>'url'))",
			GroupTitle:     "Top routes",
			Columns: []Column{
				{"Method", func(r store.Record) template.HTML { return field(r, "method") }},
				{"Path", func(r store.Record) template.HTML {
					p := anyToString(r.Data["route_path"])
					if p == "" {
						p = anyToString(r.Data["url"])
					}
					return esc(trunc(p, 80))
				}},
				{"Status", statusBadge},
				{"Duration", durationCell},
				{"Queries", func(r store.Record) template.HTML { return field(r, "queries") }},
				{"User", func(r store.Record) template.HTML { return esc(r.UserID) }},
			},
		},
		{
			Slug: "queries", Type: "query", Title: "Queries", Icon: "🗄️", StatusLabel: "",
			GroupLabelExpr: "data->>'sql'",
			GroupTitle:     "Most frequent queries",
			Columns: []Column{
				{"SQL", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["sql"]), 110)) }},
				{"Duration", durationCell},
				{"Connection", func(r store.Record) template.HTML { return field(r, "connection") }},
				{"Source", func(r store.Record) template.HTML {
					return esc(trunc(fmt.Sprintf("%s:%s", anyToString(r.Data["file"]), anyToString(r.Data["line"])), 60))
				}},
			},
		},
		{
			Slug: "exceptions", Type: "exception", Title: "Exceptions", Icon: "💥", StatusLabel: "Handled",
			GroupLabelExpr: "concat(data->>'class', ': ', data->>'message')",
			GroupTitle:     "Top exceptions",
			Columns: []Column{
				{"Class", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["class"]), 50)) }},
				{"Message", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["message"]), 90)) }},
				{"Handled", statusBadge},
				{"Source", func(r store.Record) template.HTML {
					return esc(trunc(fmt.Sprintf("%s:%s", anyToString(r.Data["file"]), anyToString(r.Data["line"])), 60))
				}},
			},
		},
		{
			Slug: "logs", Type: "log", Title: "Logs", Icon: "📜", StatusLabel: "Level",
			Columns: []Column{
				{"Level", statusBadge},
				{"Message", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["message"]), 120)) }},
			},
		},
		{
			Slug: "commands", Type: "command", Title: "Commands", Icon: "⌨️", StatusLabel: "Exit code",
			GroupLabelExpr: "data->>'name'",
			GroupTitle:     "Top commands",
			Columns: []Column{
				{"Name", func(r store.Record) template.HTML { return field(r, "name") }},
				{"Exit", statusBadge},
				{"Duration", durationCell},
				{"Queries", func(r store.Record) template.HTML { return field(r, "queries") }},
				{"Peak memory", func(r store.Record) template.HTML { return field(r, "peak_memory_usage") }},
			},
		},
		{
			Slug: "queued-jobs", Type: "queued-job", Title: "Queued Jobs", Icon: "📤", StatusLabel: "",
			GroupLabelExpr: "data->>'name'",
			GroupTitle:     "Most queued",
			Columns: []Column{
				{"Job", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["name"]), 70)) }},
				{"Queue", func(r store.Record) template.HTML { return field(r, "queue") }},
				{"Connection", func(r store.Record) template.HTML { return field(r, "connection") }},
			},
		},
		{
			Slug: "job-attempts", Type: "job-attempt", Title: "Job Attempts", Icon: "⚙️", StatusLabel: "Status",
			GroupLabelExpr: "data->>'name'",
			GroupTitle:     "Top jobs",
			Columns: []Column{
				{"Job", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["name"]), 70)) }},
				{"Status", statusBadge},
				{"Attempt", func(r store.Record) template.HTML { return field(r, "attempt") }},
				{"Queue", func(r store.Record) template.HTML { return field(r, "queue") }},
				{"Duration", durationCell},
			},
		},
		{
			Slug: "scheduled-tasks", Type: "scheduled-task", Title: "Scheduled Tasks", Icon: "⏰", StatusLabel: "Status",
			GroupLabelExpr: "data->>'name'",
			GroupTitle:     "Tasks",
			Columns: []Column{
				{"Task", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["name"]), 70)) }},
				{"Cron", func(r store.Record) template.HTML { return field(r, "cron") }},
				{"Status", statusBadge},
				{"Duration", durationCell},
			},
		},
		{
			Slug: "cache", Type: "cache-event", Title: "Cache", Icon: "🧊", StatusLabel: "Event",
			GroupLabelExpr: "data->>'key'",
			GroupTitle:     "Hot keys",
			Columns: []Column{
				{"Event", statusBadge},
				{"Key", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["key"]), 80)) }},
				{"Store", func(r store.Record) template.HTML { return field(r, "store") }},
				{"Duration", durationCell},
			},
		},
		{
			Slug: "outgoing", Type: "outgoing-request", Title: "Outgoing HTTP", Icon: "📡", StatusLabel: "Status",
			GroupLabelExpr: "concat(data->>'method', ' ', data->>'host')",
			GroupTitle:     "Top hosts",
			Columns: []Column{
				{"Method", func(r store.Record) template.HTML { return field(r, "method") }},
				{"URL", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["url"]), 90)) }},
				{"Status", statusBadge},
				{"Duration", durationCell},
			},
		},
		{
			Slug: "mail", Type: "mail", Title: "Mail", Icon: "✉️", StatusLabel: "Status",
			GroupLabelExpr: "data->>'class'",
			GroupTitle:     "Top mailables",
			Columns: []Column{
				{"Mailable", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["class"]), 60)) }},
				{"Subject", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["subject"]), 60)) }},
				{"To", func(r store.Record) template.HTML { return field(r, "to") }},
				{"Status", statusBadge},
				{"Duration", durationCell},
			},
		},
		{
			Slug: "notifications", Type: "notification", Title: "Notifications", Icon: "🔔", StatusLabel: "Status",
			GroupLabelExpr: "data->>'class'",
			GroupTitle:     "Top notifications",
			Columns: []Column{
				{"Notification", func(r store.Record) template.HTML { return esc(trunc(anyToString(r.Data["class"]), 70)) }},
				{"Channel", func(r store.Record) template.HTML { return field(r, "channel") }},
				{"Status", statusBadge},
				{"Duration", durationCell},
			},
		},
		{
			Slug: "users", Type: "user", Title: "Users", Icon: "👤", StatusLabel: "",
			Columns: []Column{
				{"ID", func(r store.Record) template.HTML { return field(r, "id") }},
				{"Name", func(r store.Record) template.HTML { return field(r, "name") }},
				{"Username", func(r store.Record) template.HTML { return field(r, "username") }},
			},
		},
	}
}
