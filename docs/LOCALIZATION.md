# Interface languages

Fleet supports English, Spanish and Portuguese. Choose **Language** on the
sign-in screen or in **Settings → General**. The choice reloads the current
page, retaining its path, query parameters and fragment so every menu, cached
label and guide uses the same language. Finish or save an open draft first.

The browser resolves `?lang=en|es|pt`, then `localStorage["elcano.language"]`,
then the host-only `elcano_language` cookie, then its preferred language, with
English as the fallback. Regional tags such as `es-MX` and `pt-BR` resolve to
the base language. The cookie is a one-year display preference with
`SameSite=Lax` and `Secure` on HTTPS; it is not an authentication credential.
Denied browser storage does not prevent rendering or sign-in. The explicit
URL still carries a selection when storage is unavailable.

The Go gateway uses the explicit language or cookie for sign-in handoff,
passing a supported `ui_locales` value to OIDC and `lang` to the legacy Auth
entry point. Session, state, nonce, PKCE and callback validation are unchanged.
Automatic sign-in keeps the selected language in a short-lived host-only,
HttpOnly `fleet_oidc_language` cookie and restores it in the return URL on
success or failure, so an older browser preference cannot replace it. This
display-only cookie is cleared with the OIDC temporary cookies and on logout.
Server-rendered page metadata uses the request preference, falling back to
the browser's language header. Each response localizes a copy of branding,
never the shared branding cache.

## Translated surfaces

The interface catalogs cover chat, projects, approvals, tools, background
tasks, dashboards, settings, administrator screens, accessibility labels,
validation messages, help guides and built-in connector/skill descriptions.
Known HTTP error messages are localized where the browser displays them.
System-check diagnostics and cost-forecast notes have a separate known-message
inventory, applied only to those owned fields.
Unknown tool errors and interpolated details remain literal. Dates and
numbers use the selected locale at display boundaries.

Browser push notifications translate the fixed task and approval prefixes.
An already registered service worker receives the preference and keeps it
in its own small CacheStorage entry, so it survives tab closure and worker
restarts. Storage denial falls back to the browser language. Selecting a
language does not enable notifications or register a worker.

User-authored chats, project names, prompts, tool results, task names and
uploaded files remain their original content. Selecting a language does not
instruct an AI model to translate its replies. Tool names, option values,
model IDs, URLs, cron expressions and exported protocol/source files retain
their exact machine meaning. Prepared drafts from shipped task templates
and prompt forms are translated before the user submits them.

## Client bundles

A bundle may include `locales/es.json` and `locales/pt.json`, each a JSON
object mapping the exact English display source to a translated string.
These files are optional and require no manifest field or environment
variable, so older Fleet versions can continue loading the same bundle.

Bundle catalogs supply branding, support copy, prompt cards, form labels,
option display labels, prepared prompt drafts, task templates, connector
metadata, bundle persona labels and skill descriptions. The persona picker
uses capitalized filenames; YAML persona prose remains model context. A missing entry
falls back to the original text. The loader rejects blank messages, invalid
JSON objects and changes to named or numeric interpolation fields. Preserve
such fields, URLs, Markdown links, identifiers and code in translations.

Only the four public branding strings appear in the public `/brand/meta`
projection. Workspace catalogs, including their English source keys, are
served through the member-gated `/client-config` endpoint and its authorized
bootstrap. Never place credentials in display copy or catalogs.

## Maintaining translations

Use `t("English source", [values])` for owned plain UI text and `rich` for a
complete sentence containing React elements. Pass a whole sentence, including
its plural form, so translations can change word order. Values are
interpolated once and React still escapes them. Do not translate arbitrary
API strings or user records merely because their text matches a catalog key.

Add both translations to `web/src/app/lib/i18n/es.json` and `pt.json`.
`npm run check:localizations` (also part of web lint) checks source coverage,
catalog parity and interpolation fields. Go's `TestCoreMetadataLocalizations`
covers the built-in catalog, skills and default bundle. When HTTP errors
change, run `cd web && npm run gen:localizations` to regenerate the known
message inventory, then translate the new entries. `TestBackendErrorCatalog`
rejects a stale inventory; it does not change the API's English error contract.

Guides live under `web/src/app/help/guides/{es,pt}` alongside the English
sources. Preserve their English section anchors and run `make sync-guides`
after editing; see [USER-GUIDES.md](USER-GUIDES.md). Browser tests cover the
three languages, narrow layouts, persistence, blocked storage and unchanged
user content. Run the full web and Go gates before submitting changes.
