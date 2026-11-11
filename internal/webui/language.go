package webui

import (
	"net/http"
	"net/url"
	"strings"
)

// requestLanguage accepts only the UI's supported language tags. It never
// participates in session validation, redirect selection or OIDC state.
func requestLanguage(r *http.Request) string {
	if value := r.URL.Query().Get("lang"); value != "" {
		if language := normalizeLanguage(value); language != "" {
			return language
		}
	}
	if cookie, err := r.Cookie("elcano_language"); err == nil && cookie.Value != "" {
		if language := normalizeLanguage(cookie.Value); language != "" {
			return language
		}
	}
	return ""
}

func normalizeLanguage(value string) string {
	base := strings.ToLower(strings.TrimSpace(value))
	base, _, _ = strings.Cut(strings.ReplaceAll(base, "_", "-"), "-")
	switch base {
	case "en":
		return "en"
	case "es":
		return "es"
	case "pt":
		return "pt"
	default:
		return ""
	}
}

// The browser uses its first language when there is no explicit preference.
// Match that choice for the initial HTML and public share metadata as well.
func pageLanguage(r *http.Request) string {
	if selected := requestLanguage(r); selected != "" {
		return selected
	}
	first, _, _ := strings.Cut(r.Header.Get("Accept-Language"), ",")
	first, _, _ = strings.Cut(first, ";")
	if selected := normalizeLanguage(first); selected != "" {
		return selected
	}
	return "en"
}

func localizedBranding(b Branding, locale string) Branding {
	catalog := b.Localizations[locale]
	fallback := defaultBrandingMessages[locale]
	translate := func(source string) string {
		if value := catalog[source]; value != "" {
			return value
		}
		if value := fallback[source]; value != "" {
			return value
		}
		return source
	}
	b.LoginTitle = translate(b.LoginTitle)
	b.LoginTagline = translate(b.LoginTagline)
	b.ShareTitle = translate(b.ShareTitle)
	b.ShareDescription = translate(b.ShareDescription)
	return b
}

func localizedPageTitle(title, locale string) string {
	messages := map[string]map[string]string{
		"es": {"Analytics": "Analítica", "Guides": "Guías", "Chat guide": "Guía del chat", "Background tasks guide": "Guía de tareas en segundo plano"},
		"pt": {"Analytics": "Análise", "Guides": "Guias", "Chat guide": "Guia do chat", "Background tasks guide": "Guia de tarefas em segundo plano"},
	}
	if value := messages[locale][title]; value != "" {
		return value
	}
	return title
}

func loginLanguageQuery(r *http.Request) string {
	if selected := requestLanguage(r); selected != "" {
		return "&lang=" + selected // supported enum only
	}
	return ""
}

// Only engine-owned fallback branding belongs here. Customer copy is supplied
// by the public projection of that deployment's bundle catalog.
var defaultBrandingMessages = map[string]map[string]string{
	"es": {
		"Fleet — your team's AI workspace":                                                        "Fleet: el espacio de trabajo de IA de tu equipo",
		"Persistent multi-turn conversations with real tool use across files, data, and the web.": "Conversaciones persistentes de varios turnos con uso real de herramientas para archivos, datos y la web.",
		"Welcome aboard.": "Bienvenido a bordo.",
		"Sign in to your workspace and pick up where you left off.": "Inicia sesión en tu espacio de trabajo y retoma donde lo dejaste.",
		"An AI workspace with real tool use.":                       "Un espacio de trabajo de IA que usa herramientas reales.",
	},
	"pt": {
		"Fleet — your team's AI workspace":                                                        "Fleet: o espaço de trabalho de IA da sua equipa",
		"Persistent multi-turn conversations with real tool use across files, data, and the web.": "Conversas persistentes com vários turnos e uso real de ferramentas para ficheiros, dados e a Web.",
		"Welcome aboard.": "Boas-vindas a bordo.",
		"Sign in to your workspace and pick up where you left off.": "Inicie sessão no seu espaço de trabalho e retome onde ficou.",
		"An AI workspace with real tool use.":                       "Um espaço de trabalho de IA que usa ferramentas reais.",
	},
}

// This short-lived display hint is independent of the authentication cookies.
// Returning it in the landing URL lets an explicit sign-in choice override an
// older localStorage preference before the SPA synchronizes its stores.
const oidcLanguageCookie = "fleet_oidc_language"

func withLanguage(path, language string) string {
	language = normalizeLanguage(language)
	if language == "" {
		return path
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return path
	}
	query := parsed.Query()
	query.Set("lang", language)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func requestLanguagePath(r *http.Request, path string) string {
	return withLanguage(path, requestLanguage(r))
}

func oidcLanguagePath(r *http.Request, path string) string {
	return withLanguage(path, cookieValue(r, oidcLanguageCookie))
}
