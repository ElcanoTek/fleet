package agentcore

import "testing"

func fpArgs(attachments interface{}) map[string]interface{} {
	args := map[string]interface{}{
		"to_email": []interface{}{"a@x.com", "b@x.com"},
		"subject":  "Weekly report",
		"content":  "<html>body</html>",
	}
	if attachments != nil {
		args["attachments"] = attachments
	}
	return args
}

// The production incident: a send WITHOUT the CSV must not make a later send
// WITH the CSV look like a duplicate — that resend is the corrective action.
func TestSendEmailFingerprint_AttachmentsChangeIdentity(t *testing.T) {
	without, ok := sendEmailFingerprint(fpArgs(nil))
	if !ok {
		t.Fatal("fingerprint without attachments must be computable")
	}
	with, ok := sendEmailFingerprint(fpArgs([]interface{}{"/var/lib/fleet/workspace/tasks/abc/report.csv"}))
	if !ok {
		t.Fatal("fingerprint with attachments must be computable")
	}
	if without == with {
		t.Fatal("adding an attachment must change the send fingerprint")
	}
	empty, _ := sendEmailFingerprint(fpArgs([]interface{}{}))
	if empty != without {
		t.Fatal("an empty attachment list is the same send as no attachments")
	}
}

// The same file referenced through a different directory is the same
// deliverable and must still dedupe; order must not matter either.
func TestSendEmailFingerprint_AttachmentIdentityIsByBaseName(t *testing.T) {
	abs, _ := sendEmailFingerprint(fpArgs([]interface{}{"/var/lib/fleet/workspace/tasks/abc/Report.CSV", "/tmp/notes.txt"}))
	rel, _ := sendEmailFingerprint(fpArgs([]interface{}{"notes.txt", "report.csv"}))
	if abs != rel {
		t.Fatal("attachment identity must be by lower-cased base name, order-insensitive")
	}
	other, _ := sendEmailFingerprint(fpArgs([]interface{}{"report.csv", "other.txt"}))
	if other == rel {
		t.Fatal("a different attachment set must change the fingerprint")
	}
	single, _ := sendEmailFingerprint(fpArgs("report.csv"))
	list, _ := sendEmailFingerprint(fpArgs([]interface{}{"report.csv"}))
	if single != list {
		t.Fatal("a bare string attachment must fingerprint like a one-element list")
	}
}

// The real wire shape is a list of {"path": ...} objects
// (tools.MaterializeAttachmentPaths); it must fingerprint exactly like the
// equivalent bare-string list, and must not collapse to the no-attachment key.
func TestSendEmailFingerprint_ObjectShapedAttachments(t *testing.T) {
	objs, ok := sendEmailFingerprint(fpArgs([]interface{}{
		map[string]interface{}{"path": "/var/lib/fleet/workspace/tasks/abc/report.csv"},
		map[string]interface{}{"path": "notes.txt", "mime_type": "text/plain"},
	}))
	if !ok {
		t.Fatal("object-shaped attachments must be computable")
	}
	strs, _ := sendEmailFingerprint(fpArgs([]interface{}{"notes.txt", "report.csv"}))
	if objs != strs {
		t.Fatal("object-shaped attachments must fingerprint like the equivalent bare-string list")
	}
	none, _ := sendEmailFingerprint(fpArgs(nil))
	if objs == none {
		t.Fatal("object-shaped attachments must not collapse to the no-attachment fingerprint")
	}
	inline, _ := sendEmailFingerprint(map[string]interface{}{
		"to_email": []interface{}{"a@x.com", "b@x.com"}, "subject": "Weekly report", "content": "<html>body</html>",
		"inline_attachments": []interface{}{map[string]interface{}{"path": "chart.png", "cid": "chart"}},
	})
	if inline == none {
		t.Fatal("inline attachments are part of the send identity too")
	}
}

func TestEmailDedupKey_AttachmentAwareThroughRawInput(t *testing.T) {
	a := emailDedupKey(`{"to_email":["a@x.com"],"subject":"s","content":"c"}`)
	b := emailDedupKey(`{"to_email":["a@x.com"],"subject":"s","content":"c","attachments":["x.csv"]}`)
	if a == b {
		t.Fatal("emailDedupKey must distinguish a send with attachments from one without")
	}
	c := emailDedupKey(`{"to_email":["a@x.com"],"subject":"s","content":"c","attachments":[{"path":"/w/x.csv"}]}`)
	if c == a {
		t.Fatal("emailDedupKey must see object-shaped attachments")
	}
	if c != b {
		t.Fatal("object-shaped and bare-string attachments of the same file must dedupe together")
	}
}
