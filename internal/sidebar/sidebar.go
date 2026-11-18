// Package sidebar validates and normalizes a saved left-rail ("sidebar")
// layout: the order, visibility, labels and icons of the rail's built-in
// destinations, the shortcuts a person has pinned to it (a chat, a project, a
// task, a page), and the visibility and labels of its sections.
//
// The server only stores layouts and holds them to this contract; it never
// renders one. The client computes the effective layout as the user's own
// layout, else the workspace default an admin saved, else its built-in
// default (which lives only in the client). Because a stored layout is read
// back by every browser the person (or, for the workspace default, everyone)
// opens, every field is held to a closed shape here: ids and refs to fixed
// alphabets, icons to an allowlist or a short emoji, free text to cleaned,
// bounded strings. Nothing in a layout is an authority: a shortcut to a chat
// the viewer cannot open is just a dead link, the client still asks the API
// for the chat itself.
package sidebar

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Version is the only layout version this build understands.
const Version = 1

// Limits of one layout.
const (
	MaxItems        = 60
	MaxLabelRunes   = 60
	MaxSectionLabel = 40
	MaxTitleRunes   = 120
	maxEmojiRunes   = 8
)

// Item kinds.
const (
	KindBuiltin = "builtin"
	KindChat    = "chat"
	KindProject = "project"
	KindTask    = "task"
	KindPage    = "page"
)

// ViewDashboard opens a chat shortcut in its dashboard view.
const ViewDashboard = "dashboard"

// BuiltinIDs are the rail's built-in destinations, in their default order.
// A layout always carries every one of them (Normalize appends the missing).
var BuiltinIDs = []string{"chat", "patrols", "tasks"}

// SectionIDs are the rail's sections, in their default order.
var SectionIDs = []string{"projects", "chats", "team"}

// PageRefs are the pages a "page" shortcut may open.
var PageRefs = []string{"analytics", "help", "settings"}

// Icons is the allowlist of named icons (the client's icon set). An item's
// icon is one of these, a short emoji, or empty.
var Icons = []string{
	"message", "bar-chart", "activity", "grid", "target", "sparkles", "zap",
	"briefcase", "database", "globe", "mail", "megaphone", "shield", "book",
	"folder", "layers", "dollar-sign", "shopping-cart", "brain", "calendar",
	"clock", "pin", "tag", "headphones", "file-text", "map-pin", "cloud", "bug",
	"persona", "tools", "plug", "puzzle",
}

var (
	itemIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
	refRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// ErrInvalid wraps every validation failure, so a caller can answer 400.
var ErrInvalid = errors.New("invalid sidebar layout")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Item is one rail entry: a built-in destination or a pinned shortcut.
type Item struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`
	Title  string `json:"title,omitempty"`
	Label  string `json:"label,omitempty"`
	Icon   string `json:"icon,omitempty"`
	View   string `json:"view,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
}

// Section is one of the rail's fixed sections.
type Section struct {
	ID        string `json:"id"`
	Label     string `json:"label,omitempty"`
	Hidden    bool   `json:"hidden,omitempty"`
	Collapsed bool   `json:"collapsed,omitempty"`
}

// Layout is a saved rail layout (version 1). Home is the id of the item the
// app opens on start, or "" for the client's default.
type Layout struct {
	Version  int       `json:"version"`
	Items    []Item    `json:"items"`
	Sections []Section `json:"sections"`
	Home     string    `json:"home"`
	// Compact hides the rail's one-line subtitles.
	Compact bool `json:"compact,omitempty"`
	// SeparateLists gives teammates' shared chats a Team chats section of
	// their own. Unset (the default, and every layout saved before this
	// field existed) lists them inside the Chats section, in one list with
	// the person's own. A view preference only: it changes where chats are
	// listed, never which chats the person may open.
	SeparateLists bool `json:"separate_lists,omitempty"`
}

