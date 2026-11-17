package sidebar

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func builtins() []Item {
	return []Item{{ID: "chat", Kind: KindBuiltin}, {ID: "patrols", Kind: KindBuiltin}, {ID: "tasks", Kind: KindBuiltin}}
}

func defaultSections() []Section {
	return []Section{{ID: "projects"}, {ID: "chats"}, {ID: "team"}}
}

// TestParseContractExample round-trips the documented example layout.
func TestParseContractExample(t *testing.T) {
	t.Parallel()
	raw := `{
	  "version": 1,
	  "items": [
	    {"id":"chat","kind":"builtin"},
	    {"id":"patrols","kind":"builtin","hidden":true},
	    {"id":"tasks","kind":"builtin","label":"Automations"},
	    {"id":"s-k3j9x","kind":"chat","ref":"c-123","title":"Sales board","label":"","icon":"bar-chart","view":"dashboard"},
	    {"id":"s-a1","kind":"project","ref":"p_1","title":"Roadmap"},
	    {"id":"s-a2","kind":"task","ref":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","title":"Nightly report"},
	    {"id":"s-a3","kind":"page","ref":"analytics"}
	  ],
	  "sections": [
	    {"id":"chats","label":"Conversations"},
	    {"id":"projects","hidden":true},
	    {"id":"team","collapsed":true}
	  ],
	  "home": "s-k3j9x",
	  "compact": true
	}`
	l, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(l)
	want := `{"version":1,"items":[{"id":"chat","kind":"builtin"},{"id":"patrols","kind":"builtin","hidden":true},{"id":"tasks","kind":"builtin","label":"Automations"},{"id":"s-k3j9x","kind":"chat","ref":"c-123","title":"Sales board","icon":"bar-chart","view":"dashboard"},{"id":"s-a1","kind":"project","ref":"p_1","title":"Roadmap"},{"id":"s-a2","kind":"task","ref":"1b4e28ba-2fa1-11d2-883f-0016d3cca427","title":"Nightly report"},{"id":"s-a3","kind":"page","ref":"analytics"}],"sections":[{"id":"chats","label":"Conversations"},{"id":"projects","hidden":true},{"id":"team","collapsed":true}],"home":"s-k3j9x","compact":true}`
	if string(got) != want {
		t.Errorf("normalized =\n %s\nwant\n %s", got, want)
	}
	// Idempotent: the normalized output normalizes to itself.
	again, err := Parse(got)
	if err != nil || !reflect.DeepEqual(again, l) {
		t.Errorf("re-normalize = %+v, %v; want %+v", again, err, l)
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	t.Parallel()
	l, err := Normalize(Layout{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := Layout{Version: 1, Items: builtins(), Sections: defaultSections()}
	if !reflect.DeepEqual(l, want) {
		t.Errorf("got %+v, want %+v", l, want)
	}

	// Missing builtins go at the end in default order; given ones keep place.
	l, err = Normalize(Layout{Version: 1,
		Items:    []Item{{ID: "x1", Kind: KindPage, Ref: "help"}, {ID: "tasks", Kind: KindBuiltin, Hidden: true}},
		Sections: []Section{{ID: "team"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantItems := []Item{{ID: "x1", Kind: KindPage, Ref: "help"}, {ID: "tasks", Kind: KindBuiltin, Hidden: true}, {ID: "chat", Kind: KindBuiltin}, {ID: "patrols", Kind: KindBuiltin}}
	if !reflect.DeepEqual(l.Items, wantItems) {
		t.Errorf("items = %+v, want %+v", l.Items, wantItems)
	}
	wantSecs := []Section{{ID: "team"}, {ID: "projects"}, {ID: "chats"}}
	if !reflect.DeepEqual(l.Sections, wantSecs) {
		t.Errorf("sections = %+v, want %+v", l.Sections, wantSecs)
	}
}

func TestNormalizeCleansText(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 130)
	l, err := Normalize(Layout{Version: 1,
		Items: []Item{
			{ID: "a", Kind: KindChat, Ref: "c1", Label: "  Sales\x00 \tboard\n ", Title: long},
			{ID: "b", Kind: KindChat, Ref: "c2", Label: strings.Repeat("ü", 60)},
		},
		Sections: []Section{{ID: "chats", Label: " \x07Talk "}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Items[0].Label; got != "Sales board" {
		t.Errorf("label = %q", got)
	}
	if got := l.Items[0].Title; got != strings.Repeat("é", 120) {
		t.Errorf("title not truncated to 120 runes: %d", len([]rune(got)))
	}
	if got := l.Sections[0].Label; got != "Talk" {
		t.Errorf("section label = %q", got)
	}
}

func TestNormalizeHome(t *testing.T) {
	t.Parallel()
	items := []Item{
		{ID: "chat", Kind: KindBuiltin},
		{ID: "patrols", Kind: KindBuiltin},
		{ID: "tasks", Kind: KindBuiltin, Hidden: true},
		{ID: "s1", Kind: KindChat, Ref: "c1"},
		{ID: "s2", Kind: KindChat, Ref: "c2", Hidden: true},
	}
	for home, want := range map[string]string{
		"":        "",
		"chat":    "chat",
		"s1":      "s1",
		"patrols": "", // the patrols builtin is never home
		"tasks":   "", // hidden
		"s2":      "", // hidden
		"gone":    "", // no such item
		"BAD ID!": "", // not even a valid id: still just dropped
	} {
		l, err := Normalize(Layout{Version: 1, Items: items, Home: home})
		if err != nil {
			t.Fatalf("home %q: %v", home, err)
		}
		if l.Home != want {
			t.Errorf("home %q → %q, want %q", home, l.Home, want)
		}
	}
}

func TestValidIcon(t *testing.T) {
	t.Parallel()
	for icon, ok := range map[string]bool{
		"":                   true,
		"bar-chart":          true,
		"puzzle":             true,
		"🚀":                  true,
		"👩‍💻":                true, // ZWJ sequence
		"❤️":                 true, // with variation selector
		"🇪🇸":                 true, // regional indicators
		"🏴󠁧󠁢󠁳󠁣󠁴󠁿":            true, // tag sequence
		"🚀🚀🚀🚀🚀🚀🚀🚀":           true, // 8 runes
		"🚀🚀🚀🚀🚀🚀🚀🚀🚀":          false,
		"rocket":             false,
		"Bar-Chart":          false,
		"a🚀":                 false,
		"\u00a0":             false, // no-break space
		"\u202e":             false, // bidi override
		"\u0085":             false, // C1 control
		"🚀\u200b":            false, // zero-width space
		string([]byte{0xff}): false,
		"<script>":           false,
	} {
		if got := validIcon(icon); got != ok {
			t.Errorf("validIcon(%q) = %v, want %v", icon, got, ok)
		}
	}
	// A trailing space is not part of an emoji (kept out of the table above,
	// where a key with whitespace reads as a typo).
	if validIcon("🚀" + " ") {
		t.Error("validIcon accepted an emoji with a trailing space")
	}
}

func TestNormalizeRejects(t *testing.T) {
	t.Parallel()
	chat := func(mod func(*Item)) Layout {
		it := Item{ID: "s1", Kind: KindChat, Ref: "c1"}
		mod(&it)
		return Layout{Version: 1, Items: []Item{it}}
	}
	many := func(n int, withBuiltins bool) Layout {
		l := Layout{Version: 1}
		if withBuiltins {
			l.Items = builtins()
		}
		for i := len(l.Items); i < n; i++ {
			l.Items = append(l.Items, Item{ID: fmt.Sprintf("s%d", i), Kind: KindPage, Ref: "help"})
		}
		return l
	}
	cases := []struct {
		name string
		in   Layout
		ok   bool
	}{
		{"version 0", Layout{}, false},
		{"version 2", Layout{Version: 2}, false},
		{"60 items with builtins", many(60, true), true},
		{"61 items", many(61, true), false},
		{"60 items needing builtins appended", many(60, false), false},
		{"57 items + 3 builtins appended", many(57, false), true},
		{"empty id", chat(func(i *Item) { i.ID = "" }), false},
		{"uppercase id", chat(func(i *Item) { i.ID = "S1" }), false},
		{"id starting with dash", chat(func(i *Item) { i.ID = "-a" }), false},
		{"id 40 chars", chat(func(i *Item) { i.ID = strings.Repeat("a", 40) }), true},
		{"id 41 chars", chat(func(i *Item) { i.ID = strings.Repeat("a", 41) }), false},
		{"duplicate id", Layout{Version: 1, Items: []Item{{ID: "a", Kind: KindPage, Ref: "help"}, {ID: "a", Kind: KindPage, Ref: "settings"}}}, false},
		{"duplicate builtin", Layout{Version: 1, Items: []Item{{ID: "chat", Kind: KindBuiltin}, {ID: "chat", Kind: KindBuiltin}}}, false},
		{"unknown builtin", Layout{Version: 1, Items: []Item{{ID: "home", Kind: KindBuiltin}}}, false},
		{"builtin with ref", Layout{Version: 1, Items: []Item{{ID: "chat", Kind: KindBuiltin, Ref: "x"}}}, false},
		{"builtin dashboard view", Layout{Version: 1, Items: []Item{{ID: "chat", Kind: KindBuiltin, View: ViewDashboard}}}, false},
		{"chat using builtin id", chat(func(i *Item) { i.ID = "tasks" }), false},
		{"unknown kind", chat(func(i *Item) { i.Kind = "link" }), false},
		{"empty kind", chat(func(i *Item) { i.Kind = "" }), false},
		{"chat without ref", chat(func(i *Item) { i.Ref = "" }), false},
		{"project ref with slash", chat(func(i *Item) { i.Kind = KindProject; i.Ref = "a/b" }), false},
		{"task ref 128", chat(func(i *Item) { i.Kind = KindTask; i.Ref = strings.Repeat("a", 128) }), true},
		{"task ref 129", chat(func(i *Item) { i.Kind = KindTask; i.Ref = strings.Repeat("a", 129) }), false},
		{"ref with colon and dot", chat(func(i *Item) { i.Ref = "a:b.c_d-e" }), true},
		{"ref leading dot", chat(func(i *Item) { i.Ref = ".a" }), false},
		{"page settings", chat(func(i *Item) { i.Kind = KindPage; i.Ref = "settings" }), true},
		{"page unknown", chat(func(i *Item) { i.Kind = KindPage; i.Ref = "orchestrator" }), false},
		{"page without ref", chat(func(i *Item) { i.Kind = KindPage; i.Ref = "" }), false},
		{"label 60 runes", chat(func(i *Item) { i.Label = strings.Repeat("x", 60) }), true},
		{"label 61 runes", chat(func(i *Item) { i.Label = strings.Repeat("x", 61) }), false},
		{"label long only by controls", chat(func(i *Item) { i.Label = strings.Repeat("x", 60) + "\x00\x01" }), true},
		{"bad icon", chat(func(i *Item) { i.Icon = "rocket" }), false},
		{"dashboard on chat", chat(func(i *Item) { i.View = ViewDashboard }), true},
		{"dashboard on project", chat(func(i *Item) { i.Kind = KindProject; i.View = ViewDashboard }), false},
		{"unknown view", chat(func(i *Item) { i.View = "board" }), false},
		{"unknown section", Layout{Version: 1, Sections: []Section{{ID: "pinned"}}}, false},
		{"duplicate section", Layout{Version: 1, Sections: []Section{{ID: "team"}, {ID: "team"}}}, false},
		{"section label 40", Layout{Version: 1, Sections: []Section{{ID: "team", Label: strings.Repeat("x", 40)}}}, true},
		{"section label 41", Layout{Version: 1, Sections: []Section{{ID: "team", Label: strings.Repeat("x", 41)}}}, false},
	}
	for _, tc := range cases {
		_, err := Normalize(tc.in)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: accepted, want an error", tc.name)
			} else if !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: error %v does not wrap ErrInvalid", tc.name, err)
			}
		}
	}
}

func TestParseBadJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{``, `{`, `[]`, `{"version":"1"}`, `{"version":1,"items":{}}`} {
		if _, err := Parse([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %v, want ErrInvalid", raw, err)
		}
	}
}

func TestWorkspaceRulesAndRedaction(t *testing.T) {
	l, err := Normalize(Layout{Version: 1, Items: []Item{
		{ID: "s-1", Kind: KindChat, Ref: "c1", Title: "Secret", Label: "Ours"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckWorkspace(l); err != nil {
		t.Errorf("CheckWorkspace(chat) = %v", err)
	}
	m := l.ForMembers()
	if m.Items[0].Title != "" || m.Items[0].Label != "Ours" || m.Items[0].Ref != "c1" {
		t.Errorf("ForMembers = %+v", m.Items[0])
	}
	if l.Items[0].Title != "Secret" {
		t.Errorf("ForMembers changed the original: %+v", l.Items[0])
	}
	task, err := Normalize(Layout{Version: 1, Items: []Item{{ID: "s-2", Kind: KindTask, Ref: "t1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckWorkspace(task); !errors.Is(err, ErrInvalid) {
		t.Errorf("CheckWorkspace(task) = %v; want ErrInvalid", err)
	}
}
