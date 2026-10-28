package tools

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func runPersonaStudio(ctx context.Context, t *testing.T, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := NewPersonaStudioTool().Run(ctx, fantasy.ToolCall{ID: "1", Name: PersonaStudioToolName, Input: input})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return resp
}

func TestPersonaStudioTool(t *testing.T) {
	if resp := runPersonaStudio(context.Background(), t, `{"action":"list"}`); !resp.IsError || !strings.Contains(resp.Content, "only available in interactive chat") {
		t.Errorf("no handler: %+v", resp)
	}
	var got PersonaStudioParams
	ctx := WithPersonaStudioHandler(context.Background(), func(_ context.Context, p PersonaStudioParams) (any, error) {
		got = p
		return map[string]string{"ok": "yes"}, nil
	})
	if resp := runPersonaStudio(ctx, t, `{"action":"bogus"}`); !resp.IsError {
		t.Errorf("bad action accepted: %+v", resp)
	}
	resp := runPersonaStudio(ctx, t, `{"action":"Draft","name":"twin","instructions":"x","patrols":[{"title":"t","cron":"0 9 * * *","prompt":"p"}]}`)
	if resp.IsError || !strings.Contains(resp.Content, `"ok": "yes"`) {
		t.Fatalf("resp = %+v", resp)
	}
	if got.Action != PersonaStudioActionPropose || got.Name != "twin" || len(got.Patrols) != 1 {
		t.Errorf("handler got %+v", got)
	}
}

func TestPersonaStudioIsInteractiveOnly(t *testing.T) {
	for _, tool := range ExcludeInteractiveOnly(DefaultTools()) {
		if tool.Info().Name == PersonaStudioToolName {
			t.Fatal("persona_studio offered to headless runs")
		}
	}
}
