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

			allow, explicit, _ := s.uiSecrets.consumeEveryoneShare(user, conv.ID)
			if !allow || !explicit {
				t.Fatalf("after a failed attempt the confirmation is (allow=%v, explicit=%v), want it still there for the retry", allow, explicit)
			}
			// It is still single use: taking it above used it up.
			if allow, _, _ := s.uiSecrets.consumeEveryoneShare(user, conv.ID); allow {
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
	allow, _, gen := swept.consumeEveryoneShare(user, conv)
	if !allow {
		t.Fatal("setup: confirmation not taken")
	}
	swept.mu.Lock()
	delete(swept.buckets, uiSecretKey(user, conv)) // the sweep of an expired bucket
	swept.mu.Unlock()
	swept.restoreEveryoneShare(user, conv, gen)
	if allow, explicit, _ := swept.consumeEveryoneShare(user, conv); allow || explicit {
		t.Fatalf("swept bucket revived: allow=%v explicit=%v", allow, explicit)
	}

	var expired uiSecretStash
	expired.setAllowEveryone(user, conv, true)
	_, _, gen = expired.consumeEveryoneShare(user, conv)
	expired.mu.Lock()
	expired.buckets[uiSecretKey(user, conv)].updatedAt = time.Now().Add(-2 * uiSecretTTL)
	expired.mu.Unlock()
	expired.restoreEveryoneShare(user, conv, gen)
	expired.mu.Lock()
	b := expired.buckets[uiSecretKey(user, conv)]
	revived := b != nil && b.allowEveryoneShare
	expired.mu.Unlock()
	if revived {
		t.Fatal("expired bucket re-armed")
	}
}

// Giving a confirmation back never overwrites a decision made since it was
// taken: neither a newer form submission nor a newer confirmation another
// connector took.
func TestConnectorStudioEveryoneShareRestoreKeepsNewerDecision(t *testing.T) {
	t.Parallel()
	const user, conv = "admin@example.com", "conv-1"

	t.Run("user chose personal while the call ran", func(t *testing.T) {
		t.Parallel()
		var st uiSecretStash
		st.setAllowEveryone(user, conv, true)
		_, _, gen := st.consumeEveryoneShare(user, conv) // call A takes it
		st.setAllowEveryone(user, conv, false)           // the user submits personal
		st.restoreEveryoneShare(user, conv, gen)         // call A fails
		if allow, explicit, _ := st.consumeEveryoneShare(user, conv); allow || !explicit {
			t.Fatalf("after the restore: allow=%v explicit=%v, want the user's personal decision", allow, explicit)
		}
	})

	t.Run("another connector took a newer confirmation", func(t *testing.T) {
		t.Parallel()
		var st uiSecretStash
		st.setAllowEveryone(user, conv, true)
		_, _, genA := st.consumeEveryoneShare(user, conv) // call A takes the first
		st.setAllowEveryone(user, conv, true)             // the user confirms again
		_, _, genB := st.consumeEveryoneShare(user, conv) // call B takes the second
		st.restoreEveryoneShare(user, conv, genA)         // call A fails: B's use stands
		if allow, _, _ := st.consumeEveryoneShare(user, conv); allow {
			t.Fatal("call A's restore revived a confirmation call B already used")
		}
		st.restoreEveryoneShare(user, conv, genB) // call B fails: its own is given back
		if allow, _, _ := st.consumeEveryoneShare(user, conv); !allow {
			t.Fatal("call B's own confirmation was not given back")
		}
	})
}
