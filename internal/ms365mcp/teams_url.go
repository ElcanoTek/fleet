package ms365mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var meetShortlinkRe = regexp.MustCompile(`^/meet/([^/?]+)`)

// ParseTeamsURLResult is the structured payload returned by parse-teams-url.
type ParseTeamsURLResult struct {
	Type          string `json:"type"`
	JoinWebURL    string `json:"joinWebUrl,omitempty"`
	MeetingID     string `json:"meetingId,omitempty"`
	Passcode      string `json:"passcode,omitempty"`
	TenantID      string `json:"tenantId,omitempty"`
	ThreadID      string `json:"threadId,omitempty"`
	OrganizerID   string `json:"organizerId,omitempty"`
	JoinOnlineURL string `json:"joinOnlineUrl,omitempty"`
	Error         string `json:"error,omitempty"`
	SupportedFmts string `json:"supportedFormats,omitempty"`
	Hint          string `json:"hint,omitempty"`
}

// ParseTeamsURL converts a Teams /meet/, /meetup-join/, or meetingrecap URL into
// a joinWebUrl and extracted identifiers suitable for Microsoft Graph queries.
func ParseTeamsURL(rawURL string) ParseTeamsURLResult {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ParseTeamsURLResult{
			Error: "Invalid URL provided",
			Hint:  "Provide a full Teams URL starting with https://teams.microsoft.com/...",
		}
	}

	if m := meetShortlinkRe.FindStringSubmatch(parsed.Path); len(m) == 2 {
		meetingID := m[1]
		passcode := parsed.Query().Get("p")
		return ParseTeamsURLResult{
			Type:          "meet-shortlink",
			MeetingID:     meetingID,
			Passcode:      passcode,
			JoinOnlineURL: fmt.Sprintf("https://teams.microsoft.com/l/meetup-join/19:meeting_%s@thread.v2/0", meetingID),
			Hint: "This is a /meet/ shortlink containing a numeric meeting ID, not a Graph thread ID. " +
				"To find the meeting in Graph: use get-calendar-view across the relevant date range; the response includes onlineMeeting.joinUrl for each event. " +
				"Alternatively, ask the user for the full /l/meetup-join/... link (found in the Outlook calendar invite body) or the meeting recap link.",
		}
	}

	if strings.Contains(parsed.Path, "/meetup-join/") {
		clean := url.URL{
			Scheme:  parsed.Scheme,
			Host:    parsed.Host,
			Path:    parsed.Path,
			RawPath: parsed.RawPath,
		}
		if ctxParam := parsed.Query().Get("context"); ctxParam != "" {
			q := url.Values{}
			q.Set("context", ctxParam)
			clean.RawQuery = q.Encode()
		}
		return ParseTeamsURLResult{
			Type:       "meetup-join",
			JoinWebURL: clean.String(),
			Hint:       "Use this joinWebUrl with get-calendar-view to match against event onlineMeeting.joinUrl",
		}
	}

	if strings.Contains(parsed.Path, "/meetingrecap") {
		q := parsed.Query()
		tenantID := q.Get("tenantId")
		threadID := q.Get("threadId")
		organizerID := q.Get("organizerId")
		if tenantID == "" || threadID == "" || organizerID == "" {
			var missing []string
			if tenantID == "" {
				missing = append(missing, "tenantId")
			}
			if threadID == "" {
				missing = append(missing, "threadId")
			}
			if organizerID == "" {
				missing = append(missing, "organizerId")
			}
			return ParseTeamsURLResult{
				Error: fmt.Sprintf("Meeting recap URL is missing required parameters: %s", strings.Join(missing, ", ")),
				Hint:  "A valid recap URL needs tenantId, threadId, and organizerId query parameters.",
			}
		}
		ctxJSON, _ := json.Marshal(map[string]string{
			"Tid": tenantID,
			"Oid": organizerID,
		})
		joinWebURL := fmt.Sprintf(
			"https://teams.microsoft.com/l/meetup-join/%s/0?context=%s",
			url.PathEscape(threadID),
			url.QueryEscape(string(ctxJSON)),
		)
		return ParseTeamsURLResult{
			Type:        "meetingrecap",
			JoinWebURL:  joinWebURL,
			TenantID:    tenantID,
			ThreadID:    threadID,
			OrganizerID: organizerID,
			Hint:        "Use this joinWebUrl with get-calendar-view to match against event onlineMeeting.joinUrl",
		}
	}

	return ParseTeamsURLResult{
		Error:         "Unrecognized Teams URL format",
		SupportedFmts: "Teams /meet/ shortlinks, /l/meetup-join/ links, and /meetingrecap links",
	}
}
