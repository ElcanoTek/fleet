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

func TestEmailDedupKey_AttachmentAwareThroughRawInput(t *testing.T) {
	a := emailDedupKey(`{"to_email":["a@x.com"],"subject":"s","content":"c"}`)
	b := emailDedupKey(`{"to_email":["a@x.com"],"subject":"s","content":"c","attachments":["x.csv"]}`)
	if a == b {
		t.Fatal("emailDedupKey must distinguish a send with attachments from one without")
	}
}
