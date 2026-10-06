// Read-only public conversation sharing (#226).
//
// An authenticated owner issues an unguessable share token for one of their
// conversations (POST /conversations/{id}/share) and hands the link to anyone.
// The public GET /shared/{token} returns a read-only snapshot — title, model,
// and the message thread — with the conversation id and author email
// deliberately omitted. The token is 256 bits of crypto/rand: token entropy is
// the confidentiality guarantee, and a per-token rate limit is the abuse gate.
//
// The public endpoint is token-gated (shared secret, so only the trusted Next
// proxy reaches it) but identity-less — the share token in the path is the
// authorization. Revoking (DELETE) NULLs the token; expiry is enforced
// server-side in the store lookup.

package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// sharedReadsPerMinutePerToken bounds GET /shared/{token} per share token —
// generous for real viewers of a popular link, a hard ceiling against scraping
// or DDoS amplification of a single share. Keyed by token (not IP) because the
// endpoint sits behind the Next proxy — but a bucket is only created AFTER the
// token resolves in the store (#571), mirroring the webhook path's
// create-bucket-after-auth pattern, so an unauthenticated flood of random
// tokens cannot grow the limiter map (memory DoS) and the map stays bounded by
// the number of real share links.
const sharedReadsPerMinutePerToken = 120

// shareTokenBytes is the entropy of a share token before base64url encoding.
// 32 bytes = 256 bits, brute-force infeasible.
const shareTokenBytes = 32

// handleConversationShare issues (or rotates) the public read-only share token
// for a conversation the caller owns. Body is optional:
//
//	{ "expires_at": <unix seconds> }   // omitted / null = never expires
//
// Returns 201 with {"url": "/shared/<token>", "token": "<token>"}.
func (s *Server) handleConversationShare(w http.ResponseWriter, r *http.Request, convID, user string) {
	// Confirm the conversation exists and belongs to the caller before minting a
	// token (Get is user-scoped, so a foreign/unknown id yields nil → 404).
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var body struct {
		ExpiresAt *int64 `json:"expires_at"`
	}
	// Body is optional; an empty body decodes to the zero value. A MALFORMED
	// body is refused, not ignored: swallowing the error used to mint a
	// non-expiring link for a client that asked for an expiring one — the
	// opposite of the caller's intent, on a security-relevant default.
	if !decodeOptionalJSONBody(w, r, &body) {
		return
	}
	if body.ExpiresAt != nil && *body.ExpiresAt <= time.Now().Unix() {
		http.Error(w, "expires_at must be a unix timestamp in the future", http.StatusBadRequest)
		return
	}

	raw := make([]byte, shareTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "token generation failed", http.StatusInternalServerError)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	if err := s.store.SetShareToken(r.Context(), user, convID, token, body.ExpiresAt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"url":   "/shared/" + token,
		"token": token,
	})
}

