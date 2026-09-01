// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package a2a

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	wire "github.com/a2aproject/a2a-go/v2/a2a"
)

// CardSpec is the operator-derived input to the Agent Card: identity from the
// branding bundle, version from the binary, and the one URL this deployment
// answers JSON-RPC on. Callers construct it in cmd/fleet, where the bundle
// and config live.
type CardSpec struct {
	Name        string // branding app name; "Fleet" when the bundle sets none
	Description string
	OrgName     string // optional → AgentCard.provider
	OrgURL      string
	Version     string // the fleet binary version
	RPCURL      string // absolute or server-relative URL of the JSON-RPC endpoint
	// PushNotifications declares the per-task webhook capability (#1279
	// Phase 2) — set only when the deployment can store push configs.
	PushNotifications bool
	// Persona / Model are the operator-pinned task settings, surfaced ONLY on
	// the extended (authenticated) card — deployment policy detail the public
	// discovery document has no business broadcasting.
	Persona string
	Model   string
}

// BuildCard assembles the v1.0 Agent Card this server publishes at
// /.well-known/agent-card.json. Every capability it declares is implemented;
// everything deferred is declared off, which per spec §3.3.4 is what makes
// the corresponding methods answer -32003/-32004 rather than MethodNotFound:
//
//   - streaming: true — SendStreamingMessage + SubscribeToTask ship.
//   - pushNotifications: from CardSpec — true only when the deployment can
//     actually store push configs (the store cipher is configured), so the
//     card never advertises a capability the dispatcher would refuse.
//   - extendedAgentCard: true — GetExtendedAgentCard serves the
//     authenticated variant built by BuildExtendedCard.
//
// One skill: fleet is a general delegation target whose personas/models are
// operator policy, not caller-selectable, so advertising a per-persona skill
// list would promise a routing knob that does not exist.
func BuildCard(spec CardSpec) *wire.AgentCard {
	name := spec.Name
	if name == "" {
		name = "Fleet"
	}
	desc := spec.Description
	if desc == "" {
		desc = "A self-hosted agent platform. Delegated messages run as governed fleet tasks " +
			"(sandboxed execution, operator-pinned persona and model, cost/token ceilings) and " +
			"report results as A2A artifacts."
	}
	card := &wire.AgentCard{
		Name:        name,
		Description: desc,
		Version:     spec.Version,
		SupportedInterfaces: []*wire.AgentInterface{{
			URL:             spec.RPCURL,
			ProtocolBinding: wire.TransportProtocolJSONRPC,
			ProtocolVersion: wire.Version,
		}},
		Capabilities:       wire.AgentCapabilities{Streaming: true, PushNotifications: spec.PushNotifications, ExtendedAgentCard: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain", "application/json"},
		Skills: []wire.AgentSkill{{
			ID:          "delegate-task",
			Name:        "Delegate a task",
			Description: "Send a natural-language request; it runs as one governed fleet task and returns text, structured output, and file artifacts.",
			Tags:        []string{"delegation", "tasks", "general-purpose"},
		}},
		SecuritySchemes: wire.NamedSecuritySchemes{
			"apiKey": wire.APIKeySecurityScheme{
				Location:    wire.APIKeySecuritySchemeLocationHeader,
				Name:        "X-API-Key",
				Description: "A fleet typed API key (fleet_task_… to create and read your own tasks; fleet_readonly_… for reads).",
			},
		},
		SecurityRequirements: wire.SecurityRequirementsOptions{
			wire.SecurityRequirements{"apiKey": wire.SecuritySchemeScopes{}},
		},
	}
	if spec.OrgName != "" {
		card.Provider = &wire.AgentProvider{Org: spec.OrgName, URL: spec.OrgURL}
	}
	return card
}

// securityRequirement / securityRequirementScopes mirror the proto's wire
// shape for AgentCard.securityRequirements: SecurityRequirement carries a
// `schemes` map whose values are StringList OBJECTS ({"list": [...]}).
// a2a-go v2.5.0's SecuritySchemeScopes ([]string) marshals the scopes as a
// bare JSON array instead, which the published schema rejects — the official
// TCK's card-structure MUST test failed on exactly this
// ($.securityRequirements[0].schemes.apiKey: [] is not of type 'object').
// MarshalCard shadows the field with these types so the wire form is
// schema-valid regardless; drop them if the SDK fixes its marshalling.
type securityRequirementScopes struct {
	List []string `json:"list"`
}

type securityRequirement struct {
	Schemes map[string]securityRequirementScopes `json:"schemes"`
}

// schemaValidRequirements re-shapes the SDK's requirements into the proto
// StringList form. Empty scopes become an explicit empty list, never null.
func schemaValidRequirements(opts wire.SecurityRequirementsOptions) []securityRequirement {
	out := make([]securityRequirement, 0, len(opts))
	for _, req := range opts {
		schemes := make(map[string]securityRequirementScopes, len(req))
		for name, scopes := range req {
			list := make([]string, 0, len(scopes))
			list = append(list, scopes...)
			schemes[string(name)] = securityRequirementScopes{List: list}
		}
		out = append(out, securityRequirement{Schemes: schemes})
	}
	return out
}

// BuildExtendedCard assembles the authenticated card (#1279 Phase 2): the
// public card plus detail reserved for callers who presented a credential
// (spec §13.3 — the operation MUST require authentication, and the extended
// card MAY carry configuration the public card does not). Fleet's extra is
// deliberately modest: the operator-pinned persona/model policy and richer
// skill examples — deployment policy an integrator can plan around, nothing
// secret (§13.3 also forbids leakable material here). Same card otherwise,
// so §3.1.11's card-replacement semantics are safe.
func BuildExtendedCard(spec CardSpec) *wire.AgentCard {
	card := BuildCard(spec)
	detail := "Delegated messages run under operator-pinned policy"
	switch {
	case spec.Persona != "" && spec.Model != "":
		detail += ": persona " + strconv.Quote(spec.Persona) + ", model " + strconv.Quote(spec.Model) + "."
	case spec.Persona != "":
		detail += ": persona " + strconv.Quote(spec.Persona) + "."
	case spec.Model != "":
		detail += ": model " + strconv.Quote(spec.Model) + "."
	default:
		detail += " (the deployment's default persona and model)."
	}
	for i := range card.Skills {
		card.Skills[i].Description += " " + detail
		card.Skills[i].Examples = []string{
			"Summarize the attached quarterly numbers and produce a report.",
			"Investigate why the nightly export failed and propose a fix.",
		}
	}
	return card
}

// MarshalCard renders the card once, with the strong ETag its handler serves
// (spec §8.6 recommends ETag + Cache-Control on the well-known document).
// The securityRequirements field is shadowed into its schema-valid shape —
// see schemaValidRequirements. Deterministic bytes (encoding/json sorts map
// keys), so the ETag stays stable across restarts of the same card.
func MarshalCard(card *wire.AgentCard) (body []byte, etag string, err error) {
	wrapper := struct {
		*wire.AgentCard
		SecurityRequirements []securityRequirement `json:"securityRequirements,omitempty"`
	}{AgentCard: card, SecurityRequirements: schemaValidRequirements(card.SecurityRequirements)}
	body, err = json.Marshal(wrapper)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, fmt.Sprintf("%q", fmt.Sprintf("%x", sum[:8])), nil
}
