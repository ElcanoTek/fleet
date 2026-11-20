package sharedfiles

import (
	"fmt"
	"mime"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// Files chats write (ADR-0114, docs/SHARED-FILES.md "Files chats write"): the
// rules a shared_file_write call is checked against before it reaches the
// store. They live here, beside the admin upload's SanitizeName/SanitizeFolder,
// so the two write paths agree on what a library path is.

// MaxChatFileBytes caps the size of one file chats write. A shared file is a
// record (a log, a register, an index), not a dataset: past this size it
// belongs in a new file (one per month, per campaign) or in an object store.
// Every turn that reads it pays for what it prints, never for its size, but a
// cap keeps a runaway loop from filling the library.
const MaxChatFileBytes = tools.SharedFileMaxBytes

// MaxChatDescriptionRunes caps a chat-written description. It is announced to
// every conversation on the server in each turn's prompt, so it is one short
// line, never a paragraph.
const MaxChatDescriptionRunes = 200

// ParseChatPath splits the path a model passes to shared_file_write into the
// library's (folder, name). It accepts "name", "folder/name", and either with
// a leading "shared/" (the path the model reads the file at). The library has
// one folder level, so a deeper path is refused rather than flattened — a
// model that wrote "a/b/c.csv" would otherwise read it back at a path it never
// wrote.
func ParseChatPath(raw string) (folder, name string, err error) {
	p := strings.TrimSpace(strings.ReplaceAll(raw, `\`, "/"))
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, tools.SharedFilesDirName+"/")
	p = strings.Trim(p, "/")
	if p == "" {
		return "", "", fmt.Errorf("path is required, e.g. %q", "optimizations/log.csv")
	}
	parts := strings.Split(p, "/")
	switch len(parts) {
	case 1:
		name = parts[0]
	case 2:
		folder, name = parts[0], parts[1]
	default:
		return "", "", fmt.Errorf("path %q has %d folder levels: the shared library has one (use folder/name, e.g. %q)", raw, len(parts)-1, parts[0]+"/"+parts[len(parts)-1])
	}
	for _, seg := range []string{folder, name} {
		if seg == "." || seg == ".." {
			return "", "", fmt.Errorf("path %q is not inside the shared library", raw)
		}
	}
	if folder, err = SanitizeFolder(folder); err != nil {
		return "", "", err
	}
	clean, err := SanitizeName(name)
	if err != nil {
		return "", "", err
	}
	if clean != name {
		// Writing under a different name than the model asked for would make
		// its next read miss; refuse and name what is acceptable instead.
		return "", "", fmt.Errorf("file name %q is not usable as is (try %q)", name, clean)
	}
	for _, seg := range []string{folder, clean} {
		if err := checkChatPathChars(seg); err != nil {
			return "", "", err
		}
	}
	return folder, clean, nil
}

// checkChatPathChars allows a chat-created path segment only letters, digits,
// spaces and . _ - , + @ =. Every conversation on the server is shown these
// paths in its prompt, so nothing one user's chat writes there may act as a
// Markdown delimiter (backtick, bracket, asterisk) or a shell metacharacter
// (& ; | # $ ' " ( ) < > ~ and friends) in anyone else's — a model must never be
// handed a path that turns into a command. Uploads (an admin's choice) keep the
// wider SanitizeName rules.
func checkChatPathChars(seg string) error {
	for _, r := range seg {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case ' ', '.', '_', '-', ',', '+', '@', '=':
			continue
		}
		return fmt.Errorf("%q contains %q: shared file and folder names written by chats may use letters, digits, spaces and . _ - , + @ = only", seg, r)
	}
	return nil
}

// ChatPromptPath is ParseChatPath's inverse: the path the model reads a
// chat-written file at.
func ChatPromptPath(folder, name string) string {
	if folder == "" {
		return tools.SharedFilesDirName + "/" + name
	}
	return tools.SharedFilesDirName + "/" + folder + "/" + name
}

// SanitizeChatDescription makes a chat-written description safe to show every
// conversation on the server: one line, no control characters or backticks
// (which would break the prompt block's code spans), and at most
// MaxChatDescriptionRunes. The text still comes from another user's chat; the
// prompt block labels it as data, and this only keeps it from taking over the
// block's layout.
func SanitizeChatDescription(raw string) string {
	var b strings.Builder
	space := false
	for _, r := range raw {
		switch {
		case r == '`':
			r = '\''
		case unicode.IsSpace(r) || unicode.IsControl(r):
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > MaxChatDescriptionRunes {
		runes := []rune(out)
		out = strings.TrimSpace(string(runes[:MaxChatDescriptionRunes-1])) + "…"
	}
	return out
}

// ValidateChatText checks that content is text the library can hold as a
// chat file: valid UTF-8 with no NUL byte (Postgres TEXT refuses NUL, and a
// binary file is not what this tool is for — an administrator publishes
// those).
func ValidateChatText(content string) error {
	if !utf8.ValidString(content) {
		return fmt.Errorf("content is not valid UTF-8 text: shared_file_write stores text files (CSV, Markdown, JSON, YAML, plain text) only")
	}
	if strings.IndexByte(content, 0) >= 0 {
		return fmt.Errorf("content contains a NUL byte: shared_file_write stores text files only")
	}
	return nil
}

// ChatContentType is the download Content-Type for a chat-written file, from
// its extension, always declaring UTF-8.
func ChatContentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".json", ".jsonl", ".ndjson":
		return "application/json; charset=utf-8"
	case ".yaml", ".yml":
		return "application/yaml; charset=utf-8"
	case ".tsv":
		return "text/tab-separated-values; charset=utf-8"
	}
	if t := mime.TypeByExtension(ext); strings.HasPrefix(t, "text/") {
		if !strings.Contains(t, "charset") {
			t += "; charset=utf-8"
		}
		return t
	}
	return "text/plain; charset=utf-8"
}