// handleConversationUnshare revokes sharing for a conversation the caller owns.
// A non-owned/unknown id is 404 (the ownership pre-check via Get, mirroring the
// share path); for an owned conversation it is idempotent — revoking when
// already unshared still returns 204, so the UI can call it without checking
// current state.
func (s *Server) handleConversationUnshare(w http.ResponseWriter, r *http.Request, convID, user string) {
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.store.RevokeShareToken(r.Context(), user, convID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleConversationShareWithTeam toggles a conversation's team-visibility flag
// (#237) — the OWNER opting their thread into (or out of) read-only visibility
// for their team. This is distinct from public share tokens (#226): it names
// the team as its audience, never mints a public link, and is the only path by
// which one teammate's conversation becomes readable by another.
//
// Body: { "visible": bool, "unshared_paths"?: [string], "listed_paths"?: [string] } (visible defaults to
// false = un-share). The ownership check is first, so a non-owned/unknown id
// is 404; the store enforces ADR-0057's pairing, so sharing without a team or
// without a team-shared project to appear in is 409 with the reason.
//
// A team share carries the chat's outputs (ADR-0079). unshared_paths, honored
// only with visible=true, is the share dialog's checklist applied as one
// decision — over exactly the paths the checklist SHOWED (listed_paths): each
// listed path is excluded when it is in unshared_paths and shared otherwise,
// and an exclusion for a path the checklist did not show is left untouched,
// so a file missing on disk (or beyond the discovery bound) when the dialog
// loaded stays held back if it comes back. A client that omits listed_paths
// is taken to have shown the chat's current outputs. It is written BEFORE the flag
// flips, so there is no instant in which the chat is shared with a file the
// owner just unchecked; if the share is then refused (409) the owner's file
// choices are kept, which is harmless — they are theirs and only matter once
// the chat is shared. Omitted, the owner's earlier choices stand, which is
// what makes the fast paths (row pill, card, toast) safe to be one click.
//
// The response reports the state that was STORED plus the file counts the
// toasts quote: shared_files is the outputs now shared — or, for an unshare,
// the outputs that WERE shared and just stopped ("3 shared files stopped being
// shared") — and total_files every output.
func (s *Server) handleConversationShareWithTeam(w http.ResponseWriter, r *http.Request, convID, user string) {
	var body struct {
		Visible       bool      `json:"visible"`
		UnsharedPaths *[]string `json:"unshared_paths"`
		ListedPaths   *[]string `json:"listed_paths"`
	}
	// A malformed body must not fall through to visible=false and silently
	// UN-share the chat the caller was trying to share.
	if !decodeOptionalJSONBody(w, r, &body) {
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
	if body.Visible && body.UnsharedPaths != nil {
		var listed []string
		if body.ListedPaths != nil {
			listed = *body.ListedPaths
		} else {
			current, _, err := s.ownerOutputs(r.Context(), convID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, o := range current {
				listed = append(listed, o.Path)
			}
		}
		if err := s.store.ApplyOutputChecklist(r.Context(), user, convID, listed, *body.UnsharedPaths); err != nil {
			switch {
			case errors.Is(err, store.ErrInvalidOutputPath):
				http.Error(w, "invalid unshared_paths", http.StatusBadRequest)
			case errors.Is(err, store.ErrConversationNotFound):
				http.Error(w, "not found", http.StatusNotFound)
			default:
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	// Counted before the flag flips: for an unshare, what is about to stop
	// being shared is what the response must report.
	outs, _, err := s.ownerOutputs(r.Context(), convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stored, err := s.store.SetConversationTeamVisible(r.Context(), user, convID, body.Visible)
	if err != nil {
		// A chat with no team, no team-shared project to appear in, or that
		// is archived cannot be shared — 409 with the store's sentence, which
		// names which it is. This is a refusal of the request, not a server
		// fault.
		if errors.Is(err, store.ErrNoTeamToShareWith) || errors.Is(err, store.ErrNoTeamShareHome) ||
			errors.Is(err, store.ErrArchivedNotShareable) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		// No conversation with this id belongs to the caller → 404, matching
		// the share path.
		if errors.Is(err, store.ErrConversationNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sharedFiles := countShared(outs)
	if stored {
		// Retires this person's getting-started card in the project. Display
		// state: a failure is logged, never a failed share.
		if err := s.store.MarkProjectSharedChat(r.Context(), conv.ProjectID, user); err != nil {
			log.Printf("share-with-team: record has_shared_chat for %s: %v", logSafeSlug(convID), logSafe(err.Error())) //nolint:gosec // G706: logSafe strips CR/LF from the id and the error text.
		}
	} else if !conv.TeamVisible {
		// Unsharing a chat that was not shared stops nothing.
		sharedFiles = 0
	}
	// The STORED state, never the requested one: a client that is told
	// team_visible:true must be able to believe it.
	writeJSON(w, map[string]any{
		"team_visible": stored,
		"shared_files": sharedFiles,
		"total_files":  len(outs),
	})
}

// teamViewResponse is the team-view body: the transcript snapshot plus what a
// teammate needs to render the chat's files and their own branch of it.
type teamViewResponse struct {
	*store.TeamSharedConversation
	// ProjectID / ProjectName are the breadcrumb back to the project home.
	// The snapshot's own ProjectID stays json:"-" (it is read for Branch);
	// this is the deliberate, named exposure — the viewer reached the chat
	// through that project, so its id and name are already theirs to know.
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	// Files is every output: shared ones are live downloads through the
	// team-files route, the rest render as locked names. A withheld file's
	// size and date are zeroed — its NAME is already in the transcript, but
	// nothing else about it was shared.
	Files []outputFile `json:"files"`
	// FilesTruncated is true when the transcript references more distinct
	// files than discovery considers (maxOutputReferences): Files holds the
	// most recent ones only, and the older references render locked.
	FilesTruncated bool `json:"files_truncated"`
	// ViewerBranch is the caller's own most recent branch of this chat, or
	// null — "You branched this", and whether messages arrived since.
	ViewerBranch *store.ViewerBranch `json:"viewer_branch"`
}

// handleConversationTeamView serves GET /conversations/{id}/team-view — the
// read-only transcript of a chat a TEAMMATE shared with the team (ADR-0057).
//
// Same rendering as a public share link, different door: membership of the
// team plus the owner's per-chat opt-in, instead of a capability URL. The
// snapshot carries the owner's email (the viewer is told whose chat this is)
// and the project it lives in, so the viewer's one forward action — Branch —
// can file the fork back into the same project.
//
// Since ADR-0079 it also lists the chat's OUTPUTS with their share state;
// the bytes of a shared one are served by GET /conversations/{id}/team-files,
// which re-checks every gate. Uploads are never outputs and never listed.
//
// A chat the caller may not read is 404, indistinguishable from one that does
// not exist — team membership is never probeable from here.
func (s *Server) handleConversationTeamView(w http.ResponseWriter, r *http.Request, convID, user string) {
	snap, err := s.store.GetTeamVisibleConversation(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if snap == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	files, filesTruncated, err := s.outputsFromHistory(r.Context(), snap.ID, outputHistoryOf(snap))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	isOwner := strings.EqualFold(snap.OwnerEmail, user)
	if !isOwner {
		for i := range files {
			if !files[i].Shared {
				files[i].Size, files[i].ModifiedAt = 0, 0
			}
		}
	}
	resp := teamViewResponse{TeamSharedConversation: snap, ProjectID: snap.ProjectID, Files: files, FilesTruncated: filesTruncated}
	if snap.ProjectID != "" {
		if p, perr := s.store.GetProject(r.Context(), snap.ProjectID); perr == nil && p != nil {
			resp.ProjectName = p.Name
		}
	}
	if !isOwner {
		branches, berr := s.store.ViewerBranches(r.Context(), user, []string{snap.ID})
		if berr != nil {
			http.Error(w, berr.Error(), http.StatusInternalServerError)
			return
		}
		if vb, ok := branches[snap.ID]; ok {
			resp.ViewerBranch = &vb
		}
	}
	writeJSON(w, resp)
}

// handleSharedConversation serves the public read-only snapshot for a share
// token. Token-gated (shared secret) but identity-less; the token in the path
// is the authorization. Per-token rate-limited AFTER the token resolves;
// returns 404 for an unknown, revoked, or expired token (indistinguishable, so
// a probe can't tell which).
func (s *Server) handleSharedConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.Trim(strings.TrimPrefix(r.URL.Path, "/shared/"), "/")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	// Resolve the token BEFORE consulting the rate limiter (#571): a bucket
	// keyed on an unvalidated, attacker-chosen string is a memory lever (every
	// distinct random token would persist a bucket for up to a day) and
	// throttles nothing, since each fresh token starts a fresh window. Token
	// entropy (256-bit) remains the confidentiality guarantee; the per-token
	// limit below throttles scraping of a link that actually exists.
	conv, err := s.store.GetConversationByShareToken(r.Context(), token, time.Now().Unix())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if ok, _ := s.shareRL.Allow(token); !ok {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	writeJSON(w, conv)
}
