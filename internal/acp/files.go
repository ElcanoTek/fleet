package acp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	acpsdk "github.com/coder/acp-go-sdk"
)

// fileReader reads a file through the ACP client (fs/read_text_file). The
// SDK's AgentSideConnection is one.
type fileReader interface {
	ReadTextFile(ctx context.Context, params acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error)
}

const (
	// linkedFileMax is the most of one linked file a message carries. A
	// longer file is cut at a line break, with a note saying so. It is the
	// size up to which Emacs agent-shell embeds a file by default.
	linkedFileMax = 100 << 10
	// linkedFilesMax is the most a message carries of all its linked files
	// together. fleet's chat server takes at most 1 MB per request, which
	// must also hold the prompt's own text and the JSON escaping of all of
	// it; past this, a file is cut or left out, with a note.
	linkedFilesMax = 300 << 10
	// linkedFilesFloor is the least of the budget worth reading a file for:
	// with less left, a file is left out rather than cut to a sliver.
	linkedFilesFloor = 1 << 10
	// linkedFileReadMax is the largest file on disk fleet acp asks the client
	// to read. The client answers a read with one JSON-RPC line, and the SDK
	// drops the whole connection at a line over 10 MiB (maxLine), which no
	// file up to 1 MB can make, even with every byte escaped (6 bytes each).
	linkedFileReadMax = 1 << 20
)

// linkedFilesWait bounds all of one message's reads, so a client that never
// answers one holds the prompt for at most this long. A var so tests can
// shorten it.
var linkedFilesWait = 10 * time.Second

// withLinkedFiles is what a prompt submits to fleet: message (promptText's),
// with the text of each file the prompt links to (a file:// resource_link)
// read through the client and put in the link's place, as an embedded text
// resource's is. fleet runs on a server, which cannot open a path on the
// user's machine, and some clients send every file as a link: JetBrains
// AI Assistant links both a file the user attaches and the file open in the
// editor, and never sends their text.
//
// Files are read only from a client that said in initialize that it
// answers fs/read_text_file, and only those a prompt links to: nothing the
// model asks for, and nothing else on the user's machine. Reading through
// the client, rather than from disk, gets what the user sees, unsaved edits
// included, and leaves to the client which files an agent may read. A file
// linked twice (an attached file that is also the open one) is included
// once, where it is first linked.
//
// A file is cut to linkedFileMax, the files together to linkedFilesMax, and
// a file over linkedFileReadMax on disk is not read at all; a cut file says
// how much of it is included. A file that is not included (too large, a
// folder, not text, a read the client refused, or no answer within
// linkedFilesWait) stays a link, followed by why, so the model can tell the
// user. Links to anything but a local file (another scheme, another host)
// are left as links.
//
// message is returned as is when no file is read. It, not the result, keys
// a resend (idempotencyKey): the same prompt sent again is the same message,
// whatever its files hold by then.
func (a *Agent) withLinkedFiles(ctx context.Context, sid acpsdk.SessionId, blocks []acpsdk.ContentBlock, message string) string {
	if a.files == nil || !a.clientReads.Load() || !linksLocalFile(blocks) {
		return message
	}
	ctx, cancel := context.WithTimeout(ctx, linkedFilesWait)
	defer cancel()
	budget := linkedFilesMax
	seen := map[string]bool{}
	out, err := renderPrompt(blocks, func(l *acpsdk.ContentBlockResourceLink) (string, bool) {
		path, ok := localPath(l.Uri)
		if !ok {
			return linkMarkdown(l), true
		}
		if seen[path] {
			return "", false
		}
		seen[path] = true
		text, note, ok := a.readLinked(ctx, sid, path, budget)
		if !ok {
			return linkMarkdown(l) + " (not included: " + note + ")", true
		}
		budget -= len(text)
		return contentsOf(l.Uri, note, text), true
	})
	if err != nil {
		return message // not reached: promptText accepted these blocks
	}
	return out
}

// readLinked reads the file at path through the client for a message with
// budget bytes of linked files left. ok reports whether its text is
// included; note is then what was cut ("" when nothing was), and otherwise
// why it is not.
func (a *Agent) readLinked(ctx context.Context, sid acpsdk.SessionId, path string, budget int) (text, note string, ok bool) {
	if budget < linkedFilesFloor {
		return "", fmt.Sprintf("this message already carries %s of linked files, the most fleet acp sends", sizeText(linkedFilesMax)), false
	}
	// A file that is not on this machine (a buffer never saved) is still
	// asked for: the client may hold it.
	if fi, err := os.Stat(path); err == nil {
		switch {
		case fi.IsDir():
			return "", "it is a folder; fleet acp includes linked files, not folders", false
		case !fi.Mode().IsRegular():
			return "", "it is not a regular file", false
		case fi.Size() > linkedFileReadMax:
			return "", fmt.Sprintf("it is %s, and fleet acp reads linked files of up to %s", sizeText(int(fi.Size())), sizeText(linkedFileReadMax)), false
		}
	}
	resp, err := a.files.ReadTextFile(ctx, acpsdk.ReadTextFileRequest{SessionId: sid, Path: path})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Sprintf("the editor had not sent it within the %s fleet acp waits for a message's linked files", linkedFilesWait), false
		}
		reason := err.Error()
		var re *acpsdk.RequestError
		if errors.As(err, &re) {
			reason = re.Message
		}
		return "", "the editor could not read it: " + errorMessage(reason), false
	}
	text = resp.Content
	if strings.ContainsRune(text, 0) {
		return "", "it is not a text file", false
	}
	switch limit := min(linkedFileMax, budget); {
	case len(text) <= limit:
		return text, "", true
	case limit == linkedFileMax:
		cut := cutAt(text, limit)
		return cut, fmt.Sprintf("the first %s of %s: fleet acp includes up to %s of a linked file",
			sizeText(len(cut)), sizeText(len(text)), sizeText(linkedFileMax)), true
	default:
		cut := cutAt(text, limit)
		return cut, fmt.Sprintf("the first %s of %s: fleet acp includes up to %s of linked files in one message",
			sizeText(len(cut)), sizeText(len(text)), sizeText(linkedFilesMax)), true
	}
}

// linksLocalFile reports whether any block is a link to a local file.
func linksLocalFile(blocks []acpsdk.ContentBlock) bool {
	for _, b := range blocks {
		if b.ResourceLink != nil {
			if _, ok := localPath(b.ResourceLink.Uri); ok {
				return true
			}
		}
	}
	return false
}

// localPath is the absolute path a file:// URI names on this machine (no
// host, or localhost), as fs/read_text_file takes it.
func localPath(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return "", false
	}
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", false
	}
	if !filepath.IsAbs(u.Path) {
		return "", false
	}
	return u.Path, true
}

// cutAt is text's head of at most limit bytes: up to the last line break
// within it, or, for a first line longer than limit, up to the last whole
// character.
func cutAt(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if i := strings.LastIndexByte(text[:limit], '\n'); i >= 0 {
		return text[:i+1]
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// sizeText is n bytes as a person reads a size: "512 bytes", "100 KB",
// "3.2 MB" (KB and MB are 1024 and 1024² bytes; KB round up).
func sizeText(n int) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%d KB", (n+1<<10-1)>>10)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}
