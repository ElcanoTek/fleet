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

// Attachment identity keeps directories and case (different files can share a
// base name), is order-insensitive, and canonicalizes only path syntax.
func TestSendEmailFingerprint_AttachmentIdentityKeepsPath(t *testing.T) {
	a, _ := sendEmailFingerprint(fpArgs([]interface{}{"tasks/abc/report.csv", "/tmp/notes.txt"}))
	reordered, _ := sendEmailFingerprint(fpArgs([]interface{}{"/tmp/notes.txt", "tasks/abc/./report.csv"}))
	if a != reordered {
		t.Fatal("order and redundant path syntax must not change the fingerprint")
	}
	otherDir, _ := sendEmailFingerprint(fpArgs([]interface{}{"tasks/xyz/report.csv", "/tmp/notes.txt"}))
	if otherDir == a {
		t.Fatal("the same base name in a different directory is a different file")
	}
	otherCase, _ := sendEmailFingerprint(fpArgs([]interface{}{"tasks/abc/Report.csv", "/tmp/notes.txt"}))
	if otherCase == a {
		t.Fatal("case differences distinguish files on the deployment filesystem")
	}
	single, _ := sendEmailFingerprint(fpArgs("tasks/abc/report.csv"))
	list, _ := sendEmailFingerprint(fpArgs([]interface{}{"tasks/abc/report.csv"}))
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
	strs, _ := sendEmailFingerprint(fpArgs([]interface{}{"notes.txt", "/var/lib/fleet/workspace/tasks/abc/report.csv"}))
	if objs != strs {
		t.Fatal("object-shaped attachments must fingerprint like the equivalent bare-string list")
	}
	none, _ := sendEmailFingerprint(fpArgs(nil))
	if objs == none {
		t.Fatal("object-shaped attachments must not collapse to the no-attachment fingerprint")
	}
}

// Inline attachments carry the content id the body references them by; a
// corrected cid for the same file is a different email.
func TestSendEmailFingerprint_InlineAttachmentsIncludeCID(t *testing.T) {
	base := func(cid string) map[string]interface{} {
		return map[string]interface{}{
			"to_email": []interface{}{"a@x.com"}, "subject": "Weekly report", "content": "<img src=\"cid:chart\">",
			"inline_attachments": []interface{}{map[string]interface{}{"path": "chart.png", "cid": cid}},
		}
	}
	chart, _ := sendEmailFingerprint(base("chart"))
	wrong, _ := sendEmailFingerprint(base("chart-old"))
	if chart == wrong {
		t.Fatal("an inline attachment re-sent under a different cid is a different email")
	}
	viaContentID, _ := sendEmailFingerprint(map[string]interface{}{
		"to_email": []interface{}{"a@x.com"}, "subject": "Weekly report", "content": "<img src=\"cid:chart\">",
		"inline_attachments": []interface{}{map[string]interface{}{"path": "chart.png", "content_id": "chart"}},
	})
	if viaContentID != chart {
		t.Fatal("cid and content_id are the same field")
	}
	none, _ := sendEmailFingerprint(map[string]interface{}{
		"to_email": []interface{}{"a@x.com"}, "subject": "Weekly report", "content": "<img src=\"cid:chart\">",
	})
	if none == chart {
		t.Fatal("inline attachments are part of the send identity")
	}
}

// A path containing the list delimiter must not alias a different list.
func TestSendEmailFingerprint_NoDelimiterCollision(t *testing.T) {
	one, _ := sendEmailFingerprint(fpArgs([]interface{}{"a,b"}))
	two, _ := sendEmailFingerprint(fpArgs([]interface{}{"a", "b"}))
	if one == two {
		t.Fatal("one attachment named \"a,b\" must not fingerprint like attachments \"a\" and \"b\"")
	}
}

// cid and path are encoded as separate components: moving characters across
// the boundary must change the identity, and the "file" alias is honored.
func TestSendEmailFingerprint_InlineComponentsAreSeparate(t *testing.T) {
	mk := func(cid, key, path string) map[string]interface{} {
		return map[string]interface{}{
			"to_email": []interface{}{"a@x.com"}, "subject": "s", "content": "<img src=\"cid:x\">",
			"inline_attachments": []interface{}{map[string]interface{}{key: path, "cid": cid}},
		}
	}
	a, _ := sendEmailFingerprint(mk("a", "path", "b=c"))
	b, _ := sendEmailFingerprint(mk("a=b", "path", "c"))
	if a == b {
		t.Fatal("cid and path must be encoded as separate components")
	}
	viaPath, _ := sendEmailFingerprint(mk("x", "path", "chart.png"))
	viaFile, _ := sendEmailFingerprint(mk("x", "file", "chart.png"))
	if viaPath != viaFile {
		t.Fatal("the file alias must identify the same attachment as path")
	}
	none, _ := sendEmailFingerprint(map[string]interface{}{"to_email": []interface{}{"a@x.com"}, "subject": "s", "content": "<img src=\"cid:x\">"})
	if viaFile == none {
		t.Fatal("a file-alias inline attachment must not collapse to the no-attachment key")
	}
}

func TestEmailDedupKey_AttachmentAwareThroughRawInput(t *testing.T) {
	a := emailDedupKey(`{"to_email":["a@x.com"],"subject":"s","content":"c"}`)
	b := emailDedupKey(`{"to_email":["a@x.com"],"subject":"s","content":"c","attachments":["x.csv"]}`)
	if a == b {
		t.Fatal("emailDedupKey must distinguish a send with attachments from one without")
	}
	c := emailDedupKey(`{"to_email":["a@x.com"],"subject":"s","content":"c","attachments":[{"path":"x.csv"}]}`)
	if c == a {
		t.Fatal("emailDedupKey must see object-shaped attachments")
	}
	if c != b {
		t.Fatal("object-shaped and bare-string references to the same path must dedupe together")
	}
}
