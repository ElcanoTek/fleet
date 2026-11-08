package clientconfig

import (
	"fmt"
	"log"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// ADR-0106: a ${…} reference to a removed CHAT_/CUTLASS_ spelling never fails
// the load. It interpolates as "" (or its :- default), and when that empties
// a field validation REQUIRES — `command: "${CHAT_TLS_MODE}"` on a stdio
// server, an http server's url, a provider's model — only the item that owns
// the field is disabled, as if it were not in the manifest, with a warning
// that names the item, the field and the FLEET_ key. A LIVE name that is
// unset still fails exactly as before: an item is only ever disabled when one
// of its fields was emptied by a removed reference AND validation rejects
// that item.

// retiredItemsRaw holds the manifest's item lists as written (before
// interpolation), aligned index for index with the Bundle's lists.
type retiredItemsRaw struct {
	MCPServers      []ServerDef             `yaml:"mcp_servers"`
	HTTPTools       []HTTPToolDef           `yaml:"http_tools"`
	A2APeers        []A2APeerDef            `yaml:"a2a_peers"`
	Providers       []ProviderDef           `yaml:"providers"`
	WebhookTriggers []WebhookTriggerDef     `yaml:"webhook_triggers"`
	RemoteMCPs      []RemoteMCPCatalogEntry `yaml:"remote_mcp_catalog"`
}

// retiredItemList is one manifest section, seen as removable items.
type retiredItemList struct {
	section string
	n       func() int
	name    func(i int) string
	raw     func(i int) any
	cooked  func(i int) any
	remove  func(i int)
}

func (b *Bundle) retiredItemLists(raw *retiredItemsRaw) []retiredItemList {
	return []retiredItemList{
		{"mcp_servers", func() int { return min(len(b.MCPCatalog), len(raw.MCPServers)) },
			func(i int) string { return b.MCPCatalog[i].Name },
			func(i int) any { return raw.MCPServers[i] }, func(i int) any { return b.MCPCatalog[i] },
			func(i int) {
				b.MCPCatalog = removeAt(b.MCPCatalog, i)
				raw.MCPServers = removeAt(raw.MCPServers, i)
			}},
		{"http_tools", func() int { return min(len(b.HTTPTools), len(raw.HTTPTools)) },
			func(i int) string { return b.HTTPTools[i].Name },
			func(i int) any { return raw.HTTPTools[i] }, func(i int) any { return b.HTTPTools[i] },
			func(i int) {
				b.HTTPTools = removeAt(b.HTTPTools, i)
				raw.HTTPTools = removeAt(raw.HTTPTools, i)
			}},
		{"a2a_peers", func() int { return min(len(b.A2APeers), len(raw.A2APeers)) },
			func(i int) string { return b.A2APeers[i].Name },
			func(i int) any { return raw.A2APeers[i] }, func(i int) any { return b.A2APeers[i] },
			func(i int) {
				b.A2APeers = removeAt(b.A2APeers, i)
				raw.A2APeers = removeAt(raw.A2APeers, i)
			}},
		{"providers", func() int { return min(len(b.Providers), len(raw.Providers)) },
			func(i int) string { return b.Providers[i].Name },
			func(i int) any { return raw.Providers[i] }, func(i int) any { return b.Providers[i] },
			func(i int) {
				// A disabled provider leaves the fallback chain too, or the
				// chain would fail validation on the now-unknown name.
				gone := strings.TrimSpace(b.Providers[i].Name)
				b.FallbackProviders = slices.DeleteFunc(slices.Clone(b.FallbackProviders), func(n string) bool { return strings.TrimSpace(n) == gone })
				b.Providers = removeAt(b.Providers, i)
				raw.Providers = removeAt(raw.Providers, i)
			}},
		{"webhook_triggers", func() int { return min(len(b.WebhookTriggers), len(raw.WebhookTriggers)) },
			func(i int) string { return b.WebhookTriggers[i].Slug },
			func(i int) any { return raw.WebhookTriggers[i] }, func(i int) any { return b.WebhookTriggers[i] },
			func(i int) {
				b.WebhookTriggers = removeAt(b.WebhookTriggers, i)
				raw.WebhookTriggers = removeAt(raw.WebhookTriggers, i)
			}},
		{"remote_mcp_catalog", func() int { return min(len(b.RemoteMCPCatalog), len(raw.RemoteMCPs)) },
			func(i int) string { return b.RemoteMCPCatalog[i].Name },
			func(i int) any { return raw.RemoteMCPs[i] }, func(i int) any { return b.RemoteMCPCatalog[i] },
			func(i int) {
				b.RemoteMCPCatalog = removeAt(b.RemoteMCPCatalog, i)
				raw.RemoteMCPs = removeAt(raw.RemoteMCPs, i)
			}},
	}
}

func removeAt[T any](s []T, i int) []T {
	return append(s[:i:i], s[i+1:]...)
}

// validateDisablingRetired runs validate; while it fails on an item one of
// whose fields a removed reference emptied, that item is disabled (removed,
// with a warning recorded in b.retiredDisabled) and validation runs again.
// Any other failure is returned unchanged.
func (b *Bundle) validateDisablingRetired(manifestPath string, manifest []byte) error {
	raw := &retiredItemsRaw{}
	if err := yaml.Unmarshal(manifest, raw); err != nil {
		return fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	for {
		err := b.validate()
		if err == nil {
			return nil
		}
		msg := err.Error()
		removed := false
		for _, list := range b.retiredItemLists(raw) {
			for i := 0; i < list.n() && !removed; i++ {
				fields := retiredEmptiedFields(list.raw(i), list.cooked(i))
				if len(fields) == 0 {
					continue
				}
				name := strings.TrimSpace(list.name(i))
				if !strings.Contains(msg, list.section+"["+strconv.Quote(name)+"]") &&
					!strings.Contains(msg, fmt.Sprintf("%s[%d]", list.section, i)) {
					continue
				}
				label := strconv.Quote(name)
				if name == "" {
					label = "#" + strconv.Itoa(i)
				}
				var keys []string
				for _, f := range fields {
					keys = append(keys, f.path+" (${"+f.retired+"}: use ${"+f.canonical+"})")
				}
				w := fmt.Sprintf("%s[%s] is disabled: %s emptied by a removed CHAT_/CUTLASS_ reference (ADR-0106), so the entry is invalid (%s); the rest of the bundle loads", list.section, label, strings.Join(keys, ", "), msg)
				log.Printf("Warning: clientconfig: %s", w)
				b.retiredDisabled = append(b.retiredDisabled, w)
				list.remove(i)
				removed = true
			}
			if removed {
				break
			}
		}
		if !removed {
			return err
		}
	}
}

// RetiredDisabledItems returns one line per manifest entry the load disabled
// because a removed CHAT_/CUTLASS_ reference emptied a field it requires
// (ADR-0106). `fleet doctor` and `fleet validate-config` report them as
// advisories.
func (b *Bundle) RetiredDisabledItems() []string {
	return append([]string(nil), b.retiredDisabled...)
}

// retiredRefPattern finds the variable name of each ${…} reference.
var retiredRefPattern = regexp.MustCompile(`\$\{\s*([A-Za-z_][A-Za-z0-9_]*)`)

type retiredField struct {
	path, retired, canonical string
}

// retiredEmptiedFields lists the string fields (by YAML path) whose written
// value references a removed spelling and whose interpolated value is empty.
// Lazily-resolved maps (env, headers) are not interpolated at load and are
// skipped.
func retiredEmptiedFields(raw, cooked any) []retiredField {
	var out []retiredField
	var walk func(r, c reflect.Value, path string)
	walk = func(r, c reflect.Value, path string) {
		switch r.Kind() {
		case reflect.Pointer:
			if !r.IsNil() && !c.IsNil() {
				walk(r.Elem(), c.Elem(), path)
			}
		case reflect.Struct:
			for i := range r.NumField() {
				f := r.Type().Field(i)
				tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
				if !f.IsExported() || tag == "" || tag == "-" {
					continue
				}
				p := tag
				if path != "" {
					p = path + "." + tag
				}
				walk(r.Field(i), c.Field(i), p)
			}
		case reflect.Slice:
			for i := 0; i < r.Len() && i < c.Len(); i++ {
				walk(r.Index(i), c.Index(i), fmt.Sprintf("%s[%d]", path, i))
			}
		case reflect.String:
			if strings.TrimSpace(c.String()) != "" {
				return
			}
			for _, m := range retiredRefPattern.FindAllStringSubmatch(r.String(), -1) {
				if canonical, ok := envreg.RetiredAlias(m[1]); ok {
					out = append(out, retiredField{path: path, retired: m[1], canonical: canonical})
					return
				}
			}
		default:
			// Maps (env, headers: resolved lazily, never emptied at load),
			// numbers and bools carry no interpolated required string.
		}
	}
	walk(reflect.ValueOf(raw), reflect.ValueOf(cooked), "")
	return out
}
