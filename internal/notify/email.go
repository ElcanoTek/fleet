package notify

import (
	"fmt"
	"log"
	"strings"
)

// renderEmail builds the RFC 5322 message bytes for a completion email: a
// multipart/alternative body with a plain-text part and an HTML part, so a
// receiving client renders whichever it prefers. It contains ONLY non-secret
// run facts (task ID/name, status, cost, duration, log link) — never any SMTP
// credential. from/to are headers; the SMTP envelope sender/recipients are
// passed separately to smtp.SendMail.
func renderEmail(from string, to []string, ev Event) []byte {
	subject := fmt.Sprintf("Fleet task %s: %s", ev.Status, ev.Name)

	textBody := buildTextBody(ev)
	htmlBody := buildHTMLBody(ev)

	// A fixed boundary is fine: it is generated per call (here, a constant) and
	// the bodies are fleet-controlled, so it cannot appear inside a part.
	const boundary = "fleet-notify-boundary-208"

	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(textBody)
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n\r\n")
	b.WriteString(htmlBody)
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String())
}

// renderReplyEmail builds the RFC 5322 bytes for a reply to an inbound-email
// trigger's original sender (#511 reply-back): a single text/plain part carrying
// the run's result, threaded to the original message via In-Reply-To/References.
// to/subject/inReplyTo derive from the UNTRUSTED inbound email, so every header
// value is CR/LF-sanitized (sanitizeHeader) to foreclose header injection; the
// body sits after the header/body separator and cannot inject headers.
func renderReplyEmail(from, to, subject, body, inReplyTo string) []byte {
	var b strings.Builder
	b.WriteString("From: " + sanitizeHeader(from) + "\r\n")
	b.WriteString("To: " + sanitizeHeader(to) + "\r\n")
	b.WriteString("Subject: " + sanitizeHeader(replySubject(subject)) + "\r\n")
	if trimmed := strings.TrimSpace(inReplyTo); trimmed != "" {
		b.WriteString("In-Reply-To: " + sanitizeHeader(trimmed) + "\r\n")
		b.WriteString("References: " + sanitizeHeader(trimmed) + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// replySubject prefixes "Re: " unless the subject already carries one (any case).
// A blank subject becomes a bare "Re:".
func replySubject(subject string) string {
	s := strings.TrimSpace(subject)
	if s == "" {
		return "Re:"
	}
	if strings.HasPrefix(strings.ToLower(s), "re:") {
		return s
	}
	return "Re: " + s
}

// buildTextBody renders the plain-text alternative.
func buildTextBody(ev Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task: %s\r\n", ev.Name)
	fmt.Fprintf(&b, "ID: %s\r\n", ev.TaskID)
	fmt.Fprintf(&b, "Status: %s\r\n", ev.Status)
	fmt.Fprintf(&b, "Cost: $%s\r\n", ev.CostUSD)
	fmt.Fprintf(&b, "Duration: %ds\r\n", ev.DurationSeconds)
	if ev.LogURL != "" {
		fmt.Fprintf(&b, "Log: %s\r\n", ev.LogURL)
	}
	return b.String()
}

// buildHTMLBody renders the HTML alternative. The interpolated values are
// HTML-escaped so a prompt-derived Name cannot inject markup into the email.
func buildHTMLBody(ev Event) string {
	logLine := ""
	if ev.LogURL != "" {
		u := htmlEscape(ev.LogURL)
		logLine = fmt.Sprintf("<p><a href=\"%s\">View run log</a></p>", u)
	}
	return fmt.Sprintf(
		"<html><body>"+
			"<h2>Fleet task %s</h2>"+
			"<table>"+
			"<tr><td><b>Task</b></td><td>%s</td></tr>"+
			"<tr><td><b>ID</b></td><td>%s</td></tr>"+
			"<tr><td><b>Status</b></td><td>%s</td></tr>"+
			"<tr><td><b>Cost</b></td><td>$%s</td></tr>"+
			"<tr><td><b>Duration</b></td><td>%ds</td></tr>"+
			"</table>%s</body></html>",
		htmlEscape(string(ev.Status)),
		htmlEscape(ev.Name),
		htmlEscape(ev.TaskID),
		htmlEscape(string(ev.Status)),
		htmlEscape(ev.CostUSD),
		ev.DurationSeconds,
		logLine,
	)
}

// htmlEscape is a tiny escaper for the handful of values we interpolate into the
// HTML body. We avoid html/template here to keep the email a single small,
// auditable string builder, but still neutralize the characters that matter.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// sanitizeHeader strips CR/LF from a header value so a crafted task name cannot
// inject extra headers (header-splitting). Defensive: Name is already truncated
// fleet-side, but the Subject embeds it.
func sanitizeHeader(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
}

// stdLogf is the default Notifier logger. It is a thin wrapper so the Notifier
// can hold a swappable log seam (tests assert no secret is ever logged). It
// deliberately mirrors log.Printf's signature.
func stdLogf(format string, args ...any) { log.Printf(format, args...) }
