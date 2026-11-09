package ms365mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
	"github.com/ElcanoTek/fleet/internal/netguard"
)

func TestConfigFromEnvAndMatching(t *testing.T) {
	t.Setenv("MS365_MCP_CLIENT_ID", "68dad9d6-e1aa-4195-9bd5-2e94a0e4a073")
	t.Setenv("MS365_MCP_TENANT_ID", "5db5070c-2c68-449d-868f-2b0d3407f6d2")
	t.Setenv("MS365_MCP_ALIASES", "https://fleetdev.elcanotek.com/")
	t.Setenv("MS365_MCP_ALLOWED_REDIRECT_URIS", "https://sandbox.reklaimfleet.com/api/oauth/mcp/callback")
	t.Setenv("MS365_MCP_ATTACHMENT_URL_TTL_S", "420")

	fleetCfg := &config.Config{
		PublicBaseURL: "https://fleet.elcanotek.com/",
	}
	cfg := ConfigFromEnv(fleetCfg)
	if !cfg.Enabled() {
		t.Fatalf("expected cfg.Enabled() = true")
	}
	if cfg.PublicURL != "https://fleet.elcanotek.com" {
		t.Fatalf("unexpected PublicURL: %q", cfg.PublicURL)
	}
	if cfg.AttachmentURLTTLSeconds != 420 {
		t.Fatalf("unexpected AttachmentURLTTLSeconds: %d", cfg.AttachmentURLTTLSeconds)
	}
	wantRedirects := []string{
		"https://fleet.elcanotek.com/api/oauth/mcp/callback",
		"https://fleetdev.elcanotek.com/api/oauth/mcp/callback",
		"https://sandbox.reklaimfleet.com/api/oauth/mcp/callback",
	}
	if len(cfg.AllowedRedirectURIs) != len(wantRedirects) {
		t.Fatalf("got redirect URIs %v, want %v", cfg.AllowedRedirectURIs, wantRedirects)
	}
	for i, want := range wantRedirects {
		if cfg.AllowedRedirectURIs[i] != want {
			t.Fatalf("AllowedRedirectURIs[%d] = %q, want %q", i, cfg.AllowedRedirectURIs[i], want)
		}
	}

	for _, matchURL := range []string{
		"https://fleet.elcanotek.com/mcp",
		"https://fleet.elcanotek.com/.well-known/oauth-authorization-server",
		"https://fleet.elcanotek.com/.well-known/oauth-protected-resource/mcp",
		"https://fleetdev.elcanotek.com/mcp",
		"https://fleetdev.elcanotek.com/token",
		"https://fleetdev.elcanotek.com/attachment?t=abc",
	} {
		u, err := url.Parse(matchURL)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.MatchesInProcessURL(u) {
			t.Errorf("expected MatchesInProcessURL(%q) = true", matchURL)
		}
	}

	for _, nonMatchURL := range []string{
		"https://fleet.elcanotek.com/api/v1/conversations",
		"https://other.example.com/mcp",
	} {
		u, _ := url.Parse(nonMatchURL)
		if cfg.MatchesInProcessURL(u) {
			t.Errorf("expected MatchesInProcessURL(%q) = false", nonMatchURL)
		}
	}
}

func TestCatalogContainsAll57Tools(t *testing.T) {
	h := NewHandler(Config{ClientID: "test-client", PublicURL: "https://fleet.elcanotek.com"})
	names := h.ToolNames()
	if len(names) != 57 {
		t.Fatalf("expected 57 tools in catalog, got %d", len(names))
	}
	wantTools := []string{
		// Mail (18)
		"list-mail-messages", "list-mail-folders", "list-mail-folder-messages", "get-mail-message",
		"read-mail-text", "list-mail-attachments", "get-mail-attachment-content", "get-download-url",
		"create-draft-email", "create-reply-draft", "create-reply-all-draft", "create-forward-draft",
		"send-mail", "send-draft-message", "reply-mail-message", "reply-all-mail-message",
		"forward-mail-message", "move-mail-message",
		// Calendar (10)
		"list-calendars", "list-calendar-events", "get-calendar-event", "get-calendar-view",
		"create-calendar-event", "update-calendar-event", "delete-calendar-event",
		"accept-calendar-event", "decline-calendar-event", "tentatively-accept-calendar-event",
		// Teams & Chats (19)
		"list-chats", "get-chat", "list-chat-messages", "get-chat-message", "list-chat-members",
		"create-chat", "send-chat-message", "reply-to-chat-message", "list-joined-teams",
		"get-team", "list-team-channels", "get-team-channel", "list-team-members",
		"list-channel-messages", "get-channel-message", "list-channel-message-replies",
		"send-channel-message", "reply-to-channel-message", "parse-teams-url",
		// OneDrive & Search (4)
		"list-folder-files", "get-drive-item", "search-onedrive-files", "search-query",
		// To Do (4)
		"list-todo-task-lists", "list-todo-tasks", "create-todo-task", "update-todo-task",
		// Contacts (2)
		"list-outlook-contacts", "get-outlook-contact",
	}
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	for _, want := range wantTools {
		if !nameSet[want] {
			t.Errorf("missing expected tool %q", want)
		}
	}
}

