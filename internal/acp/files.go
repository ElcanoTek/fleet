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

// An SDK change to ReadTextFile's signature must fail the build here, not
// leave SetConnection without a reader and every link unread.
var _ fileReader = (*acpsdk.AgentSideConnection)(nil)

const (
	// linkedFileCut is the most of one linked file a message carries. A
	// longer file is cut, with a note saying so. It is the size up to which
	// Emacs agent-shell embeds a file by default.
	linkedFileCut = 100 << 10
	// linkedFilesBudget is the most a message carries of all the files it
	// links to together; past it, a file is cut or left out, with a note. It
	// keeps a message well inside fleet's 1 MB request cap in the usual case
	// but does not promise it: the cap also counts the prompt's own text and
	// embedded resources, and the request's JSON, in which `<`, `>` and `&`
	// take 6 bytes each. A message dense in them can still pass the cap, and
	// fleet then refuses it ("request body too large").
	linkedFilesBudget = 300 << 10
	// linkedFilesFloor is the least of the budget a file is cut to: with less
	// left, a file that does not fit whole is left out rather than cut to a
	// sliver.
	linkedFilesFloor = 1 << 10
	// linkedFileReadMax is the largest file fleet acp asks the client for, by
	// its size on disk (or the size on its link, for a file not on disk). The
	// client answers a read with one JSON-RPC line, and the SDK drops the
	// whole connection at a line over 10 MiB (maxLine), which no file up to
	// 1 MB can make, even with every byte escaped (6 bytes each). The client
	// answers with its buffer, though, not the disk's copy, and unsaved edits
	// can make that far larger; linkedFileLines bounds that case.
	linkedFileReadMax = 1 << 20
	// linkedFileLines is how many lines fleet acp asks for of every file:
	// one whose size it does not know (not on disk, and no size on its link),
	// and one on disk too, whose buffer in the client may hold unsaved edits
	// far larger than the disk's copy. A line is at least one byte with its
	// line break, so when the file goes on past these lines they hold more
	// than linkedFileCut bytes, even if the client drops the last one's line
	// break, and the cut is the one the whole file would get. It bounds the
	// answer for a file of many lines (a log, a large paste); one of a few
	// very long lines can still make an answer over 10 MiB, which ends the
	// connection.
	linkedFileLines = linkedFileCut + 2
)

// linkedFilesWait bounds all of one message's reads, so a client that never
// answers one holds the prompt for at most this long. A prompt a client sends
// as a notification waits it out once and gets none of its files: the SDK
// hands an agent a read's answer only once the notifications sent before it
// have been handled, the prompt running the read among them. A var so tests
// can shorten it.
var linkedFilesWait = 10 * time.Second

// withLinkedFiles is what a prompt submits to fleet: message (promptText's),
// with the text of each file the prompt links to (a file:// resource_link)
// read through the client and put in the link's place, as an embedded text
// resource's is. The model's tools run in fleet's sandbox, which cannot open
// a path on the user's machine, and some clients send every file as a link:
// JetBrains AI Assistant links both a file the user attaches and the file
// open in the editor, and never sends their text.
//
// Files are read only from a client that said in initialize that it
// answers fs/read_text_file, and only those a prompt links to: nothing the
// model asks for, and nothing else on the user's machine. Reading through
// the client, rather than from disk, gets what the user sees, unsaved edits
// included, and leaves to the client which files an agent may read. A file
// linked twice (an attached file that is also the open one) is read once:
// its text goes where it is first linked, and a later link that names it
// differently (a line fragment, say) stays a link that says so.
//
// A file is cut to linkedFileCut, the files together to linkedFilesBudget,
// and one over linkedFileReadMax is not read at all; a cut file says how
// much of it is included and which limit cut it. A file that is not
// included (too large, a folder, not text, a read the client refused, or not
// sent within linkedFilesWait) stays a link, followed by why, so the model
// can tell the user. Once the prompt is cancelled or the wait has run out,
// no more files are asked for. Links to anything but a local file (another
// scheme, another host) are left as links.
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
	budget := linkedFilesBudget
	seen := map[string]string{} // path → the URI it was first linked by
	out, err := renderPrompt(blocks, func(l *acpsdk.ContentBlockResourceLink) (string, bool) {
		path, ok := localPath(l.Uri)
		if !ok {
			return linkMarkdown(l), true
		}
		if first, ok := seen[path]; ok {
			if first == l.Uri {
				return "", false
			}
			return linkMarkdown(l) + " (the same file as a link above)", true
		}
		seen[path] = l.Uri
		text, note, ok := a.readLinked(ctx, sid, l, path, budget)
		if !ok {
			return linkMarkdown(l) + " (not included: " + note + ")", true
		}
		budget -= len(text)
		heading := wholeFile(l.Uri)
		if heading != l.Uri {
			// The link named part of the file (#L5:9): keep it as sent,
			// so the model knows which lines the user pointed at.
			note = strings.TrimSuffix("linked as "+linkMarkdown(l)+"; "+note, "; ")
		}
		return contentsOf(heading, note, text), true
	})
	if err != nil {
		return message // not reached: promptText accepted these blocks
	}
	return out
}

