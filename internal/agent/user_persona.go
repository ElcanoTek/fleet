package agent

import (
	"fmt"
	"strings"
)

// User-authored personas (docs/USER-PERSONAS.md). A conversation whose persona
// is "user:<name>" runs on a persona its owner wrote in Settings → Personas.
// The HTTP layer resolves that reference against the owner's rows before the
// turn: TurnInput.Persona becomes the bundle persona the user persona builds on
// (its YAML and its manifest tool policy still apply — a user persona can add
// instructions, never widen the tools the operator granted), and
// TurnInput.UserPersona carries the user's layer for the prompt.

// UserPersonaRefPrefix marks a conversation persona that names a user persona.
// store.UserPersonaRefPrefix is defined from this one.
const UserPersonaRefPrefix = "user:"

// Kind and autonomy values a layer may carry (mirrored by the store).
const (
	UserPersonaKindTwin     = "twin"
	UserPersonaAutonomyAct  = "act"
	userPersonaAutonomyNote = "Autonomy: advise. Analyze, monitor and recommend. Do not take actions with side effects on the user's behalf (sending messages or email, changing records in external systems, spending money, publishing) unless the user asks for that specific action in this conversation; present proposed actions as a short, numbered list the user can approve."
	userPersonaActNote      = "Autonomy: act on the user's behalf. Within the instructions below, carry out routine actions yourself instead of only recommending them, and report plainly what you did and why. Critical tools still go through this conversation's approval setting; when something is irreversible, unusual or outside these instructions, stop and ask."
)

// UserPersonaLayer is the user-authored part of a turn's persona.
type UserPersonaLayer struct {
	// Ref is the conversation's persona reference ("user:<name>").
	Ref          string
	DisplayName  string
	Kind         string
	Autonomy     string
	Instructions string
}

// IsUserPersonaRef reports whether persona references a user persona.
func IsUserPersonaRef(persona string) bool {
	return strings.HasPrefix(strings.TrimSpace(persona), UserPersonaRefPrefix)
}

// writeUserPersonaLayer appends the layer after the base persona section. A
// nil layer writes nothing, so a bundle-persona prompt is byte-identical to
// what it was before user personas existed.
func writeUserPersonaLayer(sb *strings.Builder, layer *UserPersonaLayer) {
	if layer == nil || strings.TrimSpace(layer.Instructions) == "" {
		return
	}
	name := strings.TrimSpace(layer.DisplayName)
	if name == "" {
		name = strings.TrimPrefix(layer.Ref, UserPersonaRefPrefix)
	}
	fmt.Fprintf(sb, "---\n\n# Personal persona: %s\n\n", name)
	if layer.Kind == UserPersonaKindTwin {
		sb.WriteString("The user set this persona up as their digital twin: think, prioritize and write the way they would, using the priorities, vocabulary and judgment described below. You are still an AI assistant working through this conversation's tools and approvals — never claim to be the user or a human, and say plainly when you did or suggest something on their behalf.\n\n")
	} else {
		sb.WriteString("The user wrote this persona for themselves. It builds on the persona above; where the two conflict, follow these instructions.\n\n")
	}
	if layer.Autonomy == UserPersonaAutonomyAct {
		sb.WriteString(userPersonaActNote)
	} else {
		sb.WriteString(userPersonaAutonomyNote)
	}
	sb.WriteString("\n\n")
	sb.WriteString(strings.TrimSpace(layer.Instructions))
	sb.WriteString("\n\n")
}