func TestOAuthDiscoveryAndTwoLegPKCEFlowInProcess(t *testing.T) {
	var capturedMSCodeVerifier string
	var capturedRefreshToken string

	fakeEntra := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tenant-123/oauth2/v2.0/token" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		switch vals.Get("grant_type") {
		case "authorization_code":
			capturedMSCodeVerifier = vals.Get("code_verifier")
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token":  "graph-access-token-1",
				"refresh_token": "graph-refresh-token-1",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		case "refresh_token":
			capturedRefreshToken = vals.Get("refresh_token")
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token":  "graph-access-token-2",
				"refresh_token": "graph-refresh-token-2",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
		}
	}))
	defer fakeEntra.Close()

	cfg := Config{
		ClientID:            "68dad9d6-e1aa-4195-9bd5-2e94a0e4a073",
		TenantID:            "tenant-123",
		PublicURL:           "https://fleet.elcanotek.com",
		Aliases:             []string{"https://fleetdev.elcanotek.com"},
		AllowedRedirectURIs: []string{"https://fleet.elcanotek.com/api/oauth/mcp/callback", "https://fleetdev.elcanotek.com/api/oauth/mcp/callback"},
		AuthBaseURL:         fakeEntra.URL,
		HTTPClient:          fakeEntra.Client(),
	}
	h := NewHandler(cfg)
	netguard.RegisterInProcessHandler("test-ms365", cfg.MatchesInProcessURL, h)
	defer netguard.RegisterInProcessHandler("test-ms365", nil, nil)

	// 1. Discover against both primary PublicURL and alias URL using mcpoauth.SafeHTTPClient!
	safeClient := mcpoauth.SafeHTTPClient(10 * time.Second)
	for _, mcpURL := range []string{
		"https://fleet.elcanotek.com/mcp",
		"https://fleetdev.elcanotek.com/mcp",
	} {
		disc, err := mcpoauth.Discover(context.Background(), safeClient, mcpURL)
		if err != nil {
			t.Fatalf("mcpoauth.Discover(%s) failed: %v", mcpURL, err)
		}
		if !strings.HasSuffix(disc.AS.AuthorizationEndpoint, "/authorize") {
			t.Fatalf("unexpected authorization_endpoint: %s", disc.AS.AuthorizationEndpoint)
		}
		if !strings.HasSuffix(disc.AS.TokenEndpoint, "/token") {
			t.Fatalf("unexpected token_endpoint: %s", disc.AS.TokenEndpoint)
		}
	}

	// 2. Dynamic client registration.
	regResp, err := safeClient.Post("https://fleet.elcanotek.com/register", "application/json", strings.NewReader(`{"client_name":"Fleet MCP"}`))
	if err != nil {
		t.Fatalf("POST /register failed: %v", err)
	}
	defer regResp.Body.Close()
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /register status = %d, want 201", regResp.StatusCode)
	}

	// 3. Two-leg PKCE /authorize -> /token.
	clientVerifier := "client-pkce-verifier-secret-value-1234567890"
	clientChallenge := computeS256Challenge(clientVerifier)

	authReq := httptest.NewRequest(http.MethodGet,
		"https://fleet.elcanotek.com/authorize?response_type=code&client_id=mcp-client-1&redirect_uri="+
			url.QueryEscape("https://fleet.elcanotek.com/api/oauth/mcp/callback")+
			"&state=state-xyz&code_challenge="+url.QueryEscape(clientChallenge)+"&code_challenge_method=S256",
		nil,
	)
	authRec := httptest.NewRecorder()
	h.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusFound {
		t.Fatalf("GET /authorize status = %d, want 302", authRec.Code)
	}
	loc, err := url.Parse(authRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect Location: %v", err)
	}
	msChallengeSent := loc.Query().Get("code_challenge")
	if msChallengeSent == "" || msChallengeSent == clientChallenge {
		t.Fatalf("expected server to generate its own second-leg PKCE challenge, got %q", msChallengeSent)
	}

	// Exchange authorization_code at POST /token using clientVerifier.
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", "auth-code-from-ms")
	form.Set("redirect_uri", "https://fleet.elcanotek.com/api/oauth/mcp/callback")
	form.Set("code_verifier", clientVerifier)

	tokResp, err := safeClient.Post("https://fleet.elcanotek.com/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("POST /token failed: %v", err)
	}
	defer tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /token status = %d", tokResp.StatusCode)
	}
	if computeS256Challenge(capturedMSCodeVerifier) != msChallengeSent {
		t.Fatalf("server sent code_verifier %q whose S256 %q != challenge %q sent to Microsoft",
			capturedMSCodeVerifier, computeS256Challenge(capturedMSCodeVerifier), msChallengeSent)
	}

	// 4. Refresh token via alias URL in-process.
	refreshForm := url.Values{}
	refreshForm.Set("grant_type", "refresh_token")
	refreshForm.Set("refresh_token", "rt-from-db")
	refResp, err := safeClient.Post("https://fleetdev.elcanotek.com/token", "application/x-www-form-urlencoded", strings.NewReader(refreshForm.Encode()))
	if err != nil {
		t.Fatalf("POST /token (refresh) failed: %v", err)
	}
	defer refResp.Body.Close()
	if refResp.StatusCode != http.StatusOK || capturedRefreshToken != "rt-from-db" {
		t.Fatalf("unexpected refresh response status=%d capturedRefreshToken=%q", refResp.StatusCode, capturedRefreshToken)
	}
}