// Parse decodes raw JSON and normalizes it. A malformed document or one that
// breaks the contract is an error wrapping ErrInvalid.
func Parse(raw []byte) (Layout, error) {
	var l Layout
	if err := json.Unmarshal(raw, &l); err != nil {
		return Layout{}, invalid("bad json: %v", err)
	}
	return Normalize(l)
}

// Normalize validates l and returns its normalized form: free text cleaned,
// every built-in item and section present (missing ones appended in their
// default order), and a home that names a visible, non-patrols item (else "").
// Normalize is idempotent: a normalized layout normalizes to itself.
func Normalize(l Layout) (Layout, error) {
	if l.Version != Version {
		return Layout{}, invalid("version must be %d", Version)
	}
	if len(l.Items) > MaxItems {
		return Layout{}, invalid("at most %d items", MaxItems)
	}
	out := Layout{Version: Version, Compact: l.Compact, SeparateLists: l.SeparateLists, Items: make([]Item, 0, len(l.Items)+len(BuiltinIDs))}
	seen := map[string]bool{}
	for i, in := range l.Items {
		it, err := normalizeItem(in)
		if err != nil {
			return Layout{}, fmt.Errorf("item %d: %w", i, err)
		}
		if seen[it.ID] {
			return Layout{}, invalid("item %d: duplicate id %q", i, it.ID)
		}
		seen[it.ID] = true
		out.Items = append(out.Items, it)
	}
	for _, id := range BuiltinIDs {
		if !seen[id] {
			out.Items = append(out.Items, Item{ID: id, Kind: KindBuiltin})
		}
	}
	// The built-ins always take three slots, so the cap is on the whole
	// normalized list: a saved layout re-normalizes to itself.
	if len(out.Items) > MaxItems {
		return Layout{}, invalid("at most %d items, counting the built-in ones", MaxItems)
	}

	out.Sections = make([]Section, 0, len(SectionIDs))
	seenSec := map[string]bool{}
	for i, in := range l.Sections {
		if !slices.Contains(SectionIDs, in.ID) {
			return Layout{}, invalid("section %d: unknown id %q", i, in.ID)
		}
		if seenSec[in.ID] {
			return Layout{}, invalid("section %d: duplicate id %q", i, in.ID)
		}
		seenSec[in.ID] = true
		label, err := cleanLabel(in.Label, MaxSectionLabel)
		if err != nil {
			return Layout{}, fmt.Errorf("section %q: %w", in.ID, err)
		}
		out.Sections = append(out.Sections, Section{ID: in.ID, Label: label, Hidden: in.Hidden, Collapsed: in.Collapsed})
	}
	for _, id := range SectionIDs {
		if !seenSec[id] {
			out.Sections = append(out.Sections, Section{ID: id})
		}
	}

	if l.Home != "" {
		for _, it := range out.Items {
			if it.ID == l.Home && !it.Hidden && (it.Kind != KindBuiltin || it.ID != "patrols") {
				out.Home = it.ID
				break
			}
		}
	}
	return out, nil
}

