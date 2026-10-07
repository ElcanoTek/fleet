// Files in a team share (ADR-0079, docs/TEAM-SHARING.md "Files in a team
// share"): the owner's outputs listing and per-file toggle, the teammate's
// download route, the team link, and the branch file copy.
//
// The download route (GET /conversations/{id}/team-files/<path>) is the FIRST
// cross-user file read in fleet. Its gate is three conditions, every one
// re-checked on every request and none sufficient alone:
//
//  1. the caller can read the chat through the team door right now — the
//     exact GetTeamVisibleConversation gate team-view uses, read as
//     CanTeamRead without the transcript (owner's opt-in,
//     caller's team is the audience the owner named, not archived/deleted);
//  2. <path> is a CURRENT output — presented in an assistant reply AND a
//     regular file on disk now — matched by exact string against that list,
//     so a traversal, an upload, or a file the agent never linked cannot be
//     named at all;
//  3. the owner has not excluded it.
//
// The bytes are then read with openWorkspaceFileNoFollow, which refuses a
// symlink at any component, so the owner's sandbox cannot swap a shared name
// for a link to an upload or an unchecked output between the check and the
// read. Every refusal is a 404, indistinguishable from "no such file".

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// ownerOutputs loads what discovery needs of convID's history and its
// exclusions and resolves its outputs. The caller has already established
// that the caller may see them. truncated reports that the transcript
// references more distinct files than discovery considers
// (maxOutputReferences, or the visit budget); the oldest were skipped.
func (s *Server) ownerOutputs(ctx context.Context, convID string) (outs []outputFile, truncated bool, err error) {
	history, err := s.discoveryHistory(ctx, convID, 0)
	if err != nil {
		return nil, false, err
	}
	return s.outputsFromHistory(ctx, convID, history)
}

// discoveryHistory reads only the rows output discovery visits — the
// rendered replies' text and the boundaries between them, newest first,
// stopped where discovery's own budget (maxDiscoveryReplies /
// maxDiscoveryBytes) would stop — through message id `through` (0 = all).
// Discovery over it is identical to discovery over the full history: every
// row the walk reads is here, in order, and the rows it ignores are not.
// It is NOT a transcript (user rows carry no text) and is never rendered.
func (s *Server) discoveryHistory(ctx context.Context, convID string, through int64) ([]agent.HistoryEntry, error) {
	cut := discoveryCutoff{maxReplies: maxDiscoveryReplies, maxBytes: maxDiscoveryBytes}
	return s.store.LoadDiscoveryHistory(ctx, convID, through, maxDiscoveryBytes, cut.more)
}

// outputsFromHistory is ownerOutputs for a caller that already holds the
// transcript (team-view's snapshot, the branch path).
func (s *Server) outputsFromHistory(ctx context.Context, convID string, history []agent.HistoryEntry) (outs []outputFile, truncated bool, err error) {
	excluded, err := s.store.ListOutputExclusions(ctx, convID)
	if err != nil {
		return nil, false, err
	}
	return conversationOutputsCtx(ctx, convID, history, excluded)
}

// writeOutputsResponse is the one body GET outputs and POST outputs/share
// both answer with. truncated is additive: true when older references were
// beyond the discovery bound and are not in the list.
func writeOutputsResponse(w http.ResponseWriter, outs []outputFile, truncated, teamVisible bool) {
	writeJSON(w, map[string]any{
		"outputs":      outs,
		"total":        len(outs),
		"shared_count": countShared(outs),
		"team_visible": teamVisible,
		"truncated":    truncated,
	})
}

