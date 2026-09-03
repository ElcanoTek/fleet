# Downloading a chat

A conversation's kebab menu offers **Download chat…**, which opens a chooser
rather than starting a download. The chooser exists because the previous
affordance — a single menu item labelled *Download as JSON* — asked the reader
to know what JSON is, and then handed them the one artifact a non-technical
user can do the least with.

## The three formats

| Option | File | What it is for |
| --- | --- | --- |
| **Web page** (default) | `.html` | Opens on a double-click in any browser and looks like the chat. Print it (Ctrl/Cmd-P) to save a PDF for a client. |
| **Text document** | `.md` | Plain, readable text to paste into email, Word, Google Docs, or Notion. |
| **Raw data** | `.json` | Every message, tool call and result exactly as stored. For developers. |

Each is served by `GET /conversations/{id}/export?format=html\|markdown\|json`.
**The endpoint's default is still `json`**, byte-for-byte what it returned
before, so an existing script calling the bare URL is unaffected.

## Scope: the conversation, or the conversation plus the agent's work

A rendered transcript takes `?include=`:

- `conversation` (**default**) — the human and assistant text turns.
- `full` — adds the agent's working trail: its thinking, each tool call, each
  tool result, and any compaction summaries.

The dialog exposes this as one unchecked checkbox, *Include the agent's work*.
Off by default is the deliberate choice: a long research chat can run to
hundreds of history entries, of which a handful are the conversation, and the
readable document is what someone downloading a chat almost always wants. The
JSON export ignores the parameter — it is the archival shape and always
carries everything.

Note that this **changed the behavior of `?format=markdown`**, which shipped in
#210 rendering every entry type and was never reachable from the UI. It now
defaults to the conversation alone; pass `include=full` for the old output.

## The web page's safety properties

The HTML export is a single self-contained file: styles are inlined, no
external asset is fetched, and it renders offline from `file://`.

Message text is model- and tool-authored, so the renderer treats all of it as
untrusted:

- Markdown is rendered by goldmark built **without** `html.WithUnsafe()`, so
  raw HTML in a message can never become markup.
- goldmark's default is to *drop* raw HTML (replacing it with an invisible
  comment), which would silently delete content — an assistant reply explaining
  a `<div>` would export with the explanation missing. A small custom node
  renderer instead writes those nodes as **escaped, visible text**, which is
  faithful to what the reader saw and exactly as inert.
- Everything outside the Markdown renderer — the title, model, persona, tool
  names, tool JSON, tool output — goes through `html.EscapeString`. There is no
  template execution and no `template.HTML` conversion in the renderer.
- The response is `Content-Disposition: attachment` with
  `X-Content-Type-Options: nosniff` on every format, so the browser saves the
  file rather than rendering model-authored HTML on the app's origin.

`internal/httpapi/export_html_test.go` pins these: a fixture whose title, tool
name, tool input and message bodies are all injection payloads must produce a
page containing no unescaped `<script>`, `<iframe>`, `<img>`, `<object>` or
`<svg>`.

## Shipped scope and deliberate deferrals

- Shipped: the three formats, the include-the-work switch, the chooser dialog,
  the print stylesheet, and the escaping guarantees above.
- Deferred: server-side PDF rendering (printing the web page covers it without
  a headless-browser dependency in the serving path), a bulk/multi-chat export
  (the existing project export covers the grouped case), attachment or
  generated-file bundling into the download (the HTML references no workspace
  files — images in a reply are not embedded), and re-import of any format.