func normalizeItem(in Item) (Item, error) {
	if !itemIDRe.MatchString(in.ID) {
		return Item{}, invalid("id %q must match %s", in.ID, itemIDRe)
	}
	out := Item{ID: in.ID, Kind: in.Kind, Ref: in.Ref, Hidden: in.Hidden}
	switch in.Kind {
	case KindBuiltin:
		if !slices.Contains(BuiltinIDs, in.ID) {
			return Item{}, invalid("unknown builtin %q", in.ID)
		}
		if in.Ref != "" {
			return Item{}, invalid("builtin %q takes no ref", in.ID)
		}
	case KindChat, KindProject, KindTask:
		if !refRe.MatchString(in.Ref) {
			return Item{}, invalid("%s item %q needs a ref matching %s", in.Kind, in.ID, refRe)
		}
	case KindPage:
		if !slices.Contains(PageRefs, in.Ref) {
			return Item{}, invalid("page item %q: ref must be one of %s", in.ID, strings.Join(PageRefs, ", "))
		}
	default:
		return Item{}, invalid("item %q: unknown kind %q", in.ID, in.Kind)
	}
	if in.Kind != KindBuiltin && slices.Contains(BuiltinIDs, in.ID) {
		return Item{}, invalid("id %q is reserved for the builtin item", in.ID)
	}
	label, err := cleanLabel(in.Label, MaxLabelRunes)
	if err != nil {
		return Item{}, fmt.Errorf("item %q: %w", in.ID, err)
	}
	out.Label = label
	out.Title = truncateRunes(cleanText(in.Title), MaxTitleRunes)
	if !validIcon(in.Icon) {
		return Item{}, invalid("item %q: unknown icon %q", in.ID, in.Icon)
	}
	out.Icon = in.Icon
	switch in.View {
	case "":
	case ViewDashboard:
		if in.Kind != KindChat {
			return Item{}, invalid("item %q: view %q is only for chat items", in.ID, in.View)
		}
		out.View = in.View
	default:
		return Item{}, invalid("item %q: unknown view %q", in.ID, in.View)
	}
	return out, nil
}

// cleanText drops control characters (and any invalid UTF-8) and trims
// surrounding whitespace.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func cleanLabel(s string, maxRunes int) (string, error) {
	s = cleanText(s)
	if utf8.RuneCountInString(s) > maxRunes {
		return "", invalid("label is longer than %d characters", maxRunes)
	}
	return s, nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n]))
}

// validIcon accepts "", a name from Icons, or a short emoji: 1–8 runes, none
// of them ASCII, a control character, a separator (space), invalid UTF-8,
// or a format character other than the zero-width joiner and the tag
// characters emoji sequences are built from.
func validIcon(s string) bool {
	if s == "" || slices.Contains(Icons, s) {
		return true
	}
	if !utf8.ValidString(s) {
		return false
	}
	n := utf8.RuneCountInString(s)
	if n < 1 || n > maxEmojiRunes {
		return false
	}
	for _, r := range s {
		switch {
		case r < 0x80, r == utf8.RuneError:
			return false
		case unicode.IsControl(r), unicode.In(r, unicode.Z):
			return false
		case unicode.In(r, unicode.Cf):
			// U+200D ZWJ joins emoji; U+E0020–E007F tag a subdivision flag.
			if r != 0x200D && (r < 0xE0020 || r > 0xE007F) {
				return false
			}
		}
	}
	return true
}

// CheckWorkspace holds a workspace default to the rules an ordinary layout
// does not need: it is served to every member, so it may not pin a
// background task, nor the admin-only Analytics page (a dead end, and as Home
// an "admin access required" screen, for every member). Task visibility is owner-scoped for everyone but admins,
// so such a pin would be a dead link (and its title another person's
// metadata) for nearly every member.
func CheckWorkspace(l Layout) error {
	for _, it := range l.Items {
		if it.Kind == KindTask {
			return invalid("item %q: a workspace sidebar cannot pin a background task; only its owner and admins can open one", it.ID)
		}
		if it.Kind == KindPage && it.Ref == "analytics" {
			return invalid("item %q: a workspace sidebar cannot pin Analytics; it is an admin page", it.ID)
		}
	}
	return nil
}

// ForMembers is the workspace default as it is served to members (GET
// /sidebar): every item's captured Title is dropped. A title is a snapshot of
// the target's own name taken in the admin's browser, and a member who cannot
// open that chat or project must not learn it. The client shows a
// workspace shortcut only once it knows the member can open the target, and
// then names it from the member's own lists. An admin's Label is kept: it is
// text the admin wrote for everyone.
func (l Layout) ForMembers() Layout {
	out := l
	out.Items = make([]Item, len(l.Items))
	for i, it := range l.Items {
		it.Title = ""
		out.Items[i] = it
	}
	return out
}
