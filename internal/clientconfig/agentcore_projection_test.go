package clientconfig

import (
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

// fillNonZero sets every exported field of v (a pointer to a struct) to a
// non-zero value, recursing into nested structs, so a projection test can tell
// a field that was mapped from one that was dropped.
func fillNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillNonZero(t, v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fillNonZero(t, v.Field(i))
			}
		}
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fillNonZero(t, s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		fillNonZero(t, k)
		e := reflect.New(v.Type().Elem()).Elem()
		fillNonZero(t, e)
		m.SetMapIndex(k, e)
		v.Set(m)
	default:
		t.Fatalf("fillNonZero: unhandled kind %s (%s)", v.Kind(), v.Type())
	}
}

// TestAgentcorePolicyMapsEveryField fails when an agentcore.AgentPolicy field
// is left at its zero value by Bundle.AgentcorePolicy for a fully populated
// bundle policy — i.e. a new field was added to agentcore but not mapped here
// (or not copied by Bundle.AgentPolicy). Both fleet serve and fleet task run
// install this projection (#1934), so one missing line used to mean the local
// harness silently ran looser than the server.
func TestAgentcorePolicyMapsEveryField(t *testing.T) {
	b := &Bundle{}
	fillNonZero(t, reflect.ValueOf(&b.AgentPolicyConfig))
	// BatchSecondsPerDeal and ApprovedCallTimeoutSeconds are derived per MCP
	// server from the catalog, not read from agent_policy.
	b.MCPCatalog = []ServerDef{{Name: "srv", BatchSecondsPerDeal: 1, ApprovedCallTimeoutSeconds: 1}}
	got := reflect.ValueOf(b.AgentcorePolicy())
	for i := 0; i < got.NumField(); i++ {
		if got.Field(i).IsZero() {
			t.Errorf("agentcore.AgentPolicy.%s is not mapped from the bundle policy", got.Type().Field(i).Name)
		}
	}
}

func TestMergePinnedModelPricing(t *testing.T) {
	zero := 0.0
	pos := 5.0
	base := agentcore.PricingConfig{
		Overrides: []agentcore.PricingOverride{
			{Model: "already/overridden", InputCostPerMillionTokens: 2.5, OutputCostPerMillionTokens: 10.0},
		},
	}
	pins := []config.PinnedModel{
		{Slug: "qwen/qwen3.6-plus", Label: "Custom Label", Free: true},
		{Slug: "openrouter/z-ai/glm-5.1", InputPricePerMtok: &zero, OutputPricePerMtok: &zero},
		{Slug: "already/overridden", Free: true},
		{Slug: "billed/model", InputPricePerMtok: &pos, OutputPricePerMtok: &pos},
	}

	merged := MergePinnedModelPricing(base, pins)
	byModel := make(map[string]agentcore.PricingOverride, len(merged.Overrides))
	for _, o := range merged.Overrides {
		byModel[o.Model] = o
	}

	// Explicit manifest override wins over a free pin.
	if got := byModel["already/overridden"]; got.InputCostPerMillionTokens != 2.5 {
		t.Fatalf("already/overridden input rate = %v, want 2.5", got.InputCostPerMillionTokens)
	}
	// Free pin registers both bare and openrouter/-prefixed zero overrides.
	for _, slug := range []string{
		"qwen/qwen3.6-plus",
		"openrouter/qwen/qwen3.6-plus",
		"openrouter/z-ai/glm-5.1",
		"z-ai/glm-5.1",
	} {
		o, ok := byModel[slug]
		if !ok {
			t.Fatalf("expected zero-rate override for %q, got %+v", slug, merged.Overrides)
		}
		if o.InputCostPerMillionTokens != 0 || o.OutputCostPerMillionTokens != 0 {
			t.Fatalf("override for %q has non-zero rate: %+v", slug, o)
		}
	}
	if _, ok := byModel["billed/model"]; ok {
		t.Fatal("positive-priced pin should not be added as a zero-rate override")
	}
}
