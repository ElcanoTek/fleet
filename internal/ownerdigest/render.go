package ownerdigest

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// section is how one Kind is headed and what its "when" line is called.
type section struct {
	heading string
	intro   string
	whenAs  string
}

var sections = map[Kind]section{
	KindStopped: {
		heading: "Schedule stopped",
		intro:   "These schedules will not run again until someone fixes the cause and replays or re-creates them.",
		whenAs:  "Stopped",
	},
	KindFailed: {
		heading: "Failed",
		intro:   "The latest run failed. A recurring schedule still runs at its next time.",
		whenAs:  "Failed",
	},
	KindBlocked: {
		heading: "Blocked",
		intro:   "The latest run finished but recorded a blocked outcome (for example, its source was unreachable), so what it maintains was not updated.",
		whenAs:  "Ran",
	},
	KindOverdue: {
		heading: "Not started on time",
		intro:   "These runs were due more than two hours ago and have not started.",
		whenAs:  "Due",
	},
}

// Render returns the subject, plain-text body and HTML body of one owner's
// digest at now. Every task-derived value is HTML-escaped in the HTML part;
// the subject carries only a count.
func Render(d Digest, now time.Time) (subject, text, htmlBody string) {
	total := len(d.Items) + d.Omitted
	noun := "scheduled tasks need"
	if total == 1 {
		noun = "scheduled task needs"
	}
	subject = fmt.Sprintf("Fleet daily digest: %d %s attention", total, noun)
	asOf := now.UTC().Format("2006-01-02 15:04 UTC")
	intro := fmt.Sprintf("Your scheduled tasks in Fleet that need attention, for the 24 hours up to %s.", asOf)
	footer := "This is the daily owner digest, which an administrator turned on. It lists what is still wrong: " +
		"a task whose latest run worked is left out, and you get no email on a day with nothing to report."

	var t, h strings.Builder
	t.WriteString(intro + "\r\n")
	h.WriteString("<html><body><p>" + html.EscapeString(intro) + "</p>")

	for _, kind := range []Kind{KindStopped, KindFailed, KindBlocked, KindOverdue} {
		var items []Item
		for _, it := range d.Items {
			if it.Kind == kind {
				items = append(items, it)
			}
		}
		if len(items) == 0 {
			continue
		}
		sec := sections[kind]
		fmt.Fprintf(&t, "\r\n%s (%d)\r\n%s\r\n", strings.ToUpper(sec.heading), len(items), sec.intro)
		fmt.Fprintf(&h, "<h3>%s (%d)</h3><p>%s</p><ul>", html.EscapeString(sec.heading), len(items), html.EscapeString(sec.intro))
		for _, it := range items {
			when := it.When.UTC().Format("2006-01-02 15:04 UTC")
			fmt.Fprintf(&t, "\r\n- %s\r\n  %s: %s\r\n  Reason: %s\r\n", it.Title, sec.whenAs, when, it.Reason)
			if it.Detail != "" {
				fmt.Fprintf(&t, "  %s\r\n", it.Detail)
			}
			if it.LogURL != "" {
				fmt.Fprintf(&t, "  Open: %s\r\n", it.LogURL)
			} else {
				fmt.Fprintf(&t, "  Task ID: %s\r\n", it.TaskID)
			}

			title := html.EscapeString(it.Title)
			if it.LogURL != "" {
				title = fmt.Sprintf("<a href=\"%s\">%s</a>", html.EscapeString(it.LogURL), title)
			}
			fmt.Fprintf(&h, "<li><b>%s</b><br>%s: %s<br>Reason: %s", title, html.EscapeString(sec.whenAs), html.EscapeString(when), html.EscapeString(it.Reason))
			if it.Detail != "" {
				fmt.Fprintf(&h, "<br>%s", html.EscapeString(it.Detail))
			}
			if it.LogURL == "" {
				fmt.Fprintf(&h, "<br>Task ID: %s", html.EscapeString(it.TaskID.String()))
			}
			h.WriteString("</li>")
		}
		h.WriteString("</ul>")
	}
	if d.Omitted > 0 {
		more := fmt.Sprintf("…and %d more not listed here; see the Operations Center.", d.Omitted)
		t.WriteString("\r\n" + more + "\r\n")
		h.WriteString("<p>" + html.EscapeString(more) + "</p>")
	}
	t.WriteString("\r\n" + footer + "\r\n")
	h.WriteString("<p><small>" + html.EscapeString(footer) + "</small></p></body></html>")
	return subject, t.String(), h.String()
}
