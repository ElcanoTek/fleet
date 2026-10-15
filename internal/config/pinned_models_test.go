package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePinnedModels(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []PinnedModel
		wantErr string
	}{
		{name: "unset", raw: "", want: nil},
		{name: "blank", raw: "   ", want: nil},
		{
			name: "full entry trimmed",
			raw:  `[{"slug":" qwen/qwen3-235b ","label":" Elcano Nova ","tag":" alpha "}]`,
			want: []PinnedModel{{Slug: "qwen/qwen3-235b", Label: "Elcano Nova", Tag: "alpha"}},
		},
		{
			name: "slug only and env-file quotes",
			raw:  `'[{"slug":"a/b"},{"slug":"c/d","tag":"beta"}]'`,
			want: []PinnedModel{{Slug: "a/b"}, {Slug: "c/d", Tag: "beta"}},
		},
		{
			name: "price overrides, zero is kept",
			raw:  `[{"slug":"a/b","input_price_per_mtok":1.5,"output_price_per_mtok":0}]`,
			want: []PinnedModel{{Slug: "a/b", InputPricePerMtok: ptrFloat(1.5), OutputPricePerMtok: ptrFloat(0)}},
		},
		{name: "free", raw: `[{"slug":"a/b","free":true}]`, want: []PinnedModel{{Slug: "a/b", Free: true}}},
		{name: "free with price", raw: `[{"slug":"a/b","free":true,"input_price_per_mtok":1}]`, wantErr: "mutually exclusive"},
		{name: "negative price", raw: `[{"slug":"a/b","input_price_per_mtok":-1}]`, wantErr: "input_price_per_mtok must be between"},
		{name: "absurd price", raw: `[{"slug":"a/b","output_price_per_mtok":20000}]`, wantErr: "output_price_per_mtok must be between"},
		{name: "not json", raw: "a/b=Name", wantErr: "JSON array"},
		{name: "object not array", raw: `{"slug":"a/b"}`, wantErr: "JSON array"},
		{name: "unknown field", raw: `[{"slug":"a/b","lable":"typo"}]`, wantErr: "unknown field"},
		{name: "missing slug", raw: `[{"label":"x"}]`, wantErr: "slug is required"},
		{name: "whitespace slug", raw: `[{"slug":"a b"}]`, wantErr: "whitespace"},
		{name: "duplicate", raw: `[{"slug":"a/b"},{"slug":"a/b"}]`, wantErr: "pinned twice"},
		{name: "long tag", raw: `[{"slug":"a/b","tag":"` + strings.Repeat("x", 17) + `"}]`, wantErr: "tag is 17"},
		{name: "long label", raw: `[{"slug":"a/b","label":"` + strings.Repeat("x", 81) + `"}]`, wantErr: "label is 81"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePinnedModels(tc.raw)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A malformed FLEET_PINNED_MODELS refuses boot rather than silently dropping
// the pins.
func TestLoad_RejectsMalformedPinnedModels(t *testing.T) {
	t.Setenv("FLEET_PINNED_MODELS", "not-json")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "FLEET_PINNED_MODELS") {
		t.Fatalf("Load err = %v, want a FLEET_PINNED_MODELS error", err)
	}
}

func TestLoad_ParsesPinnedModels(t *testing.T) {
	t.Setenv("FLEET_PINNED_MODELS", `[{"slug":"qwen/qwen3-235b","label":"Elcano Nova","tag":"alpha"}]`)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []PinnedModel{{Slug: "qwen/qwen3-235b", Label: "Elcano Nova", Tag: "alpha"}}
	if !reflect.DeepEqual(cfg.PinnedModels, want) {
		t.Fatalf("PinnedModels = %#v, want %#v", cfg.PinnedModels, want)
	}
}

func ptrFloat(v float64) *float64 { return &v }
