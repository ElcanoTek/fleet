package agent

import (
	"strings"
	"testing"
)

// The user layer lands right after the base persona section; without a layer
// the prompt is byte-identical to the plain bundle-persona prompt.
func TestBuildLayeredSystemPrompt_UserPersonaLayer(t *testing.T) {
	m := fixtureManager(t)

	none, err := m.buildSystemPrompt("victoria", nil, "c", nil, "", nil, nil, nil, hostedMCPRoster{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(none, "Personal persona") || strings.Contains(none, "Autonomy:") {
		t.Fatal("a nil layer wrote a persona layer")
	}

	layer := &UserPersonaLayer{Ref: "user:twin", DisplayName: "Brad's Twin", Kind: UserPersonaKindTwin, Instructions: "Lead with the margin number."}
	got, err := m.buildSystemPrompt("victoria", layer, "c", nil, "", nil, nil, nil, hostedMCPRoster{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.Index(got, "# Persona: victoria")
	mine := strings.Index(got, "# Personal persona: Brad's Twin")
	date := strings.Index(got, "Today")
	if base < 0 || mine < base {
		t.Fatalf("layer missing or before the base persona (base=%d layer=%d)", base, mine)
	}
	if date >= 0 && date < mine {
		t.Error("layer should precede the runtime date context")
	}
	for _, want := range []string{"digital twin", "never claim to be the user", "Autonomy: advise", "Lead with the margin number."} {
		if !strings.Contains(got, want) {
			t.Errorf("layered prompt missing %q", want)
		}
	}

	layer.Kind, layer.Autonomy = "assistant", UserPersonaAutonomyAct
	got, _ = m.buildSystemPrompt("victoria", layer, "c", nil, "", nil, nil, nil, hostedMCPRoster{}, nil)
	if !strings.Contains(got, "Autonomy: act on the user's behalf") || strings.Contains(got, "digital twin") {
		t.Error("assistant/act framing wrong")
	}
}

func TestIsUserPersonaRef(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{{"user:twin", true}, {"  user:padded", true}, {"assistant", false}, {"", false}, {"users", false}} {
		if IsUserPersonaRef(c.in) != c.want {
			t.Errorf("IsUserPersonaRef(%q) != %v", c.in, c.want)
		}
	}
}