// handleConversationOutputs serves the owner's outputs listing and per-file
// toggle:
//
//	GET  /conversations/{id}/outputs
//	POST /conversations/{id}/outputs/share   {"path": "...", "shared": bool}
//
// Owner-only (404 otherwise). `shared` is the file's own state — not excluded
// — and is independent of whether the chat is currently shared, so the share
// dialog can show the checklist before the chat is shared.
func (s *Server) handleConversationOutputs(w http.ResponseWriter, r *http.Request, user, convID, subArg string) {
	switch {
	case subArg == "" && r.Method == http.MethodGet:
	case subArg == "share" && r.Method == http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Path   string `json:"path"`
			Shared *bool  `json:"shared"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		// "shared" is required: a missing field must not decode to false and
		// silently UNSHARE a file the caller meant to share.
		if body.Shared == nil {
			http.Error(w, `"shared" is required`, http.StatusBadRequest)
			return
		}
		// The listing the response carries is read BEFORE the write, and the
		// write's own effect applied to it after: once the exclusion has
		// committed, nothing may fail the request — a 500 then would tell the
		// owner a share failed while teammates can already download the file.
		// A read failure happens first and changes nothing.
		outs, truncated, err := s.ownerOutputs(r.Context(), convID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.store.SetOutputShared(r.Context(), user, convID, body.Path, *body.Shared); err != nil {
			switch {
			case errors.Is(err, store.ErrInvalidOutputPath):
				http.Error(w, "invalid path", http.StatusBadRequest)
			case errors.Is(err, store.ErrTooManyExclusions):
				http.Error(w, err.Error(), http.StatusConflict)
			case errors.Is(err, store.ErrConversationNotFound):
				http.Error(w, "not found", http.StatusNotFound)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
		for i := range outs {
			if outs[i].Path == body.Path {
				outs[i].Shared = *body.Shared
			}
		}
		writeOutputsResponse(w, outs, truncated, conv.TeamVisible)
		return
	}
	outs, truncated, err := s.ownerOutputs(r.Context(), convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeOutputsResponse(w, outs, truncated, conv.TeamVisible)
}

// activeContentTypes are the types a browser would execute as a document.
// Served from our origin they are a stored-XSS primitive (an agent writes
// report.html with a <script>, a teammate opens it with their session), so
// they are always forced to download. Mirrors the web workspace proxy.
var activeContentTypes = []string{
	"text/html", "image/svg+xml", "application/xhtml+xml", "text/xml", "application/xml",
}

func isActiveContentType(ctype string) bool {
	ctype = strings.ToLower(ctype)
	for _, t := range activeContentTypes {
		if strings.HasPrefix(ctype, t) {
			return true
		}
	}
	return false
}

// handleTeamFile serves GET /conversations/{id}/team-files/<path> — a
// teammate's download of one SHARED output. See the file comment for the
// three-part gate; every refusal is 404.
func (s *Server) handleTeamFile(w http.ResponseWriter, r *http.Request, user, convID, relPath string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	notFound := func() { http.Error(w, "not found", http.StatusNotFound) }
	// relPath arrives already percent-decoded once by net/http, which is the
	// same decoded form outputs are recorded in; it is never decoded again.
	if !store.ValidOutputPath(relPath) || isPrivateWorkspacePath(relPath) {
		notFound()
		return
	}
	// Gate 1: team-readable right now (the owner reading their own shared
	// chat resolves too, exactly like team-view). The light gate — the same
	// teamReadableClause, no transcript — because the download needs only
	// the rows discovery reads, not the whole history.
	ok, err := s.store.CanTeamRead(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		notFound()
		return
	}
	// Gates 2 and 3: a current output, not excluded. The rows the outputs are
	// derived from are a subset of the filtered transcript the teammate
	// reads (assistant text and reply boundaries — discoveryHistory).
	// A path older than the discovery bound (maxOutputReferences) is not in
	// this list and is refused like any non-output: the bound narrows what
	// can be downloaded, never widens it.
	history, err := s.discoveryHistory(r.Context(), convID, 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	outs, _, err := s.outputsFromHistory(r.Context(), convID, history)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	allowed := false
	for _, o := range outs {
		if o.Path == relPath {
			allowed = o.Shared
			break
		}
	}
	if !allowed {
		notFound()
		return
	}
	f, info, err := openWorkspaceFileNoFollow(tools.WorkspaceDirForConversation(convID), relPath)
	if err != nil {
		notFound()
		return
	}
	defer f.Close()

	name := path.Base(relPath)
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		// Never let ServeContent sniff: a sniffed type is how an
		// extensionless file becomes text/html.
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	// Belt and braces for a file that IS rendered inline (an image, a PDF):
	// even if a browser treats it as a document, it runs with no origin,
	// no scripts and no forms.
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "private, no-cache")
	if isActiveContentType(ctype) {
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleTeamLink serves GET /conversations/{id}/team-link — where a team
// link should land this signed-in caller. Statuses: owner | open |
// not_on_team (with the audience team only) | not_shared (with the chat's
// project only when the caller can see that project anyway). An unknown id is
// not_shared, so the route is no more an existence oracle than team-view.
func (s *Server) handleTeamLink(w http.ResponseWriter, r *http.Request, user, convID string) {
	tl, err := s.store.ResolveTeamLink(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := map[string]any{"status": tl.Status, "viewer_email": user}
	if tl.TeamID != "" {
		out["team_id"] = tl.TeamID
	}
	if tl.Project != nil {
		out["project"] = tl.Project
	}
	writeJSON(w, out)
}

// maxBranchCopyBytes bounds the bytes one teammate branch copies. The copy
// runs inside the branch request; a chat whose shared outputs exceed this
// still branches, and the files past the budget are recorded as withheld
// (named in the branch, not copied) rather than failing the branch.
const maxBranchCopyBytes int64 = 1 << 30

// copySharedOutputsIntoBranch copies every SHARED output of the source chat
// into the new branch's workspace at the same relative path — so the links in
// the copied transcript resolve against the brancher's own workspace — and
// returns what was copied and what was withheld (unchecked by the owner, or
// not copyable).
//
// The source is read with openWorkspaceFileNoFollow (no symlinks anywhere);
// the destination is written through an os.Root on the fresh workspace with
// O_EXCL, 0o644 files and 0o755 directories (the sandbox uid must read them).
// A destination path that would traverse one of the workspace's seeded
// bundle symlinks is refused by the root and the file is withheld.
//
// gate is asked again before EACH file: the copy can run for a while, and
// the snapshot that let the branch start does not keep the door open for the
// rest of it. It answers for one path: copy it, skip it (the owner unticked
// it since discovery — only that file is withheld), or close (the owner
// stopped sharing or archived, the brancher left the team, or the re-check
// itself failed — that file and every one not yet copied is withheld). nil
// means no re-check (tests of the copy mechanics alone).
//
// ctx bounds the whole copy (branchCopyTimeout): once it is done the file in
// flight is abandoned and removed, and every file not yet copied is withheld.
func copySharedOutputsIntoBranch(ctx context.Context, srcConvID, dstConvID string, outs []outputFile, gate func(path string) branchCopyDecision) (copied []store.BranchFile, withheld []string) {
	copied, withheld = []store.BranchFile{}, []string{}
	var shared []outputFile
	for _, o := range outs {
		if o.Shared {
			shared = append(shared, o)
		} else {
			withheld = append(withheld, o.Path)
		}
	}
	if len(shared) == 0 {
		return copied, withheld
	}
	dstDir, err := tools.EnsureWorkspaceDir(dstConvID)
	if err != nil {
		log.Printf("branch files: workspace for %s: %v", logSafeSlug(dstConvID), logSafe(err.Error()))
		for _, o := range shared {
			withheld = append(withheld, o.Path)
		}
		return copied, withheld
	}
	dst, err := os.OpenRoot(dstDir)
	if err != nil {
		log.Printf("branch files: open workspace for %s: %v", logSafeSlug(dstConvID), logSafe(err.Error()))
		for _, o := range shared {
			withheld = append(withheld, o.Path)
		}
		return copied, withheld
	}
	defer dst.Close()
	srcDir := tools.WorkspaceDirForConversation(srcConvID)
	var budget = maxBranchCopyBytes
	closed := false
	for _, o := range shared {
		if !closed && ctx.Err() != nil {
			closed = true
			log.Printf("branch files: copy budget for %s ran out; withholding the remaining files", logSafeSlug(dstConvID))
		}
		decision := branchCopyFile
		if !closed && gate != nil {
			decision = gate(o.Path)
		}
		if !closed && decision == branchCopyClosed {
			closed = true
			log.Printf("branch files: %s is no longer readable by the brancher; withholding the remaining files of %s", logSafeSlug(srcConvID), logSafeSlug(dstConvID))
		}
		if closed || decision == branchCopySkip {
			withheld = append(withheld, o.Path)
			continue
		}
		n, err := copyOneOutput(ctx, srcDir, dst, o.Path, budget)
		if err != nil {
			log.Printf("branch files: %q not copied into %s: %v", logSafeSlug(o.Path), logSafeSlug(dstConvID), logSafe(err.Error()))
			withheld = append(withheld, o.Path)
			continue
		}
		budget -= n
		copied = append(copied, store.BranchFile{Path: o.Path, Name: path.Base(o.Path), Size: n})
	}
	return copied, withheld
}

// branchCopyDecision is copySharedOutputsIntoBranch's per-file gate answer.
type branchCopyDecision int

const (
	branchCopyFile   branchCopyDecision = iota // still shared: copy it
	branchCopySkip                             // unticked since discovery: withhold this one
	branchCopyClosed                           // the chat closed to the brancher: withhold the rest
)

var errBranchCopyBudget = errors.New("branch copy budget exhausted")

// errBranchCopyShort is a copy that read fewer bytes than the file had when
// it was stat'd: the source was truncated (or replaced) mid-copy, so the
// bytes we have are not the file the owner shared. It is withheld, never
// copied as a silently truncated file.
var errBranchCopyShort = errors.New("source changed during copy")

// branchCopyAfterStat is a test seam run between the source stat and the
// copy; nil in production.
var branchCopyAfterStat func(srcPath string)

// branchCopySource is a test seam wrapping the source reader (a reader that
// stalls like a hung filesystem); nil in production.
var branchCopySource func(io.Reader) io.Reader

// branchCopyChunk is how much the copy moves between checks of ctx.
const branchCopyChunk = 1 << 20

// copyOneOutput copies one shared output into the branch workspace. It
// observes ctx: the copy runs in chunks with ctx checked between them, and a
// read or write that blocks on a stalled filesystem is abandoned when ctx is
// done — the call returns ctx's error at once, the partial destination is
// removed and the file is withheld. Both descriptors are closed when ctx is
// done (context.AfterFunc) so a blocked syscall that the platform lets a
// close interrupt returns, and the abandoned goroutine stops at its next
// chunk; one that cannot be interrupted only leaks that goroutine until the
// filesystem answers, never the branch request.
func copyOneOutput(ctx context.Context, srcDir string, dst *os.Root, rel string, budget int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	in, info, err := openWorkspaceFileNoFollow(srcDir, rel)
	if err != nil {
		return 0, err
	}
	// closeAll runs once, whichever side gets there first: the AfterFunc on
	// cancellation (interrupting a blocked read/write), or the deferred
	// close on return. out is registered under outMu once it is open.
	var closeOnce sync.Once
	var out *os.File
	var outMu sync.Mutex
	closeAll := func() {
		closeOnce.Do(func() {
			_ = in.Close()
			outMu.Lock()
			if out != nil {
				_ = out.Close()
			}
			outMu.Unlock()
		})
	}
	stop := context.AfterFunc(ctx, closeAll)
	defer func() {
		stop()
		closeAll()
	}()
	if info.Size() > budget {
		return 0, errBranchCopyBudget
	}
	if branchCopyAfterStat != nil {
		branchCopyAfterStat(filepath.Join(srcDir, filepath.FromSlash(rel)))
	}
	if dir := path.Dir(rel); dir != "." {
		if err := dst.MkdirAll(dir, 0o755); err != nil {
			return 0, err
		}
	}
	f, err := dst.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, err
	}
	outMu.Lock()
	out = f
	outMu.Unlock()
	if ctx.Err() != nil {
		// Cancelled between the open and the registration above: the
		// AfterFunc may already have run without seeing out.
		_ = f.Close()
		_ = dst.Remove(rel)
		return 0, fmt.Errorf("copy: %w", ctx.Err())
	}
	// Bounded by the size that was checked: a file still being written by
	// the owner's sandbox cannot push the copy past the budget.
	src := io.LimitReader(in, info.Size())
	if branchCopySource != nil {
		src = branchCopySource(src)
	}
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1) // buffered: an abandoned copy never blocks
	go func() {
		n, err := copyChunks(ctx, f, src)
		done <- result{n, err}
	}()
	var n int64
	select {
	case r := <-done:
		n, err = r.n, r.err
	case <-ctx.Done():
		// The copy may be stuck in a syscall; do not wait for it. Unlinking
		// is safe while it still holds the descriptor.
		_ = dst.Remove(rel)
		return 0, fmt.Errorf("copy: %w", ctx.Err())
	}
	outMu.Lock()
	out = nil
	outMu.Unlock()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	// io.Copy over a LimitReader reports an early EOF as success, so a
	// source truncated between the stat and the copy would land as a
	// shorter file. Require the full stat'd size.
	if err == nil && n != info.Size() {
		err = errBranchCopyShort
	}
	// An in-place rewrite of the same length passes the size check yet
	// leaves a file mixing old and new bytes. Re-stat the open descriptor:
	// any change in size or mtime since the pre-copy stat means the bytes
	// we read may not be one version of the file, so it is withheld.
	if err == nil {
		after, serr := in.Stat()
		switch {
		case serr != nil:
			err = serr
		case after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()):
			err = errBranchCopyShort
		}
	}
	if err != nil {
		_ = dst.Remove(rel)
		return 0, fmt.Errorf("copy: %w", err)
	}
	return n, nil
}

// copyChunks copies src to dst branchCopyChunk bytes at a time, checking ctx
// before every read so a slow (not stuck) filesystem stops at the budget.
func copyChunks(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, branchCopyChunk)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			n += int64(nw)
			if werr != nil {
				return n, werr
			}
			if nw != nr {
				return n, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// maxBranchFilesInNote bounds the injected first-turn note.
const maxBranchFilesInNote = 50

// appendBranchFilesBlock adds, on a teammate branch's FIRST turn only, a note
// telling the agent which files came with the branch and that transcript
// mentions of any other file did not. Without it the agent reads its copied
// transcript, sees a link to a file the owner withheld, and confidently tries
// to use a file it does not have. "First turn" is read from committed data
// (store.PendingBranchFilesAnnouncement): the note is due until a user
// message of the branch's own commits — and that message carries it as its
// injected context — so a failed turn or a crash never loses it. A failed
// read degrades to no note, never a failed turn.
func (s *Server) appendBranchFilesBlock(ctx context.Context, injected, convID string) string {
	origin, err := s.store.PendingBranchFilesAnnouncement(ctx, convID)
	if err != nil {
		log.Printf("branch files note for %s: %v", logSafeSlug(convID), logSafe(err.Error()))
		return injected
	}
	if origin == nil {
		return injected
	}
	return injected + branchFilesNote(origin)
}

// branchFilesNote renders the first-turn note for one branch origin.
func branchFilesNote(o *store.BranchOrigin) string {
	var b strings.Builder
	b.WriteString("\n\n---\n**This chat is a branch of a teammate's shared chat.** ")
	if len(o.CopiedFiles) == 0 {
		b.WriteString("No files came with it: the conversation above was copied, but none of the files it mentions are in this workspace. If the user needs one, ask them for it.\n")
		return b.String()
	}
	b.WriteString("These files came with it and are in this chat's workspace (link them by the same relative path):\n")
	for i, f := range o.CopiedFiles {
		if i == maxBranchFilesInNote {
			fmt.Fprintf(&b, "- …and %d more — use `bash ls` to enumerate them.\n", len(o.CopiedFiles)-maxBranchFilesInNote)
			break
		}
		fmt.Fprintf(&b, "- `%s` (%s)\n", f.Path, humanSize(f.Size))
	}
	b.WriteString("Any other file the conversation above mentions was not shared and is NOT in this workspace; do not try to open it — ask the user if it is needed.\n")
	return b.String()
}