// readLinked reads the file at path, which l links to, through the client,
// for a message with budget bytes of linked files left. ok reports whether
// its text is included; note is then what was cut ("" when nothing was),
// and otherwise why it is not.
func (a *Agent) readLinked(ctx context.Context, sid acpsdk.SessionId, l *acpsdk.ContentBlockResourceLink, path string, budget int) (text, note string, ok bool) {
	if ctx.Err() != nil {
		return "", stoppedNote(ctx), false
	}
	size := -1 // unknown
	if fi, err := os.Stat(path); err == nil {
		switch {
		case fi.IsDir():
			return "", "it is a folder; fleet acp includes linked files, not folders", false
		case !fi.Mode().IsRegular():
			return "", "it is not a regular file", false
		}
		size = int(fi.Size())
	} else if l.Size != nil && *l.Size > 0 {
		// Not on this machine (a buffer never saved, say): the client may
		// hold it, and its link may say how large it is. A size of 0 is
		// taken as unknown: a client may send it for a buffer it never sized.
		size = *l.Size
	}
	if size > linkedFileReadMax {
		return "", fmt.Sprintf("it is %s, and fleet acp reads linked files of up to %s", sizeText(size), sizeText(linkedFileReadMax)), false
	}
	if size > budget && budget < linkedFilesFloor {
		return "", budgetNote(), false
	}
	// Every read is bounded in lines, a file's size on disk included: the
	// client answers with its buffer, unsaved edits and all, which can be far
	// larger than the disk's copy, and the text is cut to limit below anyway.
	lines := linkedFileLines
	req := acpsdk.ReadTextFileRequest{SessionId: sid, Path: path, Limit: &lines}
	resp, err := a.files.ReadTextFile(ctx, req)
	if err != nil {
		a.catchHangUp() // a read fails at once when the client goes away
		if ctx.Err() != nil {
			return "", stoppedNote(ctx), false
		}
		return "", "the editor could not read it: " + errorMessage(clientError(err)), false
	}
	text = resp.Content
	if strings.ContainsRune(text, 0) {
		return "", "it is not a text file", false
	}
	limit := min(linkedFileCut, budget)
	if len(text) <= limit {
		return text, "", true
	}
	if limit < linkedFilesFloor {
		return "", budgetNote(), false
	}
	of := "of " + sizeText(len(text))
	if strings.Count(text, "\n") >= linkedFileLines-1 {
		of = "of a longer file" // the answer may stop short of the file's end
	}
	cut := cutAt(text, limit)
	if limit == linkedFileCut {
		return cut, fmt.Sprintf("the first %s %s: fleet acp includes up to %s of a linked file",
			sizeText(len(cut)), of, sizeText(linkedFileCut)), true
	}
	return cut, fmt.Sprintf("the first %s %s: fleet acp includes up to %s of linked files in one message",
		sizeText(len(cut)), of, sizeText(linkedFilesBudget)), true
}

// stoppedNote is why a file is not included once ctx has ended: the wait ran
// out, or the prompt was cancelled (and is not submitted at all).
func stoppedNote(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("the %s fleet acp waits for a message's linked files ran out before it arrived", linkedFilesWait)
	}
	return "the message was cancelled before it was read"
}

// clientError is what the client said about a failed request: its error's
// message, and the detail it put in data when the message is only the name
// of the error's code ("Internal error").
func clientError(err error) string {
	var re *acpsdk.RequestError
	if !errors.As(err, &re) {
		return err.Error()
	}
	detail := ""
	switch d := re.Data.(type) {
	case string:
		detail = d
	case map[string]any:
		for _, k := range []string{"error", "message", "details"} {
			if v, ok := d[k].(string); ok && v != "" {
				detail = v
				break
			}
		}
	}
	if detail == "" || detail == re.Message {
		return re.Message
	}
	return re.Message + ": " + detail
}

func budgetNote() string {
	return fmt.Sprintf("this message already carries %s of linked files, the most fleet acp sends", sizeText(linkedFilesBudget))
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

// wholeFile is a file URI without its query or fragment: the heading of the
// whole file's text, which a fragment naming lines (#L5) would misdescribe.
func wholeFile(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || (u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery) {
		return uri
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	return u.String()
}

// cutAt is text's head of at most limit bytes: up to the last line break
// within it, unless that keeps less than half the limit (a short first line
// before a very long one), and otherwise up to the last whole character.
func cutAt(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	if i := strings.LastIndexByte(text[:limit], '\n'); i >= limit/2 {
		return text[:i+1]
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

// sizeText is n bytes as a person reads a size: "512 bytes", "100 KB",
// "1.5 MB". KB and MB are 1024 and 1024² bytes, and both round up, so a
// size over a limit never reads as the limit.
func sizeText(n int) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n <= 1023<<10:
		return fmt.Sprintf("%d KB", (n+1<<10-1)>>10)
	}
	tenths := (n*10 + 1<<20 - 1) >> 20
	if tenths%10 == 0 {
		return fmt.Sprintf("%d MB", tenths/10)
	}
	return fmt.Sprintf("%d.%d MB", tenths/10, tenths%10)
}
