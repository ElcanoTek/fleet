package tools

import "testing"

func TestBM25Tokenize(t *testing.T) {
	got := bm25Tokenize("send_email sendSlackMessage to User-42")
	want := map[string]bool{"send": true, "email": true, "slack": true, "message": true, "to": true, "user": true, "42": true}
	for _, tok := range got {
		if !want[tok] {
			t.Errorf("unexpected token %q in %v", tok, got)
		}
	}
	// camelCase + snake_case both split so "send email" matches both forms.
	if countTok(got, "send") < 2 { // send_email + sendSlack…
		t.Fatalf("send should appear from both forms: %v", got)
	}
}

func countTok(toks []string, w string) int {
	n := 0
	for _, t := range toks {
		if t == w {
			n++
		}
	}
	return n
}

func TestBM25Search(t *testing.T) {
	idx := NewBM25Index([]BM25Doc{
		{ID: "slack_send_message", Text: "slack_send_message Send a message to a Slack channel"},
		{ID: "jira_create_issue", Text: "jira_create_issue Create a new issue/ticket in Jira"},
		{ID: "gcal_create_event", Text: "gcal_create_event Create a calendar event in Google Calendar"},
		{ID: "email_send", Text: "email_send Send an email to a recipient"},
	})

	cases := []struct {
		query string
		top   string
	}{
		{"send a slack message", "slack_send_message"},
		{"create jira ticket", "jira_create_issue"},
		{"schedule a calendar event", "gcal_create_event"},
		{"email someone", "email_send"},
	}
	for _, tc := range cases {
		hits := idx.Search(tc.query, 3)
		if len(hits) == 0 {
			t.Fatalf("%q: no hits", tc.query)
		}
		if hits[0].ID != tc.top {
			t.Errorf("%q: top = %s, want %s (all=%v)", tc.query, hits[0].ID, tc.top, hits)
		}
	}

	// No overlap → no hits; blank/empty are safe.
	if len(idx.Search("quantum teleportation", 5)) != 0 {
		t.Error("irrelevant query should return no hits")
	}
	if len(idx.Search("", 5)) != 0 {
		t.Error("blank query returns nothing")
	}
	if len(NewBM25Index(nil).Search("anything", 5)) != 0 {
		t.Error("empty index returns nothing")
	}

	// limit is honored.
	if got := idx.Search("send", 1); len(got) != 1 {
		t.Fatalf("limit=1: got %d", len(got))
	}
}

// TestBM25SearchScalesTo500 is the acceptance check: a large synthetic catalog
// ranks the right tool into the top-K by keyword alone (no embeddings).
func TestBM25SearchScalesTo500(t *testing.T) {
	docs := make([]BM25Doc, 0, 500)
	for i := 0; i < 499; i++ {
		docs = append(docs, BM25Doc{ID: "noise_tool_" + itoa(i), Text: "noise_tool generic filler capability number " + itoa(i)})
	}
	docs = append(docs, BM25Doc{ID: "stripe_refund_charge", Text: "stripe_refund_charge Refund a Stripe payment charge to a customer"})
	idx := NewBM25Index(docs)
	hits := idx.Search("refund a stripe payment", 5)
	if len(hits) == 0 || hits[0].ID != "stripe_refund_charge" {
		t.Fatalf("needle not top-ranked among 500: %v", hits)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
