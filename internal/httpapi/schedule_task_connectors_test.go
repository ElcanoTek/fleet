package httpapi

// ADR-0068: a task scheduled from chat inherits the conversation's connector
// selection. These tests pin the snapshot mechanics without a database; the
// end-to-end promote and seam paths are covered by the DB-gated tests in
// promote_task_test.go and cmd/fleet/task_scheduler_budget_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

func TestAttachTaskConnectors(t *testing.T) {
	raw := `{"name":"Daily scan","prompt":"scan all SSPs","cron":"0 9 * * *","connectors":[{"server":"model-invented"}]}`

	t.Run("snapshots the conversation's selection and overrides anything the model supplied", func(t *testing.T) {
		stager := &approvalStager{ctx: context.Background(), taskConnectors: func(context.Context) (chatTaskConnectors, error) {
			return chatTaskConnectors{
				Selection: []TaskConnector{{Server: "email"}, {Server: "magnite_mcp", Account: "reklaim"}},
				AlwaysOn:  []string{"mailbox"},
			}, nil
		}}
		out, err := stager.attachTaskConnectors(raw)
		if err != nil {
			t.Fatal(err)
		}
		var staged scheduleTaskStagedArgs
		if err := json.Unmarshal([]byte(out), &staged); err != nil {
			t.Fatal(err)
		}
		if staged.Name != "Daily scan" || staged.Prompt != "scan all SSPs" || staged.Cron != "0 9 * * *" {
			t.Fatalf("agent params lost: %+v", staged.ScheduleTaskParams)
		}
		if len(staged.Connectors) != 2 || staged.Connectors[0].Server != "email" || staged.Connectors[1].Account != "reklaim" {
			t.Fatalf("connectors = %+v", staged.Connectors)
		}
		if len(staged.AlwaysOnConnectors) != 1 || staged.AlwaysOnConnectors[0] != "mailbox" || !staged.ConnectorsResolved {
			t.Fatalf("always-on/resolved = %v/%v", staged.AlwaysOnConnectors, staged.ConnectorsResolved)
		}
		if strings.Contains(out, "model-invented") {
			t.Fatal("a model-supplied connector survived; the conversation's toggles are the authority")
		}
		// The agent-facing contract still parses the enriched payload.
		var params tools.ScheduleTaskParams
		if err := json.Unmarshal([]byte(out), &params); err != nil || params.Prompt != "scan all SSPs" {
			t.Fatalf("ScheduleTaskParams no longer parses the staged args: %v", err)
		}
	})

	t.Run("an empty selection is recorded explicitly", func(t *testing.T) {
		stager := &approvalStager{ctx: context.Background(), taskConnectors: func(context.Context) (chatTaskConnectors, error) {
			return chatTaskConnectors{}, nil
		}}
		out, err := stager.attachTaskConnectors(raw)
		if err != nil {
			t.Fatal(err)
		}
		var staged scheduleTaskStagedArgs
		_ = json.Unmarshal([]byte(out), &staged)
		if len(staged.Connectors) != 0 || !staged.ConnectorsResolved {
			t.Fatalf("staged = %+v", staged)
		}
		if summary := summarizeScheduleTaskInput("schedule_task", out); summary["no_connectors"] != true {
			t.Fatalf("card must warn on an empty snapshot: %+v", summary)
		}
	})

	t.Run("no resolver stages the payload untouched", func(t *testing.T) {
		stager := &approvalStager{ctx: context.Background()}
		out, err := stager.attachTaskConnectors(raw)
		if err != nil || out != raw {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})

	t.Run("a resolver failure fails the staging call rather than staging a connector-less card", func(t *testing.T) {
		stager := &approvalStager{ctx: context.Background(), taskConnectors: func(context.Context) (chatTaskConnectors, error) {
			return chatTaskConnectors{}, errors.New("store down")
		}}
		if _, err := stager.attachTaskConnectors(raw); err == nil || !strings.Contains(err.Error(), "store down") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("non-JSON input is left for Validate to report", func(t *testing.T) {
		stager := &approvalStager{ctx: context.Background(), taskConnectors: func(context.Context) (chatTaskConnectors, error) {
			t.Fatal("resolver must not run for unparseable args")
			return chatTaskConnectors{}, nil
		}}
		out, err := stager.attachTaskConnectors("not json")
		if err != nil || out != "not json" {
			t.Fatalf("out=%q err=%v", out, err)
		}
	})
}

func TestDescribeTaskConnectors(t *testing.T) {
	if got := describeTaskConnectors([]TaskConnector{{Server: "email"}, {Server: "gamma", Account: "work"}}, []string{"mailbox"}); got != "Connectors: email, gamma (work)" {
		t.Errorf("selected: %q", got)
	}
	if got := describeTaskConnectors(nil, []string{"mailbox", "pages"}); !strings.Contains(got, "always-on connectors (mailbox, pages)") {
		t.Errorf("always-on only: %q", got)
	}
	if got := describeTaskConnectors(nil, nil); !strings.Contains(got, "none") || !strings.Contains(got, "Operations Center") {
		t.Errorf("none: %q", got)
	}
	if got := connectorLabels([]TaskConnector{{Server: " "}, {Server: "x", Account: "a"}}); len(got) != 1 || got[0] != "x (a)" {
		t.Errorf("labels = %v", got)
	}
}

// alwaysOnFakeEngine adds the always-on catalog the Manager exposes so the
// resolver can name the servers every scheduled run binds regardless.
type alwaysOnFakeEngine struct {
	*fakeTurnEngine
	alwaysOn []agent.AlwaysOnServerInfo
}

func (e *alwaysOnFakeEngine) AlwaysOnMCPServerCatalog() []agent.AlwaysOnServerInfo { return e.alwaysOn }

func TestChatTaskConnectorsFor(t *testing.T) {
	ctx := context.Background()
	fs := newFakeChatStore()
	const user = "kartik@reklaim.example"
	conv, err := fs.CreateConversation(ctx, user, "daily scan", "victoria", "openrouter/auto", false)
	if err != nil {
		t.Fatal(err)
	}
	// The fake's setters are no-ops; the row is the pointer the fake hands back.
	conv.OptionalMCPServersEnabled = []string{"tavily", "email", "magnite_mcp"}
	conv.MCPAccounts = map[string]string{"magnite_mcp": "reklaim"}
	s := &Server{store: fs, agent: &alwaysOnFakeEngine{
		fakeTurnEngine: &fakeTurnEngine{},
		alwaysOn: []agent.AlwaysOnServerInfo{
			{Name: "mailbox", Available: true},
			{Name: "broken", Available: false}, // configured but discovery found no tools
		},
	}}

	got, err := s.chatTaskConnectorsFor(user, conv.ID)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []TaskConnector{{Server: "email"}, {Server: "magnite_mcp", Account: "reklaim"}, {Server: "tavily"}}
	if len(got.Selection) != len(want) {
		t.Fatalf("selection = %+v, want %+v", got.Selection, want)
	}
	for i := range want {
		if got.Selection[i] != want[i] {
			t.Fatalf("selection[%d] = %+v, want %+v", i, got.Selection[i], want[i])
		}
	}
	if len(got.AlwaysOn) != 1 || got.AlwaysOn[0] != "mailbox" {
		t.Fatalf("always-on = %v, want only the available server", got.AlwaysOn)
	}

	// A conversation with nothing enabled resolves to an explicit empty
	// selection — the card warns, the task is still created if approved.
	fs.convs["conv-bare"] = &store.Conversation{ID: "conv-bare", UserEmail: user}
	got, err = s.chatTaskConnectorsFor(user, "conv-bare")(ctx)
	if err != nil || len(got.Selection) != 0 {
		t.Fatalf("bare conversation: selection=%+v err=%v", got.Selection, err)
	}

	// An engine without an always-on catalog (transport fakes) yields none.
	s.agent = &fakeTurnEngine{}
	got, _ = s.chatTaskConnectorsFor(user, conv.ID)(ctx)
	if got.AlwaysOn != nil {
		t.Fatalf("always-on = %v, want nil without a catalog provider", got.AlwaysOn)
	}
}