func TestMailAutoExpandAttachmentsAndTicketDownload(t *testing.T) {
	var lastMessagesQuery url.Values

	fakeGraph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer valid-graph-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1.0/me/messages":
			lastMessagesQuery = r.URL.Query()
			writeJSON(w, http.StatusOK, map[string]any{
				"@odata.context": "https://graph.microsoft.com/v1.0/$metadata#users('me')/messages",
				"value": []any{
					map[string]any{
						"id":             "msg-001",
						"subject":        "Q3 Board Deck",
						"hasAttachments": true,
						"attachments": []any{
							map[string]any{
								"@odata.type": "#microsoft.graph.fileAttachment",
								"id":          "att-999",
								"name":        "Q3-Deck.pdf",
								"contentType": "application/pdf",
								"size":        12345,
								"isInline":    false,
							},
						},
					},
				},
			})
		case "/v1.0/me/messages/msg-001/attachments/att-999":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":          "att-999",
				"name":        "Q3-Deck.pdf",
				"contentType": "application/pdf",
				"size":        9,
			})
		case "/v1.0/me/messages/msg-001/attachments/att-999/$value":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("%PDF-1.7\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGraph.Close()

	cfg := Config{
		ClientID:                "68dad9d6-e1aa-4195-9bd5-2e94a0e4a073",
		PublicURL:               "https://fleet.elcanotek.com",
		AttachmentURLTTLSeconds: 300,
		GraphBaseURL:            fakeGraph.URL + "/v1.0",
		HTTPClient:              fakeGraph.Client(),
	}
	h := NewHandler(cfg)
	netguard.RegisterInProcessHandler("test-ms365-mail", cfg.MatchesInProcessURL, h)
	defer netguard.RegisterInProcessHandler("test-ms365-mail", nil, nil)

	callTool := func(name string, args map[string]any) map[string]any {
		t.Helper()
		rpcBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      name,
				"arguments": args,
			},
		})
		req := httptest.NewRequest(http.MethodPost, "https://fleet.elcanotek.com/mcp", bytes.NewReader(rpcBody))
		req.Header.Set("Authorization", "Bearer valid-graph-token")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/call %s status = %d: %s", name, rec.Code, rec.Body.String())
		}
		var rpcResp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &rpcResp); err != nil {
			t.Fatalf("unmarshal rpcResp: %v", err)
		}
		if rpcResp.Result.IsError {
			t.Fatalf("tool %s returned error: %s", name, rpcResp.Result.Content[0].Text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(rpcResp.Result.Content[0].Text), &out); err != nil {
			t.Fatalf("unmarshal tool text %q: %v", rpcResp.Result.Content[0].Text, err)
		}
		return out
	}

	// 1. Call list-mail-messages with $select="id,subject,hasAttachments" (omitting attachments)
	// and verify $expand=attachments(...) was sent AND attachments were preserved in output!
	listRes := callTool("list-mail-messages", map[string]any{
		"top":    5,
		"select": "id,subject,hasAttachments",
	})
	if gotExpand := lastMessagesQuery.Get("$expand"); gotExpand != defaultMailAttachmentExpand {
		t.Fatalf("expected auto $expand=%q, got %q", defaultMailAttachmentExpand, gotExpand)
	}
	vals, _ := listRes["value"].([]any)
	if len(vals) != 1 {
		t.Fatalf("expected 1 message, got %d", len(vals))
	}
	msg0 := vals[0].(map[string]any)
	atts, _ := msg0["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("expected 1 expanded attachment on message, got %v", msg0["attachments"])
	}
	att0 := atts[0].(map[string]any)
	if att0["id"] != "att-999" || att0["name"] != "Q3-Deck.pdf" {
		t.Fatalf("unexpected attachment metadata: %v", att0)
	}

	// 2. Call get-mail-attachment-content and verify single-use downloadUrl works in-process!
	dlRes := callTool("get-mail-attachment-content", map[string]any{
		"messageId":    "msg-001",
		"attachmentId": "att-999",
	})
	dlURL, _ := dlRes["downloadUrl"].(string)
	if !strings.HasPrefix(dlURL, "https://fleet.elcanotek.com/attachment?t=") {
		t.Fatalf("unexpected downloadUrl: %q", dlURL)
	}
	if dlRes["name"] != "Q3-Deck.pdf" || dlRes["contentType"] != "application/pdf" {
		t.Fatalf("expected probed name/contentType on ticket response, got %v", dlRes)
	}

	safeClient := mcpoauth.SafeHTTPClient(10 * time.Second)
	// HEAD request must be rejected with 405 without consuming the single-use ticket.
	headReq, _ := http.NewRequest(http.MethodHead, dlURL, nil)
	headResp, err := safeClient.Do(headReq)
	if err != nil {
		t.Fatalf("HEAD %s failed: %v", dlURL, err)
	}
	_ = headResp.Body.Close()
	if headResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD /attachment status = %d, want 405", headResp.StatusCode)
	}

	attResp, err := safeClient.Get(dlURL)
	if err != nil {
		t.Fatalf("GET %s failed: %v", dlURL, err)
	}
	defer attResp.Body.Close()
	if attResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /attachment status = %d", attResp.StatusCode)
	}
	if ct := attResp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Fatalf("expected probed Content-Type application/pdf, got %q", ct)
	}
	if cd := attResp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="Q3-Deck.pdf"`) {
		t.Fatalf("unexpected Content-Disposition: %q", cd)
	}
	fileBytes, _ := io.ReadAll(attResp.Body)
	if string(fileBytes) != "%PDF-1.7\n" {
		t.Fatalf("unexpected attachment bytes: %q", string(fileBytes))
	}

	// Second redeem of single-use ticket must return 401.
	attResp2, err := safeClient.Get(dlURL)
	if err != nil {
		t.Fatalf("second GET %s failed: %v", dlURL, err)
	}
	defer attResp2.Body.Close()
	if attResp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on second use of single-use ticket, got %d", attResp2.StatusCode)
	}
}

func TestUtilityToolsAndMentionsAndExpiredJWT(t *testing.T) {
	var capturedChatMessageBody map[string]any

	fakeGraph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/chats/chat-1/messages":
			_ = json.NewDecoder(r.Body).Decode(&capturedChatMessageBody)
			writeJSON(w, http.StatusCreated, map[string]any{"id": "chat-msg-1"})
		case "/v1.0/me/messages/msg-clean":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":               "msg-clean",
				"subject":          "Cleaned Mail",
				"receivedDateTime": "2026-11-09T12:00:00Z",
				"from": map[string]any{
					"emailAddress": map[string]any{"name": "Alice", "address": "alice@example.com"},
				},
				"body": map[string]any{
					"contentType": "text",
					"content":     "Check <https://nam01.safelinks.protection.outlook.com/?url=https%3A%2F%2Fexample.com%2Fdoc%3Futm_source%3Dnewsletter%26id%3D42&data=05>\n\nOn Mon, Nov 9, 2026 Alice wrote:\n> old quoted line",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGraph.Close()

	h := NewHandler(Config{
		ClientID:     "test-client",
		PublicURL:    "https://fleet.elcanotek.com",
		GraphBaseURL: fakeGraph.URL + "/v1.0",
		HTTPClient:   fakeGraph.Client(),
	})

	// 1. Expired JWT must return 401 with WWW-Authenticate.
	expiredPayload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(-10*time.Minute).Unix())))
	expiredJWT := "eyJhbGciOiJSUzI1NiJ9." + expiredPayload + ".sig"
	reqExp := httptest.NewRequest(http.MethodPost, "https://fleet.elcanotek.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	reqExp.Header.Set("Authorization", "Bearer "+expiredJWT)
	recExp := httptest.NewRecorder()
	h.ServeHTTP(recExp, reqExp)
	if recExp.Code != http.StatusUnauthorized || !strings.Contains(recExp.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("expected 401 with WWW-Authenticate on expired JWT, got %d (%q)", recExp.Code, recExp.Header().Get("WWW-Authenticate"))
	}

	// 2. parse-teams-url tool.
	parsed := ParseTeamsURL("https://teams.microsoft.com/l/meetingrecap?tenantId=t1&threadId=19:meeting_abc@thread.v2&organizerId=o1")
	if parsed.Type != "meetingrecap" || !strings.Contains(parsed.JoinWebURL, "meetup-join") {
		t.Fatalf("unexpected ParseTeamsURL result: %+v", parsed)
	}

	// 3. send-chat-message @mention shorthand rewriting.
	rpcBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "send-chat-message",
			"arguments": map[string]any{
				"chatId": "chat-1",
				"body": map[string]any{
					"body": map[string]any{
						"contentType": "html",
						"content":     `Hello <at user-id="11111111-2222-3333-4444-555555555555">Brad Flaugher</at>!`,
					},
				},
			},
		},
	})
	reqChat := httptest.NewRequest(http.MethodPost, "https://fleet.elcanotek.com/mcp", bytes.NewReader(rpcBody))
	reqChat.Header.Set("Authorization", "Bearer valid-token")
	recChat := httptest.NewRecorder()
	h.ServeHTTP(recChat, reqChat)
	if recChat.Code != http.StatusOK {
		t.Fatalf("send-chat-message status = %d", recChat.Code)
	}
	innerBody, _ := capturedChatMessageBody["body"].(map[string]any)
	if gotContent, _ := innerBody["content"].(string); gotContent != `Hello <at id="0">Brad Flaugher</at>!` {
		t.Fatalf("unexpected rewritten mention content: %q", gotContent)
	}
	mentions, _ := capturedChatMessageBody["mentions"].([]any)
	if len(mentions) != 1 {
		t.Fatalf("expected 1 mention entry, got %v", mentions)
	}

	// 4. read-mail-text quote stripping and SafeLinks/UTM unwrapping.
	res, err := h.executeReadMailText(context.Background(), "valid-token", map[string]any{
		"messageIds": []any{"msg-clean"},
	})
	if err != nil {
		t.Fatalf("executeReadMailText failed: %v", err)
	}
	entries, ok := res.([]map[string]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("unexpected read-mail-text result: %#v", res)
	}
	if gotText, _ := entries[0]["text"].(string); gotText != "Check https://example.com/doc?id=42" {
		t.Fatalf("unexpected cleaned mail text: %q", gotText)
	}

	// 5. read-mail-text rejects > 20 messageIds.
	manyIDs := make([]any, 21)
	for i := range manyIDs {
		manyIDs[i] = fmt.Sprintf("msg-%d", i)
	}
	tooManyRes, err := h.executeReadMailText(context.Background(), "valid-token", map[string]any{
		"messageIds": manyIDs,
	})
	if err != nil {
		t.Fatalf("unexpected error on >20 messageIds: %v", err)
	}
	if errMap, ok := tooManyRes.(map[string]any); !ok || errMap["error"] == nil {
		t.Fatalf("expected error map for >20 messageIds, got %#v", tooManyRes)
	}
}

func TestMutatingToolsMarkedCriticalInToolsList(t *testing.T) {
	h := NewHandler(Config{
		ClientID:  "client-123",
		PublicURL: "https://fleet.example.com",
	})
	listReq := httptest.NewRequest(http.MethodPost, "https://fleet.example.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	listReq.Header.Set("Authorization", "Bearer valid-token")
	listRec := httptest.NewRecorder()
	h.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d: %s", listRec.Code, listRec.Body.String())
	}
	var rpcResp struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Critical    bool   `json:"critical"`
				Annotations struct {
					ReadOnlyHint *bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &rpcResp); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	criticalCount := 0
	for _, tool := range rpcResp.Result.Tools {
		readOnly := tool.Annotations.ReadOnlyHint != nil && *tool.Annotations.ReadOnlyHint
		if !readOnly && !tool.Critical {
			t.Errorf("mutating tool %q must have critical=true in tools/list", tool.Name)
		}
		if readOnly && tool.Critical {
			t.Errorf("read-only tool %q must not have critical=true in tools/list", tool.Name)
		}
		if tool.Critical {
			criticalCount++
		}
	}
	if criticalCount != 23 {
		t.Fatalf("critical tool count = %d, want 23", criticalCount)
	}
}

func TestBrokerIsolatedMCPAndUnixSocketAttachmentProxy(t *testing.T) {
	mockGraph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/me/messages/msg-1/attachments/att-1":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":          "att-1",
				"name":        "broker.csv",
				"contentType": "text/csv",
				"size":        12,
			})
		case "/v1.0/me/messages/msg-1/attachments/att-1/$value":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("col1,col2\n1,2"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockGraph.Close()

	// Child broker handler: owns Graph execution and attachment tickets.
	brokerHandler := NewHandler(Config{
		ClientID:     "client-123",
		PublicURL:    "https://fleet.example.com",
		GraphBaseURL: mockGraph.URL + "/v1.0",
		HTTPClient:   mockGraph.Client(),
	})

	sockPath := filepath.Join(t.TempDir(), "attachment.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen unix: %v", err)
	}
	defer ln.Close()
	sockSrv := &http.Server{Handler: AttachmentOnlyHandler(brokerHandler)}
	defer sockSrv.Close()
	go func() { _ = sockSrv.Serve(ln) }()

	// Parent web handler: refuses authenticated POST /mcp and proxies /attachment over unix socket.
	parentHandler := NewHandler(Config{
		ClientID:                "client-123",
		PublicURL:               "https://fleet.example.com",
		DisableAuthenticatedMCP: true,
		AttachmentProxy:         NewUnixSocketAttachmentProxy(sockPath),
	})

	// 1. Unauthenticated POST /mcp on parent still returns 401 + WWW-Authenticate for OAuth discovery.
	unauthReq := httptest.NewRequest(http.MethodPost, "https://fleet.example.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	unauthRec := httptest.NewRecorder()
	parentHandler.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized || !strings.Contains(unauthRec.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("expected 401 with WWW-Authenticate on parent unauth /mcp, got %d (%q)", unauthRec.Code, unauthRec.Header().Get("WWW-Authenticate"))
	}

	// 2. Authenticated POST /mcp on parent is refused with 403 Forbidden.
	authReq := httptest.NewRequest(http.MethodPost, "https://fleet.example.com/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	authReq.Header.Set("Authorization", "Bearer graph-token")
	authRec := httptest.NewRecorder()
	parentHandler.ServeHTTP(authRec, authReq)
	if authRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden on parent authenticated /mcp, got %d: %s", authRec.Code, authRec.Body.String())
	}

	// 3. Mint ticket inside broker handler and redeem via parent handler over the token-free unix socket.
	res, isErr, err := brokerHandler.executeToolCall(context.Background(), nil, "graph-token", "get-mail-attachment-content", map[string]any{
		"messageId":    "msg-1",
		"attachmentId": "att-1",
	})
	if err != nil || isErr {
		t.Fatalf("broker get-mail-attachment-content failed: isErr=%v err=%v", isErr, err)
	}
	dlURL, _ := res.(map[string]any)["downloadUrl"].(string)
	if dlURL == "" {
		t.Fatalf("missing downloadUrl in %#v", res)
	}

	dlReq := httptest.NewRequest(http.MethodGet, dlURL, nil)
	dlRec := httptest.NewRecorder()
	parentHandler.ServeHTTP(dlRec, dlReq)
	if dlRec.Code != http.StatusOK {
		t.Fatalf("parent proxied GET /attachment status = %d: %s", dlRec.Code, dlRec.Body.String())
	}
	if dlRec.Body.String() != "col1,col2\n1,2" {
		t.Fatalf("parent proxied body = %q, want %q", dlRec.Body.String(), "col1,col2\n1,2")
	}
	if gotCD := dlRec.Header().Get("Content-Disposition"); !strings.Contains(gotCD, "broker.csv") {
		t.Fatalf("parent proxied Content-Disposition = %q, want broker.csv", gotCD)
	}
}

func TestRedirectURIMatchesQueryAndFragmentAndCalendarAndOneDriveFixes(t *testing.T) {
	patterns := []string{
		"http://localhost:*",
		"https://fleet.elcanotek.com/api/oauth/mcp/callback",
	}
	if !isRedirectURIAllowed("https://fleet.elcanotek.com/api/oauth/mcp/callback", patterns) {
		t.Fatal("exact callback URI must be allowed")
	}
	if isRedirectURIAllowed("https://fleet.elcanotek.com/api/oauth/mcp/callback?code=injected", patterns) {
		t.Fatal("callback URI with unexpected query must be rejected")
	}
	if isRedirectURIAllowed("https://fleet.elcanotek.com/api/oauth/mcp/callback#frag", patterns) {
		t.Fatal("callback URI with fragment must be rejected")
	}
	if isRedirectURIAllowed("http://localhost:8080/callback?injected=1", patterns) {
		t.Fatal("loopback URI with unexpected query must be rejected")
	}

	var gotCalExpand, gotDriveSearchPath string
	mockGraph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1.0/me/events":
			gotCalExpand = r.URL.Query().Get("$expand")
			writeJSON(w, http.StatusOK, map[string]any{"value": []any{}})
		case strings.HasPrefix(r.URL.Path, "/v1.0/drives/"):
			gotDriveSearchPath = r.URL.Path
			writeJSON(w, http.StatusOK, map[string]any{"value": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockGraph.Close()

	h := NewHandler(Config{
		ClientID:     "client-123",
		PublicURL:    "https://fleet.elcanotek.com",
		GraphBaseURL: mockGraph.URL + "/v1.0",
		HTTPClient:   mockGraph.Client(),
	})

	if _, isErr, err := h.executeToolCall(context.Background(), nil, "tok", "list-calendar-events", map[string]any{
		"expandExtendedProperties": true,
	}); err != nil || isErr {
		t.Fatalf("list-calendar-events failed: isErr=%v err=%v", isErr, err)
	}
	if gotCalExpand != "singleValueExtendedProperties($filter=id ne '')" {
		t.Fatalf("calendar $expand = %q, want singleValueExtendedProperties($filter=id ne '')", gotCalExpand)
	}

	if _, isErr, err := h.executeToolCall(context.Background(), nil, "tok", "search-onedrive-files", map[string]any{
		"driveId": "drv-1",
		"q":       "budget",
	}); err != nil || isErr {
		t.Fatalf("search-onedrive-files failed: isErr=%v err=%v", isErr, err)
	}
	if gotDriveSearchPath != "/v1.0/drives/drv-1/root/search(q='budget')" {
		t.Fatalf("search-onedrive-files path = %q, want /v1.0/drives/drv-1/root/search(q='budget')", gotDriveSearchPath)
	}
}

func TestDCRValidationPKCENonEvictionListAttachmentsAndForwardedMail(t *testing.T) {
	var gotAttachmentSelect string
	mockGraph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.0/me/messages/msg-1/attachments" {
			gotAttachmentSelect = r.URL.Query().Get("$select")
			writeJSON(w, http.StatusOK, map[string]any{
				"value": []any{
					map[string]any{
						"id":           "att-1",
						"name":         "huge.pdf",
						"contentType":  "application/pdf",
						"size":         1048576,
						"isInline":     false,
						"contentBytes": "BASE64_PAYLOAD_SHOULD_BE_STRIPPED",
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockGraph.Close()

	h := NewHandler(Config{
		ClientID:            "client-123",
		PublicURL:           "https://fleet.elcanotek.com",
		AllowedRedirectURIs: []string{"https://fleet.elcanotek.com/api/oauth/mcp/callback"},
		GraphBaseURL:        mockGraph.URL + "/v1.0",
		HTTPClient:          mockGraph.Client(),
	})

	// 1. Dynamic client registration rejects disallowed redirect_uris.
	badRegReq := httptest.NewRequest(http.MethodPost, "https://fleet.elcanotek.com/register", strings.NewReader(`{"client_name":"Test","redirect_uris":["https://evil.example.com/cb"]}`))
	badRegRec := httptest.NewRecorder()
	h.ServeHTTP(badRegRec, badRegReq)
	if badRegRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for disallowed DCR redirect_uri, got %d: %s", badRegRec.Code, badRegRec.Body.String())
	}

	// 2. PKCE per-IP cap rejects flood from one IP without evicting another IP's active entry.
	legitVerifier := "legit-verifier-value-1234567890"
	if !h.pkce.put("198.51.100.10", "legit-state", computeS256Challenge(legitVerifier), "srv-verifier-1", "Mail.Read") {
		t.Fatal("expected legitimate PKCE put to succeed")
	}
	for i := range pkceStoreMaxPerIP {
		ok := h.pkce.put("203.0.113.99", fmt.Sprintf("flood-%d", i), fmt.Sprintf("chal-%d", i), "srv-flood", "Mail.Read")
		if !ok {
			t.Fatalf("expected put %d under per-IP cap to succeed", i)
		}
	}
	if h.pkce.put("203.0.113.99", "flood-over", "chal-over", "srv-flood", "Mail.Read") {
		t.Fatal("expected put exceeding per-IP cap to be rejected")
	}
	if gotSrv, _, ok := h.pkce.lookupMatchingVerifier(legitVerifier); !ok || gotSrv != "srv-verifier-1" {
		t.Fatalf("legitimate PKCE entry was evicted: ok=%v gotSrv=%q", ok, gotSrv)
	}

	// 3. list-mail-attachments defaults $select and strips contentBytes.
	attRes, isErr, err := h.executeToolCall(context.Background(), nil, "tok", "list-mail-attachments", map[string]any{
		"messageId": "msg-1",
	})
	if err != nil || isErr {
		t.Fatalf("list-mail-attachments failed: isErr=%v err=%v", isErr, err)
	}
	if gotAttachmentSelect != "id,name,contentType,size,isInline" {
		t.Fatalf("list-mail-attachments $select = %q, want id,name,contentType,size,isInline", gotAttachmentSelect)
	}
	attJSON, _ := json.Marshal(attRes)
	if strings.Contains(string(attJSON), "contentBytes") || strings.Contains(string(attJSON), "BASE64_PAYLOAD") {
		t.Fatalf("list-mail-attachments leaked contentBytes: %s", string(attJSON))
	}

	// 4. cleanMailText preserves forwarded message content while still stripping reply history.
	forwarded := "FYI see below.\n\n---------- Forwarded message ----------\nFrom: Alice <alice@example.com>\nDate: Mon, Nov 9, 2026 at 10:00 AM\nSubject: Q4 Numbers\nTo: Bob <bob@example.com>\n\nRevenue grew 28% YoY.\n\n-----Original Message-----\nFrom: Old Thread <old@example.com>\nSent: Sunday\nTo: Alice\nSubject: Re: Q4 Numbers\n\nOlder reply quote."
	cleaned := cleanMailText(forwarded, true)
	if !strings.Contains(cleaned, "Revenue grew 28% YoY.") {
		t.Fatalf("cleanMailText stripped forwarded body: %q", cleaned)
	}
	if strings.Contains(cleaned, "Older reply quote.") {
		t.Fatalf("cleanMailText failed to strip trailing Original Message reply: %q", cleaned)
	}
}

func TestReview5833450252Findings(t *testing.T) {
	t.Parallel()

	var lastPath, lastContentType string
	var lastBody map[string]any
	mockGraph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		lastContentType = r.Header.Get("Content-Type")
		lastBody = nil
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&lastBody)
		}
		writeJSON(w, http.StatusOK, map[string]any{"value": []any{}})
	}))
	defer mockGraph.Close()

	h := NewHandler(Config{
		ClientID:     "client-123",
		PublicURL:    "https://fleet.elcanotek.com",
		GraphBaseURL: mockGraph.URL + "/v1.0",
		HTTPClient:   mockGraph.Client(),
	})

	// 1. reply-to-chat-message accepts chatMessageId + body and rejects incomplete payloads.
	if _, isErr, err := h.executeToolCall(context.Background(), nil, "tok", "reply-to-chat-message", map[string]any{
		"chatId": "chat-1",
	}); err == nil || !isErr {
		t.Fatal("expected reply-to-chat-message with only chatId to fail")
	}
	if _, isErr, err := h.executeToolCall(context.Background(), nil, "tok", "reply-to-chat-message", map[string]any{
		"chatId":        "chat-1",
		"chatMessageId": "msg-99",
		"body": map[string]any{
			"body": map[string]any{"contentType": "html", "content": "Quoted reply"},
		},
	}); err != nil || isErr {
		t.Fatalf("reply-to-chat-message with chatMessageId + body failed: isErr=%v err=%v", isErr, err)
	}
	if lastPath != "/v1.0/chats/chat-1/messages/replyWithQuote" {
		t.Fatalf("unexpected path %q", lastPath)
	}
	ids, _ := lastBody["messageIds"].([]any)
	if len(ids) != 1 || ids[0] != "msg-99" {
		t.Fatalf("expected messageIds=[msg-99], got %#v", lastBody["messageIds"])
	}

	// 2. get-calendar-view routes to /me/calendars/{calendarId}/calendarView when calendarId is provided.
	if _, isErr, err := h.executeToolCall(context.Background(), nil, "tok", "get-calendar-view", map[string]any{
		"calendarId":    "cal-secondary",
		"startDateTime": "2026-11-09T00:00:00Z",
		"endDateTime":   "2026-11-10T00:00:00Z",
	}); err != nil || isErr {
		t.Fatalf("get-calendar-view with calendarId failed: isErr=%v err=%v", isErr, err)
	}
	if lastPath != "/v1.0/me/calendars/cal-secondary/calendarView" {
		t.Fatalf("get-calendar-view path = %q, want /v1.0/me/calendars/cal-secondary/calendarView", lastPath)
	}

	// 3. send-mail with string body sets Content-Type: text/plain.
	if _, isErr, err := h.executeToolCall(context.Background(), nil, "tok", "send-mail", map[string]any{
		"body": "RAW_BASE64_MIME_PAYLOAD",
	}); err != nil || isErr {
		t.Fatalf("send-mail string body failed: isErr=%v err=%v", isErr, err)
	}
	if lastContentType != "text/plain" {
		t.Fatalf("send-mail string body Content-Type = %q, want text/plain", lastContentType)
	}

	// 4. AttachmentTicketStore preserves live tickets when full instead of evicting them.
	store := NewAttachmentTicketStore()
	firstID, err := store.Issue("tok-1", "/me/messages/m1/attachments/a1/$value", 300, "first.pdf", "application/pdf")
	if err != nil {
		t.Fatalf("first Issue failed: %v", err)
	}
	for i := 1; i < maxAttachmentTickets; i++ {
		if _, err := store.Issue("tok-flood", "/me/messages/m1/attachments/a1/$value", 300, "f.pdf", "application/pdf"); err != nil {
			t.Fatalf("Issue %d failed before cap: %v", i, err)
		}
	}
	if _, err := store.Issue("tok-over", "/me/messages/m1/attachments/a1/$value", 300, "over.pdf", "application/pdf"); err == nil {
		t.Fatal("expected Issue at capacity to return an error instead of evicting live tickets")
	}
	if redeemed, ok := store.Redeem(firstID); !ok || redeemed.Filename != "first.pdf" {
		t.Fatalf("first ticket was evicted at capacity: ok=%v redeemed=%+v", ok, redeemed)
	}
}
