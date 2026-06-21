package tools

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

// PreviewEmailParams mirrors the shape of mcp_sendgrid_send_email's
// payload so the agent can reuse its existing knowledge of how to
// build an email, but call this tool when it wants to show the user
// a preview without committing to send.
//
// The tool's Run is a no-op — orchestration intercepts every call,
// stages a preview record, and returns a blocking PREVIEW_DISPLAYED
// response so the model stops iterating. The user sees the same
// inbox-style card the send_email approval uses, but the resolution
// is "Dismiss" instead of "Send" — nothing is approved, nothing is
// sent. We reuse the approvals table under the hood because the
// card chrome + SSE plumbing are identical; the distinction lives
// in the tool-name branch in approvals.go and the agent-facing
// message wording here.
type PreviewEmailParams struct {
	ToEmail     string   `json:"to_email" description:"Recipient address (displayed in the preview header; no email is actually sent)."`
	CcEmails    []string `json:"cc_emails,omitempty" description:"Cc recipients (header-only, not sent)."`
	BccEmails   []string `json:"bcc_emails,omitempty" description:"Bcc recipients (header-only, not sent)."`
	Subject     string   `json:"subject" description:"Subject line displayed in the preview header."`
	FromEmail   string   `json:"from_email,omitempty" description:"Sender address shown in the preview header."`
	Content     string   `json:"content,omitempty" description:"Email body, inline. Use HTML for content_type=text/html (default). Rendered in a sandboxed iframe. REQUIRED unless content_file is set."`
	ContentFile string   `json:"content_file,omitempty" description:"Path to a file holding the email body — typical when a run_python step just wrote the HTML. Relative paths resolve inside the per-conversation workspace; absolute /opt/chat/workspace/<convID>/... paths also work. REQUIRED unless content is set; takes precedence over content."`
	ContentType string   `json:"content_type,omitempty" description:"MIME type — defaults to text/html. Set text/plain for plain-text previews."`
}

const previewEmailDescription = `Shows the user an inbox-style preview of an email you're drafting WITHOUT sending.

Use this when the user asks to "see the draft", "preview the email", "how would this look", etc. It takes the same fields as mcp_sendgrid_send_email — to_email, subject, content — and renders them in the same approval card + sandboxed iframe that a real send uses, except the only action is "Dismiss". No SendGrid call is made.

REQUIRED FIELDS — the call is rejected before staging if both are missing:
- ` + "`content`" + ` (an HTML or plain-text string), OR
- ` + "`content_file`" + ` (path to a file in your workspace; an absolute /opt/chat/workspace/<convID>/email.html works, so does the bare filename ` + "`email.html`" + `).
Don't pass only headers (to_email/subject/inline_attachments) — the iframe renders empty and the user reports "I can't see the preview".

Prefer this over:
- Pasting raw HTML into your reply (renders as escaped text).
- Writing HTML to workspace/ and telling the user a file path (they can't open it from chat).
- Calling mcp_sendgrid_send_email just for the preview — that implies intent to send.

After the user reviews the preview, they'll either ask for edits or tell you to send it — at which point call mcp_sendgrid_send_email for real (passing the same content/content_file).`

// NewPreviewEmailTool returns the preview_email tool. Its Run is an
// explicit error because orchestration should intercept before we get
// here; if it fires, something is mis-wired.
func NewPreviewEmailTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("preview_email", previewEmailDescription,
		func(_ context.Context, _ PreviewEmailParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextErrorResponse(
				"preview_email was executed directly — orchestration should have staged it for approval. This is a bug.",
			), fmt.Errorf("preview_email bypass")
		})
}
