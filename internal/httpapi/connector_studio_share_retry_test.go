package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// A fleet-wide share confirmation is used once, by the connector it was given
// for. A connect or create that fails before the connector is shared has not
// used it, so the agent's corrected retry goes through without asking the
// user to confirm again.
func TestConnectorStudioEveryoneShareSurvivesFailedAttempt(t *testing.T) {
	t.Parallel()
	const user = "admin@example.com"

	cases := []struct {
		name   string
		params tools.ConnectorStudioParams
	}{
		{
			name: "create_custom_connector rejected before create",
			params: tools.ConnectorStudioParams{
				Action:     tools.ConnectorStudioActionCreateCustom,
				Name:       "acme_ads",
				URL:        "https://ads.example.com",
				Transport:  "not-a-transport",
				ShareScope: "everyone",
				CustomTools: []tools.ConnectorStudioToolSpec{
					{Name: "list_campaigns", Method: "GET", URL: "https://ads.example.com/v1/campaigns"},
				},
			},
		},
		{
			name: "connect_mcp secret not found",
			params: tools.ConnectorStudioParams{
				Action:     tools.ConnectorStudioActionConnectMCP,
				Name:       "acme_mcp",
				URL:        "https://mcp.example.com/mcp",
				ShareScope: "everyone",
				SecretRef:  "$secret:never_submitted",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chatSt := newFakeChatStore()
			conv, _ := chatSt.CreateConversation(context.Background(), user, "Build Connector", "default", "", false)
			s := &Server{
				store: chatSt,
				remoteMCP: remotemcp.NewService(newMemRemoteMCPStore(), remotemcp.Config{
					PublicBaseURL:     "https://fleet.example.com",
					AllowInsecureHTTP: true,
				}),
			}
			s.uiSecrets.setAllowEveryone(user, conv.ID, true) // the user chose "everyone" in the form

			h := s.newConnectorStudioHandler(conv, user)
			_, err := h(context.Background(), tc.params)
			if err == nil {
				t.Fatal("first attempt succeeded; the case needs it to fail after the share check")
			}
			if strings.Contains(err.Error(), "requires explicit user confirmation") {
				t.Fatalf("first attempt refused at the share check: %v", err)
			}

			allow, explicit := s.uiSecrets.consumeEveryoneShare(user, conv.ID)
			if !allow || !explicit {
				t.Fatalf("after a failed attempt the confirmation is (allow=%v, explicit=%v), want it still there for the retry", allow, explicit)
			}
			// It is still single use: taking it above used it up.
			if allow, _ := s.uiSecrets.consumeEveryoneShare(user, conv.ID); allow {
				t.Fatal("confirmation reusable after it was taken")
			}
		})
	}
}

// A confirmation whose bucket expired, or was swept while the call ran, is not
// revived by a failed attempt.
func TestConnectorStudioEveryoneShareNotRevivedAfterExpiry(t *testing.T) {
	t.Parallel()
	const user, conv = "admin@example.com", "conv-1"

	var swept uiSecretStash
	swept.setAllowEveryone(user, conv, true)
	if allow, _ := swept.consumeEveryoneShare(user, conv); !allow {
		t.Fatal("setup: confirmation not taken")
	}
	swept.mu.Lock()
	delete(swept.buckets, uiSecretKey(user, conv)) // the sweep of an expired bucket
	swept.mu.Unlock()
	swept.restoreEveryoneShare(user, conv)
	if allow, explicit := swept.consumeEveryoneShare(user, conv); allow || explicit {
		t.Fatalf("swept bucket revived: allow=%v explicit=%v", allow, explicit)
	}

	var expired uiSecretStash
	expired.setAllowEveryone(user, conv, true)
	_, _ = expired.consumeEveryoneShare(user, conv)
	expired.mu.Lock()
	expired.buckets[uiSecretKey(user, conv)].updatedAt = time.Now().Add(-2 * uiSecretTTL)
	expired.mu.Unlock()
	expired.restoreEveryoneShare(user, conv)
	expired.mu.Lock()
	b := expired.buckets[uiSecretKey(user, conv)]
	revived := b != nil && b.allowEveryoneShare
	expired.mu.Unlock()
	if revived {
		t.Fatal("expired bucket re-armed")
	}
}
