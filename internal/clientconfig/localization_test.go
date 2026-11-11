package clientconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLocalizationsLoadAndValidation(t *testing.T) {
	t.Parallel()
	for name, content := range map[string]string{
		"valid":        `{"Welcome {name}":"Hola {name}"}`,
		"empty":        `{"Welcome":""}`,
		"missing slot": `{"Welcome {name}":"Hola"}`,
		"wrong slot":   `{"Welcome {name}":"Hola {person}"}`,
		"null":         `null`,
		"not strings":  `{"Welcome":42}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "locales"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "locales", "es.json"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := loadLocalizations(dir)
			if name == "valid" {
				if err != nil || got["es"]["Welcome {name}"] != "Hola {name}" {
					t.Fatalf("%v: %v", got, err)
				}
			} else if err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
	if got, err := loadLocalizations(t.TempDir()); err != nil || len(got) != 0 {
		t.Fatalf("optional catalogs: %v, %v", got, err)
	}
}

func TestPublicLocalizationsExcludeWorkspaceCopy(t *testing.T) {
	t.Parallel()
	b := &Bundle{
		Branding:      Branding{LoginTitle: "Welcome", LoginTagline: "Sign in"},
		Localizations: Localizations{"es": {"Welcome": "Bienvenido", "Sign in": "Iniciar sesión", "Private workflow": "Flujo privado"}},
	}
	want := Localizations{"es": {"Welcome": "Bienvenido", "Sign in": "Iniciar sesión"}}
	if got := b.PublicLocalizations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("public catalog = %v", got)
	}
	if b.Localizations["es"]["Private workflow"] != "Flujo privado" {
		t.Fatal("projection mutated private catalog")
	}
}
